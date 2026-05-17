# Cursor-Based Iteration

For processing large result sets without loading everything into memory.

**Always prefer cursor-based (keyset) pagination** — offset-based pagination is
unstable if rows are inserted or deleted during iteration (you'll skip or
double-process items). Advance by the last-seen key:

```go
// Keyed constrains types that expose a cursor key for pagination.
type Keyed interface {
    GetID() int64
}

func IterateAll[T Keyed](ctx context.Context, q Querier, batchSize int, fn func(item T) error) error {
    var cursor int64
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }

        batch, err := fetchBatch[T](ctx, q, cursor, batchSize)
        if err != nil {
            return fmt.Errorf("fetch batch after id=%d: %w", cursor, err)
        }
        if len(batch) == 0 {
            return nil
        }
        for _, item := range batch {
            if err := fn(item); err != nil {
                return err
            }
        }
        cursor = batch[len(batch)-1].GetID()
    }
}

// fetchBatch is a sketch — replace the query and scan with your actual schema.
// In practice, use sqlc or a scan helper (e.g., pgx.CollectRows) to avoid
// hand-written row scanning.
func fetchBatch[T Keyed](ctx context.Context, q Querier, afterID int64, limit int) ([]T, error) {
    rows, err := q.QueryContext(ctx,
        `SELECT id, name, created_at FROM items WHERE id > $1 ORDER BY id ASC LIMIT $2`,
        afterID, limit,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var batch []T
    for rows.Next() {
        var item T
        // Replace with your actual Scan implementation.
        if err := scanRow(rows, &item); err != nil {
            return nil, err
        }
        batch = append(batch, item)
    }
    return batch, rows.Err()
}
```

If your table lacks a stable, ordered key (rare), fall back to offset-based
pagination — but document the instability risk and ensure the callback is
idempotent.

## Properties

- **Cursor-based by default** — stable under concurrent inserts/deletes.
- Respects context cancellation between batches.
- Batch size from configuration, not hardcoded.
- Processes items one at a time (back-pressure friendly).
