package asn

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

func TestResolve_NegativeCachesFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("cymru unreachable")
	calls := 0

	r := New(time.Second, time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		calls++
		return Result{}, wantErr
	}

	ip := netip.MustParseAddr("192.0.2.1")
	for i := range 3 {
		res, err := r.Resolve(context.Background(), ip)
		if !errors.Is(err, wantErr) {
			t.Fatalf("resolve #%d: err = %v, want %v", i, err, wantErr)
		}
		if res != (Result{}) {
			t.Fatalf("resolve #%d: result = %+v, want zero", i, res)
		}
	}

	if calls != 1 {
		t.Fatalf("lookups = %d, want 1 (failure negative-cached)", calls)
	}
}

func TestResolve_CachesSuccess(t *testing.T) {
	t.Parallel()

	want := Result{Country: geofeed.CountryCode{'F', 'I'}, Name: "Example AS"}
	calls := 0

	r := New(time.Second, time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		calls++
		return want, nil
	}

	ip := netip.MustParseAddr("192.0.2.2")
	for i := range 3 {
		res, err := r.Resolve(context.Background(), ip)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		if res != want {
			t.Fatalf("resolve #%d: result = %+v, want %+v", i, res, want)
		}
	}

	if calls != 1 {
		t.Fatalf("lookups = %d, want 1 (success cached)", calls)
	}
}

func TestResolve_CanceledContextNotNegativeCached(t *testing.T) {
	t.Parallel()

	calls := 0

	r := New(time.Second, time.Hour)
	r.lookupFn = func(ctx context.Context, _ netip.Addr) (Result, error) {
		calls++
		return Result{}, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ip := netip.MustParseAddr("192.0.2.3")
	for i := range 2 {
		if _, err := r.Resolve(ctx, ip); !errors.Is(err, context.Canceled) {
			t.Fatalf("resolve #%d: err = %v, want context.Canceled", i, err)
		}
	}

	if calls != 2 {
		t.Fatalf("lookups = %d, want 2 (cancellation must not poison the cache)", calls)
	}
}

func TestResolve_CacheBounded(t *testing.T) {
	t.Parallel()

	r := New(time.Second, time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		return Result{Country: geofeed.CountryCode{'D', 'E'}}, nil
	}

	n := maxCacheEntries + maxCacheEntries/2
	for i := range n {
		ip := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		if _, err := r.Resolve(context.Background(), ip); err != nil {
			t.Fatalf("resolve %s: %v", ip, err)
		}
		if got := r.CacheLen(); got > maxCacheEntries {
			t.Fatalf("cache grew to %d entries, cap is %d", got, maxCacheEntries)
		}
	}
}

func TestResolve_UsesConfiguredCacheTTL(t *testing.T) {
	t.Parallel()

	r := New(time.Second, 12*time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		return Result{Country: geofeed.CountryCode{'F', 'I'}, Name: "AS"}, nil
	}
	ip := netip.MustParseAddr("192.0.2.10")
	before := time.Now()
	if _, err := r.Resolve(context.Background(), ip); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	exp := r.cache[ip].expiresAt
	r.mu.Unlock()
	if d := exp.Sub(before); d < 11*time.Hour || d > 13*time.Hour {
		t.Fatalf("positive TTL = %v, want ~12h", d)
	}

	// A zero/negative configured TTL falls back to the package default.
	if rd := New(time.Second, 0); rd.cacheTTL != defaultCacheTTL {
		t.Fatalf("zero ttl should fall back to %v, got %v", defaultCacheTTL, rd.cacheTTL)
	}
}
