# Network optimization and compiler build flags

## Part 1: Network performance patterns

A collection of 13+ articles from goperf.dev covering network interaction optimization.

### Benchmark first

Before optimizing — measure. Tools for realistic load testing:

- **vegeta** — simple HTTP benchmark with rate limiting support
- **wrk** — multithreaded HTTP load generator
- **k6** — scripted load testing (JavaScript)

Measure: throughput (RPS), latency percentiles (p50, p95, p99), number of concurrent connections.

### Internal structure of the Go network stack

Go uses goroutines + the `net` package + the runtime scheduler. Under the hood — **epoll** (Linux) or **kqueue** (macOS) for non-blocking I/O. Each goroutine blocks independently — no "one thread per connection" overhead like in classic threading models.

### Effective use of net/http

- **Connection pool**: `http.Transport` pools TCP connections by default (keep-alive)
- **Custom dialers** with timeouts:

```go
transport := &http.Transport{
    DialContext: (&net.Dialer{
        Timeout:   30 * time.Second,
        KeepAlive: 30 * time.Second,
    }).DialContext,
}
```

- **Buffer tuning**: `WriteBufferSize`, `ReadBufferSize` in `http.Transport`
- **Connection leaks**: always close `resp.Body.Close()` — otherwise the connection is not returned to the pool

### 10,000+ concurrent connections

- **Goroutine stack** starts at ~4 KB and grows as needed
- **Worker pools**: limit the number of goroutines via buffered channels
- **Socket tuning**:

```go
lc := net.ListenConfig{
    Control: func(network, address string, c syscall.RawConn) error {
        return c.Control(func(fd uintptr) {
            syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
        })
    },
}
```

| Parameter | Purpose |
|---|---|
| `TCP_NODELAY` | Disables Nagle's algorithm (important for latency) |
| `SO_REUSEPORT` | Multiple processes on the same port |
| `SO_RCVBUF` / `SO_SNDBUF` | Receive/transmit buffer sizes |
| `SOMAXCONN` | Incoming connection queue |
| TCP keepalives | Dead connection detection |

### GOMAXPROCS and scheduler tuning

- **Default** = number of CPUs (logical cores)
- Increasing beyond the number of CPUs rarely helps — goroutines are multiplexed onto OS threads
- In containers, consider reducing to the cgroup limit:

```bash
GOMAXPROCS=$(nproc --all) go run main.go
```

- `GODEBUG` for scheduler tracing: `GODEBUG=schedtrace=1000,scheddetail=1`

### Fault tolerance patterns

- **Circuit breaker**: aborting requests when the success rate drops below a threshold
- **Load shedding**: rejecting excess requests under overload
- **Backpressure**: channel buffering + timeouts
- **Read/Write deadlines**: mandatory for long-lived connections (WebSocket, TCP streams)
- **Leak prevention**: always close resources, use `context.WithTimeout`

### Transport protocol comparison

| Protocol | Advantages | When to use |
|---|---|---|
| **Raw TCP** | Minimal latency, full control | High-frequency trading, custom protocols |
| **HTTP/2** | Multiplexed streams | Many concurrent requests to a single host |
| **gRPC** | HTTP/2 under the hood, streaming | Service-to-service, bi-directional streaming |
| **QUIC** (quic-go) | Connection migration, 0-RTT | Mobile clients, unstable networks |

### DNS performance

- Go-native resolver (direct DNS queries over UDP) vs cgo resolver (system resolver calls)
- **Cache DNS**: use a custom dialer with pre-resolved IPs
- Alternative: `github.com/patrickmn/go-cache` for TTL-based result caching

### TLS optimization

- **Session resumption**: reusing TLS sessions (reduces handshake latency)
- **Fast cipher suites**: prefer `TLS_AES_128_GCM_SHA256` and `TLS_CHACHA20_POLY1305_SHA256`
- **ALPN**: application-layer protocol negotiation during the handshake
- Minimize certificate verification in the hot path

## Part 2: Compiler optimization flags

### -gcflags

Flags for `go build`:

```bash
# Show optimizer decisions (inlining, escape analysis)
go build -gcflags="-m" ./...

# Disable inlining (debugging)
go build -gcflags="-l" ./...

# Disable all optimizations (debugging)
go build -gcflags="-N" ./...
```

The Go compiler does **not** support aggressive optimization flags like `-O2`/`-O3`. Using `-gcflags="-m"` is the primary way to understand what is being optimized.

### -ldflags

```bash
# Reduce binary size (~30%): strip debug information
go build -ldflags="-s -w" ./...

# Embed version/commit in the binary
go build -ldflags="-X main.version=$(git describe --tags) -X main.commit=$(git rev-parse HEAD)" ./...
```

### GOARCH and target architecture

Building for the **native architecture** gives you the best instruction set. For example, `GOARCH=amd64` enables optimizations not available with `GOARCH=386`.

```bash
# Check the current architecture
go env GOHOSTARCH

# Always build for the target platform
GOARCH=amd64 GOOS=linux go build ./...
```

## Part 3: Performance tracking across Go versions

Regular benchmarks are published on goperf.dev — 76+ benchmarks of runtime, stdlib, and the network stack on dedicated EC2 instances.

**Key improvements in recent versions:**

| Version | Improvement | Effect |
|---|---|---|
| **Go 1.24** | Swiss Tables hash map | Faster insert/lookup across all map sizes |
| **Go 1.25** | TLS handshake (fast path) | ~58% cumulative TLS 1.3 speedup since Go 1.23 |
| **Go 1.26** | Small allocation optimization (<32 bytes) | io.ReadAll ~2× faster, RSA-4096 keygen ~3× faster |

**Test platforms**: Linux amd64 (Ice Lake), Linux arm64 (Graviton3), macOS arm64 (Apple Silicon). Versions: 1.24, 1.25, 1.26.

**Bottom line**: keep your Go version up to date — each release brings meaningful performance improvements without changing your code.
