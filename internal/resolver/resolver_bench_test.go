package resolver_test

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/resolver"
)

// fakeDNS starts a UDP listener that responds to any DNS A query with a fixed
// response pointing example.com → 93.184.216.34. Returns the address and a
// cleanup function.
func fakeDNS(tb testing.TB) (addr string, cleanup func()) {
	tb.Helper()
	addr, _, cleanup = countingDNS(tb, answeringResponder)
	return addr, cleanup
}

// countingDNS starts a UDP listener that counts incoming DNS queries and
// answers each one via the given responder. Returns the address, the query
// counter, and a cleanup function.
func countingDNS(tb testing.TB, respond func(query []byte) []byte) (addr string, queries *atomic.Int64, cleanup func()) {
	tb.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("countingDNS ListenPacket: %v", err)
	}

	queries = new(atomic.Int64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			n, peer, readErr := conn.ReadFrom(buf)
			if readErr != nil {
				return
			}
			if n < 12 {
				continue
			}
			queries.Add(1)
			resp := respond(buf[:n])
			conn.WriteTo(resp, peer)
		}
	}()

	return conn.LocalAddr().String(), queries, func() {
		conn.Close()
		<-done
	}
}

// nodataResponder builds a NOERROR response with zero answer records, which
// the Go resolver reports as "no such host".
func nodataResponder(query []byte) []byte {
	qEnd := questionEnd(query)
	if qEnd < 0 {
		return nil
	}

	resp := make([]byte, 0, 512)
	resp = append(resp, query[0:2]...)     // ID
	resp = append(resp, 0x81, 0x80)        // flags: standard response, NOERROR
	resp = append(resp, 0x00, 0x01)        // QDCOUNT = 1
	resp = append(resp, 0x00, 0x00)        // ANCOUNT = 0
	resp = append(resp, 0x00, 0x00)        // NSCOUNT = 0
	resp = append(resp, 0x00, 0x00)        // ARCOUNT = 0
	resp = append(resp, query[12:qEnd]...) // question (mirror)
	return resp
}

// questionEnd returns the offset just past the question section, or -1 when
// the query is malformed.
func questionEnd(query []byte) int {
	pos := 12
	for pos < len(query) && query[pos] != 0 {
		pos += int(query[pos]) + 1
	}
	if pos >= len(query)-4 {
		return -1
	}
	pos++    // skip 0x00 terminator
	pos += 4 // skip QTYPE + QCLASS
	return pos
}

// answeringResponder constructs a valid DNS A-record response from a query.
// Copies the transaction ID and question section, appends a fixed answer.
func answeringResponder(query []byte) []byte {
	qEnd := questionEnd(query)
	if qEnd < 0 {
		return nil
	}

	// Pre-computed answer suffix:
	//   name pointer: 0xc0 0x0c (offset 12 = start of question name)
	//   type A:       0x00 0x01
	//   class IN:     0x00 0x01
	//   TTL 300:      0x00 0x00 0x01 0x2c
	//   RDLength 4:   0x00 0x04
	//   IP:           93.184.216.34
	resp := make([]byte, 0, 512)
	resp = append(resp, query[0:2]...)          // ID
	resp = append(resp, 0x81, 0x80)             // flags: standard response
	resp = append(resp, 0x00, 0x01)             // QDCOUNT = 1
	resp = append(resp, 0x00, 0x01)             // ANCOUNT = 1
	resp = append(resp, 0x00, 0x00)             // NSCOUNT = 0
	resp = append(resp, 0x00, 0x00)             // ARCOUNT = 0
	resp = append(resp, query[12:qEnd]...)      // question (mirror)
	resp = append(resp, 0xc0, 0x0c)             // name pointer
	resp = append(resp, 0x00, 0x01)             // type A
	resp = append(resp, 0x00, 0x01)             // class IN
	resp = append(resp, 0x00, 0x00, 0x01, 0x2c) // TTL 300
	resp = append(resp, 0x00, 0x04)             // RDLength 4
	resp = append(resp, 93, 184, 216, 34)       // IP bytes
	return resp
}

func BenchmarkResolution(b *testing.B) {
	addr, cleanup := fakeDNS(b)
	defer cleanup()

	r := resolver.New(5_000_000_000, addr, 0, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := r.Resolve(context.Background(), "example.com")
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResolution_Concurrent(b *testing.B) {
	addr, cleanup := fakeDNS(b)
	defer cleanup()

	r := resolver.New(5_000_000_000, addr, 0, 0)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := r.Resolve(context.Background(), "example.com")
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkResolution_CachedHit(b *testing.B) {
	addr, cleanup := fakeDNS(b)
	defer cleanup()

	r := resolver.New(5_000_000_000, addr, time.Hour, time.Hour)
	if _, err := r.Resolve(context.Background(), "example.com"); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := r.Resolve(context.Background(), "example.com")
		if err != nil {
			b.Fatal(err)
		}
	}
}

type benchEntry struct {
	ips     []netip.Addr
	expires time.Time
}

func benchHosts(n int) []string {
	hosts := make([]string, n)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("host-%d.example.com", i)
	}
	return hosts
}

// BenchmarkCacheStore_RWMutexMap and BenchmarkCacheStore_SyncMap compare the
// two candidate cache primitives on the resolver's read-heavy hit path, both
// sequentially and with parallel readers.
func BenchmarkCacheStore_RWMutexMap(b *testing.B) {
	hosts := benchHosts(1024)
	entry := benchEntry{ips: []netip.Addr{netip.MustParseAddr("93.184.216.34")}, expires: time.Now().Add(time.Hour)}
	var mu sync.RWMutex
	cache := make(map[string]benchEntry, len(hosts))
	for _, h := range hosts {
		cache[h] = entry
	}

	b.ReportAllocs()
	b.Run("sequential", func(b *testing.B) {
		for i := range b.N {
			mu.RLock()
			e, ok := cache[hosts[i%len(hosts)]]
			mu.RUnlock()
			if !ok || len(e.ips) == 0 {
				b.Fatal("miss")
			}
		}
	})
	b.Run("parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				mu.RLock()
				e, ok := cache[hosts[i%len(hosts)]]
				mu.RUnlock()
				if !ok || len(e.ips) == 0 {
					b.Fatal("miss")
				}
				i++
			}
		})
	})
}

func BenchmarkCacheStore_SyncMap(b *testing.B) {
	hosts := benchHosts(1024)
	entry := benchEntry{ips: []netip.Addr{netip.MustParseAddr("93.184.216.34")}, expires: time.Now().Add(time.Hour)}
	var cache sync.Map
	for _, h := range hosts {
		cache.Store(h, entry)
	}

	b.ReportAllocs()
	b.Run("sequential", func(b *testing.B) {
		for i := range b.N {
			v, ok := cache.Load(hosts[i%len(hosts)])
			if !ok {
				b.Fatal("miss")
			}
			if e, _ := v.(benchEntry); len(e.ips) == 0 {
				b.Fatal("empty")
			}
		}
	})
	b.Run("parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				v, ok := cache.Load(hosts[i%len(hosts)])
				if !ok {
					b.Fatal("miss")
				}
				if e, _ := v.(benchEntry); len(e.ips) == 0 {
					b.Fatal("empty")
				}
				i++
			}
		})
	})
}
