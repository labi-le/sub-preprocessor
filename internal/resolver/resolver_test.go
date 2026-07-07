package resolver_test

import (
	"context"
	"net/netip"
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
