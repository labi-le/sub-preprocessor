package preprocess

import (
	"context"
	"net/netip"
	"regexp"
	"testing"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

type fakeASNResolver struct {
	results map[netip.Addr]asn.Result
}

func (f *fakeASNResolver) Resolve(_ context.Context, ip netip.Addr) (asn.Result, error) {
	if r, ok := f.results[ip]; ok {
		return r, nil
	}
	return asn.Result{}, nil
}

func TestASNFilterEnforcesAllowedCountries(t *testing.T) {
	t.Parallel()

	fakeR := &fakeASNResolver{results: map[netip.Addr]asn.Result{
		netip.MustParseAddr("1.2.3.4"):    {Country: geofeed.CountryCode{'F', 'I'}, Name: "CleanProvider"},
		netip.MustParseAddr("5.6.7.8"):    {Country: geofeed.CountryCode{'R', 'U'}, Name: "BlockedProvider"},
		netip.MustParseAddr("9.10.11.12"): {Country: geofeed.CountryCode{'D', 'E'}, Name: "CleanProvider DE"},
	}}

	f := NewASNFilter((*asn.Resolver)(nil), nil)
	f.resolver = fakeR

	allowed := filter.CountrySet{}
	allowed.Add("FI")
	allowed.Add("DE")

	pctx := &PipelineContext{
		Allowed: allowed,
		Stats:   &Stats{},
	}

	ips := []netip.Addr{
		netip.MustParseAddr("1.2.3.4"),    // FI — allowed
		netip.MustParseAddr("5.6.7.8"),    // RU — excluded
		netip.MustParseAddr("9.10.11.12"), // DE — allowed
	}

	got := f.Process(context.Background(), ips, pctx)
	expectedCount := 2
	if len(got) != expectedCount {
		t.Fatalf("expected %d IPs, got %d: %v", expectedCount, len(got), got)
	}
	if pctx.Stats.GeoDrop != 0 {
		t.Fatalf("expected 0 GeoDrop (not empty result), got %d", pctx.Stats.GeoDrop)
	}
}

func TestASNFilterCountryDropIncrementsGeoDrop(t *testing.T) {
	t.Parallel()

	fakeR := &fakeASNResolver{results: map[netip.Addr]asn.Result{
		netip.MustParseAddr("1.2.3.4"): {Country: geofeed.CountryCode{'R', 'U'}, Name: "SomeProvider"},
		netip.MustParseAddr("5.6.7.8"): {Country: geofeed.CountryCode{'R', 'U'}, Name: "AnotherProvider"},
	}}

	f := NewASNFilter((*asn.Resolver)(nil), nil)
	f.resolver = fakeR

	allowed := filter.CountrySet{}
	allowed.Add("FI")

	pctx := &PipelineContext{
		Allowed: allowed,
		Stats:   &Stats{},
	}

	ips := []netip.Addr{
		netip.MustParseAddr("1.2.3.4"),
		netip.MustParseAddr("5.6.7.8"),
	}

	got := f.Process(context.Background(), ips, pctx)
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d IPs", len(got))
	}
	if pctx.Stats.GeoDrop != 1 {
		t.Fatalf("expected 1 GeoDrop, got %d", pctx.Stats.GeoDrop)
	}
	if pctx.Stats.ASNDrop != 0 {
		t.Fatalf("expected 0 ASNDrop, got %d", pctx.Stats.ASNDrop)
	}
}

func TestASNFilterDenyNamePriorityOverCountry(t *testing.T) {
	t.Parallel()

	fakeR := &fakeASNResolver{results: map[netip.Addr]asn.Result{
		netip.MustParseAddr("1.2.3.4"): {Country: geofeed.CountryCode{'F', 'I'}, Name: "VDSINA Hosting"},
	}}

	pat := []*regexp.Regexp{regexp.MustCompile("(?i)VDSINA")}
	f := NewASNFilter((*asn.Resolver)(nil), pat)
	f.resolver = fakeR

	allowed := filter.All()

	pctx := &PipelineContext{
		Allowed: allowed,
		Stats:   &Stats{},
	}

	ips := []netip.Addr{netip.MustParseAddr("1.2.3.4")}

	got := f.Process(context.Background(), ips, pctx)
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d IPs", len(got))
	}
	if pctx.Stats.ASNDrop != 1 {
		t.Fatalf("expected 1 ASNDrop, got %d", pctx.Stats.ASNDrop)
	}
	if pctx.Stats.GeoDrop != 0 {
		t.Fatalf("expected 0 GeoDrop, got %d", pctx.Stats.GeoDrop)
	}
}
