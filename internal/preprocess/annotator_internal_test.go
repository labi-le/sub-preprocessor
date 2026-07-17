package preprocess

import (
	"bytes"
	"context"
	"net/netip"
	"testing"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geo"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// fakeProvider is a geo.Provider that returns a fixed Info regardless of the IP.
type fakeProvider struct {
	name string
	info geo.Info
}

func (f fakeProvider) Name() string                                { return f.name }
func (f fakeProvider) Lookup(context.Context, netip.Addr) geo.Info { return f.info }

func parseOneNode(t *testing.T, line string) subscription.Node {
	t.Helper()
	var node subscription.Node
	ok := false
	subscription.Parse([]byte(line), func(n subscription.Node) bool {
		node = n
		ok = true
		return false
	})
	if !ok {
		t.Fatalf("no node parsed from %q", line)
	}
	return node
}

func TestAnnotatorTagListOrder(t *testing.T) {
	t.Parallel()

	geofeedProv := fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}}
	asnProv := fakeProvider{name: "asn", info: geo.Info{ASN: "AS64500 EXAMPLE"}}
	a := newAnnotator([]config.AnnotateSpec{
		{Tag: config.TagGEO, Provider: config.ProviderGeofeed},
		{Tag: config.TagIP},
		{Tag: config.TagASN, Provider: config.ProviderASN},
	}, geofeedProv, asnProv)
	if a == nil {
		t.Fatal("expected a non-nil annotator")
	}

	node := parseOneNode(t, "vless://u@example.com:443#Old")
	var buf, tagBuf bytes.Buffer
	a.annotate(context.Background(), &buf, &tagBuf, node, netip.MustParseAddr("1.2.3.4"))

	want := "vless://u@example.com:443#[GEO:NL][IP:1.2.3.4][ASN:AS64500 EXAMPLE] Old"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

func TestAnnotatorUnknownGeoAndASN(t *testing.T) {
	t.Parallel()

	// Zero-value Info => unknown country and empty ASN, which must render as ??.
	geofeedProv := fakeProvider{name: "geofeed"}
	asnProv := fakeProvider{name: "asn"}
	a := newAnnotator([]config.AnnotateSpec{
		{Tag: config.TagGEO, Provider: config.ProviderGeofeed},
		{Tag: config.TagASN, Provider: config.ProviderASN},
		{Tag: config.TagIP},
	}, geofeedProv, asnProv)

	node := parseOneNode(t, "vless://u@example.com:443#Old")
	var buf, tagBuf bytes.Buffer
	a.annotate(context.Background(), &buf, &tagBuf, node, netip.MustParseAddr("9.9.9.9"))

	want := "vless://u@example.com:443#[GEO:??][ASN:??][IP:9.9.9.9] Old"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

func TestNewAnnotatorEmptyIsNil(t *testing.T) {
	t.Parallel()

	if a := newAnnotator(nil, nil, nil); a != nil {
		t.Fatal("empty specs must yield a nil annotator (annotation disabled)")
	}
}

func TestGeofeedFilterFullSetDropsNothing(t *testing.T) {
	t.Parallel()

	// A full allow set (no exclusions in effect) makes the country filter a
	// no-op: every IP is kept, including those with an unknown country.
	f := NewGeofeedFilter()
	pctx := &PipelineContext{Allowed: filter.All(), Lookup: nil, Stats: &Stats{}}
	ips := []netip.Addr{netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("5.6.7.8")}

	got := f.Process(context.Background(), ips, pctx)
	if len(got) != 2 {
		t.Fatalf("full allow set must drop nothing, kept %d of 2", len(got))
	}
	if pctx.Stats.GeoDrop != 0 {
		t.Fatalf("full allow set must not record GeoDrop, got %d", pctx.Stats.GeoDrop)
	}
}

func TestGeofeedFilterSubsetDropsUnknown(t *testing.T) {
	t.Parallel()

	// A non-full allow set drops IPs whose country is not allowed; a nil lookup
	// resolves every IP to the unknown country, which is not in {NL}.
	f := NewGeofeedFilter()
	var allowed filter.CountrySet
	allowed.Add("NL")
	pctx := &PipelineContext{Allowed: allowed, Lookup: nil, Stats: &Stats{}}
	ips := []netip.Addr{netip.MustParseAddr("1.2.3.4")}

	got := f.Process(context.Background(), ips, pctx)
	if len(got) != 0 {
		t.Fatalf("subset allow set must drop unknown-country IPs, kept %d", len(got))
	}
	if pctx.Stats.GeoDrop != 1 {
		t.Fatalf("expected GeoDrop=1, got %d", pctx.Stats.GeoDrop)
	}
}
