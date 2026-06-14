# Maps and Slices: Optimization and Memory

> Sources: Go Blog — "Faster Go maps with Swiss Tables" (2025),
> "Robust generic functions on slices" (2024),
> "Arrays, slices (and strings): The mechanics of 'append'" (2013),
> "Go Slices: usage and internals" (2011)

---

## Swiss Table Maps (Go 1.24+)

Go 1.24 completely rewrote the map implementation using **Swiss Tables** — an open-addressed hash table.

### Architecture

- 8 slots per group
- 64-bit control word (1 byte per slot) stores the lower 7 bits of the hash + status (empty/deleted)
- SIMD comparison: **8 probe steps in a single instruction**
- **Extendible hashing**: each map = several independent Swiss Tables (max 1024 entries each)

### Benefits

- **Incremental growth**: max 1024 entries copied per grow — no single insertion causes a lag spike
- **Higher load factor**: 87.5% vs 81.25% → lower memory footprint
- **Speed**: map operations up to **60% faster** in micro-benchmarks
- Real-world applications: geometric mean CPU **~1.5% improvement**

### Migration

No migration needed — just rebuild on Go 1.24+:
```bash
go version go1.24.0
```

## Slices: Performance

### Slice header

```go
type sliceHeader struct {
    Data *T    // pointer to the first element
    Len  int   // length
    Cap  int   // capacity
}
```

A slice is a value (struct), not a pointer. When passed to a function, the header is copied, but the **data array is shared**.

### Append: growth strategy

```go
// Built-in append doubles the capacity:
// cap < 256:  ×2
// cap >= 256: ×1.25
s = append(s, elem)
```

**Tip:** pre-allocate the expected size in advance:
```go
s := make([]int, 0, 1000) // 1 allocation instead of ~10
for i := 0; i < 1000; i++ {
    s = append(s, i)
}
```

### "Gotcha": sub-slice keeps the entire backing array

```go
big := make([]byte, 100*1024*1024) // 100 MB
small := big[99999000:]             // references the same array

// GC won't free the 100 MB! ✗
// Solution:
small := append([]byte(nil), big[99999000:]...) // copy
```

## Memory leaks in slice functions (Go 1.22+)

**Problem:** `slices.Delete` left "stale pointers" in the unreachable portion of the array:

```go
type T struct { p *int }
s := []T{{p: p1}, {p: p2}, {p: p3}}
s = slices.Delete(s, 1, 2)
// s = [{p: p1}, {p: p3}]
// but a reference to p2 remained in memory — GC couldn't collect it!
```

**Fix (Go 1.22+):** The `slices.Compact`, `CompactFunc`, `Delete`, `DeleteFunc`, and `Replace` functions now zero out the "tail" using the `clear()` builtin.

```go
// After Go 1.22 — safe:
s = slices.Delete(s, 1, 2)
// GC will collect p2
```

### Always capture the return value

```go
// CORRECT:
s = slices.Delete(s, 1, 2)

// INCORRECT: s remains the old slice, GC won't run
slices.Delete(s, 1, 2) // return value ignored!
```

## Practical slice tips

1. **Pre-allocate with known cap** — `make([]T, 0, expectedCap)`
2. **Copy sub-slices** to avoid keeping large backing arrays alive
3. **Use `copy()` with caution** — it handles overlaps gracefully
4. **nil slice = zero-length slice** functionally
5. **Don't blindly do `s = append(s, s...)`** — it can recursively reallocate
6. **Prefer `make` over literals** when the size is known
7. **`slices.Delete` and friends don't copy data** — they only change len/masks. But since Go 1.22+ they zero out the tail

## Hash function for comparable (future)

Go plans to add a [public hash function for comparable values](https://github.com/golang/go/issues/54670). This will let you build efficient custom hash tables and memory-efficient caches — keep an eye on it.
