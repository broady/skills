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
