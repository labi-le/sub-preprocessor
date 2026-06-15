package resolver_test

import (
	"context"
	"net"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/resolver"
)

// fakeDNS starts a UDP listener that responds to any DNS A query with a fixed
// response pointing example.com → 93.184.216.34. Returns the address and a
// cleanup function.
func fakeDNS(tb testing.TB) (addr string, cleanup func()) {
	tb.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("fakeDNS ListenPacket: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			resp := buildDNSResponse(buf[:n])
			conn.WriteTo(resp, peer)
		}
	}()

	return conn.LocalAddr().String(), func() {
		conn.Close()
		<-done
	}
}

// buildDNSResponse constructs a valid DNS A-record response from a query.
// Copies the transaction ID and question section, appends a fixed answer.
func buildDNSResponse(query []byte) []byte {
	// Find end of question section: scan QNAME labels, skip 0x00 + 4 (QTYPE+QCLASS).
	pos := 12
	for pos < len(query) && query[pos] != 0 {
		pos += int(query[pos]) + 1
	}
	if pos >= len(query)-4 {
		return nil
	}
	pos++    // skip 0x00 terminator
	pos += 4 // skip QTYPE + QCLASS
	qEnd := pos

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

// BenchmarkResolution_Cold measures DNS resolution where every call hits the
// mock DNS server (no prior cache fill). This is the worst-case path.
func BenchmarkResolution_Cold(b *testing.B) {
	addr, cleanup := fakeDNS(b)
	defer cleanup()

	r := resolver.New(5_000_000_000, addr, 5*time.Minute) // 5s timeout, custom DNS addr

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := r.Resolve(context.Background(), "example.com")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResolution_Hot measures DNS resolution where the cache is
// pre-warmed: the first call populates the cache, subsequent calls hit it.
// Before cache implementation, this behaves identically to Cold.
func BenchmarkResolution_Hot(b *testing.B) {
	addr, cleanup := fakeDNS(b)
	defer cleanup()

	r := resolver.New(5_000_000_000, addr, 5*time.Minute)
	// Pre-warm: resolve once to populate the cache.
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

// BenchmarkResolution_Concurrent measures parallel resolution for the same
// hostname — simulates multiple concurrent HTTP requests hitting the cache.
func BenchmarkResolution_Concurrent(b *testing.B) {
	addr, cleanup := fakeDNS(b)
	defer cleanup()

	r := resolver.New(5_000_000_000, addr, 5*time.Minute)
	// Pre-warm once
	if _, err := r.Resolve(context.Background(), "example.com"); err != nil {
		b.Fatal(err)
	}

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
