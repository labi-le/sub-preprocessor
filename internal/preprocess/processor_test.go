package preprocess_test

import (
	"bytes"
	"context"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
	"github.com/rs/zerolog"
)

func TestRewriteNodeName(t *testing.T) {
	t.Parallel()

	var nodes []subscription.Node
	subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#Old Name"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})

	var b bytes.Buffer
	rewrite.NodeName(&b, nodes[0], "[GEO:NL][IP:198.51.100.10]")
	got := b.String()
	want := "vless://uuid@example.com:443?security=tls#[GEO:NL][IP:198.51.100.10] Old Name"
	if got != want {
		t.Fatalf("unexpected rewritten uri:\n got: %q\nwant: %q", got, want)
	}
}

func TestRewriteNodeNameUnknownSchemeStillRewritesURIFragment(t *testing.T) {
	t.Parallel()

	var nodes []subscription.Node
	subscription.Parse([]byte("trojan://uuid@example.com:443#Old Name"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})

	var b bytes.Buffer
	rewrite.NodeName(&b, nodes[0], "[GEO:NL][IP:198.51.100.10]")
	got := b.String()
	want := "trojan://uuid@example.com:443#[GEO:NL][IP:198.51.100.10] Old Name"
	if got != want {
		t.Fatalf("unexpected rewritten uri:\n got: %q\nwant: %q", got, want)
	}
}

func TestStripKnownTags(t *testing.T) {
	t.Parallel()

	if got := rewrite.StripKnownTags("[GEO:NL][IP:1.2.3.4][OK] Amsterdam 01"); got != "Amsterdam 01" {
		t.Fatalf("unexpected cleaned name: %q", got)
	}
}

func TestFormatStats(t *testing.T) {
	t.Parallel()

	got := preprocess.FormatStats(preprocess.Stats{Total: 10, Kept: 3, DNSDrop: 1, GeoDrop: 6, ASNDrop: 1})
	if !strings.Contains(got, "total=10") || !strings.Contains(got, "kept=3") {
		t.Fatalf("unexpected stats: %q", got)
	}
}

type fakeCountryLookup struct{}

func (fakeCountryLookup) LookupCountry(_ netip.Addr) geofeed.CountryCode {
	return geofeed.CountryCode{'N', 'L'}
}

func TestNewProcessorUsesPreloadedGeofeed(t *testing.T) {
	t.Parallel()

	fixedTime := time.Now().Add(-time.Hour)
	opts := preprocess.Options{
		PreloadedGeofeed:  fakeCountryLookup{},
		PreloadedLoadedAt: fixedTime,
		// SSRF-unreachable loopback: err==nil proves LoadAll was skipped.
		GeofeedSources: []geofeed.Source{{URL: "https://127.0.0.1:1/nonexistent", Type: "raw"}},
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor with preloaded geofeed must not fetch or error: %v", err)
	}

	lookup, at := p.GeofeedState()
	if lookup == nil {
		t.Fatal("expected preloaded lookup to be carried over, got nil")
	}
	if !at.Equal(fixedTime) {
		t.Fatalf("expected LoadedAt to carry over preloaded time %v, got %v", fixedTime, at)
	}
}

func TestNewProcessorLoadsGeofeedWhenNotPreloaded(t *testing.T) {
	if os.Getenv("LIVE_TESTS") == "" {
		t.Skip("live network test; set LIVE_TESTS=1 to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opts := preprocess.Options{
		GeofeedSources: []geofeed.Source{
			{URL: "https://www.gstatic.com/geofeed/corp_external", Type: "raw"},
		},
	}

	before := time.Now()
	p, err := preprocess.NewProcessor(ctx, zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor must load geofeed when not preloaded: %v", err)
	}

	lookup, at := p.GeofeedState()
	if lookup == nil {
		t.Fatal("expected freshly loaded lookup, got nil")
	}
	if at.Before(before) || time.Since(at) > 5*time.Second {
		t.Fatalf("expected LoadedAt within 5s of now, got %v (before=%v)", at, before)
	}
}

// TestNewProcessorSkipsUnreferencedGeoDBs: when no annotate entry references
// dbip/registry, the databases are never built — the state getters return nil
// even though the configs carry (unreachable) URLs that a build would hit.
func TestNewProcessorSkipsUnreferencedGeoDBs(t *testing.T) {
	t.Parallel()

	opts := preprocess.Options{
		PreloadedGeofeed: fakeCountryLookup{},
		Annotate:         []config.AnnotateSpec{{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}}},
		DBIP:             config.DBIPConfig{URL: "https://127.0.0.1:1/db-{yyyy-mm}.csv.gz", RefreshInterval: time.Hour},
		Registry:         config.RegistryConfig{URLs: []string{"https://127.0.0.1:1/delegated"}, RefreshInterval: time.Hour},
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	if lookup, at := p.DBIPState(); lookup != nil || !at.IsZero() {
		t.Fatalf("unreferenced dbip must not be built: got (%v, %v)", lookup, at)
	}
	if lookup, at := p.RegistryState(); lookup != nil || !at.IsZero() {
		t.Fatalf("unreferenced registry must not be built: got (%v, %v)", lookup, at)
	}
}

// TestNewProcessorUsesPreloadedGeoDBs: referenced dbip/registry databases take
// the preloaded lookup + LoadedAt (reload carry-over) instead of downloading —
// the unreachable URLs prove no fetch happened.
func TestNewProcessorUsesPreloadedGeoDBs(t *testing.T) {
	t.Parallel()

	dbipAt := time.Now().Add(-time.Hour)
	registryAt := time.Now().Add(-2 * time.Hour)
	opts := preprocess.Options{
		PreloadedGeofeed: fakeCountryLookup{},
		Annotate: []config.AnnotateSpec{
			{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderDBIP, config.ProviderRegistry}},
		},
		DBIP:                      config.DBIPConfig{URL: "https://127.0.0.1:1/db-{yyyy-mm}.csv.gz", RefreshInterval: time.Hour},
		Registry:                  config.RegistryConfig{URLs: []string{"https://127.0.0.1:1/delegated"}, RefreshInterval: time.Hour},
		PreloadedDBIP:             fakeCountryLookup{},
		PreloadedDBIPLoadedAt:     dbipAt,
		PreloadedRegistry:         fakeCountryLookup{},
		PreloadedRegistryLoadedAt: registryAt,
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor with preloaded geo DBs must not fetch or error: %v", err)
	}

	lookup, at := p.DBIPState()
	if lookup == nil || !at.Equal(dbipAt) {
		t.Fatalf("dbip preload not used: got (%v, %v), want LoadedAt %v", lookup, at, dbipAt)
	}
	lookup, at = p.RegistryState()
	if lookup == nil || !at.Equal(registryAt) {
		t.Fatalf("registry preload not used: got (%v, %v), want LoadedAt %v", lookup, at, registryAt)
	}
}

// TestNewProcessorGeoDBLoadFailureDegrades: a failing initial dbip/registry
// download must NOT fail startup (unlike geofeed) — the processor starts with
// an empty lookup and a zero LoadedAt so the next refresh trigger retries.
func TestNewProcessorGeoDBLoadFailureDegrades(t *testing.T) {
	t.Parallel()

	opts := preprocess.Options{
		PreloadedGeofeed: fakeCountryLookup{},
		Annotate: []config.AnnotateSpec{
			{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP, config.ProviderRegistry}},
		},
		// SSRF-unreachable loopback: both loads fail without touching the network.
		DBIP:     config.DBIPConfig{URL: "https://127.0.0.1:1/db-{yyyy-mm}.csv.gz", RefreshInterval: time.Hour},
		Registry: config.RegistryConfig{URLs: []string{"https://127.0.0.1:1/delegated"}, RefreshInterval: time.Hour},
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("geo DB load failure must degrade, not fail startup: %v", err)
	}

	lookup, at := p.DBIPState()
	if lookup == nil {
		t.Fatal("failed dbip load must yield an empty lookup, not nil")
	}
	if !at.IsZero() {
		t.Fatalf("failed dbip load must leave LoadedAt zero for retry, got %v", at)
	}
	if c := lookup.LookupCountry(netip.MustParseAddr("1.2.3.4")); c != (geofeed.CountryCode{}) {
		t.Fatalf("empty dbip lookup must miss every IP, got %v", c)
	}
	lookup, at = p.RegistryState()
	if lookup == nil || !at.IsZero() {
		t.Fatalf("failed registry load must yield empty lookup + zero LoadedAt, got (%v, %v)", lookup, at)
	}
}
