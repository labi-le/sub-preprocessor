# Runtime Optimizations: Registers, Scheduler, Timers, Defer

> Sources: Go Blog — "Go runtime: 4 years later" (2022),
> "Go 1.17 is released" (2021), "Go 1.14 is released" (2020)

---

## Register-based Calling Convention (Go 1.17+)

**The biggest performance change in recent years.**

Before Go 1.17: Go used a stack-based calling convention (all arguments through the stack).
Since Go 1.17: **register-based** on amd64/arm64/ppc64.

### Impact

- CPU efficiency: **up to +15%**
- Fewer memory accesses
- Better compatibility with hardware prefetchers

### For the developer

Transparent — simply update Go.

## Scheduler: less spinning (Go 1.17)

Go runtime scheduler:
- **Before:** A P (processor) actively spun looking for work — up to 30% CPU wasted
- **Now:** Smart search algorithm, up to **30% less CPU** on spinning

## Timers (Go 1.14)

Timers (time.After, time.Ticker, time.NewTimer) were rewritten for machines with many CPU cores.
Impact: significantly less contention on the timer heap.

## Defer (Go 1.14)

`defer func()` now costs about as much as a regular function call.

```go
// Before Go 1.14: defer was expensive (chain on stack + malloc)
// After Go 1.14: defer = ~2-3 instructions (inline + stack allocation)
defer mu.Unlock()
```

## Goroutine Preemption (Go 1.14)

Before Go 1.14: a goroutine could hold onto a P for a long time in a tight loop → GC delays.
After Go 1.14: **async preemption** — the runtime can interrupt any goroutine at a safe point.

**Impact:** stop-the-world latency reduced by **up to 90%**.

## Memory return to OS (Go 1.13+)

`MADV_FREE` (Linux): The runtime returns unused heap memory to the OS.

- **Used:** runtime returns unneeded memory more proactively
- **Result:** up to **20% reduction in idle memory consumption**
- **Important:** this does not affect performance, only memory footprint

## sync.Pool (Go 1.13+)

`sync.Pool` was significantly improved:

- Less latency impact on GC
- Pool **reuses memory much more efficiently** between GC cycles
- During GC the pool is cleared — after GC you get fresh objects

```go
var pool = sync.Pool{
    New: func() any { return make([]byte, 4096) },
}

func handle() {
    buf := pool.Get().([]byte)
    defer pool.Put(buf)
    // use it
}
```

## Allocator slow path: better scaling (Go 1.14-1.15)

**Problem:** in highly concurrent programs, the allocator was a bottleneck.

**Solution:** reworked the allocator slow path:
- Throughput: **+10%**
- Tail latency: **up to -30%**

## GC CPU self-limitation during idle (Go 1.19)

When the application is idle (not actively allocating):
- GC previously could start and consume CPU unnecessarily
- Go 1.19: GC **limits its own CPU usage** during idle
- Impact: **up to 75% less CPU** on GC during idle
- Important: eliminates CPU spikes that confused job shapers

## runtime/metrics (Go 1.16+)

Alternative to `ReadMemStats`:

```go
import "runtime/metrics"

// ReadMemStats — expensive (STW-like), up to milliseconds
// runtime/metrics — microseconds (2 orders of magnitude faster)
```

Usage:
```go
samples := make([]metrics.Sample, 1)
samples[0].Name = "/gc/heap/allocs:bytes"
metrics.Read(samples)
val := samples[0].Value.Uint64()
```

## Useful Environment Variables

| Env | Purpose |
|-----|---------|
| `GOGC` | GC aggressiveness (default: 100) |
| `GOMEMLIMIT` | Soft memory limit (Go 1.19+) |
| `GOMAXPROCS` | Number of P (processors). Go 1.25+ auto-detects for containers |
| `GODEBUG=gctrace=1` | Prints GC debug information to stderr |

## Container-aware GOMAXPROCS (Go 1.25+)

Before Go 1.25: in containers GOMAXPROCS = host CPU count → over-provisioning.
After Go 1.25: GOMAXPROCS **automatically** accounts for the container's CPU quota.
