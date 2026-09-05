//go:build !race

package resolver_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/resolver"
)

// TestResolve_CacheHitAllocatesNothing pins the probe order in Resolve: the
// cache must be consulted before netip.ParseAddr. A warm domain hit is the
// steady per-request shape, and ParseAddr heap-allocates its ~48 B parse error
// on every domain host (the cost fetch.parseIPHost guards against), which made
// BenchmarkResolution_CachedHit read 1 alloc/op / 48 B/op before the reorder
// and 0 / 0 after. The rest of the hit path — RLock, map read, time.Now —
// cannot allocate, so zero is assertable.
func TestResolve_CacheHitAllocatesNothing(t *testing.T) {
	addr, cleanup := fakeDNS(t)
	defer cleanup()

	r := resolver.New(5*time.Second, addr, time.Hour, time.Hour)
	if _, err := r.Resolve(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	want := netip.MustParseAddr("93.184.216.34")
	allocs := testing.AllocsPerRun(50, func() {
		ips, err := r.Resolve(ctx, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if len(ips) != 1 || ips[0] != want {
			t.Fatalf("cached resolve = %v, want [%v]", ips, want)
		}
	})
	if allocs != 0 {
		t.Fatalf("a warm cache hit allocates %.0f per resolve, want 0: the cache probe moved behind netip.ParseAddr again", allocs)
	}
}
