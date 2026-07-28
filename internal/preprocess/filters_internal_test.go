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

// geofeedOneRange is the fixture both tests below share: it knows one prefix as
// RU and nothing else, so every other address resolves to the zero CountryCode
// the two tests deliberately disagree about.
func geofeedOneRange() []geofeed.Entry {
	return []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: geofeed.CountryCode{'R', 'U'}},
	}
}

func TestGeofeedFilterExclusionKeepsUnknownCountry(t *testing.T) {
	t.Parallel()

	// Exclusion-only request (exclude_countries=RU): the node in RU goes, and
	// the one the geofeed cannot place stays — it is in no excluded country.
	f := NewGeofeedFilter()
	pctx := &PipelineContext{
		Lookup:  geofeed.NewLookup(geofeedOneRange()),
		Allowed: filter.All(),
		Denied:  filter.ParseAllowed("RU"),
		Stats:   &Stats{},
	}
	ips := []netip.Addr{
		netip.MustParseAddr("198.51.100.10"),
		netip.MustParseAddr("203.0.113.5"),
	}

	got := f.Process(context.Background(), ips, pctx)
	if len(got) != 1 {
		t.Fatalf("expected 1 of 2 IPs to survive an RU exclusion, kept %d", len(got))
	}
	if got[0] != netip.MustParseAddr("203.0.113.5") {
		t.Fatalf("expected the unplaceable IP to survive, got %s", got[0])
	}
	if pctx.Stats.GeoDrop != 0 {
		t.Fatalf("the node kept an IP, so no GeoDrop should be booked, got %d", pctx.Stats.GeoDrop)
	}
}

func TestGeofeedFilterAllowListDropsUnknownCountry(t *testing.T) {
	t.Parallel()

	// Same lookup, allow-list request (countries=DE): an IP the geofeed cannot
	// place is not in the allow-list, so it is dropped. Deny-list semantics must
	// not leak into this path.
	f := NewGeofeedFilter()
	pctx := &PipelineContext{
		Lookup:  geofeed.NewLookup(geofeedOneRange()),
		Allowed: filter.ParseAllowed("DE"),
		Stats:   &Stats{},
	}
	ips := []netip.Addr{netip.MustParseAddr("203.0.113.5")}

	got := f.Process(context.Background(), ips, pctx)
	if len(got) != 0 {
		t.Fatalf("allow-list must drop an unknown-country IP, kept %d", len(got))
	}
	if pctx.Stats.GeoDrop != 1 {
		t.Fatalf("expected GeoDrop=1, got %d", pctx.Stats.GeoDrop)
	}
}
