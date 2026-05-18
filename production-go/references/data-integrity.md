# Data Integrity

Patterns for crash-safe file operations, write verification, conflict detection,
and transaction safety extracted from Syncthing, restic, pgx, and TiDB.

## Contents

- [1. Atomic File Writes (Six-Step Pattern)](#1-atomic-file-writes-six-step-pattern)
- [2. Verify-After-Write (restic)](#2-verify-after-write-restic)
- [3. Pre-Modification Safety Check (Syncthing)](#3-pre-modification-safety-check-syncthing)
- [4. Content-Aware Conflict Detection (Syncthing)](#4-content-aware-conflict-detection-syncthing)
- [5. Transaction Safety (pgx Patterns)](#5-transaction-safety-pgx-patterns)
- [6. Statement-Level Staging (TiDB)](#6-statement-level-staging-tidb)
- [Decision Table](#decision-table)
- [Anti-Patterns](#anti-patterns)

## 1. Atomic File Writes (Six-Step Pattern)

The full POSIX crash-safety protocol. A crash at any point leaves either the
old file or the new file intact, never a partial write.

```go
func AtomicWrite(path string, data []byte, perm os.FileMode) (retErr error) {
	dir := filepath.Dir(path)

	// 1. Create temp file in target directory (same filesystem).
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = tmp.Close()        //nolint:errcheck // best-effort cleanup
			_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		}
	}()

	// 2. Write data and verify byte count.
	n, err := tmp.Write(data)
	if err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("short write: %d of %d bytes", n, len(data))
	}

	// 3. fsync the file.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	// 4. Close the file.
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// 5. Rename temp to final (atomic on POSIX).
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp to final: %w", err)
	}

	// 6. fsync the parent directory (required on Linux/ext4).
	dirFd, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent dir for sync: %w", err)
	}
	defer dirFd.Close()
	return dirFd.Sync()
}
```

Temp file must be on the same filesystem (rename is not cross-device atomic).
Directory sync ensures the directory entry is durable after power loss.

---

## 2. Verify-After-Write (restic)

After encrypting data, immediately decrypt and re-hash to verify the
ciphertext. Catches hardware bit-flips and software bugs at write time, not
during a future restore when the original data is gone.

```go
func (r *Repo) SaveAndVerify(ctx context.Context, data []byte) (ID, error) {
	id := sha256.Sum256(data)
	ciphertext := r.key.Encrypt(data)
	if err := r.backend.Save(ctx, id, ciphertext); err != nil {
		return ID{}, fmt.Errorf("save blob %x: %w", id[:8], err)
	}

	// Read back and verify round-trip integrity.
	stored, err := r.backend.Load(ctx, id)
	if err != nil {
		return ID{}, fmt.Errorf("load for verify %x: %w", id[:8], err)
	}
	plaintext, err := r.key.Decrypt(stored)
	if err != nil {
		return ID{}, fmt.Errorf("decrypt for verify %x: %w", id[:8], err)
	}
	if sha256.Sum256(plaintext) != id {
		return ID{}, fmt.Errorf("verify %x: hash mismatch after round-trip", id[:8])
	}
	return id, nil
}
```

**When to use:** backup systems, data archival -- any path where corruption
discovered later is catastrophic.

---

## 3. Pre-Modification Safety Check (Syncthing)

Before modifying or deleting a file, compare on-disk state against the
database. If they differ, defer the operation and schedule a rescan. Prevents
overwriting local changes the system has not detected yet.

```go
func (f *FileManager) SafeToModify(ctx context.Context, path string, expected FileInfo) error {
	current, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && expected.Deleted {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if current.Size() != expected.Size || !current.ModTime().Equal(expected.ModTime) {
		f.scheduleRescan(ctx, path)
		return fmt.Errorf("file %s modified outside system: deferring", path)
	}

	hash, err := f.hashFile(ctx, path)
	if err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}
	if hash != expected.BlocksHash {
		f.scheduleRescan(ctx, path)
		return fmt.Errorf("file %s content hash mismatch: deferring", path)
	}
	return nil
}
```

**Principle:** never assume the database is authoritative. The filesystem is
ground truth. Check before mutating.

---

## 4. Content-Aware Conflict Detection (Syncthing)

Do not rely solely on timestamps or version vectors. Compare content hashes to
eliminate false conflicts from clock skew and independent identical changes.

```go
func IsConflict(local, incoming FileVersion) bool {
	if local.BlocksHash == (Hash{}) || incoming.BlocksHash == (Hash{}) {
		return false // create or delete, not a conflict
	}
	if incoming.PreviousBlocksHash == local.BlocksHash {
		return false // fast-forward: incoming is based on our content
	}
	if local.BlocksHash == incoming.BlocksHash {
		return false // same content reached independently
	}
	return true // concurrent versions with different content
}
```

---

## 5. Transaction Safety (pgx Patterns)

### Connection poisoning

Kill the connection on failed `BEGIN`/`COMMIT`/`ROLLBACK` -- state is
indeterminate.

```go
func (tx *Tx) Commit(ctx context.Context) error {
	_, err := tx.conn.Exec(ctx, "COMMIT")
	if err != nil {
		tx.conn.die(fmt.Errorf("commit failed: %w", err))
	}
	return err
}
```

### Pool release guard

Never return a mid-transaction connection to the pool.

```go
func (p *Pool) releaseConn(conn *Conn) {
	if conn.TxStatus() != 'I' { // 'I' = idle (not in transaction)
		conn.die(errors.New("connection released while in transaction"))
		return
	}
	p.idleConns <- conn
}
```

### SafeToRetry

Only retry if no data was sent to the server. Once bytes are on the wire, the
server may have partially processed the request.

```go
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	if err := c.sendQuery(ctx, sql, args...); err != nil {
		return nil, &PgError{err: err, safeToRetry: true} // never sent
	}
	rows, err := c.readResult(ctx)
	if err != nil {
		return nil, &PgError{err: err, safeToRetry: false} // partially sent
	}
	return rows, nil
}
```

### MaxConnLifetimeJitter

Prevent thundering herd reconnection when all connections expire together.

```go
func newConn(cfg PoolConfig) *Conn {
	jitter := time.Duration(rand.Int64N(int64(cfg.MaxConnLifetimeJitter)))
	return &Conn{
		createdAt: time.Now(),
		lifetime:  cfg.MaxConnLifetime + jitter,
	}
}
```

### BeginFunc pattern

Automatic commit on success, rollback on error, with deferred safety net.

```go
func BeginFunc(ctx context.Context, db *Pool, fn func(tx *Tx) error) (retErr error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if retErr != nil {
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if rbErr := tx.Rollback(rbCtx); rbErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
			}
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
```

---

## 6. Statement-Level Staging (TiDB)

Stage mutations in a per-statement buffer within a transaction. Flush on
success, discard on failure. Provides statement-level atomicity inside a
larger transaction.

```go
type StmtBuffer struct {
	tx      *Transaction
	pending map[string][]byte
}

func (s *StmtBuffer) Set(key string, value []byte) { s.pending[key] = value }

func (s *StmtBuffer) Get(key string) ([]byte, bool) {
	if v, ok := s.pending[key]; ok {
		return v, true
	}
	s.tx.mu.Lock()
	defer s.tx.mu.Unlock()
	v, ok := s.tx.committed[key]
	return v, ok
}

// Flush promotes staged mutations to the transaction on statement success.
func (s *StmtBuffer) Flush() {
	s.tx.mu.Lock()
	defer s.tx.mu.Unlock()
	for k, v := range s.pending {
		s.tx.committed[k] = v
	}
	s.pending = nil
}

// Discard drops staged mutations on statement failure.
func (s *StmtBuffer) Discard() { s.pending = nil }
```

---

## Decision Table

| Situation | Pattern | Key property |
|-----------|---------|--------------|
| Writing config/state files | Atomic file write (6-step) | Crash leaves valid file |
| Backup/archival writes | Verify-after-write | Catches bit-flips at write time |
| Sync engine modifying local files | Pre-modification safety check | Never overwrites undetected changes |
| Distributed file sync conflicts | Content-aware conflict detection | Eliminates false conflicts |
| Database transaction errors | Connection poisoning + BeginFunc | No leaked mid-transaction connections |
| Retry after database error | SafeToRetry check | Only retry if no data sent |
| Connection pool expiry | MaxConnLifetimeJitter | Prevents thundering herd |
| Multi-statement rollback | Statement-level staging | Per-statement atomicity |

---

## Anti-Patterns

- **Non-atomic file writes.** Writing directly to the target. A crash mid-write
  leaves a corrupt file. Always use temp-file + rename.
- **Missing fsync.** Rename without fsync. Data reaches the directory entry but
  not the disk. Power loss loses the data.
- **Missing directory fsync.** File fsync without directory fsync on Linux. The
  rename is not durable until the directory entry is flushed.
- **Cross-device rename.** `os.Rename` across filesystems is not atomic. Create
  the temp file in the target directory.
- **Trust-the-database.** Assuming the database matches the filesystem. Verify
  on-disk state before destructive operations.
- **Timestamp-only conflict detection.** Clock skew causes false positives and
  missed conflicts. Use content hashes.
- **Returning mid-transaction connections to pool.** Next borrower inherits
  uncommitted state. Check `TxStatus` before release.
- **Retrying after partial send.** If bytes were sent, the server may have
  partially executed. Only retry when `SafeToRetry` is true.
- **Synchronized connection expiry.** All connections expire simultaneously,
  causing a reconnection storm. Add jitter to lifetime.
- **Manual Commit/Rollback.** Error-prone with panics and early returns. Use
  `BeginFunc` for automatic commit/rollback.
- **Verify only on read.** Discovering corruption during restore when original
  data is gone. Verify at write time.
