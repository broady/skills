# Performance

Profile before optimizing. Benchmark before claiming faster.

These guidelines apply to **hot paths** — code proven by profiling to matter.
Do not apply them speculatively; premature optimization adds complexity without
measurable benefit.

## Contents

- [Profiling Workflow](#profiling-workflow)
- [Escape Analysis](#escape-analysis)
- [Allocation Reduction](#allocation-reduction)
- [Receiver and Copy Costs](#receiver-and-copy-costs)
- [GC Tuning](#gc-tuning)
- [Measurement Tools](#measurement-tools)
- [Rules of Thumb](#rules-of-thumb)

## Profiling Workflow

Always start with profiling. The workflow: identify the bottleneck → benchmark
the hot path → optimize → re-benchmark → compare with `benchstat`.

```go
// Add to a benchmark or a test:
//   go test -run=^$ -bench=BenchmarkProcess -cpuprofile=cpu.prof -memprofile=mem.prof -count=10
//
// Analyze:
//   go tool pprof -http=:8080 cpu.prof     # interactive web UI
//   go tool pprof -top mem.prof             # top allocators
//   go tool pprof -list=ProcessBatch mem.prof  # annotated source for a function

func BenchmarkProcess(b *testing.B) {
    data := loadTestData(b)
    b.ResetTimer()
    for b.Loop() {
        process(data)
    }
}
```

Compare two runs with `benchstat`:
```
go test -bench=BenchmarkProcess -count=10 > old.txt
# make changes
go test -bench=BenchmarkProcess -count=10 > new.txt
benchstat old.txt new.txt
```

`benchstat` reports statistical significance. If the delta column says `~` (not
significant), the change didn't help — don't keep it.

## Escape Analysis

Escape analysis determines whether a value can stay on the stack (fast) or must
be heap-allocated (triggers GC). Use it to understand *why* allocations happen
before trying to fix them.

```bash
go build -gcflags='-m' ./pkg/...       # show escape decisions
go build -gcflags='-m -m' ./pkg/...    # verbose: show reasoning
```

Common escape causes and fixes:

| Cause | Why it escapes | Fix |
|---|---|---|
| Returning `&localVar` | Address outlives stack frame | Return by value if struct is small |
| Passing to `interface{}` / `any` | Compiler can't prove size at compile time | Use concrete type or generic |
| Closure captures loop variable | Variable outlived by goroutine/closure | Pass as function parameter |
| `fmt.Sprintf` args | Variadic `any` parameters | `strconv` + `strings.Builder` on hot paths |
| Slice grows past initial capacity | Runtime calls `growslice` | Pre-allocate with known capacity |

```go
// BAD — s escapes to heap via interface conversion in fmt.Sprintf.
func formatID(id int64) string {
    return fmt.Sprintf("%d", id)
}

// GOOD — no allocation on hot path.
func formatID(id int64) string {
    return strconv.FormatInt(id, 10)
}
```

## Allocation Reduction

- Pre-allocate slices and maps when size is known: `make([]T, 0, n)`.
- `strconv.Itoa` / `strconv.AppendInt` over `fmt.Sprintf` for numeric conversions.
- Avoid repeated `string` ↔ `[]byte` conversions; convert once and pass the result.
- `strings.Builder` for multi-part string construction (pre-size with `Grow` if total length is predictable).
- `sync.Pool` only when profiling shows a hot allocation; benchmark with and without. Pool misuse adds GC pressure from pinning objects.
- Return small structs by value (not pointer) to keep them on the stack.
- Use `append(buf[:0], ...)` to reuse a buffer's backing array without allocation.

```go
// Reuse a byte buffer across iterations in a tight loop.
buf := make([]byte, 0, 256)
for _, item := range items {
    buf = buf[:0] // reset length, keep capacity
    buf = appendRecord(buf, item)
    write(buf)
}
```

## Receiver and Copy Costs

- Pointer receivers for large structs (>128 bytes or containing slices/maps).
- Value receivers for small structs (few scalar fields, no internal references).
- Avoid copying mutexes, sync primitives, or types with `noCopy` sentinels.

## GC Tuning

Two knobs control GC behavior. Set them in the process entry point or via
environment variables — never inside library code.

```go
// GOGC: target ratio of new heap to live heap before triggering GC.
// Default: 100 (GC when heap doubles). Lower = more frequent GC, lower peak.
// Higher = less frequent GC, higher peak.
// Set via GOGC env var or debug.SetGCPercent().

// GOMEMLIMIT: soft memory ceiling. When heap approaches this limit, GC runs
// more aggressively. Prevents OOM in memory-constrained containers.
// Set via GOMEMLIMIT env var or debug.SetMemoryLimit().
//
// Example for a 512Mi container (leave headroom for goroutine stacks + non-heap):
//   GOMEMLIMIT=400MiB
```

**When to tune:**

| Scenario | Approach |
|---|---|
| Container OOM kills | Set `GOMEMLIMIT` to ~80% of container memory |
| High GC CPU overhead (>5% in pprof) | Increase `GOGC` or set `GOMEMLIMIT` |
| Latency-sensitive service | Lower `GOGC` for smaller, faster collections |
| Batch processing | `GOGC=off` with `GOMEMLIMIT` set — GC only when needed |

## Measurement Tools

| Tool | Use for |
|---|---|
| `go build -gcflags='-m'` | Escape analysis — understand why allocations happen |
| `go tool pprof` | CPU, heap, goroutine, mutex profiles |
| `testing.B` | Micro-benchmarks (use `b.Loop()` in Go 1.24+) |
| `benchstat` | Statistical comparison of benchmark runs |
| `runtime/metrics` | GC cycles, goroutine count, heap stats at runtime |
| `runtime/trace` | Scheduler latency, GC pauses, goroutine blocking |

## Rules of Thumb

- Measure the baseline before changing anything.
- Run benchmarks multiple times (`-count=10`) and use `benchstat` to confirm significance.
- Optimize the algorithm first (O(n) vs O(n^2) matters more than allocation tricks).
- Avoid premature `sync.Pool`, `unsafe`, or manual memory management without profile evidence.
- Document why a hot-path optimization was added and link the benchmark result.
