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

	r := resolver.New(5*time.Second, addr)

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
	r := resolver.New(5*time.Second, "")

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
	r := resolver.New(5*time.Second, "")

	if _, err := r.Resolve(context.Background(), "2001:db8::1"); err == nil {
		t.Fatal("expected error for bare IPv6 address")
	}
}
