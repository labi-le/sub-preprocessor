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

// sampleNodeLine is the one node every annotator test relabels; the tests are
// about the tag prefix, not about parsing.
const sampleNodeLine = "vless://u@example.com:443#Old"

func parseOneNode(t *testing.T) subscription.Node {
	t.Helper()
	return parseNodeLine(t, sampleNodeLine)
}

func parseNodeLine(t *testing.T, line string) subscription.Node {
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

// TestAnnotateStripsUpstreamTagsItCannotWrite drives an upstream-authored name
// through the REAL annotate path — annotator plus rewrite.NodeName — with the
// shipped shape of one GEO tag. Since the ASN tag was retired nothing here
// writes `[ASN:`, and rewrite.isKnownTag's ASN arm is the only thing left that
// removes one: StripKnownTags consumes a CONTIGUOUS run, so if that arm goes,
// the scan stops at `[ASN:` and republishes an AS attribution we never made,
// with our own [GEO:] tag in front of it. Same argument as the `IP:` arm,
// which three tests in internal/rewrite already pin.
func TestAnnotateStripsUpstreamTagsItCannotWrite(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeoTrace, config.ProviderGeofeed}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}},
	})

	node := parseNodeLine(t, "vless://u@example.com:443#[GEO:RU][ASN:SOME-AS, RU] Moscow")
	var buf, tagBuf bytes.Buffer
	a.Annotate(context.Background(), &buf, &tagBuf, AnnotateRequest{Node: node, IP: netip.MustParseAddr("1.2.3.4")})

	want := "vless://u@example.com:443#[GEO:NL] Moscow"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

// TestAnnotatorTagListOrder: entries render in list order. GEO is the only tag
// the loader accepts, but nothing forbids a second entry with a different
// chain, so that is what a multi-tag name is made of now — and the country
// Annotate reports is the LEFTMOST one, the tag a reader of the name reads.
func TestAnnotatorTagListOrder(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
		{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}},
		config.ProviderDBIP:    fakeProvider{name: "dbip", info: geo.Info{Country: geofeed.CountryCode{'D', 'E'}}},
	})
	if a == nil {
		t.Fatal("expected a non-nil annotator")
	}

	node := parseOneNode(t)
	var buf, tagBuf bytes.Buffer
	got := a.Annotate(context.Background(), &buf, &tagBuf, AnnotateRequest{Node: node, IP: netip.MustParseAddr("1.2.3.4")})

	want := "vless://u@example.com:443#[GEO:NL][GEO:DE] Old"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
	if got != (geofeed.CountryCode{'N', 'L'}) {
		t.Fatalf("got country %q, want the leftmost tag's NL", got)
	}
}

// TestAnnotatorUnknownRendersQuestionMarks: a tag whose whole chain missed
// renders [GEO:??] in place, and does NOT claim the node's country — the
// reported code is the leftmost entry that RESOLVED, not the leftmost entry.
func TestAnnotatorUnknownRendersQuestionMarks(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
		{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed"},
		config.ProviderDBIP:    fakeProvider{name: "dbip", info: geo.Info{Country: geofeed.CountryCode{'S', 'E'}}},
	})

	node := parseOneNode(t)
	var buf, tagBuf bytes.Buffer
	got := a.Annotate(context.Background(), &buf, &tagBuf, AnnotateRequest{Node: node, IP: netip.MustParseAddr("9.9.9.9")})

	want := "vless://u@example.com:443#[GEO:??][GEO:SE] Old"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
	if got != (geofeed.CountryCode{'S', 'E'}) {
		t.Fatalf("got country %q, want SE: a missed tag must not claim the node's country", got)
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
	return annotateReq(t, a, AnnotateRequest{IP: netip.MustParseAddr(ip)})
}

// annotateReq fills in the node req leaves unset and returns the output.
func annotateReq(t *testing.T, a *annotator, req AnnotateRequest) string {
	t.Helper()
	if req.Node.Raw == "" {
		req.Node = parseOneNode(t)
	}
	var buf, tagBuf bytes.Buffer
	a.Annotate(context.Background(), &buf, &tagBuf, req)
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

// TestAnnotatorGeoChainFallback: a miss falls through to the next provider in
// the chain. The dbip step answers with an Info that is NOT empty but carries
// no country — the shape geo.asnProvider returns when Cymru names the AS and
// leaves the registry country blank — so this also pins that a chain reads the
// field its own tag renders, not "the provider answered something".
func TestAnnotatorGeoChainFallback(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderDBIP, config.ProviderRegistry}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed:  fakeProvider{name: "geofeed"},
		config.ProviderDBIP:     fakeProvider{name: "dbip", info: geo.Info{ASN: "SOME-AS, RU"}},
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

// geotraceAnnotator builds a GEO chain over the given provider names, wiring
// only geofeed into the provider map — geotrace deliberately has no entry
// there, so this also pins that newAnnotator resolves it without the
// "referenced but not built" fallback.
func geotraceAnnotator(t *testing.T, order ...string) *annotator {
	t.Helper()
	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: order},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}},
	})
	if a == nil {
		t.Fatal("expected a non-nil annotator")
	}
	return a
}

// TestAnnotatorGeoTraceChain: the geotrace step is a chain member like any
// other — it wins when it leads and answered, misses through to the offline
// database when the trace never ran, and loses to a provider ahead of it.
func TestAnnotatorGeoTraceChain(t *testing.T) {
	t.Parallel()

	egress := Egress{IP: netip.MustParseAddr("203.0.113.7"), Country: geofeed.CountryCode{'D', 'E'}}

	tests := []struct {
		name   string
		order  []string
		egress Egress
		want   string
	}{
		{
			name:   "trace first wins",
			order:  []string{config.ProviderGeoTrace, config.ProviderGeofeed},
			egress: egress,
			want:   "vless://u@example.com:443#[GEO:DE] Old",
		},
		{
			name:  "unmeasured trace falls through",
			order: []string{config.ProviderGeoTrace, config.ProviderGeofeed},
			want:  "vless://u@example.com:443#[GEO:NL] Old",
		},
		{
			name:   "trace behind a hit never runs",
			order:  []string{config.ProviderGeofeed, config.ProviderGeoTrace},
			egress: egress,
			want:   "vless://u@example.com:443#[GEO:NL] Old",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := geotraceAnnotator(t, tc.order...)
			req := AnnotateRequest{IP: netip.MustParseAddr("1.2.3.4"), Egress: tc.egress}
			if got := annotateReq(t, a, req); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAnnotateReturnsResolvedCountry: the returned code is what the publisher
// records as the node's country, so it must be the one the GEO chain resolved
// — and zero, not a rendered "??", when nothing did.
func TestAnnotateReturnsResolvedCountry(t *testing.T) {
	t.Parallel()

	a := geotraceAnnotator(t, config.ProviderGeoTrace, config.ProviderGeofeed)
	node := parseOneNode(t)
	var buf, tagBuf bytes.Buffer

	got := a.Annotate(context.Background(), &buf, &tagBuf, AnnotateRequest{
		Node:   node,
		IP:     netip.MustParseAddr("1.2.3.4"),
		Egress: Egress{IP: netip.MustParseAddr("203.0.113.7"), Country: geofeed.CountryCode{'D', 'E'}},
	})
	if got != (geofeed.CountryCode{'D', 'E'}) {
		t.Fatalf("got country %q, want DE", got)
	}

	// The zero code, not the "??" the name renders: a node whose country
	// nothing resolved must not be BOOKED under a country.
	allMiss := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed"},
	})
	buf.Reset()
	got = allMiss.Annotate(context.Background(), &buf, &tagBuf, AnnotateRequest{
		Node: node, IP: netip.MustParseAddr("1.2.3.4"),
	})
	if got != (geofeed.CountryCode{}) {
		t.Fatalf("an all-miss GEO chain must resolve no country, got %q", got)
	}
	if want := "vless://u@example.com:443#[GEO:??] Old"; buf.String() != want {
		t.Fatalf("got %q, want %q — the ?? is rendered, never returned", buf.String(), want)
	}
}

// TestAnnotatePrefixLeadsConfiguredTags: the caller-rendered prefix is the
// leftmost thing in the name — the worker's [SPD:] tag has no config entry to
// be ordered by, so its position is this contract alone.
func TestAnnotatePrefixLeadsConfiguredTags(t *testing.T) {
	t.Parallel()

	a := newAnnotator(zerolog.Nop(), []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
		{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP}},
	}, map[string]geo.Provider{
		config.ProviderGeofeed: fakeProvider{name: "geofeed", info: geo.Info{Country: geofeed.CountryCode{'N', 'L'}}},
		config.ProviderDBIP:    fakeProvider{name: "dbip", info: geo.Info{Country: geofeed.CountryCode{'D', 'E'}}},
	})
	req := AnnotateRequest{IP: netip.MustParseAddr("1.2.3.4"), Prefix: "[SPD:60M] "}

	want := "vless://u@example.com:443#[SPD:60M] [GEO:NL][GEO:DE] Old"
	if got := annotateReq(t, a, req); got != want {
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
