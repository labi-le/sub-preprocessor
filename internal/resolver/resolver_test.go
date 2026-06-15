package resolver_test

import (
	"context"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/resolver"
)

func TestResolve_HotCacheHitHasZeroAllocs(t *testing.T) {
	addr, cleanup := fakeDNS(t)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, 5*time.Minute)
	if _, err := r.Resolve(context.Background(), "example.com"); err != nil {
		t.Fatalf("prewarm resolve: %v", err)
	}

	allocs := testing.AllocsPerRun(1000, func() {
		ips, err := r.Resolve(context.Background(), "example.com")
		if err != nil {
			t.Fatalf("hot resolve: %v", err)
		}
		if len(ips) == 0 {
			t.Fatal("expected cached ips")
		}
	})

	if allocs != 0 {
		t.Fatalf("expected zero allocations for hot cache hit, got %v", allocs)
	}
}
