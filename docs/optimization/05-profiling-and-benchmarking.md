# Profiling and Benchmarking

> Sources: Go Blog — "Profiling Go Programs" (2011/2013),
> "More predictable benchmarking with testing.B.Loop" (2025),
> "More powerful Go execution traces" (2024)

---

## CPU Profiling

### Enabling

```go
import _ "net/http/pprof"

func main() {
    http.HandleFunc("/", handler)
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Profile available at: `http://localhost:8080/debug/pprof/profile`

### Collecting

```bash
curl -o cpu.pprof "http://localhost:8080/debug/pprof/profile?seconds=30"
```

### Analysis

```bash
go tool pprof -http=:8081 cpu.pprof
# or interactively:
go tool pprof cpu.pprof
(pprof) top10      # top by flat samples
(pprof) top10 -cum  # top by cumulative samples
(pprof) list FuncName  # source + samples per line
(pprof) web         # SVG call graph
(pprof) web FuncName  # subgraph around FuncName
```

## Heap Profiling

```bash
curl -o heap.pprof "http://localhost:8080/debug/pprof/heap"
go tool pprof -http=:8081 heap.pprof
```

### Key commands

```bash
# by allocation size (default)
go tool pprof heap.pprof

# by NUMBER of allocations (better for CPU correlation)
go tool pprof -sample_index=alloc_objects heap.pprof

# current in-use memory (after GC)
go tool pprof -sample_index=inuse_objects heap.pprof
```

### Differential profiling

Comparing two profiles — find what changed between versions:

```bash
# before vs after optimization:
go tool pprof -diff_base=cpu.before.pprof cpu.after.pprof
(pprof) top
```

Negative values = savings (less CPU).

## Benchmarking: testing.B.Loop (Go 1.24+)

### The new preferred way

```go
// Go 1.24+: b.Loop() instead of for b.N
func BenchmarkMyFunc(b *testing.B) {
    // setup — not timed (automatically)
    for b.Loop() {
        // code — timed
        result := MyFunc()
        _ = result
    }
    // cleanup — not timed
}
```

### Advantages of b.Loop

| Aspect | b.N style | b.Loop style |
|--------|-----------|--------------|
| Dead code elimination | Can eliminate code (if result is unused) | **Cannot** — compiler does not inline into loop body |
| Setup/cleanup timing | Must manually `ResetTimer()` `/` `StopTimer()` | **Automatically** excludes setup/cleanup from timing |
| Ramp-up | Multiple function invocations with different N | **Single invocation** — internal ramp-up |
| Execution speed | Slower (many runs) | **Faster** |

### When you need manual timer control

```go
func BenchmarkSort(b *testing.B) {
    ints := make([]int, N)
    for b.Loop() {
        b.StopTimer()
        fillRandom(ints)
        b.StartTimer()
        slices.Sort(ints)
    }
}
```

### Collecting and comparing results

```bash
go test -bench=. -count=20 -benchmem > results.txt

# Installing benchstat:
go install golang.org/x/perf/cmd/benchstat@latest

# Comparing two versions:
benchstat before.txt after.txt
```

## Execution Traces (Go 1.22+)

New improved execution tracer:

```go
import "runtime/trace"

func main() {
    f, _ := os.Create("trace.out")
    defer f.Close()
    trace.Start(f)
    defer trace.Stop()
    // ...
}
```

Analysis:
```bash
go tool trace trace.out
```

What it shows: GC, goroutine scheduling, network/syscall latency, mutex contention.

## Useful Packages

| Package | Purpose |
|---------|---------|
| `net/http/pprof` | HTTP endpoints for profiles |
| `runtime/pprof` | Programmatic profiling |
| `runtime/trace` | Execution tracing |
| `runtime/metrics` | Low-level runtime metrics (Go 1.16+) |
| `testing` | Benchmarks |
| `golang.org/x/perf/cmd/benchstat` | Statistical benchmark comparison |

## Profiles for PGO

Collect a 30-second CPU profile from production traffic:

```bash
# Important: the profile must be REPRESENTATIVE
curl -o default.pgo "http://localhost:8080/debug/pprof/profile?seconds=30"
go build -pgo=default.pgo
```

## Typical Profiling Patterns

1. **`runtime.mallocgc` at the top** → too many heap allocations
2. **`runtime.scanobject` at the top** → GC overhead, need to reduce heap allocations
3. **`syscall.Read/Write` at the top** → I/O bottleneck
4. **`runtime.growslice` at the top** → slices without pre-allocated capacity
5. **`runtime.mapaccess*` at the top** → map hot path, consider an alternative structure
