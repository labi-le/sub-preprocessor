package resolver_test

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/resolver"
)

func TestResolve_ReturnsResolvedIP(t *testing.T) {
	addr, cleanup := fakeDNS(t)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, 0, 0)

	ips, err := r.Resolve(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	want := netip.MustParseAddr("93.184.216.34")
	if len(ips) != 1 || ips[0] != want {
		t.Fatalf("got %v, want [%v]", ips, want)
	}
}

func TestResolve_BareIPv4SkipsDNS(t *testing.T) {
	r := resolver.New(5*time.Second, "", 0, 0)

	ips, err := r.Resolve(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("resolve bare ip: %v", err)
	}

	want := netip.MustParseAddr("203.0.113.7")
	if len(ips) != 1 || ips[0] != want {
		t.Fatalf("got %v, want [%v]", ips, want)
	}
}

func TestResolve_BareIPv6Rejected(t *testing.T) {
	r := resolver.New(5*time.Second, "", 0, 0)

	if _, err := r.Resolve(context.Background(), "2001:db8::1"); err == nil {
		t.Fatal("expected error for bare IPv6 address")
	}
}

func TestResolve_CacheHitSkipsSecondLookup(t *testing.T) {
	addr, queries, cleanup := countingDNS(t, answeringResponder)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, time.Minute, time.Minute)

	want := netip.MustParseAddr("93.184.216.34")
	for i := range 3 {
		ips, err := r.Resolve(context.Background(), "example.com")
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		if len(ips) != 1 || ips[0] != want {
			t.Fatalf("resolve #%d: got %v, want [%v]", i, ips, want)
		}
	}

	if got := queries.Load(); got != 1 {
		t.Fatalf("dns queries = %d, want 1 (cache hit)", got)
	}
}

func TestResolve_CacheDisabledAlwaysLooksUp(t *testing.T) {
	addr, queries, cleanup := countingDNS(t, answeringResponder)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, 0, 0)

	for i := range 2 {
		if _, err := r.Resolve(context.Background(), "example.com"); err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
	}

	if got := queries.Load(); got < 2 {
		t.Fatalf("dns queries = %d, want >= 2 (cache disabled)", got)
	}
}

func TestResolve_CacheExpires(t *testing.T) {
	addr, queries, cleanup := countingDNS(t, answeringResponder)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, time.Millisecond, 0)

	if _, err := r.Resolve(context.Background(), "example.com"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := r.Resolve(context.Background(), "example.com"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}

	if got := queries.Load(); got < 2 {
		t.Fatalf("dns queries = %d, want >= 2 (entry expired)", got)
	}
}

func TestResolve_NegativeCacheServesEmptyWithoutLookup(t *testing.T) {
	addr, queries, cleanup := countingDNS(t, nodataResponder)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, time.Minute, time.Minute)

	if _, err := r.Resolve(context.Background(), "dead.example.com"); err == nil {
		t.Fatal("first resolve: expected error for empty answer")
	}
	after := queries.Load()

	ips, err := r.Resolve(context.Background(), "dead.example.com")
	if err != nil {
		t.Fatalf("cached negative resolve: %v", err)
	}
	if len(ips) != 0 {
		t.Fatalf("cached negative resolve: got %v, want empty", ips)
	}

	if got := queries.Load(); got != after {
		t.Fatalf("dns queries = %d, want %d (negative cache hit)", got, after)
	}
}

func TestResolve_NegativeCacheDisabledRetries(t *testing.T) {
	addr, queries, cleanup := countingDNS(t, nodataResponder)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, time.Minute, 0)

	if _, err := r.Resolve(context.Background(), "dead.example.com"); err == nil {
		t.Fatal("first resolve: expected error")
	}
	after := queries.Load()

	if _, err := r.Resolve(context.Background(), "dead.example.com"); err == nil {
		t.Fatal("second resolve: expected error")
	}

	if got := queries.Load(); got <= after {
		t.Fatalf("dns queries = %d, want > %d (negative caching disabled)", got, after)
	}
}

// TestResolve_ConcurrentSameHostSharesOneQuery pins the in-flight dedup a
// per-call net.Resolver cannot do: the singleflight group that collapses
// concurrent identical lookups lives on the Resolver instance, so building one
// per Resolve gives every caller an empty group and its own wire query. Both
// TTL caches are off here, so the group is the only thing that can collapse
// them.
func TestResolve_ConcurrentSameHostSharesOneQuery(t *testing.T) {
	const (
		callers = 16
		// settle covers the window between the barrier and the last caller
		// registering with the group; the server answers nothing before it.
		settle = 200 * time.Millisecond
	)

	release := make(chan struct{})
	addr, queries, cleanup := heldCountingDNS(t, release)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, 0, 0)

	var barrier, done sync.WaitGroup
	barrier.Add(callers)
	done.Add(callers)
	got := make([][]netip.Addr, callers)
	errs := make([]error, callers)
	for i := range callers {
		go func() {
			defer done.Done()
			barrier.Done()
			barrier.Wait()
			got[i], errs[i] = r.Resolve(context.Background(), "example.com")
		}()
	}
	barrier.Wait()
	time.Sleep(settle)
	close(release)
	done.Wait()

	want := netip.MustParseAddr("93.184.216.34")
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("resolve #%d: %v", i, errs[i])
		}
		if len(got[i]) != 1 || got[i][0] != want {
			t.Fatalf("resolve #%d: got %v, want [%v]", i, got[i], want)
		}
	}

	// Not an exact 1: a caller descheduled between the barrier and the group
	// registration still issues its own query, and the resolv.conf search list
	// can turn one lookup into several queries. Both scale every caller
	// equally, so a shared group stays an order below callers while a per-call
	// resolver sits at or above it.
	if q := queries.Load(); q > callers/2 {
		t.Fatalf("dns queries = %d for %d concurrent lookups of one host, want <= %d", q, callers, callers/2)
	}
}

// heldCountingDNS is countingDNS with every answer withheld until release
// closes, so all callers are still in flight when the count is taken. Answers
// go out off the read loop: a query arriving while an earlier one is
// unanswered must still be read, or the socket buffer would hide it from the
// count and the test would pass on its own blocking.
func heldCountingDNS(tb testing.TB, release <-chan struct{}) (addr string, queries *atomic.Int64, cleanup func()) {
	tb.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("heldCountingDNS ListenPacket: %v", err)
	}

	queries = new(atomic.Int64)
	closed := make(chan struct{})
	var answers sync.WaitGroup
	go func() {
		defer close(closed)
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
			query := append([]byte(nil), buf[:n]...)
			answers.Go(func() {
				select {
				case <-release:
				case <-closed:
				}
				conn.WriteTo(answeringResponder(query), peer)
			})
		}
	}()

	return conn.LocalAddr().String(), queries, func() {
		conn.Close()
		<-closed
		answers.Wait()
	}
}
