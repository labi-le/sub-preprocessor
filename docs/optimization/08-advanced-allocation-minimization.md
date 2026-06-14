# 08. Advanced Allocation Minimization

Complex structures and hot paths are the main sources of unnecessary allocations.
This document covers non-trivial optimization techniques.

## 1. Storage Packing

Structures with multiple pointers (`*B`, `*C`, `*D`) produce N+1 allocations.
**Storage packing** reduces this to a single one:

```go
type A struct { b *B; c *C; d *D }
// 5 allocations (A, B, C, D, D.C) → 1 allocation
s := &struct {
    A; _b B; _c C; _d D; _d_c C
}{}
a := &s.A
a.b = &s._b; a.c = &s._c; a.d = &s._d; a.d.c = &s._d_c
```

**Pros**: Better locality, fewer allocations, cache-friendly.
**Cons**: Loss of runtime flexibility, shared field lifetimes, byte size does not decrease.
**Use only when there's measurable benefit**.

## 2. Closure Capture Optimization

A closure capturing N variables → N+1 allocations.
Grouping into a struct → 2 allocations (closure + struct) regardless of field count:

```go
// 3 allocations → 2 allocations
go func() { doSomething(a, b) }()

// 2 allocations (closure + x)
x := struct{a *bar; b int}{a, b}
go func() { doSomething(x.a, x.b) }()
```

## 3. Pointer Slices — `[]*T`

`[]*T` naively yields N+1 allocations. A utility gives 2 allocations:

```go
func NewPtrSlice[T any](n int) []*T {
    s := make([]*T, n)
    p := make([]T, n)
    for i := range s { s[i] = &p[i] }
    return s
}
```

**Important**: all elements share the lifetime of a single backing array.

## 4. Shrinking Slices and Maps

**Slices**: re-slicing keeps the entire backing array. `slices.Clone(big[:100])` creates a minimal copy; the old array is reclaimed by GC.
**Maps**: Go does not shrink maps automatically. Use `maps.Clone` for long-lived oscillating maps — after initialization or on a schedule.

## 5. Pre-Allocated APIs

Design functions so the caller can reuse buffers:
- Functions returning a slice: accept a slice argument for append
- Functions returning `*T`: accept a `*T` to fill in instead of allocating

## 6. Using Smaller Types

| Instead of | Better | Savings |
|------------|--------|---------|
| `int` | `int8`/`int16` | up to 24 bytes per field |
| `string` (1 char) | `byte`/`rune` | string header (16 bytes) |
| `string` (≤16 bytes/UUID) | `[16]byte` | no string header |

## 7. Interface Boxing

Assigning a concrete value to an interface causes boxing — a heap allocation. Since Go 1.4 this behavior is mandatory.

**Techniques to avoid boxing**:
- Pass pointers into interfaces for large structs (avoids copying)
- Use generics instead of `interface{}` where the type is known
- Avoid interfaces on hot paths
- Search via pprof for `runtime.convT2E` in allocation profiles

**Benchmark**: boxing a `[4096]byte` is ~19 % slower than boxing a pointer (slice of 1000 elements).

## 8. Object Pool — `sync.Pool`

Pooling `bytes.Buffer` is a telling example:

| Approach | B/op | allocs/op | ns/op |
|----------|------|-----------|-------|
| Without pool | 4 096 | 1 | 864 |
| With `sync.Pool` | 0 | 0 | 42 |

**Use sync.Pool for**: short-lived reusable objects with measurable allocation pressure.
**Avoid sync.Pool for**: long-lived objects, low reuse frequency, when predictability matters more than speed.
