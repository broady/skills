# Production Go Review Calibration

Use this reference when reviewing Go code, triaging third-party findings, or
deciding whether a suspected production-go violation is real.

The review goal is not to maximize finding count. The goal is to report
reachable production failure modes with accurate severity and confidence.

## Review Workflow

For each suspected issue:

1. Read the cited code and its owner: constructor, start path, stop path,
   callers, and tests.
2. Identify the concrete trigger: request, role transition, shutdown, retry,
   malformed input, slow peer, or configuration.
3. Prove the failure mode:
   - Data race: name the unsynchronized writer, reader, and shared field.
   - Goroutine leak: name the owner, cancellation path, wait path, and bound.
   - Resource exhaustion: name the unbounded resource and input/load that grows it.
   - Swallowed error: name the error producer, boundary, and caller-visible lie.
   - Ownership bug: name the mutable data crossing the boundary and who can mutate it.
4. Check existing synchronization and contracts before reporting. Look for
   mutexes, atomics, channel close ownership, context cancellation, typed
   domain errors, generated-code boundaries, and documented call ordering.
5. Run targeted tests when useful. Passing `go test -race` only proves the
   exercised interleavings; it does not clear static races in untested paths.
6. Classify the finding with both confidence and severity.

## Confidence

- **Confirmed**: current code has a reachable failure mode.
- **Partial**: the pattern is unsafe, but current wiring or callers limit reach.
- **Design hazard**: safe today only because of undocumented call ordering,
  limited callers, or convention rather than an enforced API contract.
- **Not real**: synchronization, ownership, or an existing contract eliminates
  the claimed bug.

Use `partial` or `design hazard` instead of inflating severity when the code is
currently safe but fragile.

## Severity

- **Critical**: can corrupt committed data, make successful writes silently lie,
  accumulate indefinitely across shutdown/role changes, or take down the process
  under ordinary production conditions.
- **High**: real failure mode under plausible production load, common error
  paths, or normal lifecycle transitions.
- **Medium**: real but bounded, uncommon, dependent on unusual inputs, or
  recoverable without data loss.
- **Low**: maintainability, style, weak API shape, or future-proofing unless tied
  to a specific failure mode.

Severity follows impact and reachability, not just rule violation.

### The "so what?" test

Before assigning severity, answer these questions honestly:

1. **Does this break today, or only if someone changes something?** A type-level
   API that permits misuse but whose current callers are all correct is a design
   hazard, not a High. Don't inflate severity based on hypothetical future
   callers — report the actual risk and note the fragility.

2. **Does process exit handle this?** Goroutine leaks, unfinished shutdown
   steps, and leaked tickers are non-issues in orchestrated deployments where
   the process exits after Stop(). They matter in library/embedded usage (test
   harnesses that create and destroy subsystems without process exit, custom
   binaries that restart components in-process). State which context the finding
   applies to. A "goroutine leak at shutdown" that only matters for test hygiene
   is Low, not High.

3. **Is the infrastructure already handling this?** HTTP clients without explicit
   timeouts are still bounded by TCP keepalive, load balancer timeouts, DNS TTL,
   and context deadlines from callers. Check whether a higher-level timeout
   exists before reporting an unbounded call. If the real-world bound is "30s
   from the load balancer," say so and rate accordingly.

4. **What actually happens when this fires?** "Goroutine leak" sounds scary but
   if the goroutine is lightweight, bounded to one per component, and exits
   within seconds of context cancellation, the operational impact is negligible.
   Name the concrete consequence: does it exhaust memory? Hold a DB connection?
   Block a port from rebinding? Corrupt data? If you can't name a concrete
   production consequence, it's Low.

5. **Is this a code bug or a rule violation?** A real bug (response body not
   closed, error silently discarded, nil pointer reachable) is worth reporting
   regardless of severity. A rule violation where the code works correctly
   (missing mutex on a field only accessed from one goroutine, HTTP client
   without Timeout but behind a context with deadline) is a design hazard at
   best. Don't conflate "doesn't follow best practice" with "will break."

## False-Positive Checks

Do not call a locked data structure a data race. If cancellation happens without
waiting, call it a lifecycle race, stale goroutine risk, or shutdown ordering
bug.

Do not flag package-level sentinel errors just because they are `var`. Rule 1
allows sentinel errors; report them only if code actually reassigns them or
exposes a meaningful mutation path.

Do not report a setter as a live race unless current code can call it after
concurrent readers/writers start. If startup ordering currently prevents the
race, classify it as a design hazard and explain the ordering dependency.

Do not report raw SQL construction as injection unless untrusted input reaches
the SQL text. If current callers pass only constants and bind user values as
parameters, classify it as an API hazard.

Do not treat `go test -race` passing as a refutation unless the test exercises
the exact concurrent path and interleaving. Mention residual risk when tests are
not covering role churn, shutdown, slow clients, or stuck I/O.

Do not flag generated code for production-go style unless hand-written wrapper
code around it creates the issue.

Do not report a goroutine leak at shutdown as High unless the goroutine holds a
scarce resource (DB connection, open port, file lock) or the code is used as a
library where the process does not exit after Stop(). A goroutine that outlives
Stop() by a few seconds in a process that's about to exit is Low — name the
specific resource it holds or downgrade.

Do not report a missing HTTP client timeout as High unless you've checked that
no higher-level timeout exists (context deadline, load balancer, infrastructure
keepalive). If the call is already bounded by a context with a deadline, the
missing client-level timeout is defense-in-depth (Medium at most), not an
unbounded call.

Do not report a type-level API hazard (e.g., unsynchronized field that all
current callers access from one goroutine) at the same severity as a live bug.
The type permits misuse, but the code works. Rate as design hazard with a note
about which invariant protects it today.

These false-positive checks overlap deliberately with Phase 3.5 triage in
[audit.md](audit.md). This file calibrates single reviews and per-finding
verification; audit.md applies the same lens at the whole-audit level.

## Wording

Prefer precise labels:

- `data race` only when unsynchronized concurrent access exists.
- `lifecycle race` when stale goroutines can act after cancellation or role loss.
- `goroutine leak` when a goroutine can outlive its owner with no wait path.
- `unbounded concurrency` when work can grow with input/load without a limit.
- `silent failure` when callers observe success after an unexpected error.
- `design hazard` when safety depends on convention rather than enforced API.

In the final review, put confirmed production risks first. Then list partial
findings and false flags separately so the user can act without re-triaging.
