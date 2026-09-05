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

// TestParseOriginRecord_MultiOrigin pins the country of a prefix announced by
// several ASes. Field 0 is an AS LIST, and parsing it whole used to fail the
// record and throw away the country in field 2 -- so the annotate chain missed
// and the ASN filter fell open on every multi-origin prefix. Records below are
// verbatim Cymru origin answers for multi-origin prefixes.
func TestParseOriginRecord_MultiOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		txt         string
		wantASN     uint32
		wantCountry geofeed.CountryCode
	}{
		{
			name:        "single origin",
			txt:         "216071 | 146.103.121.0/24 | AE | ripencc | 1992-10-23",
			wantASN:     216071,
			wantCountry: geofeed.CountryCode{'A', 'E'},
		},
		{
			name:        "two origins arin",
			txt:         "15169 43515 | 35.212.128.0/17 | US | arin | 2017-09-29",
			wantASN:     15169,
			wantCountry: geofeed.CountryCode{'U', 'S'},
		},
		{
			name:        "two origins ripencc",
			txt:         "13238 208398 | 87.250.224.0/19 | RU | ripencc | 2006-01-30",
			wantASN:     13238,
			wantCountry: geofeed.CountryCode{'R', 'U'},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotASN, gotCountry, err := parseOriginRecord(tc.txt)
			if err != nil {
				t.Fatalf("parseOriginRecord(%q) = %v", tc.txt, err)
			}
			if gotCountry != tc.wantCountry {
				t.Errorf("country = %q, want %q", gotCountry.String(), tc.wantCountry.String())
			}
			// One number, because the caller builds AS<n>.asn.cymru.com from it.
			if gotASN != tc.wantASN {
				t.Errorf("asn = %d, want %d (first listed origin)", gotASN, tc.wantASN)
			}
		})
	}

	// A field 0 with no number at all is still an error: there is no AS to name
	// and no reason to invent a Resolve success shape for it.
	if _, _, err := parseOriginRecord("nonsense | 10.0.0.0/8 | US | arin | 2020-01-01"); err == nil {
		t.Error("an unparseable AS field must still error")
	}
}

// TestParseOriginRecords_EmptyAnswerIsNoRecord pins the guard that keeps an
// empty origin answer from reading as a parsed record: the caller builds
// AS<n>.asn.cymru.com from the result, and continuing with asn 0 would query a
// name Cymru does not serve. An answer with no parseable record must error.
func TestParseOriginRecords_EmptyAnswerIsNoRecord(t *testing.T) {
	t.Parallel()

	if _, _, err := parseOriginRecords(nil); !errors.Is(err, errEmptyOriginAnswer) {
		t.Fatalf("nil answer err = %v, want errEmptyOriginAnswer", err)
	}
	if _, _, err := parseOriginRecords([]string{}); !errors.Is(err, errEmptyOriginAnswer) {
		t.Fatalf("empty answer err = %v, want errEmptyOriginAnswer", err)
	}
	if _, _, err := parseOriginRecords([]string{"not a record"}); err == nil {
		t.Fatal("an answer with no parseable record must error")
	}

	// The first parseable record wins, as it did when the loop lived in
	// lookup: a leading malformed record must not lose the good one after it.
	asn, country, err := parseOriginRecords([]string{
		"garbage | x | ZZ | arin | 2020-01-01",
		"64500 | 192.0.2.0/24 | US | arin | 2020-01-01",
	})
	if err != nil {
		t.Fatalf("a later parseable record must win: %v", err)
	}
	if asn != 64500 || country != (geofeed.CountryCode{'U', 'S'}) {
		t.Fatalf("got asn %d country %q, want 64500 US", asn, country)
	}
}
