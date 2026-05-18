# Full-Repo Audit Workflow

A structured multi-pass review for auditing an existing Go codebase against the
production-go standard. The goal is reachable production failure modes with
accurate severity and confidence — not maximizing finding count.

Read [review.md](review.md) first. It calibrates severity, confidence,
false-positive checks, and wording. This document defines the workflow that
applies that calibration.

## When to Use

- **Full-repo audit**: new team inheriting a codebase, pre-production readiness
  review, periodic health check. Use the full workflow below.
- **Diff review**: reviewing a PR or recent changes. Use the single-pass
  process in the SKILL.md Review Mode section — this workflow is overkill for
  diffs.

## Overview

The audit is a fan-out/fan-in process, not a linear checklist:

1. **Survey** — map the repo into domain-scoped review units
2. **Domain sweeps** — parallel subagents review each unit against the relevant
   references
3. **Per-finding verification** — parallel subagents confirm or refute each
   suspected issue, with reproducer tests where possible
4. **Synthesis** — collect verified findings into a prioritized summary

Each phase produces findings that feed the next. Subagents are the primary
mechanism for parallelism and for keeping each review focused on a manageable
scope with the right domain context loaded.

---

## Phase 1: Survey & Partition

**Goal**: understand the repo shape and split it into reviewable units, each
mapped to the domain references that apply.

1. Scan the repo structure: packages, binaries, significant files.
2. For each package or file group, identify which domains apply:

   | Code does... | Domain reference |
   |---|---|
   | Spawns goroutines, uses channels, protects shared state | [concurrency.md](concurrency.md) |
   | Handles or maps errors at boundaries | [errors.md](errors.md) |
   | Sets up servers, shutdown, middleware | [server.md](server.md) |
   | Accesses a database, processes async work | [database.md](database.md) |
   | Defines public APIs, constructors, config | [design.md](design.md) |
   | Logging, metrics, tracing | [observability.md](observability.md) |

3. Group into review units: one subagent per domain × package-set. Split
   further if a domain area is large (e.g., a package with 20 files touching
   concurrency gets its own subagent, separate from a package with 3).

**Output**: a list of review units, each with: file set, domain reference(s),
and review focus.

**Subagent guidance**: the survey itself is a single-agent task. The
partitioning determines how many subagents Phase 2 spawns.

---

## Phase 2: Domain Sweeps

**Goal**: each subagent reviews its assigned file set against its domain
reference(s), producing findings with initial classifications.

Spawn one subagent per review unit. Each subagent receives:

- The file set (package paths or file list)
- The relevant domain reference(s) to read
- [review.md](review.md) for calibration
- The production-go checklist (Tier 1 safety rules, Tier 2 only when
  correctness is implicated)
- Instruction to focus on correctness, not style — per the Existing Codebases
  section for legacy repos

Each subagent scans its files and records suspected issues. Each finding
should include:

- **findingId**: unique identifier (e.g., `pkg-function-category`)
- **location**: file, function, line range
- **category**: data race, lifecycle race, goroutine leak, unbounded
  concurrency, silent failure, ownership bug, boundary contract, design hazard
- **trigger**: what production condition reaches this code (request, shutdown,
  role transition, retry, malformed input, slow peer, config change)
- **severity**: Critical / High / Medium / Low
- **confidence**: Confirmed / Partial / Design hazard
- **summary**: one-sentence description of the suspected issue

Subagents should follow the false-positive checks in review.md before
reporting: check existing synchronization, call ordering, mutexes, atomics,
channel ownership, context cancellation, and documented contracts.

**Output**: per-unit finding lists with structured classifications. These feed
directly into Phase 3 — each finding becomes a verification subagent input.

**Subagent guidance**: run up to 8 sweep subagents in parallel. Each subagent
needs enough context to understand the code it's reviewing (constructors,
callers, tests) but does not need to read the entire repo. If the repo
produces more than 8 review units, batch related units or run in waves.

---

## Phase 3: Per-Finding Verification

**Goal**: confirm or refute each finding independently. This is where false
positives die and reproducer tests are born.

For each finding from Phase 2, spawn a subagent with:

- The finding (location, category, trigger, initial classification)
- The relevant domain reference
- [review.md](review.md) for calibration and false-positive checks
- Instruction to either **confirm** or **refute** the finding

Each verification subagent must:

1. **Read the full context**: the cited code, its constructor, callers, tests,
   start path, stop path. Follow the call chain — don't just read the flagged
   function.

2. **Attempt to confirm the failure mode** per review.md step 3:
   - Data race: name the unsynchronized writer, reader, and shared field.
   - Goroutine leak: name the owner, cancellation path, wait path, and bound.
   - Resource exhaustion: name the unbounded resource and the input that grows
     it.
   - Swallowed error: name the error producer, boundary, and caller-visible
     lie.
   - Ownership bug: name the mutable data crossing the boundary and who can
     mutate it.

3. **Check existing mitigations** per review.md step 4: mutexes, atomics,
   channel close ownership, context cancellation, typed domain errors,
   generated-code boundaries, documented call ordering.

4. **Attempt a reproducer test** where possible:
   - Data races: test with `-race`, concurrent goroutines exercising the path.
   - Lifecycle races: test shutdown/cancellation ordering, verify goroutines
     exit.
   - Goroutine leaks: `goleak.VerifyNone(t)` after lifecycle exercise.
   - Silent failures: assert errors propagate when underlying operations fail.
   - Design hazards: reorder initialization or add a concurrent caller to break
     the convention.

5. **Classify the finding**:
   - **Confirmed**: code has a reachable failure mode today.
   - **Partial**: pattern is unsafe but current wiring limits reach.
   - **Design hazard**: safe only because of undocumented ordering or
     convention.
   - **Not real**: synchronization or contract eliminates the bug.

6. **Record reproducer status**:
   - **Reproduced**: test demonstrates the failure.
   - **Partially reproduced**: test shows the unsafe pattern but doesn't
     trigger the exact production failure (e.g., race detector fires on a
     related path).
   - **Not reproduced**: couldn't trigger in test; explain why (timing
     dependent, requires production load, depends on external system).
   - **Not attempted**: finding is style-only or low-confidence.

Not every finding needs a reproducer. Confirmed data races and goroutine leaks
should get one. Design hazards and style issues typically don't.

Each verification subagent returns a structured result:

- **findingId**: identifier matching the Phase 2 finding
- **verdict**: confirmed / partial / design-hazard / not-real
- **confidence**: high / medium / low
- **reasoning**: why the verdict was reached, with specific code references
- **traceNotes**: call chain followed, mitigations checked, callers examined
- **reproducerStatus**: reproduced / partially-reproduced / not-reproduced /
  not-attempted
- **reproducerPath**: test file path if a reproducer was written

**Output**: per-finding verification results with structured classification
and optional test files.

**Subagent guidance**: run up to 8 verification subagents in parallel, one per
finding (or batch small related findings into one subagent). Each subagent
should be adversarial — its job is to try to kill the finding, not to confirm
it. Findings that survive are real.

---

## Phase 4: Synthesis

**Goal**: collect all verified findings into a prioritized summary the team can
act on.

Collect findings from Phase 3 and produce a summary with:

### Priority mapping

Priority reflects severity and confidence together:

| Priority | Criteria |
|----------|----------|
| P0 | Critical severity + Confirmed confidence |
| P1 | High + Confirmed, or Critical + Partial |
| P2 | Medium + Confirmed, or High + Partial |
| P3 | Low, or Design hazard, or Medium + Partial |
| P4 | Style-only (include only if specifically requested) |

### Summary table

| Priority | Finding | Severity | Confidence | Category | Reproducer | Location |
|----------|---------|----------|------------|----------|------------|----------|

### Summary stats

- Total findings by severity
- Total findings by category (correctness, reliability, safety, style)
- Reproducer coverage (how many have tests)
- Key themes (e.g., "most findings are lifecycle races in shutdown path")

### Per-finding detail

For each finding, include:

- **Title**: precise label from review.md wording (data race, lifecycle race,
  goroutine leak, etc.)
- **Severity / Confidence / Priority**
- **Category**: Correctness / Reliability / Safety / Style
- **Location**: file path(s), function(s), line range(s)
- **Description**: what the bug is, what the concrete trigger is
- **Failure mode**: what happens in production
- **Proof**: the specific evidence per review.md step 3
- **Existing mitigations**: what's already in place
- **Suggested fix**: reference specific patterns from the production-go
  references and packages/safe
- **Reproducer status**: reproduced / partially reproduced / not reproduced /
  not attempted
- **Production-go rule**: which rule(s) this violates

**Output format**: present findings in-conversation by default. If the user
requests file output, use whatever path they specify. Do not create files
unless asked.

---

## Adapting the Workflow

**Smaller audits.** A focused review of one package can skip Phase 1 (the
partition is obvious) and go straight to Phase 2 with a single subagent, then
Phase 3 for verification.

**Incremental audits.** After the initial full audit, subsequent reviews can
use the diff review mode (single-pass, per the SKILL.md Review Mode section).

**Legacy codebases.** Per the main skill's Existing Codebases section: fix
safety issues, leave aesthetic preferences alone. Style-only findings go to P4
or are omitted entirely.

**Large repos.** Phase 1 partitioning is critical. A repo with 50 packages
might produce 15-20 review units. Phase 2 subagents can run in parallel; Phase
3 verification can batch related findings (e.g., all lifecycle race findings in
one shutdown path).

**Resumability.** If the audit is interrupted (timeout, context limit, user
pause), it should be resumable. Keep per-finding state: each Phase 2 finding
and Phase 3 verification result should be identifiable so that re-running the
audit skips already-verified findings. When writing findings to files (at
user's request), use one file per finding so partial progress is preserved. On
resume, read existing verification results and skip findings that already have
a verdict.
