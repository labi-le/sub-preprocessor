# Profile-Guided Optimization (PGO)

> Sources: Go Blog — "Profile-guided optimization in Go 1.21" (2023),
> "Profile-guided optimization preview" (2023)

---

## What is PGO?

PGO (Profile-Guided Optimization) is a technique where the **production workload profile** is passed to the compiler so it can make more informed optimization decisions.

Without PGO, the compiler relies on static heuristics. With PGO, it knows which functions are hot and which types most commonly flow through interface calls.

## How to enable

### Go 1.21+ (production-ready)

```bash
# Save the profile as default.pgo in the main package directory
curl -o default.pgo "http://localhost:8080/debug/pprof/profile?seconds=30"
go build -o app
# Or explicitly:
go build -pgo=default.pgo -o app
```

Starting with Go 1.24, `-pgo=auto` is the default (looks for `default.pgo`).

### In practice: CI/CD

1. **Collect a profile** from production (30s CPU profile)
2. **Commit** `default.pgo` to the repository
3. **On every build** PGO is automatically enabled
4. **Periodically (every 1–4 weeks)** refresh the profile

```bash
# In Makefile or CI:
collect-profile:
    kubectl exec deploy/myapp -- \
        curl -s http://localhost:8080/debug/pprof/profile?seconds=30 \
        > default.pgo
```

## What optimizations PGO uses

### 1. Inlining (main driver)

PGO inlines hot functions even if they are too large for the regular heuristics.

**Chain:** inline → escape analysis sees across the boundary → objects stay on the stack → fewer allocations.

### 2. Devirtualization

If an interface call goes to one specific type 90% of the time:

```go
// Without PGO: indirect call io.Reader.Read
// With PGO:
if f, ok := r.(*os.File); ok {
    f.Read(b)          // direct call — can be inlined!
} else {
    r.Read(b)          // fallback
}
```

### 3. What's coming next

Go plans to extend PGO to:
- Register allocation
- Code layout / basic block placement
- Loop unrolling

## Results

| Version | CPU improvement (typical) |
|---------|---------------------------|
| Go 1.20 (preview) | 2–4% |
| Go 1.21 (GA) | 2–7% |
| Go 1.24+ | 5–10%+ (with better maps + PGO) |

### Example from the blog (Markdown server)

Without PGO:  374.5µs per request  
With PGO:    360.2µs per request  
**Speedup: ~3.8%**

Differential profile showed:
- `mdurl.Parse` — 100% of allocations removed (became inline, `URL` on stack)
- GC savings: `runtime.scanobject` -19%, `runtime.mallocgc` -1.44%
- Allocation count: `mdurl.Parse` -3.4%, `mdurl.URL.String` -2.9%

## Technique: building with PGO for production

```makefile
APP=myapp

.PHONY: build-pgo
build-pgo: default.pgo
    go build -o $(APP) .

.PHONY: build-nopgo
build-nopgo:
    go build -pgo=off -o $(APP).nopgo .

# compare:
benchstat nopgo.txt pgo.txt
```

## Important caveats

1. **Small code changes are fine.** A profile from last week works well with small diffs
2. **Large changes = refresh the profile.** If a refactor changed the architecture
3. **The profile must be representative.** If the profile is from workload A but you deploy to workload B, there will be no benefit
4. **Commit `default.pgo`** — it's part of the source tree, like `go.sum`, and ensures reproducibility
5. **PGO does not replace normal optimizations** — it's a multiplier, not a silver bullet

## How to evaluate PGO's effect

```bash
# 1. Build two binaries
go build -pgo=off -o app.nopgo .
go build -pgo=default.pgo -o app.pgo .

# 2. Deploy to production, then compare
# Or use benchstat
```

## Profiles: CPU, Heap, Block

Via `net/http/pprof`:

| Endpoint | Profile |
|----------|---------|
| `/debug/pprof/profile?seconds=30` | CPU profile (for PGO) |
| `/debug/pprof/heap` | Heap profile (allocation hotspots) |
| `/debug/pprof/block` | Goroutine blocking profile |
| `/debug/pprof/goroutine` | Goroutine stacks |

## Useful links

- [PGO documentation](https://go.dev/doc/pgo)
- [GC Guide](https://go.dev/doc/gc-guide)
