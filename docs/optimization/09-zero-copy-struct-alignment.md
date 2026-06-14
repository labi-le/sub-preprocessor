# Optimization in Go: Zero-Copy, Struct Alignment, and Memory Preallocation

## Part 1: Zero-Copy Techniques

**Basic zero-copy** — slicing a large byte array instead of copying:

```go
func process(buffer []byte) []byte {
    return buffer[128:256] // slice reference, no copy
}
```

Benchmark: copying 64 KB vs. creating a slice:

| Benchmark | Time | Memory | Allocations |
|-----------|-------|--------|-------------|
| `BenchmarkCopy` | 4,246 ns/op | 65,536 B/op | 1 alloc/op |
| `BenchmarkSlice` | 0.592 ns/op | 0 B/op | 0 alloc/op |

**io.CopyBuffer** — reusing a buffer to eliminate intermediate copies:

```go
buf := make([]byte, 4096)
io.CopyBuffer(dst, src, buf)
```

**True zero-copy via mmap** (`unix.Mmap`):

Two effects: skipping system calls only (mmap + ReadAt) vs. true zero-copy with direct page access.

| Scenario | Reading via `os.ReadAt` | mmap + ReadAt (skip syscall only) | mmap + direct access (true zero-copy) |
|----------|--------------------------|------------------------------------|---------------------------------------|
| XXHash (memory-bound) | 539,512 ns/op | ~25% faster | 281,249 ns/op (~2x) |
| SHA256 (compute-bound) | 2,636,956 ns/op | — | 2,287,858 ns/op (~15%) |

Zero-copy yields the greatest gain for memory-bound workloads. For compute-bound tasks the benefit is more modest — the cost of copying is masked by computation.

**Zero-copy in real-world projects**: fasthttp, gRPC-Go, MinIO, Protobuf, Badger, InfluxDB.

**Caveat**: zero-copy = shared memory → strict ownership discipline is required to avoid data races.

---

## Part 2: Struct Field Alignment

Due to alignment, the compiler inserts padding (empty bytes) between struct fields. The order of fields affects the final size.

```go
// Bad: 24 bytes (flag + 7 pad + count + id + 7 pad)
type PoorlyAligned struct {
    flag  bool
    count int64
    id    byte
}

// Good: 16 bytes
type WellAligned struct {
    count int64
    flag  bool
    id    byte
}
```

Benchmark (10 million structs, slice creation):

| Variant | Time | Memory |
|---------|-------|--------|
| `PoorlyAligned` | 20,095,621 ns/op | 240 MB |
| `WellAligned` | 19,265,714 ns/op | 160 MB (80 MB less) |

**False Sharing under concurrent access**: goroutines writing to different fields on the same cache line (64 bytes) invalidate each other's cache.

Solution — insert padding between frequently updated fields:

```go
type SharedCounter struct {
    a  int64
    _ [56]byte // padding to separate cache lines
    b  int64
}
```

Benchmark (2 goroutines, 1 million increments each):

| Variant | Time | Effect |
|---------|-------|--------|
| Without padding | 996,234 ns/op | false sharing |
| With padding | 958,180 ns/op | ~3.8% faster |

**Tools**: `fieldalignment` (built-in linter), `structslop`.

**Recommendations**: sort fields from largest alignment to smallest, group fields of the same size, add padding for fields frequently written from different goroutines, use linters.

---

## Part 3: Memory Preallocation

Preallocating slices and maps when the size is known eliminates repeated resizing:

```go
// Bad: 19 allocs, 357 KB, 28,539 ns
var s []int
for j := 0; j < 10000; j++ { s = append(s, j) }

// Good: 1 alloc, 82 KB, 7,093 ns
s := make([]int, 0, 10000)
for j := 0; j < 10000; j++ { s = append(s, j) }

// Better: no bounds checks
s := make([]int, 10000)
for j := range s { s[j] = j }
```

For maps — same approach: `make(map[int]string, 10000)` eliminates repeated rehashing during growth.
