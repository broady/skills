# Performance

Profile before optimizing. Benchmark before claiming faster.

These guidelines apply to **hot paths** — code proven by profiling to matter.
Do not apply them speculatively; premature optimization adds complexity without
measurable benefit.

## Allocation Reduction

- Pre-allocate slices and maps when size is known: `make([]T, 0, n)`.
- `strconv.Itoa` / `strconv.AppendInt` over `fmt.Sprintf` for numeric conversions.
- Avoid repeated `string` ↔ `[]byte` conversions; convert once and pass the result.
- `strings.Builder` for multi-part string construction (pre-size with `Grow` if total length is predictable).
- `sync.Pool` only when profiling shows a hot allocation; benchmark with and without. Pool misuse adds GC pressure from pinning objects.

## Receiver and Copy Costs

- Pointer receivers for large structs (>128 bytes or containing slices/maps).
- Value receivers for small structs (few scalar fields, no internal references).
- Avoid copying mutexes, sync primitives, or types with `noCopy` sentinels.

## Measurement Tools

| Tool | Use for |
|---|---|
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
