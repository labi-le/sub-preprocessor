# unique, weak.Pointer, AddCleanup: New Efficiency Tools

> Sources: Go Blog — "New unique package" (2024),
> "From unique to cleanups and weak: new low-level tools for efficiency" (2025)

---

## Package unique (Go 1.23+) — Canonicalization / Interning

### Purpose

Deduplication (canonicalization/copy elision) of comparable values. Known as **interning**.

### API

```go
import "unique"

// Make canonicalizes a value — returns a Handle[T].
// As long as the Handle is alive, the value stays in the internal map (GC-aware).
h1 := unique.Make("hello")
h2 := unique.Make("hello")

h1 == h2          // true — pointer comparison (fast)
h1.Value()        // "hello" — get the original value
```

### Handle[T] — the key idea

- `Handle[T]` — a lightweight wrapper
- **Pointer comparison** — O(1) instead of comparing long strings
- **GC-friendly** — when all `Handle`s for a value are gone, GC collects the entry
- Works with any comparable type, not just string

### Example: netip.Addr

```go
type addrDetail struct {
    isV6   bool
    zoneV6 string
}

var z6noz = unique.Make(addrDetail{isV6: true})

func (ip Addr) WithZone(zone string) Addr {
    if zone == "" {
        ip.z = z6noz
        return ip
    }
    ip.z = unique.Make(addrDetail{isV6: true, zoneV6: zone})
    return ip
}
```

Effect: **less memory** for identical zone strings, **fast Addr comparison** (pointer comparison).

### When to use

- Parsing text formats (many duplicate strings)
- Canonicalization of enum-like values
- Normalizing IDs, zones, domains, URLs

### Transparent string interning (future)

In the future (Go roadmap) it may be possible to do transparent string interning. For now, a workaround:

```go
h := unique.Make(s)
// h.Value() returns a string that may already be collected by GC
// between GC cycles it still works fine
```

## weak.Pointer — Weak References (Go 1.24+)

### Purpose

A GC-ignored pointer: does not prevent garbage collection.

### API

```go
import "weak"

p := weak.Make(&obj)   // create a weak pointer
ptr := p.Value()       // *T if alive; nil if collected by GC
```

### Properties

- **Comparable** — `p1 == p2` works
- **Stable identity** — even after the object is collected, the weak pointer retains identity (can be used as a map key)
- **Does not prevent GC** — the object can be collected even if a weak.Pointer points to it

### Main use cases

1. **Maps for canonicalization** (like `unique`) — `map[weak.Pointer[T]]T`
2. **Lifetime linking** — `map[weak.Pointer[K]]V`, where V should not keep K alive
3. **Memory-efficient caches** — a cache that does not interfere with GC

### Caveats

- **Do not use as a map key if the value contains a strong reference to the key** — this is resurrection
- Weak pointers are **non-deterministic** — GC may never run
- Check `p.Value() == nil` — it can be nil

## runtime.AddCleanup (Go 1.24+)

### Purpose

Replacement for `runtime.SetFinalizer` — safer and more efficient.

### API

```go
import "runtime"

// When obj becomes unreachable, cleanup(arg) will be called
runtime.AddCleanup(obj, cleanup, arg)
```

### Differences from Finalizer

| Aspect | SetFinalizer | AddCleanup |
|--------|--------------|------------|
| Resurrection | Possible (cleanup receives the reference itself) | **Impossible** (obj and arg are different) |
| When GC collects | **≥2 GC cycles** (due to resurrection check) | **≥1 GC cycle** (no resurrection) |
| Reference cycles | **Does not work** (cycles block collection) | **Works** (cleanup is not tied to obj) |
| Parameter | cleanup(obj) | cleanup(arg) — pass anything |

### Example: automatic closing

```go
type File struct { ... }

func NewFile() *File {
    f := &File{fd: openFD()}
    runtime.AddCleanup(f, func(fd int) {
        syscall.Close(fd)  // fd is a value, does not reference f
    }, f.fd)
    return f
}
```

### Caveats

- The cleanup object must not be reachable from the cleanup function or its arg — otherwise cleanup will never execute
- Non-deterministic: GC may never run

## Summary Recommendations

1. **`unique.Make()`** for string/value deduplication on the hot path
2. **`weak.Pointer`** for building memory-efficient caches and canonicalization maps
3. **`runtime.AddCleanup`** instead of `*.SetFinalizer` for safe resource cleanup
4. **Do not hold strong references** in weak.Pointer map values — this is resurrection
5. Remember about **non-determinism** — all three tools depend on GC
