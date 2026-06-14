# Allocation Reduction: Stack vs Heap, Escape Analysis

> Sources: Go Blog — "Profiling Go Programs" (2011, updated 2013),
> "Go runtime: 4 years later" (2022), "Green Tea Garbage Collector" (2025)

---

## The Main Rule

**Allocation on the stack is free. Allocation on the heap is expensive (GC).**
The fewer heap allocations you have, the faster your program runs.

## Escape Analysis

The Go compiler performs **escape analysis** — it determines whether an object can "escape" from the current function.

### When an Object Goes to the Heap

```go
func allocateOnHeap() *int {
    x := 42
    return &x   // x can't stay on the stack (returning a pointer)
}
```

### When an Object Stays on the Stack

```go
func stayOnStack() int {
    x := 42
    return x   // x is a value, it gets copied
}
```

### How to Check What Goes to the Heap

```bash
go build -gcflags='-m' 2>&1 | grep "escapes to heap"
go build -gcflags='-m -m' 2>&1  # verbose
```

## Techniques for Reducing Allocations

### 1. Pass Values, Not Pointers

```go
// BAD: object escapes to the heap
func process(r *Reader) error

// BETTER: copying is cheap for small structs
func process(r Reader) error
```

Rule: for structs ≤4 words (~32 bytes on 64-bit), passing by value is faster than passing by pointer.

### 2. Use Value Receivers

```go
type Point struct{ X, Y float64 }

// BAD — object may escape
func (p *Point) Distance() float64

// BETTER — stays on the stack
func (p Point) Distance() float64
```

### 3. Use make with Known Capacity

```go
// BAD — append will reallocate (3+ times)
s := []int{}
for i := 0; i < 1000; i++ {
    s = append(s, i)
}

// GOOD — one allocation
s := make([]int, 0, 1000)
for i := 0; i < 1000; i++ {
    s = append(s, i)
}
```

### 4. Use Arrays Instead of Slices for Small Sizes

```go
// BAD — slice is always on the heap
func fn() {
    buf := make([]byte, 64)
}

// BETTER — array on the stack
func fn() {
    var buf [64]byte
    // pass buf[:] if you need a slice
}
```

### 5. Inlining Helps Allocations

When a function is inlined, escape analysis sees across boundaries:

```go
func allocOnStack() {
    v := getPoint() // after inlining: Point stays on the stack
}
```

PGO (Profile-Guided Optimization) makes inlining more aggressive for hot functions — **side effect: fewer heap allocations**.

### 6. Strings: Convert with Caution

```go
// string → []byte — ALWAYS an allocation (copy)
b := []byte(str)

// For read-only, use unsafe (but carefully!)
b := unsafe.Slice(unsafe.StringData(s), len(s))
```

### 7. Slicing Doesn't Copy Data, But Holds a Reference

```go
big := make([]byte, 100*1024*1024) // 100 MB
small := big[99999000:]            // references the same backing array

// GC won't free the 100 MB while small is alive!
// Solution: copy
small := append([]byte(nil), big[99999000:]...)
```

### 8. Avoid Escape via fmt.Sprintf and String Concatenation

```go
// fmt.Sprintf often causes escape
id := fmt.Sprintf("user-%d", uid)

// for simple cases, strconv is faster and allocation-free
id := "user-" + strconv.Itoa(uid)
```

### 9. Use `sync.Pool` for Hot Temporary Objects

```go
var bufPool = sync.Pool{
    New: func() any { return new(bytes.Buffer) },
}

func handle() {
    buf := bufPool.Get().(*bytes.Buffer)
    defer bufPool.Put(buf)
    buf.Reset()
    // use buf
}
```

## Metrics: How to Measure Allocations

### Benchmarks

```go
func BenchmarkMyFunc(b *testing.B) {
    for b.Loop() {          // Go 1.24+ preferred
        result := MyFunc()
        _ = result
    }
}
```

Output: `xxx ns/op, yyy B/op, zzz allocs/op`

### Real Program

```go
import "runtime"
var m runtime.MemStats
runtime.ReadMemStats(&m)
fmt.Printf("Alloc = %v MiB", m.Alloc / 1024 / 1024)
```

### pprof Heap Profile

```go
import _ "net/http/pprof"
// /debug/pprof/heap
```

## Inline-Aware Optimization

Go 1.26 introduces a source-level inliner via `//go:fix inline`. This lets you automatically migrate APIs and optimize hot paths.

**Before PGO:** the compiler only inlines small functions (judging by AST size).

**With PGO:** hot functions (from the profile) can be inlined, even if they are large. This enables escape analysis, which can move allocations from the stack to... **the stack** (remove them from the heap).

## Summary: Allocation Reduction Checklist

- [ ] Are you using value receivers instead of pointer receivers?
- [ ] Are you passing small structs by value?
- [ ] Are you pre-allocating slices with `make(..., cap)`?
- [ ] Are you using `var arr [N]T` for small arrays?
- [ ] Have you checked with `-gcflags='-m'`?
- [ ] Are you copying sub-slices to avoid holding onto a large backing array?
- [ ] Are you using `sync.Pool` in hot paths?
- [ ] Are you using `strconv` instead of `fmt.Sprintf`?
- [ ] Have you enabled PGO for better inlining?
