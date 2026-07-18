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
	"github.com/rs/zerolog"
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
	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
		{Tag: config.TagIP},
		{Tag: config.TagASN, Providers: []string{config.ProviderASN}},
	}, map[string]geo.Provider{config.ProviderGeofeed: geofeedProv, config.ProviderASN: asnProv})
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
	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
		{Tag: config.TagASN, Providers: []string{config.ProviderASN}},
		{Tag: config.TagIP},
	}, map[string]geo.Provider{config.ProviderGeofeed: geofeedProv, config.ProviderASN: asnProv})

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

	if a := newAnnotator(zerolog.Nop(), nil, nil); a != nil {
		t.Fatal("empty specs must yield a nil annotator (annotation disabled)")
	}
}

// annotateOne runs a single node through the annotator and returns the output.
func annotateOne(t *testing.T, a *annotator, ip string) string {
	t.Helper()
	node := parseOneNode(t, "vless://u@example.com:443#Old")
	var buf, tagBuf bytes.Buffer
	a.annotate(context.Background(), &buf, &tagBuf, node, netip.MustParseAddr(ip))
	return buf.String()
}

// TestAnnotatorGeoChainOrder: the FIRST provider in the chain that returns a
// non-zero country wins, even when later providers would also answer.
func TestAnnotatorGeoChainOrder(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderDBIP}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}},
		config.ProviderDBIP:    fakeProvider{name: "dbip", info: geo.Info{Country: geofeed.CountryCode{'D', 'E'}}},
	})

	want := "vless://u@example.com:443#[GEO:NL] Old"
	if got := annotateOne(t, a, "1.2.3.4"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestAnnotatorGeoChainFallback: a miss (zero country) falls through to the
// next provider in the chain.
func TestAnnotatorGeoChainFallback(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderDBIP, config.ProviderRegistry}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed:  fakeProvider{name: "geofeed"},
		config.ProviderDBIP:     fakeProvider{name: "dbip"},
		config.ProviderRegistry: fakeProvider{name: "registry", info: geo.Info{Country: geofeed.CountryCode{'S', 'E'}}},
	})

	want := "vless://u@example.com:443#[GEO:SE] Old"
	if got := annotateOne(t, a, "1.2.3.4"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestAnnotatorGeoChainAllMiss: every provider missing renders [GEO:??].
func TestAnnotatorGeoChainAllMiss(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderDBIP}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed"},
		config.ProviderDBIP:    fakeProvider{name: "dbip"},
	})

	want := "vless://u@example.com:443#[GEO:??] Old"
	if got := annotateOne(t, a, "9.9.9.9"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestAnnotatorASNChainFallback: the ASN tag takes the first NON-EMPTY AS name
// in the chain; a provider that returns a country but no ASN is a miss.
func TestAnnotatorASNChainFallback(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagASN, Providers: []string{config.ProviderGeofeed, config.ProviderASN}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}},
		config.ProviderASN:     fakeProvider{name: "asn", info: geo.Info{ASN: "AS64500 EXAMPLE"}},
	})

	want := "vless://u@example.com:443#[ASN:AS64500 EXAMPLE] Old"
	if got := annotateOne(t, a, "1.2.3.4"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestAnnotatorSkipsUnbuiltProvider: a referenced-but-missing provider (a
// wiring bug by the lazy-build rule) is skipped, not fatal — the rest of the
// chain still resolves.
func TestAnnotatorSkipsUnbuiltProvider(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP, config.ProviderGeofeed}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}},
	})

	want := "vless://u@example.com:443#[GEO:NL] Old"
	if got := annotateOne(t, a, "1.2.3.4"); got != want {
		t.Fatalf("got %q, want %q", got, want)
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
