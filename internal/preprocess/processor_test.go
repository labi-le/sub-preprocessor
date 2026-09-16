package preprocess_test

import (
	"bytes"
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geoblock"
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

type fakeCountryLookup struct{}

func (fakeCountryLookup) LookupCountry(_ netip.Addr) geofeed.CountryCode {
	return geofeed.CountryCode{'N', 'L'}
}

func TestNewProcessorUsesPreloadedGeofeed(t *testing.T) {
	t.Parallel()

	fixedTime := time.Now().Add(-time.Hour)
	opts := preprocess.Options{
		PreloadedGeofeed: preprocess.GeoState{Lookup: fakeCountryLookup{}, LoadedAt: fixedTime},
		// SSRF-unreachable loopback: err==nil proves LoadAll was skipped.
		GeofeedSources: []geofeed.Source{{URL: "https://127.0.0.1:1/nonexistent", Type: "raw"}},
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor with preloaded geofeed must not fetch or error: %v", err)
	}

	state := p.GeofeedState()
	if state.Lookup == nil {
		t.Fatal("expected preloaded lookup to be carried over, got nil")
	}
	if !state.LoadedAt.Equal(fixedTime) {
		t.Fatalf("expected LoadedAt to carry over preloaded time %v, got %v", fixedTime, state.LoadedAt)
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

	state := p.GeofeedState()
	if state.Lookup == nil {
		t.Fatal("expected freshly loaded lookup, got nil")
	}
	if state.LoadedAt.Before(before) || time.Since(state.LoadedAt) > 5*time.Second {
		t.Fatalf("expected LoadedAt within 5s of now, got %v (before=%v)", state.LoadedAt, before)
	}
}

// TestNewProcessorGeofeedLoadFailureDegrades: a total geofeed load failure is
// not fatal when a refresh can close the window. Starting empty costs the
// feed's contribution to the lookup chain for one retry delay — the whole
// answer only empties where the chain names no other LOCAL provider; refusing to
// boot answers nothing at all until an operator notices.
func TestNewProcessorGeofeedLoadFailureDegrades(t *testing.T) {
	t.Parallel()

	opts := preprocess.Options{
		// SSRF-unreachable loopback: the load fails without touching the network.
		GeofeedSources:  []geofeed.Source{{URL: "https://127.0.0.1:1/geofeed.csv", Type: "raw"}},
		RefreshInterval: time.Hour,
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("geofeed load failure must degrade, not fail startup: %v", err)
	}

	state := p.GeofeedState()
	if state.Lookup == nil {
		t.Fatal("failed geofeed load must yield an empty lookup, not nil")
	}
	if !state.LoadedAt.IsZero() {
		t.Fatalf("failed geofeed load must leave LoadedAt zero for retry, got %v", state.LoadedAt)
	}
	if state.RetryAt.IsZero() || state.Failures != 1 {
		t.Fatalf("failed geofeed load must arm a backing-off retry, got RetryAt=%v failures=%d",
			state.RetryAt, state.Failures)
	}
}

// TestNewProcessorGeofeedLoadFailureFatalWithoutRefresh: with the refresh
// explicitly disabled no retry can fire, so the empty lookup would be
// permanent and every allow-list answer silently empty. That one stays fatal.
func TestNewProcessorGeofeedLoadFailureFatalWithoutRefresh(t *testing.T) {
	t.Parallel()

	opts := preprocess.Options{
		GeofeedSources: []geofeed.Source{{URL: "https://127.0.0.1:1/geofeed.csv", Type: "raw"}},
	}

	if _, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts); err == nil {
		t.Fatal("a geofeed failure with no retry scheduled must fail the build, not place nothing forever")
	}
}

// TestNewProcessorRefusesEmptyCarriedGeofeedWithoutRefresh: the reload
// carry-over is the second way a permanently-empty lookup could be reached —
// adopting a degraded state into a config that can never refresh it. That one
// logs nothing but "using preloaded geofeed lookup", so it is refused.
func TestNewProcessorRefusesEmptyCarriedGeofeedWithoutRefresh(t *testing.T) {
	t.Parallel()

	opts := preprocess.Options{
		PreloadedGeofeed: preprocess.GeoState{Lookup: geofeed.NewLookup(nil), Failures: 1},
		GeofeedSources:   []geofeed.Source{{URL: "https://127.0.0.1:1/geofeed.csv", Type: "raw"}},
	}

	if _, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts); err == nil {
		t.Fatal("an empty carried lookup with the refresh disabled must be refused, not adopted forever")
	}

	opts.RefreshInterval = time.Hour
	if _, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts); err != nil {
		t.Fatalf("the same carried state is fine where a retry can fire: %v", err)
	}
}

// TestNewProcessorWithoutGeofeedSourcesArmsNoRetry: no sources is the legal
// shape of "nothing asks the provider" (validateGeofeed), but LoadAll calls it
// an error, so the failure path would arm a retry that cannot succeed and warn
// on every backoff step forever.
func TestNewProcessorWithoutGeofeedSourcesArmsNoRetry(t *testing.T) {
	t.Parallel()

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), preprocess.Options{
		RefreshInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("a config that asks the geofeed nothing must build: %v", err)
	}

	state := p.GeofeedState()
	if state.Lookup == nil {
		t.Fatal("an unconfigured geofeed must still hand out an empty lookup, not nil")
	}
	if !state.RetryAt.IsZero() || state.Failures != 0 || state.LoadedAt.IsZero() {
		t.Fatalf("nothing to load means nothing to retry, got RetryAt=%v failures=%d loadedAt=%v",
			state.RetryAt, state.Failures, state.LoadedAt)
	}
}

// TestNewProcessorSkipsUnreferencedGeoDBs: when no annotate entry references
// dbip/registry, the databases are never built — the state getters return the
// zero GeoState even though the configs carry (unreachable) URLs that a build
// would hit.
func TestNewProcessorSkipsUnreferencedGeoDBs(t *testing.T) {
	t.Parallel()

	opts := preprocess.Options{
		PreloadedGeofeed: preprocess.GeoState{Lookup: fakeCountryLookup{}},
		Annotate:         []config.AnnotateSpec{{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}}},
		DBIP:             config.DBIPConfig{URL: "https://127.0.0.1:1/db-{yyyy-mm}.csv.gz", RefreshInterval: new(time.Hour)},
		Registry:         config.RegistryConfig{URLs: []string{"https://127.0.0.1:1/delegated"}, RefreshInterval: new(time.Hour)},
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	if state := p.DBIPState(); state.Lookup != nil || !state.LoadedAt.IsZero() {
		t.Fatalf("unreferenced dbip must not be built: got %+v", state)
	}
	if state := p.RegistryState(); state.Lookup != nil || !state.LoadedAt.IsZero() {
		t.Fatalf("unreferenced registry must not be built: got %+v", state)
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
		PreloadedGeofeed: preprocess.GeoState{Lookup: fakeCountryLookup{}},
		Annotate: []config.AnnotateSpec{
			{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderDBIP, config.ProviderRegistry}},
		},
		DBIP:              config.DBIPConfig{URL: "https://127.0.0.1:1/db-{yyyy-mm}.csv.gz", RefreshInterval: new(time.Hour)},
		Registry:          config.RegistryConfig{URLs: []string{"https://127.0.0.1:1/delegated"}, RefreshInterval: new(time.Hour)},
		PreloadedDBIP:     preprocess.GeoState{Lookup: fakeCountryLookup{}, LoadedAt: dbipAt},
		PreloadedRegistry: preprocess.GeoState{Lookup: fakeCountryLookup{}, LoadedAt: registryAt},
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor with preloaded geo DBs must not fetch or error: %v", err)
	}

	dbip := p.DBIPState()
	if dbip.Lookup == nil || !dbip.LoadedAt.Equal(dbipAt) {
		t.Fatalf("dbip preload not used: got %+v, want LoadedAt %v", dbip, dbipAt)
	}
	registry := p.RegistryState()
	if registry.Lookup == nil || !registry.LoadedAt.Equal(registryAt) {
		t.Fatalf("registry preload not used: got %+v, want LoadedAt %v", registry, registryAt)
	}
}

// TestNewProcessorGeoDBLoadFailureDegrades: a failing initial dbip/registry
// download must NOT fail startup — the processor starts with
// an empty lookup and a zero LoadedAt so the next refresh trigger retries.
func TestNewProcessorGeoDBLoadFailureDegrades(t *testing.T) {
	t.Parallel()

	opts := preprocess.Options{
		PreloadedGeofeed: preprocess.GeoState{Lookup: fakeCountryLookup{}},
		Annotate: []config.AnnotateSpec{
			{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP, config.ProviderRegistry}},
		},
		// SSRF-unreachable loopback: both loads fail without touching the network.
		DBIP:     config.DBIPConfig{URL: "https://127.0.0.1:1/db-{yyyy-mm}.csv.gz", RefreshInterval: new(time.Hour)},
		Registry: config.RegistryConfig{URLs: []string{"https://127.0.0.1:1/delegated"}, RefreshInterval: new(time.Hour)},
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("geo DB load failure must degrade, not fail startup: %v", err)
	}

	dbip := p.DBIPState()
	if dbip.Lookup == nil {
		t.Fatal("failed dbip load must yield an empty lookup, not nil")
	}
	if !dbip.LoadedAt.IsZero() {
		t.Fatalf("failed dbip load must leave LoadedAt zero for retry, got %v", dbip.LoadedAt)
	}
	if c := dbip.Lookup.LookupCountry(netip.MustParseAddr("1.2.3.4")); c != (geofeed.CountryCode{}) {
		t.Fatalf("empty dbip lookup must miss every IP, got %v", c)
	}
	registry := p.RegistryState()
	if registry.Lookup == nil || !registry.LoadedAt.IsZero() {
		t.Fatalf("failed registry load must yield empty lookup + zero LoadedAt, got %+v", registry)
	}
}

// TestGeoBlockedHostDroppedRegardlessOfCase drives the real *geoblock.Store
// through the pipeline, because this drop is the ONLY thing that carries a
// through-node API refusal past the cycle that found it: gemini/claude/chatgpt
// write the refused host here, and `processNode` drops it before DNS on every
// later cycle, so it never reaches the probe or /stable.txt again.
//
// The lookup must be case-insensitive. A host is a DNS name, and a second source
// is free to spell it differently; that mixed-case duplicate used to walk
// straight past a blocked host.
func TestGeoBlockedHostDroppedRegardlessOfCase(t *testing.T) {
	t.Parallel()

	store, err := geoblock.Open(filepath.Join(t.TempDir(), "gb.db"), 720*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	// The casing an API check would have written: APIOutcome.Server comes from
	// the mihomo proxy's Addr(), i.e. from whichever source line built it.
	if blockErr := store.Block("Blocked.Example.COM"); blockErr != nil {
		t.Fatal(blockErr)
	}

	p, err := preprocess.NewProcessor(context.Background(), zerolog.Nop(), preprocess.Options{
		PreloadedGeofeed: preprocess.GeoState{Lookup: fakeCountryLookup{}, LoadedAt: time.Now()},
		Blocklist:        store,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The blocked host lowercased, plus a bare-IP control that needs no DNS.
	nodes, stats, err := p.FilterNodes(context.Background(), preprocess.FilterRequest{
		Body:             []byte("vless://u@blocked.example.com:443#blocked\nvless://u@192.0.2.7:443#ok\n"),
		AllowedCountries: filter.All(),
	})
	if err != nil {
		t.Fatalf("FilterNodes: %v", err)
	}
	if stats.GeoBlockDrop != 1 {
		t.Errorf("geoblock_drop = %d, want 1: a blocked host drops whatever case the source used", stats.GeoBlockDrop)
	}
	if len(nodes) != 1 || stats.Kept != 1 {
		t.Fatalf("kept %d nodes (stats %+v), want the unblocked node alone", len(nodes), stats)
	}
	if !strings.Contains(nodes[0].Raw, "192.0.2.7") {
		t.Errorf("a geo-blocked host was republished: %q", nodes[0].Raw)
	}
}
