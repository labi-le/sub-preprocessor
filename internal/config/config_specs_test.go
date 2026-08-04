package config_test

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

// loadYAML writes content verbatim as config.yaml in a fresh temp dir and loads
// it. Distinct from config_test.go's loadRaw to keep the two files independent.
func loadYAML(t *testing.T, content string) (config.Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
}

const geoBase = "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"

const geoBaseGroups = geoBase + "groups:\n  geo_blocked: [RU, CN]\n"

// TestIPFilterSpecsSplit proves the unified filters list splits into IP-stage
// specs (country/asn) in config order, dropping the through-node types -- and
// that the projection carries only what preprocess consumes. The country
// exclusions are deliberately absent from IPFilterSpec: preprocess never read
// them, so projecting them advertised an enforcement that did not exist.
func TestIPFilterSpecsSplit(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Filters: []config.FilterConfig{
			{Type: config.FilterCountry, Provider: config.ProviderGeofeed, ExcludeGroups: []string{"geo_blocked"}, ExcludeCountries: []string{"CN"}},
			{Type: config.FilterClaude},
			{Type: config.FilterASN, DenyPatterns: []string{"spammy"}},
			{Type: config.FilterBandwidth, MinMbps: new(5)},
		},
	}

	got := cfg.IPFilterSpecs()
	want := []config.IPFilterSpec{
		{Type: config.FilterCountry, Provider: config.ProviderGeofeed},
		{Type: config.FilterASN, Provider: config.ProviderASN, DenyPatterns: []string{"spammy"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IPFilterSpecs()=%+v, want %+v", got, want)
	}
}

// TestDeniedCountries proves filters[].exclude_countries / exclude_groups have
// exactly one consumer: the worker's deny-set. Group membership is expanded,
// codes accumulate across country entries, and a non-country entry contributes
// nothing even when it carries the keys.
func TestDeniedCountries(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Groups: config.Groups{"geo_blocked": {"RU", "CN"}},
		Filters: []config.FilterConfig{
			{Type: config.FilterCountry, Provider: config.ProviderGeofeed, ExcludeGroups: []string{"geo_blocked"}},
			{Type: config.FilterCountry, Provider: config.ProviderASN, ExcludeCountries: []string{"ir"}},
			{Type: config.FilterClaude, ExcludeCountries: []string{"US"}},
		},
	}

	denied := cfg.DeniedCountries()
	for _, code := range []geofeed.CountryCode{{'R', 'U'}, {'C', 'N'}, {'I', 'R'}} {
		if !denied.Has(code) {
			t.Errorf("denied set must contain %s", code)
		}
	}
	if denied.Has(geofeed.CountryCode{'U', 'S'}) {
		t.Error("a non-country filter entry must contribute no exclusions")
	}
}

// TestNodeFilterSpecsSplit proves the through-node types (gemini/claude/
// chatgpt/tidal/bandwidth) split out in order, with the API configs merged over
// the geoblock defaults and bandwidth carrying its entry params.
func TestNodeFilterSpecsSplit(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		GeoBlock: config.GeoBlockConfig{
			Gemini:  config.GeminiConfig{Endpoint: "https://gemini.base", Marker: "base-marker", Model: "base-model", Timeout: 15 * time.Second, Concurrency: 8},
			Claude:  config.ClaudeConfig{Endpoint: "https://claude.base", Marker: "base-claude", Version: "2023-06-01", Timeout: 15 * time.Second, Concurrency: 8},
			ChatGPT: config.ChatGPTConfig{Endpoint: "https://chatgpt.base", Marker: "base-chatgpt", Timeout: 15 * time.Second, Concurrency: 8},
			Tidal:   config.TidalConfig{Endpoint: "https://tidal.base", Timeout: 15 * time.Second, Concurrency: 8},
		},
		Filters: []config.FilterConfig{
			{Type: config.FilterCountry, Provider: config.ProviderGeofeed},
			{Type: config.FilterClaude, Marker: "override-claude"},
			{Type: config.FilterBandwidth, MinMbps: new(9), TestURL: "https://speed/x", Timeout: 30 * time.Second, Concurrency: 2},
			{Type: config.FilterGemini, Model: "override-model"},
			{Type: config.FilterChatGPT, Concurrency: 3},
			{Type: config.FilterTidal, Timeout: 30 * time.Second},
		},
	}

	got := cfg.NodeFilterSpecs()
	if len(got) != 5 {
		t.Fatalf("NodeFilterSpecs() len=%d, want 5", len(got))
	}
	if got[0].Type != config.FilterClaude || got[1].Type != config.FilterBandwidth ||
		got[2].Type != config.FilterGemini || got[3].Type != config.FilterChatGPT ||
		got[4].Type != config.FilterTidal {
		t.Fatalf("order = %s,%s,%s,%s,%s", got[0].Type, got[1].Type, got[2].Type, got[3].Type, got[4].Type)
	}

	// claude: overridden marker, other fields inherited from geoblock base.
	if got[0].Claude.Marker != "override-claude" || got[0].Claude.Endpoint != "https://claude.base" || got[0].Claude.Version != "2023-06-01" {
		t.Fatalf("claude merge wrong: %+v", got[0].Claude)
	}
	// bandwidth: params come entirely from the entry.
	bw := got[1].Bandwidth
	if bw.MinMbps == nil || *bw.MinMbps != 9 || bw.TestURL != "https://speed/x" || bw.Timeout != 30*time.Second || bw.Concurrency != 2 {
		t.Fatalf("bandwidth spec wrong: %+v", bw)
	}
	// gemini: overridden model, other fields inherited from geoblock base.
	if got[2].Gemini.Model != "override-model" || got[2].Gemini.Endpoint != "https://gemini.base" || got[2].Gemini.Marker != "base-marker" {
		t.Fatalf("gemini merge wrong: %+v", got[2].Gemini)
	}
	// chatgpt: overridden concurrency, endpoint/marker inherited from the base.
	if got[3].ChatGPT.Concurrency != 3 || got[3].ChatGPT.Endpoint != "https://chatgpt.base" || got[3].ChatGPT.Marker != "base-chatgpt" {
		t.Fatalf("chatgpt merge wrong: %+v", got[3].ChatGPT)
	}
	// tidal: the overridden timeout applies, endpoint and the rest are inherited.
	if got[4].Tidal.Timeout != 30*time.Second ||
		got[4].Tidal.Endpoint != "https://tidal.base" || got[4].Tidal.Concurrency != 8 {
		t.Fatalf("tidal merge wrong: %+v", got[4].Tidal)
	}
}

// TestLoadFiltersCountryClaudeBandwidth loads a realistic filters block and
// asserts parsing plus per-entry defaulting (country provider, bandwidth knobs).
func TestLoadFiltersCountryClaudeBandwidth(t *testing.T) {
	t.Parallel()

	yaml := geoBaseGroups +
		"filters:\n" +
		"  - type: country\n" +
		"    exclude_groups: [geo_blocked]\n" +
		"  - type: claude\n" +
		"  - type: bandwidth\n" +
		"    min_mbps: 5\n" +
		"subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n"
	cfg, err := loadYAML(t, yaml)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Filters) != 3 {
		t.Fatalf("filters len=%d, want 3", len(cfg.Filters))
	}
	// country provider defaults to geofeed.
	if cfg.Filters[0].Provider != config.ProviderGeofeed {
		t.Fatalf("country provider default = %q, want geofeed", cfg.Filters[0].Provider)
	}
	// bandwidth entry defaults applied.
	bw := cfg.Filters[2]
	if bw.TestURL != "https://speed.cloudflare.com/__down?bytes=2000000" {
		t.Fatalf("bandwidth test_url default = %q", bw.TestURL)
	}
	if bw.Timeout != 20*time.Second || bw.Concurrency != 4 {
		t.Fatalf("bandwidth defaults = %+v", bw)
	}
	if bw.MinMbps == nil || *bw.MinMbps != 5 {
		t.Fatalf("bandwidth min_mbps = %v, want 5", bw.MinMbps)
	}
	// The split-out specs reflect the same list.
	if n := len(cfg.IPFilterSpecs()); n != 1 {
		t.Fatalf("IPFilterSpecs len=%d, want 1 (country)", n)
	}
	if n := len(cfg.NodeFilterSpecs()); n != 2 {
		t.Fatalf("NodeFilterSpecs len=%d, want 2 (claude+bandwidth)", n)
	}
}

// TestLoadRejectsBadFilters covers filter type/field validation at load time.
func TestLoadRejectsBadFilters(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		yaml    string
		wantErr string
	}{
		"unknown type":          {geoBase + "filters:\n  - type: bogus\n", `unknown type "bogus"`},
		"country bad provider":  {geoBase + "filters:\n  - type: country\n    provider: bogus\n", "country provider must be"},
		"country unknown group": {geoBase + "filters:\n  - type: country\n    exclude_groups: [nope]\n", "unknown group"},
		"country bad code":      {geoBase + "filters:\n  - type: country\n    exclude_countries: [RUS]\n", "invalid country code"},
		"asn bad regexp":        {geoBase + "filters:\n  - type: asn\n    deny_patterns: [\"(\"]\n", "invalid regexp"},
		"bandwidth neg mbps":    {geoBase + "filters:\n  - type: bandwidth\n    min_mbps: -1\n", "min_mbps must not be negative"},
		"bandwidth neg timeout": {geoBase + "filters:\n  - type: bandwidth\n    timeout: -1s\n", "timeout must be positive"},
		"bandwidth bad url":     {geoBase + "filters:\n  - type: bandwidth\n    test_url: ftp://x/y\n", "test_url"},
		"tidal neg concurrency": {geoBase + "filters:\n  - type: tidal\n    concurrency: -1\n", "filters[0].concurrency must not be negative"},
		"tidal neg timeout":     {geoBase + "filters:\n  - type: tidal\n    timeout: -1s\n", "filters[0].timeout must not be negative"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadYAML(t, tc.yaml)
			if err == nil {
				t.Fatalf("expected error for %s", name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s: error %q does not contain %q", name, err, tc.wantErr)
			}
		})
	}
}

// TestLoadTidalFilterInheritsDefaults proves a bare `{type: tidal}` entry
// resolves to a usable spec with no per-entry config: the keyless endpoint plus
// the built-in supported-country list come from the geoblock block verbatim.
func TestLoadTidalFilterInheritsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadYAML(t, geoBase+"filters:\n  - type: tidal\n")
	if err != nil {
		t.Fatal(err)
	}
	specs := cfg.NodeFilterSpecs()
	if len(specs) != 1 || specs[0].Type != config.FilterTidal {
		t.Fatalf("NodeFilterSpecs() = %+v, want one tidal spec", specs)
	}
	if !reflect.DeepEqual(specs[0].Tidal, cfg.GeoBlock.Tidal) {
		t.Fatalf("bare entry must inherit geoblock.tidal: %+v vs %+v", specs[0].Tidal, cfg.GeoBlock.Tidal)
	}
	if specs[0].Tidal.Endpoint == "" {
		t.Fatalf("tidal spec must be usable unconfigured: %+v", specs[0].Tidal)
	}
}

// TestLoadGeoDatabaseDefaults proves the dbip/registry blocks default to the
// free public database URLs with a 24h refresh when omitted.
func TestLoadGeoDatabaseDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadYAML(t, geoBase)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geo.DBIP.URL != "https://download.db-ip.com/free/dbip-country-lite-{yyyy-mm}.csv.gz" {
		t.Fatalf("dbip url default = %q", cfg.Geo.DBIP.URL)
	}
	if cfg.Geo.DBIP.RefreshInterval == nil || *cfg.Geo.DBIP.RefreshInterval != 24*time.Hour {
		t.Fatalf("dbip refresh default = %v, want 24h", cfg.Geo.DBIP.RefreshInterval)
	}
	wantURLs := []string{
		"https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-extended-latest",
		// APNIC via LACNIC's mirror: ftp.apnic.net's ServerHello does not echo
		// the legacy session ID, so crypto/tls cannot fetch it at all, and the
		// RIPE copy of the same bytes would stack apnic on top of ripencc's own
		// host (61% of the ranges behind one outage instead of 33%).
		"https://ftp.lacnic.net/pub/stats/apnic/delegated-apnic-extended-latest",
		"https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest",
		"https://ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-extended-latest",
		"https://ftp.afrinic.net/stats/afrinic/delegated-afrinic-extended-latest",
	}
	if !reflect.DeepEqual(cfg.Geo.Registry.URLs, wantURLs) {
		t.Fatalf("registry urls default = %v", cfg.Geo.Registry.URLs)
	}
	// LoadRegistry's outage promise is worth the number of DISTINCT hosts, and
	// ripencc is the big one: whatever the mirrors do, apnic must not land on
	// ripencc's host. Asserted as a rule rather than as a URL so a future edit
	// that re-points either one has to think about the concentration.
	hostOf := func(raw string) string {
		u, parseErr := url.Parse(raw)
		if parseErr != nil {
			t.Fatalf("parse %q: %v", raw, parseErr)
		}
		return u.Host
	}
	var ripencc, apnic string
	for _, u := range cfg.Geo.Registry.URLs {
		switch {
		case strings.Contains(u, "delegated-ripencc-"):
			ripencc = hostOf(u)
		case strings.Contains(u, "delegated-apnic-"):
			apnic = hostOf(u)
		}
	}
	if ripencc == "" || apnic == "" {
		t.Fatalf("default registry urls no longer cover ripencc and apnic: %v", cfg.Geo.Registry.URLs)
	}
	if ripencc == apnic {
		t.Fatalf("apnic is mirrored on ripencc's own host %q; that is 61%% of the ranges "+
			"behind one outage, see defaultRegistryURLs", apnic)
	}
	if cfg.Geo.Registry.RefreshInterval == nil || *cfg.Geo.Registry.RefreshInterval != 24*time.Hour {
		t.Fatalf("registry refresh default = %v, want 24h", cfg.Geo.Registry.RefreshInterval)
	}
}

// TestLoadGeoDatabaseOverrides proves explicit dbip/registry settings are kept
// verbatim, including a {yyyy-mm} placeholder in the URL.
func TestLoadGeoDatabaseOverrides(t *testing.T) {
	t.Parallel()

	yaml := geoBase +
		"  dbip:\n    url: https://mirror.example.com/db-{yyyy-mm}.csv.gz\n    refresh_interval: 1h\n" +
		"  registry:\n    urls: [https://mirror.example.com/delegated]\n    refresh_interval: 2h\n"
	cfg, err := loadYAML(t, yaml)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geo.DBIP.URL != "https://mirror.example.com/db-{yyyy-mm}.csv.gz" || *cfg.Geo.DBIP.RefreshInterval != time.Hour {
		t.Fatalf("dbip override = %+v", cfg.Geo.DBIP)
	}
	if !reflect.DeepEqual(cfg.Geo.Registry.URLs, []string{"https://mirror.example.com/delegated"}) || *cfg.Geo.Registry.RefreshInterval != 2*time.Hour {
		t.Fatalf("registry override = %+v", cfg.Geo.Registry)
	}
}

// TestLoadRejectsBadGeoDatabases covers dbip/registry URL and refresh
// validation at load time.
func TestLoadRejectsBadGeoDatabases(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		yaml    string
		wantErr string
	}{
		"dbip negative refresh":     {geoBase + "  dbip:\n    refresh_interval: -1s\n", "geo.dbip.refresh_interval must not be negative"},
		"registry negative refresh": {geoBase + "  registry:\n    refresh_interval: -1s\n", "geo.registry.refresh_interval must not be negative"},
		"dbip non-https":            {geoBase + "  dbip:\n    url: http://x.example.com/y.csv.gz\n", "geo.dbip.url"},
		"dbip missing host":         {geoBase + "  dbip:\n    url: https:///y.csv.gz\n", "geo.dbip.url"},
		"registry non-https":        {geoBase + "  registry:\n    urls: [ftp://x.example.com/delegated]\n", "geo.registry.urls[0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadYAML(t, tc.yaml)
			if err == nil {
				t.Fatalf("expected error for %s", name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s: error %q does not contain %q", name, err, tc.wantErr)
			}
		})
	}
}

// TestLoadAnnotateDefaultsAndValidation covers per-tag provider-chain
// defaulting and tag/providers validation, plus the retired `provider` key,
// which the strict decode now rejects by name instead of a hand-written check.
func TestLoadAnnotateDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	cfg, err := loadYAML(t, geoBase+"annotate:\n  - tag: GEO\n  - tag: GEO\n    providers: [dbip]\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Annotate) != 2 {
		t.Fatalf("annotate len=%d, want 2", len(cfg.Annotate))
	}
	if !reflect.DeepEqual(cfg.Annotate[0].Providers, []string{config.ProviderGeofeed}) {
		t.Fatalf("GEO providers default = %v, want [geofeed]", cfg.Annotate[0].Providers)
	}
	// GEO is the only accepted tag, so the list's remaining job is ordering
	// repeated entries: an explicit chain on the second one is not overwritten
	// by the default the first one got.
	if !reflect.DeepEqual(cfg.Annotate[1].Providers, []string{config.ProviderDBIP}) {
		t.Fatalf("explicit providers = %v, want [dbip]", cfg.Annotate[1].Providers)
	}

	rejects := map[string]struct {
		yaml    string
		wantErr string
	}{
		"unknown tag":      {geoBase + "annotate:\n  - tag: SPD\n", "unknown tag"},
		"renamed provider": {geoBase + "annotate:\n  - tag: GEO\n    provider: geofeed\n", "field provider not found in type config.AnnotateSpec"},
		"unknown provider": {geoBase + "annotate:\n  - tag: GEO\n    providers: [bogus]\n", `unknown provider "bogus"`},
		"retired ip tag":   {geoBase + "annotate:\n  - tag: IP\n", `unknown tag "IP"`},
		"retired asn tag":  {geoBase + "annotate:\n  - tag: ASN\n", `unknown tag "ASN"`},
		// The provider is named after its DATA SOURCE now, like every sibling.
		// No back-compat: the old spelling must fail the load naming the set
		// that is accepted, not decode to a provider nothing builds.
		"retired geotrace provider": {geoBase + "annotate:\n  - tag: GEO\n    providers: [geotrace]\n", `unknown provider "geotrace" (must be "cloudflare", "geofeed", "dbip", "registry" or "asn")`},
		"duplicate provider":        {geoBase + "annotate:\n  - tag: GEO\n    providers: [geofeed, dbip, geofeed]\n", `duplicate provider "geofeed"`},
	}
	for name, tc := range rejects {
		if _, rejErr := loadYAML(t, tc.yaml); rejErr == nil {
			t.Fatalf("%s: expected error", name)
		} else if !strings.Contains(rejErr.Error(), tc.wantErr) {
			t.Fatalf("%s: error %q does not contain %q", name, rejErr, tc.wantErr)
		}
	}
}

// TestLoadAnnotateProviderChain proves an explicit ordered chain is preserved
// verbatim across all five provider names.
func TestLoadAnnotateProviderChain(t *testing.T) {
	t.Parallel()

	cfg, err := loadYAML(t, geoBase+"annotate:\n  - tag: GEO\n    providers: [cloudflare, geofeed, dbip, registry, asn]\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		config.ProviderCloudflare, config.ProviderGeofeed,
		config.ProviderDBIP, config.ProviderRegistry, config.ProviderASN,
	}
	if !reflect.DeepEqual(cfg.Annotate[0].Providers, want) {
		t.Fatalf("providers = %v, want %v", cfg.Annotate[0].Providers, want)
	}
}

// TestAnnotateUsesProvider proves the flag the stable worker gates its trace
// probe on: a chain that cannot render the answer must not cost one request
// through every survivor.
func TestAnnotateUsesProvider(t *testing.T) {
	t.Parallel()

	cfg, err := loadYAML(t, geoBase+"annotate:\n  - tag: GEO\n    providers: [cloudflare, geofeed]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AnnotateUsesProvider(config.ProviderCloudflare) {
		t.Fatalf("cloudflare is in the GEO chain %v", cfg.Annotate[0].Providers)
	}
	if cfg.AnnotateUsesProvider(config.ProviderDBIP) {
		t.Fatal("dbip is in no chain, yet reported as used")
	}

	offline, err := loadYAML(t, geoBase+"annotate:\n  - tag: GEO\n    providers: [geofeed]\n")
	if err != nil {
		t.Fatal(err)
	}
	if offline.AnnotateUsesProvider(config.ProviderCloudflare) {
		t.Fatal("an offline-only chain must not arm the trace probe")
	}
}

// TestGeoTraceRenamedToCloudflare pins the rename with NO back-compat, in
// every namespace the old name ever appeared in: `geotrace` was never a filter
// type (it moved into the annotate chain in abf452b), is no longer a provider
// name, and no longer opens a geoblock sub-block. Each must fail loudly rather
// than silently drop a stage the operator asked for. The tail asserts the
// surviving surface: geo.cloudflare.{timeout,concurrency} and nothing else --
// `endpoint` was dropped outright, not moved.
func TestGeoTraceRenamedToCloudflare(t *testing.T) {
	t.Parallel()

	_, err := loadYAML(t, geoBase+"filters:\n  - type: geotrace\n")
	if err == nil {
		t.Fatal("expected error for the retired geotrace filter type")
	}
	if !strings.Contains(err.Error(), `unknown type "geotrace"`) {
		t.Fatalf("error %q does not name the unknown filter type", err)
	}

	_, err = loadYAML(t, geoBase+"geoblock:\n  geotrace:\n    timeout: 15s\n")
	if err == nil {
		t.Fatal("expected error for the retired geoblock.geotrace block")
	}
	if !strings.Contains(err.Error(), "field geotrace not found in type config.GeoBlockConfig") {
		t.Fatalf("error %q does not name the retired geoblock key", err)
	}

	// The replacement, and the whole surviving surface: two keys under geo,
	// no endpoint. An `endpoint:` here must fail the same way geoblock does.
	cfg, err := loadYAML(t, geoBase+"  cloudflare:\n    timeout: 30s\n    concurrency: 3\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geo.Cloudflare != (config.CloudflareConfig{Timeout: 30 * time.Second, Concurrency: 3}) {
		t.Fatalf("geo.cloudflare = %+v, want {30s 3}", cfg.Geo.Cloudflare)
	}
	_, err = loadYAML(t, geoBase+"  cloudflare:\n    endpoint: https://example.com/trace\n")
	if err == nil {
		t.Fatal("geo.cloudflare.endpoint was dropped, not renamed; it must fail the load")
	}
	if !strings.Contains(err.Error(), "field endpoint not found in type config.CloudflareConfig") {
		t.Fatalf("error %q does not name the dropped endpoint key", err)
	}
}

// TestShippedConfigLoads decodes the repository's own config/config.yaml with
// the strict decoder, so a key the schema stopped accepting cannot ship. It is
// copied into a temp dir on purpose: Load merges sibling overlays, and this
// test is about config.yaml alone.
func TestShippedConfigLoads(t *testing.T) {
	t.Parallel()

	shipped, err := os.ReadFile(filepath.Join("..", "..", "config", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadYAML(t, string(shipped))
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range cfg.Filters {
		if f.Type == "geotrace" {
			t.Fatalf("filters[%d] still lists the retired geotrace filter", i)
		}
	}
	if !cfg.AnnotateUsesProvider(config.ProviderCloudflare) {
		t.Fatal("shipped annotate chain no longer asks the node for its egress")
	}

	// asn is deliberately OUT of the shipped chain: it redistributes the same
	// RIR data registry already holds in memory, and measured behind
	// dbip+registry it answered nothing they had not. The chain is pinned whole
	// so re-adding it is a visible edit here, not a silent one.
	wantChain := []string{
		config.ProviderCloudflare, config.ProviderGeofeed,
		config.ProviderDBIP, config.ProviderRegistry,
	}
	if len(cfg.Annotate) != 1 || !reflect.DeepEqual(cfg.Annotate[0].Providers, wantChain) {
		t.Fatalf("shipped GEO chain = %+v, want one entry with %v", cfg.Annotate, wantChain)
	}
	if cfg.AnnotateUsesProvider(config.ProviderASN) {
		t.Fatal("asn is back in the shipped chain; it needs a fresh measurement, not a re-add")
	}

	// Retained on purpose, and neither form is reachable from the chain: the
	// country filter's asn provider, and the {type: asn} deny-pattern filter
	// whose AS names only Cymru carries. Both must still load.
	viaFilter, err := loadYAML(t, geoBase+
		"filters:\n  - type: country\n    provider: asn\n    exclude_countries: [ir]\n"+
		"  - type: asn\n    deny_patterns: [\"(?i)hetzner\"]\n"+
		"annotate:\n  - tag: GEO\n    providers: [geofeed]\n")
	if err != nil {
		t.Fatalf("the asn filter forms must still load: %v", err)
	}
	wantSpecs := []config.IPFilterSpec{
		{Type: config.FilterCountry, Provider: config.ProviderASN},
		{Type: config.FilterASN, Provider: config.ProviderASN, DenyPatterns: []string{"(?i)hetzner"}},
	}
	if !reflect.DeepEqual(viaFilter.IPFilterSpecs(), wantSpecs) {
		t.Fatalf("asn filter specs = %+v, want %+v", viaFilter.IPFilterSpecs(), wantSpecs)
	}
}

// TestFiltersChanged proves the filters list drives its own change detection.
func TestFiltersChanged(t *testing.T) {
	t.Parallel()

	a := config.Config{Filters: []config.FilterConfig{{Type: config.FilterCountry, Provider: config.ProviderGeofeed}}}
	b := a
	if config.FiltersChanged(a, b) {
		t.Fatal("identical filters must not differ")
	}
	b.Filters = append([]config.FilterConfig{}, a.Filters...)
	b.Filters = append(b.Filters, config.FilterConfig{Type: config.FilterClaude})
	if !config.FiltersChanged(a, b) {
		t.Fatal("appended filter must be detected")
	}
}
