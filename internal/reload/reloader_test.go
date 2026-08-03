package reload_test

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/reload"
	"domains.lst/sub-preprocessor/internal/server"
	"domains.lst/sub-preprocessor/internal/stable"
)

// baseGeofeedYAML is a minimal, valid config whose single geofeed source points
// at an SSRF-unreachable loopback address. Combined with a preloaded geofeed in
// setupReloader, this guarantees no test ever performs a network fetch: if the
// reload path tried to fetch it, NewProcessor would fail (proving carry-over).
const baseGeofeedYAML = "geo:\n" +
	"  geofeed:\n" +
	"    sources:\n" +
	"      - url: https://127.0.0.1:1/geofeed\n" +
	"        type: raw\n"

// stubLookup is a no-op geofeed.CountryLookup used to preload a Processor so it
// never fetches geofeed data during tests.
type stubLookup struct{}

func (stubLookup) LookupCountry(_ netip.Addr) geofeed.CountryCode {
	return geofeed.CountryCode{'N', 'L'}
}

// failingFilterer satisfies stable.Filterer and always errors, guaranteeing the
// stable worker never performs network fetches from reload tests.
type failingFilterer struct{}

func (failingFilterer) FilterNodes(
	_ context.Context,
	_ preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	return nil, preprocess.Stats{}, errors.New("stub filterer: no network in tests")
}

//nolint:ireturn // implements stable.Filterer; handing out the interface is the point
func (failingFilterer) Annotator() preprocess.Annotator { return nil }

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config %s: %v", path, err)
	}
}

// setupReloader writes the base valid config to a temp file, loads it, builds a
// preloaded Processor seeded at loadedAt, installs it in a fresh Holder, and
// returns a Reloader primed with that state plus the config file path.
func setupReloader(
	t *testing.T,
	logger zerolog.Logger,
	loadedAt time.Time,
	ctl reload.Applier,
) (*reload.Reloader, *server.Holder, string) {
	t.Helper()
	ctx := t.Context()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeConfig(t, path, baseGeofeedYAML)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("initial config load: %v", err)
	}

	opts := reload.OptionsFromConfig(cfg)
	opts.PreloadedGeofeed = preprocess.GeoState{Lookup: stubLookup{}, LoadedAt: loadedAt}
	proc, err := preprocess.NewProcessor(ctx, logger, opts)
	if err != nil {
		t.Fatalf("initial processor: %v", err)
	}

	holder := server.NewHolder(&server.Snapshot{Svc: proc, Groups: cfg.Groups})
	r := reload.NewReloader(path, holder, logger, cfg, proc, ctl, nil)
	return r, holder, path
}

// TestOptionsFromConfig locks the cfg -> preprocess.Options mapping so it stays
// identical to the startup mapping in internal/app, and confirms the preload
// fields are left unset (startup fetches geofeed).
func TestOptionsFromConfig(t *testing.T) {
	t.Parallel()

	var cfg config.Config
	cfg.Geo.Geofeed.Sources = []geofeed.Source{{URL: "https://example.com/feed.csv", Type: "raw"}}
	cfg.Geo.Geofeed.RefreshInterval = new(7 * time.Minute)
	cfg.Resolver.Address = "9.9.9.9:53"
	cfg.Resolver.Timeout = 3 * time.Second
	dnsTTL, dnsNegTTL := 30*time.Minute, 10*time.Minute
	cfg.Resolver.CacheTTL = &dnsTTL
	cfg.Resolver.CacheNegativeTTL = &dnsNegTTL
	cfg.Geo.ASN.Timeout = 4 * time.Second
	cfg.Geo.ASN.CacheTTL = 24 * time.Hour
	cfg.Filters = []config.FilterConfig{{Type: config.FilterASN, DenyPatterns: []string{"^AS1234 ", "spammy"}}}
	// Two entries, both GEO: the only multi-entry annotate list the loader
	// still accepts, and the arity the DeepEqual below needs to prove the
	// whole ordered list is copied rather than its first element.
	cfg.Annotate = []config.AnnotateSpec{
		{Tag: "GEO", Providers: []string{"geofeed", "dbip"}},
		{Tag: "GEO", Providers: []string{"registry"}},
	}
	cfg.Geo.DBIP = config.DBIPConfig{URL: "https://mirror.example.com/db-{yyyy-mm}.csv.gz", RefreshInterval: new(time.Hour)}
	cfg.Geo.Registry = config.RegistryConfig{URLs: []string{"https://mirror.example.com/delegated"}, RefreshInterval: new(2 * time.Hour)}
	cfg.Fetch.Timeout = 3 * time.Second

	opts := reload.OptionsFromConfig(cfg)

	assertGeoOptions(t, opts, cfg)
	assertResolverOptions(t, opts, cfg)
	assertPipelineOptions(t, opts, cfg)
	assertNoPreloadedState(t, opts)
}

func assertGeoOptions(t *testing.T, opts preprocess.Options, cfg config.Config) {
	t.Helper()
	if !slices.Equal(opts.GeofeedSources, cfg.Geo.Geofeed.Sources) {
		t.Errorf("GeofeedSources: got %v want %v", opts.GeofeedSources, cfg.Geo.Geofeed.Sources)
	}
	if opts.RefreshInterval != *cfg.Geo.Geofeed.RefreshInterval {
		t.Errorf("RefreshInterval: got %v want %v", opts.RefreshInterval, *cfg.Geo.Geofeed.RefreshInterval)
	}
	if opts.ASNTimeout != cfg.Geo.ASN.Timeout {
		t.Errorf("ASNTimeout: got %v want %v", opts.ASNTimeout, cfg.Geo.ASN.Timeout)
	}
	if opts.ASNCacheTTL != cfg.Geo.ASN.CacheTTL {
		t.Errorf("ASNCacheTTL: got %v want %v", opts.ASNCacheTTL, cfg.Geo.ASN.CacheTTL)
	}
	if !reflect.DeepEqual(opts.DBIP, cfg.Geo.DBIP) {
		t.Errorf("DBIP: got %+v want %+v", opts.DBIP, cfg.Geo.DBIP)
	}
	if !reflect.DeepEqual(opts.Registry, cfg.Geo.Registry) {
		t.Errorf("Registry: got %+v want %+v", opts.Registry, cfg.Geo.Registry)
	}
}

func assertResolverOptions(t *testing.T, opts preprocess.Options, cfg config.Config) {
	t.Helper()
	if opts.DNSTimeout != cfg.Resolver.Timeout {
		t.Errorf("DNSTimeout: got %v want %v", opts.DNSTimeout, cfg.Resolver.Timeout)
	}
	if opts.DNSAddress != cfg.Resolver.Address {
		t.Errorf("DNSAddress: got %q want %q", opts.DNSAddress, cfg.Resolver.Address)
	}
	if opts.DNSCacheTTL != *cfg.Resolver.CacheTTL {
		t.Errorf("DNSCacheTTL: got %v want %v", opts.DNSCacheTTL, *cfg.Resolver.CacheTTL)
	}
	if opts.DNSCacheNegativeTTL != *cfg.Resolver.CacheNegativeTTL {
		t.Errorf("DNSCacheNegativeTTL: got %v want %v", opts.DNSCacheNegativeTTL, *cfg.Resolver.CacheNegativeTTL)
	}
}

// assertPipelineOptions covers the per-node stages: IPFilterSpecs() is a derived
// projection of cfg.Filters, not a plain copy, so it is the one mapping here
// that can silently drift.
func assertPipelineOptions(t *testing.T, opts preprocess.Options, cfg config.Config) {
	t.Helper()
	if !reflect.DeepEqual(opts.IPFilters, cfg.IPFilterSpecs()) {
		t.Errorf("IPFilters: got %v want %v", opts.IPFilters, cfg.IPFilterSpecs())
	}
	if !reflect.DeepEqual(opts.Annotate, cfg.Annotate) {
		t.Errorf("Annotate: got %v want %v", opts.Annotate, cfg.Annotate)
	}
	if opts.FetchTimeout != cfg.Fetch.Timeout {
		t.Errorf("FetchTimeout: got %v want %v", opts.FetchTimeout, cfg.Fetch.Timeout)
	}
}

// assertNoPreloadedState pins the half of the contract that is about what
// OptionsFromConfig must NOT do: carry-over of live geofeed/dbip/registry
// databases and of the DNS/ASN resolvers is the caller's decision, and a
// mapping that filled these in would hand the startup path stale objects.
func assertNoPreloadedState(t *testing.T, opts preprocess.Options) {
	t.Helper()
	assertZeroGeoState(t, "PreloadedGeofeed", opts.PreloadedGeofeed)
	assertZeroGeoState(t, "PreloadedDBIP", opts.PreloadedDBIP)
	assertZeroGeoState(t, "PreloadedRegistry", opts.PreloadedRegistry)
	if opts.PreloadedResolver != nil || opts.PreloadedASN != nil {
		t.Error("OptionsFromConfig must not set the preloaded resolvers")
	}
}

// assertZeroGeoState checks a carry-over slot is untouched in every field, so a
// mapping that started filling in the retry schedule alone would still fail.
func assertZeroGeoState(t *testing.T, name string, state preprocess.GeoState) {
	t.Helper()
	if state.Lookup != nil || !state.LoadedAt.IsZero() || !state.RetryAt.IsZero() || state.Failures != 0 {
		t.Errorf("OptionsFromConfig must not set %s: got %+v", name, state)
	}
}

// TestReloadNoOpOnIdenticalConfig covers AC4: reloading byte-identical config is
// a no-op — no new Processor is built and the holder snapshot pointer is
// unchanged (the only black-box signal available without a builder spy).
func TestReloadNoOpOnIdenticalConfig(t *testing.T) {
	loadedAt := time.Now().Add(-time.Hour)
	r, holder, _ := setupReloader(t, zerolog.Nop(), loadedAt, nil)

	before := holder.Load()
	r.Reload(t.Context()) // file content unchanged
	after := holder.Load()

	if before != after {
		t.Fatal("AC4: no-op reload must not swap the holder snapshot")
	}
}

// TestReloadKeepsOldOnError covers AC2, AC3, AC9 (plus malformed YAML): every
// load/validate/build failure must keep the previous settings — the holder
// snapshot pointer must never change.
func TestReloadKeepsOldOnError(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty file (AC2)", ""},
		{"valid yaml but empty geofeed sources (AC3)", "server:\n  listen: \":7777\"\n"},
		{"malformed yaml", "key: [a, b, c"},
		{"invalid asn regex (AC9)", baseGeofeedYAML + "filters:\n  - type: asn\n    deny_patterns:\n      - \"(\"\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loadedAt := time.Now().Add(-time.Hour)
			r, holder, path := setupReloader(t, zerolog.Nop(), loadedAt, nil)

			before := holder.Load()
			writeConfig(t, path, tc.content)
			r.Reload(t.Context())

			if holder.Load() != before {
				t.Fatalf("%s: holder must stay unchanged on error", tc.name)
			}
		})
	}
}

// TestReloadValidSwapCarriesGeofeed covers AC8: a valid edit (new ASN deny
// pattern) builds a new Processor and swaps it into the holder, while geofeed
// data (lookup + LoadedAt) is carried over because geofeed.sources are
// unchanged — proven by the preserved LoadedAt (no re-fetch occurred).
func TestReloadValidSwapCarriesGeofeed(t *testing.T) {
	loadedAt := time.Now().Add(-time.Hour)
	r, holder, path := setupReloader(t, zerolog.Nop(), loadedAt, nil)

	before := holder.Load()
	writeConfig(t, path, baseGeofeedYAML+"resolver:\n  timeout: 10s\n")
	r.Reload(t.Context())

	after := holder.Load()
	if after == before {
		t.Fatal("AC8: valid reload must swap the holder snapshot")
	}
	if after.Svc == before.Svc {
		t.Fatal("AC8: valid reload must build a new Processor")
	}

	newProc, ok := after.Svc.(*preprocess.Processor)
	if !ok {
		t.Fatalf("snapshot Svc must be *preprocess.Processor, got %T", after.Svc)
	}
	state := newProc.GeofeedState()
	if state.Lookup == nil {
		t.Fatal("geofeed lookup must be carried over (non-nil)")
	}
	if !state.LoadedAt.Equal(loadedAt) {
		t.Fatalf("AC8: LoadedAt must be carried over: got %v want %v", state.LoadedAt, loadedAt)
	}
}

// TestReloadFirstReloadCarriesLoadedAt covers AC12: on the first reload after
// startup, when geofeed.sources are unchanged, the existing LoadedAt must be
// carried over to the rebuilt Processor (no spurious geofeed reload), and the
// other changed fields (groups) must be applied to the new snapshot.
func TestReloadFirstReloadCarriesLoadedAt(t *testing.T) {
	loadedAt := time.Now().Add(-90 * time.Minute)
	r, holder, path := setupReloader(t, zerolog.Nop(), loadedAt, nil)

	writeConfig(t, path, baseGeofeedYAML+"groups:\n  nordics:\n    - FI\n    - SE\n")
	r.Reload(t.Context())

	after := holder.Load()
	newProc, ok := after.Svc.(*preprocess.Processor)
	if !ok {
		t.Fatalf("snapshot Svc must be *preprocess.Processor, got %T", after.Svc)
	}
	if at := newProc.GeofeedState().LoadedAt; !at.Equal(loadedAt) {
		t.Fatalf("AC12: first reload must carry LoadedAt: got %v want %v", at, loadedAt)
	}
	if len(after.Groups["nordics"]) != 2 {
		t.Fatalf("AC12: new groups must be applied in the swapped snapshot, got %v", after.Groups)
	}
}

// TestReloadCarriesGeofeedRetrySchedule pins VP-02 at the reload seam. loadedAt
// marks the last GOOD data, so after a failed load only retryAt holds the next
// attempt back; rebuilding the Processor without it re-downloads every geofeed
// source on the next request. The crawler rewrites private.yaml hourly, so a
// reload lands before any backoff longer than an hour ever comes due — carrying
// the data but not its schedule leaves the backoff permanently at attempt one.
func TestReloadCarriesGeofeedRetrySchedule(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeConfig(t, path, baseGeofeedYAML)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("initial config load: %v", err)
	}

	// What a chronically failing geofeed source leaves behind.
	failed := preprocess.GeoState{
		Lookup:   stubLookup{},
		LoadedAt: time.Now().Add(-25 * time.Hour),
		RetryAt:  time.Now().Add(20 * time.Minute),
		Failures: 3,
	}
	opts := reload.OptionsFromConfig(cfg)
	opts.PreloadedGeofeed = failed
	proc, err := preprocess.NewProcessor(ctx, zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("initial processor: %v", err)
	}

	holder := server.NewHolder(&server.Snapshot{Svc: proc, Groups: cfg.Groups})
	r := reload.NewReloader(path, holder, zerolog.Nop(), cfg, proc, nil, nil)

	writeConfig(t, path, baseGeofeedYAML+"resolver:\n  timeout: 10s\n")
	r.Reload(ctx)

	carried := reloadedProcessor(t, holder).GeofeedState()
	if !carried.RetryAt.Equal(failed.RetryAt) {
		t.Fatalf("reload must carry the geofeed retry deadline: got %v want %v", carried.RetryAt, failed.RetryAt)
	}
	if carried.Failures != failed.Failures {
		t.Fatalf("reload must carry the consecutive-failure count: got %d want %d", carried.Failures, failed.Failures)
	}
}

// TestReloadListenChangeWarns covers AC7: a server.listen-only change logs a
// WARN that a restart is required and is otherwise applied (the snapshot is
// still swapped) — the listener itself is never rebound by the reloader.
func TestReloadListenChangeWarns(t *testing.T) {
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	loadedAt := time.Now().Add(-time.Hour)
	r, holder, path := setupReloader(t, logger, loadedAt, nil)

	before := holder.Load()
	logBuf.Reset() // discard setup logs; capture only the reload

	writeConfig(t, path, "server:\n  listen: \":9999\"\n"+baseGeofeedYAML)
	r.Reload(t.Context())

	after := holder.Load()
	if after == before {
		t.Fatal("AC7: listen change must still swap the snapshot (other fields applied)")
	}
	if logs := logBuf.String(); !strings.Contains(logs, "requires restart") {
		t.Fatalf("AC7: expected a 'requires restart' warning, got logs:\n%s", logs)
	}
}

// subsYAML adds a minimal valid subscriptions block on top of the base config.
const subsYAML = baseGeofeedYAML +
	"subscriptions:\n" +
	"  sources:\n" +
	"    - name: alpha\n" +
	"      url: https://example.com/sub.txt\n"

// TestReloadAppliesSubscriptions: adding a subscriptions block must trigger
// Controller.Apply on the wired controller (observed via the reloader's own
// "subscriptions config applied" log, written only on a successful Apply).
// The controller gets a Nop logger and a failing filterer so its worker
// goroutine stays silent and offline.
func TestReloadAppliesSubscriptions(t *testing.T) {
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	ctl := stable.NewController(t.Context(), stable.NewHolder(),
		func() stable.Filterer { return failingFilterer{} }, nil, nil, zerolog.Nop(), nil)
	t.Cleanup(ctl.Stop)

	loadedAt := time.Now().Add(-time.Hour)
	r, _, path := setupReloader(t, logger, loadedAt, ctl)
	logBuf.Reset()

	writeConfig(t, path, subsYAML)
	r.Reload(t.Context())
	ctl.Stop()

	if logs := logBuf.String(); !strings.Contains(logs, "subscriptions config applied") {
		t.Fatalf("expected 'subscriptions config applied' log, got:\n%s", logs)
	}
}

// TestReloadSkipsApplyOnUnrelatedChange: an asn.deny_patterns edit (what the
// gemini sidecar rewrites periodically) must NOT restart the stable worker —
// no "subscriptions config applied" log may appear.
func TestReloadSkipsApplyOnUnrelatedChange(t *testing.T) {
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	ctl := stable.NewController(t.Context(), stable.NewHolder(),
		func() stable.Filterer { return failingFilterer{} }, nil, nil, zerolog.Nop(), nil)
	t.Cleanup(ctl.Stop)

	loadedAt := time.Now().Add(-time.Hour)
	r, holder, path := setupReloader(t, logger, loadedAt, ctl)

	before := holder.Load()
	logBuf.Reset()

	writeConfig(t, path, baseGeofeedYAML+"resolver:\n  timeout: 10s\n")
	r.Reload(t.Context())

	if holder.Load() == before {
		t.Fatal("valid asn edit must still swap the snapshot")
	}
	if logs := logBuf.String(); strings.Contains(logs, "subscriptions config applied") {
		t.Fatalf("unrelated change must not re-apply subscriptions, got:\n%s", logs)
	}
}

// fakeApplier implements reload.Applier: the first `failures` Apply calls
// error, later ones succeed; calls counts every invocation.
type fakeApplier struct {
	calls    int
	failures int
}

func (f *fakeApplier) Apply(config.Config) error {
	f.calls++
	if f.calls <= f.failures {
		return errors.New("fake apply failure")
	}
	return nil
}

// TestReloadRetriesApplyAfterFailure: a failed ctl.Apply must not commit the
// new config as current — re-saving the identical file must diff as changed
// and retry Apply instead of hitting the config.Equal fast path. Once Apply
// succeeds the config is committed and a further identical save is a no-op.
func TestReloadRetriesApplyAfterFailure(t *testing.T) {
	fake := &fakeApplier{failures: 1}
	loadedAt := time.Now().Add(-time.Hour)
	r, _, path := setupReloader(t, zerolog.Nop(), loadedAt, fake)

	writeConfig(t, path, subsYAML)
	r.Reload(t.Context())
	if fake.calls != 1 {
		t.Fatalf("first reload: expected 1 Apply call, got %d", fake.calls)
	}

	r.Reload(t.Context()) // identical file re-saved after the failure
	if fake.calls != 2 {
		t.Fatalf("re-save after failed Apply must retry: expected 2 calls, got %d", fake.calls)
	}

	r.Reload(t.Context()) // committed now; identical config is a no-op
	if fake.calls != 2 {
		t.Fatalf("identical config after successful Apply must not re-apply, got %d calls", fake.calls)
	}
}

// TestReloadRevertAfterFailedApplyRebuilds: after a failed ctl.Apply the holder
// serves a processor built from the NEW config while currentCfg still records the
// old one. Restoring the previous file is exactly what an operator does next, and
// keying the Equal fast path on currentCfg alone made that revert a silent no-op
// — the holder kept serving the rejected config with no further log line. A
// reload is redundant only when BOTH configs match.
func TestReloadRevertAfterFailedApplyRebuilds(t *testing.T) {
	fake := &fakeApplier{failures: 1}
	r, holder, path := setupReloader(t, zerolog.Nop(), time.Now().Add(-time.Hour), fake)

	writeConfig(t, path, subsYAML)
	r.Reload(t.Context())
	if fake.calls != 1 {
		t.Fatalf("expected the failing Apply to be attempted once, got %d", fake.calls)
	}
	rejected := holder.Load()

	writeConfig(t, path, baseGeofeedYAML) // the operator reverts
	r.Reload(t.Context())

	if holder.Load() == rejected {
		t.Fatal("reverting after a failed Apply must rebuild the holder, not hit the Equal fast path")
	}
	if fake.calls != 1 {
		t.Fatalf("the revert matches the last committed config, so the worker needs no re-apply: got %d calls", fake.calls)
	}
}

// TestReloadAppliesOnGeoBlockChange: a geoblock-only edit (gemini/claude prober
// settings) must reach ctl.Apply even though subscriptions and groups are
// unchanged — the prober is rebuilt from geoblock config on Apply.
func TestReloadAppliesOnGeoBlockChange(t *testing.T) {
	fake := &fakeApplier{}
	loadedAt := time.Now().Add(-time.Hour)
	r, _, path := setupReloader(t, zerolog.Nop(), loadedAt, fake)

	writeConfig(t, path, subsYAML)
	r.Reload(t.Context())
	if fake.calls != 1 {
		t.Fatalf("adding subscriptions: expected 1 Apply call, got %d", fake.calls)
	}

	writeConfig(t, path, subsYAML+"geoblock:\n  gemini:\n    timeout: 30s\n")
	r.Reload(t.Context())
	if fake.calls != 2 {
		t.Fatalf("geoblock edit must trigger Apply: expected 2 calls, got %d", fake.calls)
	}
}

// TestReloadStoresChangeWarns: a deadcache.ttl edit must log a restart-required
// WARN (the stores are built once at startup) while the snapshot still swaps.
func TestReloadStoresChangeWarns(t *testing.T) {
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	loadedAt := time.Now().Add(-time.Hour)
	r, holder, path := setupReloader(t, logger, loadedAt, nil)

	before := holder.Load()
	logBuf.Reset()

	writeConfig(t, path, baseGeofeedYAML+"deadcache:\n  ttl: 4h\n")
	r.Reload(t.Context())

	if holder.Load() == before {
		t.Fatal("valid deadcache edit must still swap the snapshot")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "requires restart") || !strings.Contains(logs, "deadcache.ttl") {
		t.Fatalf("expected restart-required warning naming deadcache.ttl, got:\n%s", logs)
	}
}

// TestReloadAppliesOnAnnotateChange: a workflow.annotate-only edit must reach
// ctl.Apply, because the stable worker bakes annotate into the bandwidth
// [SPD:] tag; otherwise the published tag goes stale until an unrelated change.
func TestReloadAppliesOnAnnotateChange(t *testing.T) {
	fake := &fakeApplier{}
	loadedAt := time.Now().Add(-time.Hour)
	r, _, path := setupReloader(t, zerolog.Nop(), loadedAt, fake)

	writeConfig(t, path, subsYAML)
	r.Reload(t.Context())
	if fake.calls != 1 {
		t.Fatalf("adding subscriptions: expected 1 Apply call, got %d", fake.calls)
	}

	writeConfig(t, path, subsYAML+"annotate:\n  - tag: GEO\n    providers: [geofeed]\n")
	r.Reload(t.Context())
	if fake.calls != 2 {
		t.Fatalf("annotate-only edit must trigger Apply: expected 2 calls, got %d", fake.calls)
	}
}

// geoDBYAML extends the base config with SSRF-unreachable dbip/registry URLs
// and an annotate chain referencing them, so both databases are built (from
// the preloads below — never the network) on every processor construction.
const geoDBYAML = baseGeofeedYAML +
	"  dbip:\n" +
	"    url: https://127.0.0.1:1/db-{yyyy-mm}.csv.gz\n" +
	"  registry:\n" +
	"    urls:\n" +
	"      - https://127.0.0.1:1/delegated\n" +
	"annotate:\n" +
	"  - tag: GEO\n" +
	"    providers: [geofeed, dbip, registry]\n"

// setupGeoDBReloader mirrors setupReloader for the geoDBYAML config, seeding
// the processor with preloaded geofeed/dbip/registry state so no network fetch
// ever happens.
func setupGeoDBReloader(
	t *testing.T,
	geofeedAt, dbipAt, registryAt time.Time,
) (*reload.Reloader, *server.Holder, string) {
	t.Helper()
	ctx := t.Context()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeConfig(t, path, geoDBYAML)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("initial config load: %v", err)
	}

	opts := reload.OptionsFromConfig(cfg)
	opts.PreloadedGeofeed = preprocess.GeoState{Lookup: stubLookup{}, LoadedAt: geofeedAt}
	opts.PreloadedDBIP = preprocess.GeoState{Lookup: stubLookup{}, LoadedAt: dbipAt}
	opts.PreloadedRegistry = preprocess.GeoState{Lookup: stubLookup{}, LoadedAt: registryAt}
	proc, err := preprocess.NewProcessor(ctx, zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("initial processor: %v", err)
	}

	holder := server.NewHolder(&server.Snapshot{Svc: proc, Groups: cfg.Groups})
	r := reload.NewReloader(path, holder, zerolog.Nop(), cfg, proc, nil, nil)
	return r, holder, path
}

// TestReloadCarriesDBIPAndRegistry mirrors TestReloadValidSwapCarriesGeofeed:
// an unrelated edit rebuilds the processor while the dbip and registry
// databases (lookup + LoadedAt) are carried over because their config blocks
// are unchanged — proven by the preserved LoadedAt (no re-download occurred).
func TestReloadCarriesDBIPAndRegistry(t *testing.T) {
	geofeedAt := time.Now().Add(-time.Hour)
	dbipAt := time.Now().Add(-2 * time.Hour)
	registryAt := time.Now().Add(-3 * time.Hour)
	r, holder, path := setupGeoDBReloader(t, geofeedAt, dbipAt, registryAt)

	before := holder.Load()
	writeConfig(t, path, geoDBYAML+"resolver:\n  timeout: 10s\n")
	r.Reload(t.Context())

	after := holder.Load()
	if after == before {
		t.Fatal("valid reload must swap the holder snapshot")
	}
	newProc, ok := after.Svc.(*preprocess.Processor)
	if !ok {
		t.Fatalf("snapshot Svc must be *preprocess.Processor, got %T", after.Svc)
	}
	dbip := newProc.DBIPState()
	if dbip.Lookup == nil {
		t.Fatal("dbip lookup must be carried over (non-nil)")
	}
	if !dbip.LoadedAt.Equal(dbipAt) {
		t.Fatalf("dbip LoadedAt must be carried over: got %v want %v", dbip.LoadedAt, dbipAt)
	}
	registry := newProc.RegistryState()
	if registry.Lookup == nil {
		t.Fatal("registry lookup must be carried over (non-nil)")
	}
	if !registry.LoadedAt.Equal(registryAt) {
		t.Fatalf("registry LoadedAt must be carried over: got %v want %v", registry.LoadedAt, registryAt)
	}
}

// TestReloadRefetchesDBIPOnConfigChange: a dbip.url edit must NOT carry the
// old database. The re-download against the unreachable URL fails, which by
// the degrade-to-empty rule yields a zero LoadedAt (retry on next refresh)
// instead of a build error; the unchanged registry is still carried.
func TestReloadRefetchesDBIPOnConfigChange(t *testing.T) {
	geofeedAt := time.Now().Add(-time.Hour)
	dbipAt := time.Now().Add(-2 * time.Hour)
	registryAt := time.Now().Add(-3 * time.Hour)
	r, holder, path := setupGeoDBReloader(t, geofeedAt, dbipAt, registryAt)

	changed := strings.Replace(geoDBYAML,
		"https://127.0.0.1:1/db-{yyyy-mm}.csv.gz",
		"https://127.0.0.1:1/other-{yyyy-mm}.csv.gz", 1)
	writeConfig(t, path, changed)
	r.Reload(t.Context())

	newProc, ok := holder.Load().Svc.(*preprocess.Processor)
	if !ok {
		t.Fatalf("snapshot Svc must be *preprocess.Processor, got %T", holder.Load().Svc)
	}
	dbip := newProc.DBIPState()
	if dbip.Lookup == nil {
		t.Fatal("changed dbip must still be built (empty lookup), not nil")
	}
	if !dbip.LoadedAt.IsZero() {
		t.Fatalf("changed dbip must not carry LoadedAt: got %v", dbip.LoadedAt)
	}
	if at := newProc.RegistryState().LoadedAt; !at.Equal(registryAt) {
		t.Fatalf("unchanged registry must be carried over: got %v want %v", at, registryAt)
	}
}

// cacheYAML adds an ASN annotate provider to the base config so the processor
// builds an asn.Resolver as well as the DNS one; the geofeed source is still
// the SSRF-unreachable loopback, and the ASN resolver never queries anything
// until a request arrives.
const cacheYAML = "geo:\n" +
	"  geofeed:\n" +
	"    sources:\n" +
	"      - url: https://127.0.0.1:1/geofeed\n" +
	"        type: raw\n" +
	"  asn:\n" +
	"    timeout: 5s\n" +
	"annotate:\n" +
	"  - tag: GEO\n" +
	"    providers: [geofeed, asn]\n"

func reloadedProcessor(t *testing.T, holder *server.Holder) *preprocess.Processor {
	t.Helper()
	proc, ok := holder.Load().Svc.(*preprocess.Processor)
	if !ok {
		t.Fatalf("snapshot Svc must be *preprocess.Processor, got %T", holder.Load().Svc)
	}
	return proc
}

// TestReloadCarriesResolverCaches: the DNS and Cymru caches are the reload's
// only state whose value IS the cache, and the crawler rewrites private.yaml
// hourly, so rebuilding them per reload means resolver.cache_ttl (1h) and
// geo.asn.cache_ttl (24h) can never be reached. An edit outside resolver.* /
// geo.asn.* must therefore hand both resolvers to the new processor, while an
// edit inside either block must rebuild that one — the TTLs and timeouts are
// baked in at construction, so carrying one would silently ignore the change.
func TestReloadCarriesResolverCaches(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeConfig(t, path, cacheYAML)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("initial config load: %v", err)
	}
	opts := reload.OptionsFromConfig(cfg)
	opts.PreloadedGeofeed = preprocess.GeoState{Lookup: stubLookup{}, LoadedAt: time.Now().Add(-time.Hour)}
	proc, err := preprocess.NewProcessor(ctx, zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("initial processor: %v", err)
	}
	dns, asnR := proc.ResolverState(), proc.ASNState()
	if dns == nil {
		t.Fatal("processor must own a DNS resolver")
	}
	if asnR == nil {
		t.Fatal("an asn annotate provider must build an ASN resolver")
	}

	holder := server.NewHolder(&server.Snapshot{Svc: proc, Groups: cfg.Groups})
	r := reload.NewReloader(path, holder, zerolog.Nop(), cfg, proc, nil, nil)

	// An edit that changes the processor but touches neither resolver block.
	unrelated := cacheYAML + "fetch:\n  timeout: 7s\n"
	writeConfig(t, path, unrelated)
	r.Reload(ctx)
	next := reloadedProcessor(t, holder)
	if next == proc {
		t.Fatal("valid reload must build a new processor")
	}
	if next.ResolverState() != dns {
		t.Error("edit outside resolver.* must carry the DNS resolver, not drop its cache")
	}
	if next.ASNState() != asnR {
		t.Error("edit outside geo.asn.* must carry the ASN resolver, not drop its cache")
	}

	dnsChanged := unrelated + "resolver:\n  cache_ttl: 5m\n"
	writeConfig(t, path, dnsChanged)
	r.Reload(ctx)
	next = reloadedProcessor(t, holder)
	if next.ResolverState() == dns {
		t.Error("resolver.cache_ttl edit must rebuild the resolver; the TTL is baked in at construction")
	}
	if next.ASNState() != asnR {
		t.Error("a resolver-only edit must still carry the untouched ASN resolver")
	}

	writeConfig(t, path, strings.Replace(dnsChanged, "timeout: 5s", "timeout: 6s", 1))
	r.Reload(ctx)
	if last := reloadedProcessor(t, holder); last.ASNState() == asnR {
		t.Error("geo.asn.timeout edit must rebuild the ASN resolver")
	}
}
