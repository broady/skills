# Full-Repo Audit Workflow

A structured multi-pass review for auditing an existing Go codebase against the
production-go standard. The goal is reachable production failure modes with
accurate priority and confidence — not maximizing finding count.

Read [review.md](review.md) first. It calibrates priority, confidence,
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
3.5. **Triage** — coordinator clusters patterns, cuts noise, keeps real bugs
4. **Synthesis** — collect triaged findings into a prioritized summary

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

Each subagent scans its files and records suspected issues. **Quality over
quantity**: a sweep that finds 3 real bugs is better than one that lists 15
rule violations. Before recording a finding, the subagent should ask: "If I
told a senior engineer about this, would they say 'good catch' or 'so what?'"
Skip the "so what?" ones.

Each finding should include:

- **findingId**: unique identifier (e.g., `pkg-function-category`)
- **location**: file, function, line range
- **category**: data race, lifecycle race, goroutine leak, unbounded
  concurrency, silent failure, ownership bug, boundary contract, design hazard
- **trigger**: what production condition reaches this code (request, shutdown,
  role transition, retry, malformed input, slow peer, config change)
- **consequence**: concrete impact if triggered
- **status**: established defect / unverified risk / design or policy observation
- **priority**: P0–P3 when assessable; otherwise unassigned
- **confidence**: high / medium / low, independent of status and priority
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
- Instruction to test the claim against available evidence, including
  mitigations, and report inconclusive results as unverified

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

5. **Classify the finding** per review.md: established defect, unverified
   risk, design/policy observation, or refuted claim. Independently assess
   confidence (high / medium / low) and priority (P0–P3 where assessable).
   Name any material unknowns. A severe plausible risk can warrant urgent
   investigation despite incomplete evidence; don't assume it is confirmed.

6. **Record reproducer status**:
   - **Reproduced**: test demonstrates the failure.
   - **Partially reproduced**: test shows the unsafe pattern but doesn't
     trigger the exact production failure (e.g., race detector fires on a
     related path).
   - **Not reproduced**: couldn't trigger in test; explain why (timing
     dependent, requires production load, depends on external system).
   - **Not attempted**: explain why, including when a low-confidence risk requires
     investigation that could not be performed.

Not every finding needs a reproducer. Confirmed data races and goroutine leaks
should get one when practical and authorized. Observations typically don't.

Each verification subagent returns a structured result:

- **findingId**: identifier matching the Phase 2 finding
- **status**: established-defect / unverified-risk / observation / refuted
- **priority**: P0 / P1 / P2 / P3 / unassigned
- **confidence**: high / medium / low
- **reasoning**: why the status was reached, with specific code references
- **traceNotes**: call chain followed, mitigations checked, callers examined
- **reproducerStatus**: reproduced / partially-reproduced / not-reproduced /
  not-attempted
- **reproducerPath**: test file path if a reproducer was written

**Output**: per-finding verification results with structured classification
and optional test files.

**Subagent guidance**: run up to 8 verification subagents in parallel, one per
finding (or batch small related findings into one subagent). Each subagent
should actively look for evidence both supporting and refuting the claim.
An inconclusive investigation remains unverified; it is not automatically real
or refuted.

---

## Phase 3.5: Triage — Kill the Noise

**Goal**: separate real bugs from rule violations before writing anything up.
This is where audit quality is decided. An audit that reports 40 findings with 5
real bugs is worse than one that reports 8 findings that are all actionable.

**This is coordinator work, not subagent work.** The coordinator must see all
Phase 3 results together to identify pattern clusters and make holistic triage
decisions. Individual subagents can't detect that 6 findings are the same
pattern repeated across components.

### Step 1: Cluster by pattern

Before triaging individual findings, group them by category + trigger. If
multiple findings share the same root pattern (e.g., "Stop doesn't wait for
goroutine" across 6 components), they are one finding with a list of locations —
not 6 findings. Identify these clusters first, then triage the cluster as a
unit.

### Step 2: Apply the "so what?" test

For each verified finding (or cluster), apply the "so what?" test from
review.md:

| Category | Action | Example |
|----------|--------|---------|
| **Real bug** | Report. This is why the audit exists. | Response body not closed on error path. Error silently discarded so caller sees success. Nil pointer reachable via empty config. |
| **Real bug, wrong context** | Report with correct context. | Goroutine leak at shutdown — matters in library/test usage, but may not matter when the verified deployment exits immediately after shutdown. State the actual lifecycle explicitly. |
| **Unverified risk** | Report significant plausible impact with unknowns and next verification. | A write may be acknowledged before durability, but the storage contract is unavailable. |
| **Hardening** | Report briefly, don't inflate. | HTTP client without Timeout but behind context deadline. Missing mutex on field accessed from one goroutine. |
| **Rule violation, code works** | Omit or footnote. | Type-level API hazard where all callers are correct. Shutdown ordering that's safe by convention. |

### What to cut

- **"All callers are correct" findings**: if every caller serializes access or
  provides the missing bound, and the unsafe API is internal, this is a
  footnote — not a finding. Mention it in the themes section as "fragile API"
  and move on.
- **"Process exits anyway" findings**: goroutine leaks, ticker leaks, and
  incomplete shutdown where the actual deployment guarantees immediate process
  exit. Verify that lifecycle rather than assuming an orchestrator. If the code
  is also used as a library (tests, embedded), frame the finding around that
  usage rather than overstating production impact.
- **"Theoretical slow peer" findings**: missing HTTP client timeout where a
  verified bound covers the whole call, such as a caller context deadline that
  also spans the body read. These are defense-in-depth suggestions, not
  findings. "Internal service" or "behind a load balancer" alone is not a
  bound: neither stops a peer that stalls mid-response.
- **Pattern repetitions**: already handled by Step 1 clustering. One finding
  with a list of locations, not N findings.

### What survives triage

Report established defects and significant unverified risks supported by a
plausible trigger and consequence. Name material unknowns, especially for
unverified risks, and suggest a fix or next verification step. Do not impose a
finding-count target or discard a serious but uncommon failure merely because
it is uncommon.

Themes and patterns go in the summary narrative, not as individual findings.

---

## Phase 4: Synthesis

**Goal**: collect all triaged findings into a prioritized summary the team can
act on. Write one markdown file per finding plus an index file.

### Prioritization

Use the P0–P3 definitions in [review.md](review.md#priority-and-confidence).
Assess priority from impact, reachability, scope, and urgency; record confidence
and status independently. An unverified severe risk may require urgent
investigation. Refuted claims receive no priority. Include policy or design
observations separately when useful or requested, without automatically treating
a rule violation as an incident.

### Themes (more important than individual findings)

Patterns that span multiple locations are more useful than a long finding list.
The index preamble should include a short themes section before the table.
Examples:
- "Shutdown doesn't wait for goroutines — 12 components have this pattern.
  Matters for library/test usage, not production." (One theme, not 12 findings.)
- "HTTP clients inconsistently configured — 4 production paths use
  DefaultClient. Fix with a standard constructor."
- "Three worker shutdown paths do not wait for in-flight writes; verify which
  can discard acknowledged work."

The finding count reflects post-triage totals, not the raw sweep count. Keep
refuted claims separate if the user requests an investigation history.

### Output: index file (`README.md`)

The index is a single table sorted by priority. Include a short preamble
explaining the priority scale. Example:

```markdown
# Review Findings Index

Priority reflects impact, reachability, scope and urgency; confidence is independent.

| Priority | Status | Confidence | Finding | Reproducer |
|---|---|---|---|---|
| P1 | Established defect | High | [Elector Bootstrap Stuck](elector-bootstrap-stuck.md) | Added targeted test |
| P1 | Unverified risk | Medium | [Potential write loss](potential-write-loss.md) | Not attempted; trace storage contract |
```

### Output: per-finding files

**Filename convention**: `<kebab-case-slug>.md`. Keep any requested refutation
history distinct from actionable findings.

Each finding file follows this template:

````markdown
# <Finding Title>

| Field       | Value |
|-------------|-------|
| Priority    | P0 / P1 / P2 / P3 / unassigned |
| Status      | Established defect / Unverified risk / Design or policy observation |
| Type        | Correctness / Concurrency / Lifecycle / Data ownership / ... |
| Confidence  | High / Medium / Low |
| Guide rule  | Optional: rule and reference; not evidence of impact |

## Location

- `path/to/file.go:line` (description of what's here)

## Description

What the bug is, with specific code references. Name the concrete mechanism
(unsynchronized writer/reader, missing wait path, etc.).

## Trigger

What production condition reaches this code.

## Impact

What happens when it fires.

## Suggested fix

Suggest a targeted remedy or next verification step; reference a skill pattern
only when it fits the code and established constraints.
````

**For dismissed findings**, replace Description/Trigger/Impact/Suggested fix
with:

```markdown
## Original claim

What was originally suspected.

## Investigation result

**Dismissed.** Why it's not real, with specific evidence (mutex coverage,
call ordering, grep results).
```

**For unverified risks**, explain the evidence, material unknowns, and the next
verification step. Do not assert the suspected consequence as established.

---

## Adapting the Workflow

**Smaller audits.** A focused review of one package can skip Phase 1 (the
partition is obvious) and go straight to Phase 2 with a single subagent, then
Phase 3 for verification.

**Incremental audits.** After the initial full audit, subsequent reviews can
use the diff review mode (single-pass, per the SKILL.md Review Mode section).

**Legacy codebases.** Per the main skill's Existing Codebases section: fix
safety issues in scope, leave aesthetic preferences alone. Style-only
observations are separate or omitted unless requested.

**Large repos.** Phase 1 partitioning is critical. A repo with 50 packages
might produce 15-20 review units. Phase 2 subagents can run in parallel; Phase
3 verification can batch related findings (e.g., all lifecycle race findings in
one shutdown path).

**Resumability.** If the audit is interrupted (timeout, context limit, user
pause), it should be resumable. Each Phase 2 finding and Phase 3 verification
result is a separate file, so partial progress is preserved automatically. On
resume, read existing finding files and skip findings whose status has already been established.
