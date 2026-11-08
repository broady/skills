# Production Go Review Calibration

Use this reference when reviewing Go code, triaging third-party findings, or
deciding whether a suspected production-go violation is real.

The goal is actionable findings with accurate priority and confidence, not
maximum finding count or mechanical rule compliance. Rules generate hypotheses;
the code, its callers, and verified deployment behavior establish impact.

## Review Workflow

For each suspected issue:

1. Trace the relevant code, callers, ownership, lifecycle, and tests. For a diff,
   identify whether the change introduced the issue; separate unrelated
   pre-existing risks. For a full audit, assess existing code as requested.
2. Identify a plausible trigger and the mechanism and consequence: for a data
   race name the unsynchronized reader, writer, and shared state; for a leak
   name the owner, stop and wait paths, and resource; for an error identify the
   producer, handling boundary, and caller-visible result.
3. Check mitigations and contracts, including synchronization, cancellation,
   deadlines, call ordering, and relevant deployment behavior. Do not assume a
   protection exists or does not exist when evidence is unavailable.
4. Run targeted tests or a reproducer when useful and permitted. A passing race
   test covers only exercised interleavings. A static trace can confirm a defect;
   a reproducer is evidence, not a prerequisite.
5. Decide the **status**: established defect, plausible but unverified risk,
   design/policy observation, or refuted claim. Record what remains unknown.
   Assess **confidence** and **priority** separately below.

## Priority and confidence

Priority communicates action, based on consequence, reachability and scope, and
urgency in this review. These are guidance, not an automatic formula:

- **P0** — release-blocking or critical failure demanding immediate action.
- **P1** — urgent, substantial impact under a plausible trigger; fix or
  investigate next.
- **P2** — actionable defect with meaningful but less urgent impact.
- **P3** — low-impact issue still worth fixing.

A plausible high-impact risk can merit urgent investigation even if its
reachability is unverified. Label it **unverified** and state the evidence
needed; do not present it as a confirmed defect or lower its priority solely
because certainty is incomplete. If the trigger or consequence is too
speculative to assess, do not invent a priority; identify the unknown or omit
it as appropriate. Project-specific release gates or compliance obligations may
require a fix independently of incident priority: identify the actual obligation
and report it separately, not as proof of a production failure.

Confidence expresses support for the claim, independently of its category or
priority:

- **High** — the claim's relevant behavior and conditions are established by
  code, a targeted test, or other direct evidence.
- **Medium** — a specific failure mechanism is supported, but some material
  assumption remains unverified.
- **Low** — material reachability, mitigation, or consequence is unknown;
  investigate rather than asserting the failure.

A design hazard can have high confidence; a plausible severe defect can have
medium or low confidence. A refuted claim is not a finding and gets no priority.
Report policy/style deviations separately and only when useful or requested;
never promote them solely because a skill rule calls them an invariant.

For actionable findings use `[P1] Title — path:line`, followed by the trigger,
consequence, evidence and confidence, and a targeted remedy if useful. Order
established defects and significant unverified risks by priority; clearly label
the latter and their unknowns. Optional design/policy observations come separately. If no actionable findings
exist, say so; note material test gaps or uncertainty without inventing defects.
Follow a user-requested format when compatible.

### The "so what?" test

Before assigning priority, answer these questions honestly:

1. **Does this break today, or only if someone changes something?** A type-level
   API that permits misuse but whose current callers are all correct is a design
   hazard, not an established live defect. Report the actual risk and note
   the fragility where useful; don't inflate priority for hypothetical callers.

2. **Does verified process exit handle this?** Goroutine leaks, unfinished
   shutdown steps, and leaked tickers may be non-issues when the actual deployment
   guarantees process exit after Stop(). Do not assume an orchestrator or process
   lifecycle that you have not verified. These issues matter in library/embedded
   usage (test harnesses that create and destroy subsystems without process exit,
   custom binaries that restart components in-process). State which context the
   finding applies to; prioritize its actual consequence in that context.

3. **Is the infrastructure already handling this?** Check for a higher-level
   bound before reporting an unbounded call, but credit one only after
   verifying which phase it covers and that it applies on this path. A caller's
   context deadline bounds the whole request, including the body read, if the
   body is read under that context. TCP keepalive detects dead peers, not a live
   peer that stalls. A load balancer idle timeout may not cut a response that
   trickles. DNS TTL bounds nothing in flight. If the verified bound is "30s
   caller deadline," say so and rate accordingly; if nothing covers a stalled
   body read, it is a real hang.

4. **What actually happens when this fires?** "Goroutine leak" sounds scary but
   if the goroutine is lightweight, bounded to one per component, and exits
   within seconds of context cancellation, the operational impact is negligible.
   Name the concrete consequence: does it exhaust memory? Hold a DB connection?
   Block a port from rebinding? Corrupt data? If you can't name a concrete
   consequence, describe the unknown rather than inventing an impact.

5. **Is this a code bug or a rule violation?** A real bug (response body not
   closed, error silently discarded, nil pointer reachable) is worth reporting
   regardless of priority. A rule violation where the code works correctly
   (missing mutex on a field only accessed from one goroutine, HTTP client
   without Timeout but behind a context with deadline) is a design hazard at
   best. Don't conflate "doesn't follow best practice" with "will break."

## False-Positive Checks

Do not report a data race where verified synchronization covers all relevant
accesses. If cancellation happens without waiting, call it a lifecycle race,
stale goroutine risk, or shutdown ordering bug.

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

Do not flag generated code for hand-written production-go style. Review generated
behavior when it creates a correctness issue, and make fixes through the source
or generator followed by reproducible regeneration. Also review hand-written
wrapper code around generated output normally.

For goroutines that outlive shutdown, verify process and component lifetimes
and identify actual consequences, such as retained resources, stale work, or
duplicate effects. An imminent process exit does not undo incorrect side effects
or prove durable work was completed.

Do not prioritize a missing HTTP client timeout until you've applied the
phase-by-phase check in "so what?" item 3. A load balancer or keepalive counts
only if you verified it covers a stalled response on this path. If the call is
already bounded end to end by a context deadline, the missing client-level
timeout is defense-in-depth, not an unbounded call.

For a type-level API hazard, distinguish verified current behavior from possible
misuse. Describe the protecting contract and its limitations; assess priority
from actual impact and scope rather than the category alone.

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

In the final review, distinguish established defects from significant unverified
risks and optional observations while ordering actionable items by priority. Do not list refuted claims unless the user asks
for triage history.
