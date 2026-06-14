# GC (Garbage Collector) Optimization

> Sources: Go Blog — "Green Tea Garbage Collector" (2025), "Go runtime: 4 years later" (2022),
> "Getting to Go: The Journey of Go's Garbage Collector" (2018), "Go GC: Prioritizing low latency and simplicity" (2015)

---

## The Only Tuning Parameter: GOGC

Go's GC has **only one** tuning parameter — `GOGC` (or `debug.SetGCPercent`).

| GOGC | Behavior |
|------|----------|
| `100` (default) | Heap after GC = 100% of reachable = ×2 live size |
| `200` | = 200% of reachable = ×3 |
| `off` / `-1` | Disables GC manually |

**How to use:**
- Increase `GOGC` → fewer GC cycles → less CPU on GC → **more memory**
- Decrease `GOGC` → more GC cycles → **less memory**, but more CPU
- Seasonal tuning: GOGC = 200 if you have plenty of memory, GOGC = 50 if memory is critical

```go
import "runtime/debug"
debug.SetGCPercent(200) // fewer GC cycles, more memory
```

## Memory Limit (Go 1.19+)

`debug.SetMemoryLimit()` — the second and final knob, introduced in Go 1.19.

**Problem it solves:** without a memory limit, the GC doesn't know how much memory is available and can cause OOM during sudden load spikes.

**Usage:**
```go
debug.SetMemoryLimit(1 << 30) // 1 GB
```

**Important rules:**
- The limit accounts for the entire Go process memory (G), **not just the heap**
- When exceeded, the runtime more aggressively returns memory to the OS
- GC thrashing (when GC uses >50% CPU) — the runtime chooses to exceed the limit instead of thrashing
- The `GOMEMLIMIT` env var does the same thing

**Example: container with 512 MB:**
```bash
GOMEMLIMIT=400MiB GOGC=200 ./app  # 400 MB limit, 200% GOGC — fewer GC cycles
```

## Green Tea GC (experimental, Go 1.25+)

A new GC in Go 1.25, available with the build flag `GOEXPERIMENT=greenteagc`. It will become the default GC in Go 1.26.

### Key Idea

Instead of traversing the object graph (object-by-object), Green Tea works **with whole pages**. Each page is 8 KiB.

Instead of working with a list of individual objects, the **work list stores pages**, not objects.

### Why It's Faster

1. **Better cache locality** — objects are scanned sequentially within a page, not jumping across the entire heap
2. **Less contention** — the work list is shorter, fewer CPU stalls
3. **Vector hardware support** (AVX-512) — processing bit metadata of a whole page via SIMD

### Results

- GC CPU: reduction of **10–40%** for different workloads
- +10% more from vector instructions
- Scanning even **2% of a page** in a single pass is already faster than a regular graph flood

### How to Try It

```bash
GOEXPERIMENT=greenteagc go build -o myapp
```

## GC Improvement History (2014→2025)

| Version | What Changed |
|---------|--------------|
| Go 1.5 | Concurrent GC: latency dropped from 300-400ms to 30-40ms |
| Go 1.6 | Removed O(heap) STW: ~4-5ms |
| Go 1.7 | More O(heap) cleanup |
| Go 1.8 | Sub-millisecond (<1ms) STW |
| Go 1.13 | `sync.Pool` — lower latency impact, better recycle |
| Go 1.13-14 | Return unused memory to OS; idle memory ↓20% |
| Go 1.14 | Goroutine preemption — STW latency up to 90% lower |
| Go 1.14 | Defer ~= normal function call speed |
| Go 1.14-15 | Allocator slow path scales better with CPU cores: throughput +10%, tail latency -30% |
| Go 1.17 | Scheduler: 30% less CPU on spinning |
| Go 1.17-18 | Register-based calling convention: CPU efficiency +15% |
| Go 1.18 | GC accounting redesign: tail latency up to -66% |
| Go 1.19 | GC self-limits CPU when idle: -75% CPU on GC |
| Go 1.19 | Memory limit added |
| Go 1.24 | Swiss Table maps (faster GC indirectly) |
| Go 1.25 | Green Tea GC (experimental) |
| Go 1.26 | Green Tea GC (default) |

## Practical Tips

1. **Increase GOGC in containers** — if you have memory to spare, reduce CPU on GC
2. **Use `GOMEMLIMIT`** — it protects against OOM during load spikes
3. **Green Tea on Go 1.25+** — `GOEXPERIMENT=greenteagc` gives up to 40% less CPU on GC
4. **Don't overdo intermediate allocations** — the GC still collects them, but CPU is wasted
5. **Go GC is not generational** — in Go, young objects typically live on the stack, so generations don't make sense
6. **Go favors low latency over throughput** — at the cost of slightly more CPU
7. **sync.Pool** — GC-aware memory reuse, use it for hot temporary objects
