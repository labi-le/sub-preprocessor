package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

// Mirrors config_specs_test.go's geoBase; kept as a separate name so the two
// test files stay independently readable.
const geoPreamble = "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(geoPreamble)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Fatalf("unexpected listen: %q", cfg.Server.Listen)
	}
	if cfg.Resolver.Timeout != 5*time.Second {
		t.Fatalf("unexpected timeout: %v", cfg.Resolver.Timeout)
	}
	if len(cfg.Geo.Geofeed.Sources) != 1 {
		t.Fatalf("unexpected sources count: %d", len(cfg.Geo.Geofeed.Sources))
	}
	if cfg.Geo.Geofeed.Sources[0].Type != "gzip" {
		t.Fatalf("unexpected source type: %q", cfg.Geo.Geofeed.Sources[0].Type)
	}
	// Inverted deliberately: omitting geo.geofeed.refresh_interval used to leave
	// it 0, which the processor reads as "never refresh" -- a geofeed frozen for
	// the whole process lifetime. It now defaults like its dbip/registry
	// siblings, and only an explicit 0 disables the refresh.
	if cfg.Geo.Geofeed.RefreshInterval == nil || *cfg.Geo.Geofeed.RefreshInterval != 24*time.Hour {
		t.Fatalf("refresh interval default = %v, want 24h", cfg.Geo.Geofeed.RefreshInterval)
	}
	// geo.cloudflare's two defaults are the ones GeoConfig.applyDefaults owns
	// that no other test reads, and dropping them is silent: a zero Timeout
	// reaches apiProbeOne, context.WithTimeout(ctx, 0) expires before the dial,
	// and every survivor is booked unanswered -- the trace annotation goes
	// inert while nothing drops and no other assertion fails.
	if cfg.Geo.Cloudflare.Timeout != 15*time.Second {
		t.Fatalf("geo.cloudflare.timeout default = %v, want 15s", cfg.Geo.Cloudflare.Timeout)
	}
	if cfg.Geo.Cloudflare.Concurrency != 8 {
		t.Fatalf("geo.cloudflare.concurrency default = %d, want 8", cfg.Geo.Cloudflare.Concurrency)
	}
	// Same method, same gap: geo.asn.cache_ttl has TestLoadASNCacheTTL, the
	// timeout beside it had nothing.
	if cfg.Geo.ASN.Timeout != 5*time.Second {
		t.Fatalf("geo.asn.timeout default = %v, want 5s", cfg.Geo.ASN.Timeout)
	}
}

func TestLoadRejectsMissingGeofeedType(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error")
	}
}

// TestLoadGeofeedRefreshInterval covers the two explicit forms: a duration is
// kept verbatim, and an explicit 0 survives defaulting because the processor
// reads a non-positive interval as "load once, never refresh".
func TestLoadGeofeedRefreshInterval(t *testing.T) {
	t.Parallel()

	const sources = "    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"

	cfg, err := loadRaw(t, "geo:\n  geofeed:\n    refresh_interval: 24h\n"+sources)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geo.Geofeed.RefreshInterval == nil || *cfg.Geo.Geofeed.RefreshInterval != 24*time.Hour {
		t.Fatalf("unexpected refresh interval: %v", cfg.Geo.Geofeed.RefreshInterval)
	}

	disabled, err := loadRaw(t, "geo:\n  geofeed:\n    refresh_interval: 0s\n"+sources)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Geo.Geofeed.RefreshInterval == nil || *disabled.Geo.Geofeed.RefreshInterval != 0 {
		t.Fatalf("explicit 0 must be preserved as disable, got %v", disabled.Geo.Geofeed.RefreshInterval)
	}
}

// TestLoadGeoDBRefreshIntervalZeroDefaults pins the asymmetry with the geofeed
// sibling above: for the two downloadable databases an explicit 0 means the 24h
// default, exactly as it did before RefreshInterval became a pointer. Reading it
// as "load once, never refresh" would both flip a deployed config's behaviour
// silently and strand a failed initial download, because preprocess's geoDB
// short-circuits staleLocked on a non-positive interval before it consults the
// retry it armed.
func TestLoadGeoDBRefreshIntervalZeroDefaults(t *testing.T) {
	t.Parallel()

	const geoYAML = "geo:\n" +
		"  geofeed:\n" +
		"    sources:\n" +
		"      - url: https://example.com/geofeed.csv.gz\n" +
		"        type: gzip\n"

	// Both spelled "0s": a bare `0` is not a duration at all and fails the
	// decode outright, so the only explicit zero an operator can actually write
	// is this one.
	cfg, err := loadRaw(t, geoYAML+"  dbip:\n    refresh_interval: 0s\n  registry:\n    refresh_interval: 0s\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geo.DBIP.RefreshInterval == nil || *cfg.Geo.DBIP.RefreshInterval != 24*time.Hour {
		t.Errorf("explicit dbip 0 must default to 24h, got %v", cfg.Geo.DBIP.RefreshInterval)
	}
	if cfg.Geo.Registry.RefreshInterval == nil || *cfg.Geo.Registry.RefreshInterval != 24*time.Hour {
		t.Errorf("explicit registry 0 must default to 24h, got %v", cfg.Geo.Registry.RefreshInterval)
	}

	// The coercion must not swallow a negative: that is a typo, not a request
	// for the default.
	for _, block := range []string{"dbip", "registry"} {
		if _, err = loadRaw(t, geoYAML+"  "+block+":\n    refresh_interval: -1s\n"); err == nil {
			t.Errorf("geo.%s.refresh_interval: -1s must be rejected", block)
		}
	}
}

func TestLoadGroups(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(geoPreamble + "groups:\n  nordics:\n    - FI\n    - SE\n    - NO\n    - DK\n  baltics:\n    - EE\n    - LV\n    - LT\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Groups) != 2 {
		t.Fatalf("unexpected groups count: %d", len(cfg.Groups))
	}
	if len(cfg.Groups["nordics"]) != 4 {
		t.Fatalf("unexpected nordics countries: %v", cfg.Groups["nordics"])
	}
	if len(cfg.Groups["baltics"]) != 3 {
		t.Fatalf("unexpected baltics countries: %v", cfg.Groups["baltics"])
	}
}

func TestLoadRejectsInvalidGroupCountryCode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(geoPreamble + "groups:\n  invalid:\n    - XYZ\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for invalid country code")
	}
}

func TestLoadRejectsGroupWithEmptyName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(geoPreamble + "groups:\n  \"\":\n    - FI\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for empty group name")
	}
}

func TestEqual(t *testing.T) {
	var cfgA config.Config
	cfgA.Server.Listen = ":8080"
	cfgB := cfgA
	if !config.Equal(cfgA, cfgB) {
		t.Fatal("identical configs should be equal")
	}
	cfgB.Server.Listen = ":9090"
	if config.Equal(cfgA, cfgB) {
		t.Fatal("configs with different listen should not be equal")
	}
}

func TestGeofeedSourcesChanged(t *testing.T) {
	src := geofeed.Source{URL: "https://example.com/feed.csv", Type: "raw"}
	cfgA := config.Config{Geo: config.GeoConfig{Geofeed: config.GeofeedConfig{Sources: []geofeed.Source{src}}}}
	cfgB := cfgA
	if config.GeofeedSourcesChanged(cfgA, cfgB) {
		t.Fatal("identical sources should not be changed")
	}
	cfgB.Geo.Geofeed.Sources = append(cfgB.Geo.Geofeed.Sources, geofeed.Source{URL: "https://other.com/feed.csv", Type: "gzip"})
	if !config.GeofeedSourcesChanged(cfgA, cfgB) {
		t.Fatal("added source should be detected as changed")
	}
}

func TestDBIPChanged(t *testing.T) {
	t.Parallel()

	var cfgA config.Config
	cfgA.Geo.DBIP = config.DBIPConfig{URL: "https://example.com/db-{yyyy-mm}.csv.gz", RefreshInterval: new(24 * time.Hour)}
	cfgB := cfgA
	if config.DBIPChanged(cfgA, cfgB) {
		t.Fatal("identical dbip config should not be changed")
	}
	cfgB.Geo.DBIP.URL = "https://mirror.example.com/db.csv.gz"
	if !config.DBIPChanged(cfgA, cfgB) {
		t.Fatal("url change should be detected")
	}
	cfgC := cfgA
	cfgC.Geo.Registry.RefreshInterval = new(time.Hour)
	if config.DBIPChanged(cfgA, cfgC) {
		t.Fatal("registry change must not affect dbip diff")
	}
}

func TestRegistryChanged(t *testing.T) {
	t.Parallel()

	var cfgA config.Config
	cfgA.Geo.Registry = config.RegistryConfig{URLs: []string{"https://ftp.ripe.net/x"}, RefreshInterval: new(24 * time.Hour)}
	cfgB := cfgA
	if config.RegistryChanged(cfgA, cfgB) {
		t.Fatal("identical registry config should not be changed")
	}
	cfgB.Geo.Registry.URLs = append([]string{}, cfgA.Geo.Registry.URLs...)
	cfgB.Geo.Registry.URLs = append(cfgB.Geo.Registry.URLs, "https://ftp.apnic.net/y")
	if !config.RegistryChanged(cfgA, cfgB) {
		t.Fatal("added url should be detected")
	}
	cfgC := cfgA
	cfgC.Geo.DBIP.RefreshInterval = new(time.Hour)
	if config.RegistryChanged(cfgA, cfgC) {
		t.Fatal("dbip change must not affect registry diff")
	}
}

func TestListenChanged(t *testing.T) {
	var cfgA config.Config
	cfgA.Server.Listen = ":8080"
	cfgB := cfgA
	if config.ListenChanged(cfgA, cfgB) {
		t.Fatal("same listen should not be changed")
	}
	cfgB.Server.Listen = ":9090"
	if !config.ListenChanged(cfgA, cfgB) {
		t.Fatal("different listen should be detected")
	}
}

func writeConfig(t *testing.T, subsBlock string) (config.Config, error) {
	t.Helper()
	base := geoPreamble + "groups:\n  geo_blocked: [RU, IR]\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(base+subsBlock), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
}

func TestLoadResolverCacheDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resolver.CacheTTL == nil || *cfg.Resolver.CacheTTL != 30*time.Minute {
		t.Fatalf("cache_ttl: %v", cfg.Resolver.CacheTTL)
	}
	if cfg.Resolver.CacheNegativeTTL == nil || *cfg.Resolver.CacheNegativeTTL != 10*time.Minute {
		t.Fatalf("cache_negative_ttl: %v", cfg.Resolver.CacheNegativeTTL)
	}
}

func TestLoadResolverCacheExplicit(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "resolver:\n  cache_ttl: 1h\n  cache_negative_ttl: 5m\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resolver.CacheTTL == nil || *cfg.Resolver.CacheTTL != time.Hour {
		t.Fatalf("cache_ttl: %v", cfg.Resolver.CacheTTL)
	}
	if cfg.Resolver.CacheNegativeTTL == nil || *cfg.Resolver.CacheNegativeTTL != 5*time.Minute {
		t.Fatalf("cache_negative_ttl: %v", cfg.Resolver.CacheNegativeTTL)
	}
}

func TestLoadRejectsNegativeResolverCache(t *testing.T) {
	t.Parallel()

	for _, block := range []string{
		"resolver:\n  cache_ttl: -1s\n",
		"resolver:\n  cache_negative_ttl: -1s\n",
	} {
		if _, err := writeConfig(t, block); err == nil {
			t.Fatalf("expected error for %q", block)
		}
	}
}

func TestLoadSubscriptionsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "subscriptions:\n  sources:\n    - name: mifa\n      url: https://mifa.world/vless\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SubscriptionsEnabled() {
		t.Fatal("expected subscriptions enabled")
	}
	s := cfg.Subscriptions
	if s.Interval != 30*time.Minute {
		t.Fatalf("interval: %v", s.Interval)
	}
	c := s.Check
	if c.Rounds != 5 || c.MaxFail != 0 || c.MaxAvgMs != 1000 || c.Concurrency != 16 {
		t.Fatalf("check defaults: %+v", c)
	}
	if c.Timeout != 2*time.Second {
		t.Fatalf("check durations: %+v", c)
	}
	if c.TestURL != "https://www.gstatic.com/generate_204" || c.ExpectedStatus != "204" {
		t.Fatalf("check url/status: %+v", c)
	}
	if len(s.Sources) != 1 || s.Sources[0].Name != "mifa" || s.Sources[0].URL != "https://mifa.world/vless" {
		t.Fatalf("sources: %+v", s.Sources)
	}
}

func TestLoadSubscriptionsFullBlock(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, `subscriptions:
  interval: 15m
  check:
    rounds: 3
    timeout: 1500ms
    test_url: https://cp.cloudflare.com/generate_204
    expected_status: "200/204"
    max_fail: 1
    max_avg_ms: 800
    concurrency: 8
  sources:
    - name: alpha
      url: https://a.example.com/sub
    - name: beta
      url: https://b.example.com/sub
`)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Subscriptions
	if s.Interval != 15*time.Minute {
		t.Fatalf("subs: %+v", s)
	}
	c := s.Check
	if c.Rounds != 3 || c.Timeout != 1500*time.Millisecond ||
		c.TestURL != "https://cp.cloudflare.com/generate_204" || c.ExpectedStatus != "200/204" ||
		c.MaxFail != 1 || c.MaxAvgMs != 800 || c.Concurrency != 8 {
		t.Fatalf("check: %+v", c)
	}
	if len(s.Sources) != 2 || s.Sources[1].Name != "beta" {
		t.Fatalf("sources: %+v", s.Sources)
	}
}

func TestSubscriptionsDisabledWhenAbsent(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SubscriptionsEnabled() {
		t.Fatal("expected disabled")
	}
}

func TestLoadRejectsBadSubscriptions(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"bad name":        "subscriptions:\n  sources:\n    - name: Mifa!\n      url: https://a.example.com/s\n",
		"dup name":        "subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n    - name: a\n      url: https://b.example.com/s\n",
		"http url":        "subscriptions:\n  sources:\n    - name: a\n      url: http://a.example.com/s\n",
		"private ip":      "subscriptions:\n  sources:\n    - name: a\n      url: https://192.168.1.1/s\n",
		"unknown group":   "filters:\n  - type: country\n    exclude_groups: [nope]\n",
		"bad country":     "filters:\n  - type: country\n    exclude_countries: [RUS]\n",
		"short interval":  "subscriptions:\n  interval: 10s\n  sources:\n    - name: a\n      url: https://a.example.com/s\n",
		"zero rounds":     "subscriptions:\n  check:\n    rounds: -1\n  sources:\n    - name: a\n      url: https://a.example.com/s\n",
		"bad concurrency": "subscriptions:\n  check:\n    concurrency: -2\n  sources:\n    - name: a\n      url: https://a.example.com/s\n",
	}
	for name, block := range cases {
		if _, err := writeConfig(t, block); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestSubscriptionsChanged(t *testing.T) {
	t.Parallel()

	a, err := writeConfig(t, "subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n")
	if err != nil {
		t.Fatal(err)
	}
	b := a
	if config.SubscriptionsChanged(a, b) {
		t.Fatal("identical configs must not differ")
	}
	b.Subscriptions.Sources = append([]config.SubscriptionSource{}, a.Subscriptions.Sources...)
	b.Subscriptions.Sources = append(b.Subscriptions.Sources, config.SubscriptionSource{Name: "b", URL: "https://b.example.com/s"})
	if !config.SubscriptionsChanged(a, b) {
		t.Fatal("source add must be detected")
	}
	c := a
	c.Geo.ASN.Timeout = 42 * time.Second
	if config.SubscriptionsChanged(a, c) {
		t.Fatal("asn change must not affect subscriptions diff")
	}
}

func TestGroupsChanged(t *testing.T) {
	t.Parallel()

	a, err := writeConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	b := a
	if config.GroupsChanged(a, b) {
		t.Fatal("identical groups must not differ")
	}
	b.Groups = config.Groups{"geo_blocked": {"RU"}}
	if !config.GroupsChanged(a, b) {
		t.Fatal("groups change must be detected")
	}
}

// loadRaw writes content verbatim as config.yaml in a fresh temp dir and loads it.
func loadRaw(t *testing.T, content string) (config.Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	const base = geoPreamble
	const subs = "subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n"
	cases := map[string]struct {
		yaml    string
		wantErr string
	}{
		"negative gemini concurrency":     {base + "geoblock:\n  gemini:\n    concurrency: -1\n", "geoblock.gemini.concurrency"},
		"negative claude concurrency":     {base + "geoblock:\n  claude:\n    concurrency: -1\n", "geoblock.claude.concurrency"},
		"negative gemini timeout":         {base + "geoblock:\n  gemini:\n    timeout: -1s\n", "geoblock.gemini.timeout"},
		"negative claude timeout":         {base + "geoblock:\n  claude:\n    timeout: -1s\n", "geoblock.claude.timeout"},
		"negative chatgpt concurrency":    {base + "geoblock:\n  chatgpt:\n    concurrency: -1\n", "geoblock.chatgpt.concurrency"},
		"negative chatgpt timeout":        {base + "geoblock:\n  chatgpt:\n    timeout: -1s\n", "geoblock.chatgpt.timeout"},
		"negative tidal concurrency":      {base + "geoblock:\n  tidal:\n    concurrency: -1\n", "geoblock.tidal.concurrency"},
		"negative tidal timeout":          {base + "geoblock:\n  tidal:\n    timeout: -1s\n", "geoblock.tidal.timeout"},
		"negative cloudflare concurrency": {geoPreamble + "  cloudflare:\n    concurrency: -1\n", "geo.cloudflare.concurrency"},
		"negative cloudflare timeout":     {geoPreamble + "  cloudflare:\n    timeout: -1s\n", "geo.cloudflare.timeout"},
		"negative geoblock ttl":           {base + "geoblock:\n  ttl: -1h\n", "geoblock.ttl"},
		"negative resolver timeout":       {base + "resolver:\n  timeout: -1s\n", "resolver.timeout"},
		"negative asn timeout":            {geoPreamble + "  asn:\n    timeout: -1s\n", "geo.asn.timeout"},
		"negative fetch timeout":          {base + "fetch:\n  timeout: -1s\n", "fetch.timeout"},
		"negative deadcache ttl":          {base + "deadcache:\n  ttl: -1h\n", "deadcache.ttl"},
		"negative geofeed refresh":        {"geo:\n  geofeed:\n    refresh_interval: -1m\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n", "geo.geofeed.refresh_interval"},
		"negative subs interval":          {base + "subscriptions:\n  interval: -1m\n", "subscriptions.interval"},
		"negative check timeout":          {base + "subscriptions:\n  check:\n    timeout: -1s\n", "subscriptions.check.timeout"},
		"negative check source timeout":   {base + "subscriptions:\n  check:\n    source_timeout: -1s\n", "subscriptions.check.source_timeout"},
		"unknown filter type":             {base + "filters:\n  - type: bogus\n", `unknown type "bogus"`},
		"bad log level":                   {base + "log:\n  level: verbose\n", "log.level"},
		"bad expected status":             {base + subs + "  check:\n    expected_status: not-a-range\n", "expected_status"},
		"non-http test url":               {base + subs + "  check:\n    test_url: ftp://example.com/generate_204\n", "test_url"},
		"hostless test url":               {base + subs + "  check:\n    test_url: ./relative\n", "test_url"},
	}
	for name, tc := range cases {
		_, err := loadRaw(t, tc.yaml)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: error %q does not name %q", name, err, tc.wantErr)
		}
	}
}

func TestLoadAcceptsValidNewKnobs(t *testing.T) {
	t.Parallel()

	cfg, err := loadRaw(t, "log:\n  level: WARN\n"+
		"geoblock:\n  gemini:\n    concurrency: 4\n    timeout: 20s\n"+
		geoPreamble+
		"subscriptions:\n  check:\n    expected_status: 200/204\n    test_url: http://www.gstatic.com/generate_204\n  sources:\n    - name: a\n      url: https://a.example.com/s\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "WARN" {
		t.Fatalf("log level: %q", cfg.Log.Level)
	}
	if cfg.GeoBlock.Gemini.Concurrency != 4 || cfg.GeoBlock.Gemini.Timeout != 20*time.Second {
		t.Fatalf("gemini: %+v", cfg.GeoBlock.Gemini)
	}
	// The URL test egresses through the proxy node; plain http is legitimate
	// there and must not be rejected by host-side SSRF rules.
	if cfg.Subscriptions.Check.TestURL != "http://www.gstatic.com/generate_204" {
		t.Fatalf("test_url: %q", cfg.Subscriptions.Check.TestURL)
	}
}

// TestChatGPTDefaults pins the shipped OpenAI check defaults: the compliance
// endpoint host and the error code it returns for a refused egress. Both are
// keyless, so the filter works unconfigured.
func TestChatGPTDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadRaw(t, geoPreamble)
	if err != nil {
		t.Fatal(err)
	}
	cg := cfg.GeoBlock.ChatGPT
	if cg.Endpoint != "https://api.openai.com" || cg.Marker != "unsupported_country" {
		t.Fatalf("chatgpt endpoint/marker defaults: %+v", cg)
	}
	if cg.Timeout != 15*time.Second || cg.Concurrency != 8 {
		t.Fatalf("chatgpt timeout/concurrency defaults: %+v", cg)
	}
}

// TestTidalDefaults pins the shipped Tidal check defaults. Keyless and with no
// country list: the gate only asks whether Tidal answered the egress at all.
func TestTidalDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadRaw(t, geoPreamble)
	if err != nil {
		t.Fatal(err)
	}
	td := cfg.GeoBlock.Tidal
	if td.Endpoint != "https://api.tidal.com" {
		t.Fatalf("tidal endpoint default: %+v", td)
	}
	if td.Timeout != 15*time.Second || td.Concurrency != 8 {
		t.Fatalf("tidal timeout/concurrency defaults: %+v", td)
	}
}

func TestLoadMergesPrivateConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := geoPreamble + "subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n"
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	priv := "subscriptions:\n  sources:\n    - name: b\n      url: https://b.example.com/s\n"
	if err := os.WriteFile(filepath.Join(dir, "private.yaml"), []byte(priv), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Subscriptions.Sources) != 2 || cfg.Subscriptions.Sources[1].Name != "b" {
		t.Fatalf("private sources not merged: %+v", cfg.Subscriptions.Sources)
	}
}

// TestLoadFailsOnUnreadablePrivateConfig: a private.yaml that exists but cannot
// be read must fail Load — silently skipping it would drop the crawler-managed
// sources from the output. Only fs.ErrNotExist skips the overlay.
func TestLoadFailsOnUnreadablePrivateConfig(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := geoPreamble
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private.yaml"), []byte("subscriptions: {}\n"), 0o000); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for unreadable private.yaml")
	} else if !strings.Contains(err.Error(), "read private config") {
		t.Fatalf("error %q does not name the private config read", err)
	}
}

func TestProberChanged(t *testing.T) {
	t.Parallel()

	a, err := writeConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	b := a
	if config.ProberChanged(a, b) {
		t.Fatal("identical configs must not differ")
	}
	b.GeoBlock.Gemini.Timeout = 42 * time.Second
	if !config.ProberChanged(a, b) {
		t.Fatal("gemini sub-config change must be detected")
	}
	cg := a
	cg.GeoBlock.ChatGPT.Marker = "changed"
	if !config.ProberChanged(a, cg) {
		t.Fatal("chatgpt sub-config change must be detected")
	}
	td := a
	td.GeoBlock.Tidal.Endpoint = "https://tidal.changed"
	if !config.ProberChanged(a, td) {
		t.Fatal("tidal sub-config change must be detected")
	}
	c := a
	c.Filters = []config.FilterConfig{{Type: config.FilterASN, DenyPatterns: []string{"changed"}}}
	if config.ProberChanged(a, c) {
		t.Fatal("asn change must not affect prober diff")
	}
	d := a
	d.GeoBlock.DBPath = "/elsewhere.db"
	d.GeoBlock.TTL = 99 * time.Hour
	if config.ProberChanged(a, d) {
		t.Fatal("store-only geoblock fields (db_path/ttl) must not restart the worker; StoresChanged covers them")
	}
}

func TestStoresChanged(t *testing.T) {
	t.Parallel()

	a, err := writeConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	b := a
	if config.StoresChanged(a, b) {
		t.Fatal("identical configs must not differ")
	}
	for name, mut := range map[string]func(*config.Config){
		"db_path":       func(c *config.Config) { c.GeoBlock.DBPath = "/new/path.db" },
		"geoblock ttl":  func(c *config.Config) { c.GeoBlock.TTL = time.Hour },
		"deadcache ttl": func(c *config.Config) { d := time.Hour; c.DeadCache.TTL = &d },
	} {
		m := a
		mut(&m)
		if !config.StoresChanged(a, m) {
			t.Fatalf("%s change must be detected", name)
		}
	}
	c := a
	c.GeoBlock.Gemini.Timeout = 42 * time.Second
	if config.StoresChanged(a, c) {
		t.Fatal("gemini change must not require restart")
	}
}

func TestLoadBandwidthDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "subscriptions:\n  sources:\n    - name: mifa\n      url: https://mifa.world/vless\nfilters:\n  - type: bandwidth\n")
	if err != nil {
		t.Fatal(err)
	}
	var b config.BandwidthConfig
	found := false
	for _, spec := range cfg.NodeFilterSpecs() {
		if spec.Type == config.FilterBandwidth {
			b = spec.Bandwidth
			found = true
		}
	}
	if !found {
		t.Fatal("no bandwidth filter spec found")
	}
	if b.TestURL != "https://speed.cloudflare.com/__down?bytes=2000000" {
		t.Fatalf("test_url default = %q", b.TestURL)
	}
	if b.MinMbps == nil || *b.MinMbps != 5 {
		t.Fatalf("min_mbps default = %v, want 5", b.MinMbps)
	}
	if b.Timeout != 20*time.Second {
		t.Fatalf("timeout default = %v, want 20s", b.Timeout)
	}
	if b.Concurrency != 4 {
		t.Fatalf("concurrency default = %d, want 4", b.Concurrency)
	}

	// An explicit min_mbps: 0 means "no floor" and must survive defaulting.
	cfg0, err := writeConfig(t, "subscriptions:\n  sources:\n    - name: mifa\n      url: https://mifa.world/vless\nfilters:\n  - type: bandwidth\n    min_mbps: 0\n")
	if err != nil {
		t.Fatal(err)
	}
	var m *int
	for _, spec := range cfg0.NodeFilterSpecs() {
		if spec.Type == config.FilterBandwidth {
			m = spec.Bandwidth.MinMbps
		}
	}
	if m == nil || *m != 0 {
		t.Fatalf("explicit min_mbps=0 must be preserved, got %v", m)
	}
}

func TestLoadRejectsInvalidBandwidth(t *testing.T) {
	t.Parallel()

	// Negative values survive the "==0 -> default" coercion and reach validation.
	const base = geoPreamble
	cases := map[string]string{
		"negative timeout":     "filters:\n  - type: bandwidth\n    timeout: -1s\n",
		"negative concurrency": "filters:\n  - type: bandwidth\n    concurrency: -1\n",
		"negative min_mbps":    "filters:\n  - type: bandwidth\n    min_mbps: -1\n",
		"non-http test_url":    "filters:\n  - type: bandwidth\n    test_url: ftp://example.com/x\n",
	}
	for name, block := range cases {
		if _, err := loadRaw(t, base+block); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestLoadASNCacheTTL(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geo.ASN.CacheTTL != 24*time.Hour {
		t.Fatalf("asn.cache_ttl default = %v, want 24h", cfg.Geo.ASN.CacheTTL)
	}

	const base = geoPreamble
	cfg2, err := loadRaw(t, base+"  asn:\n    cache_ttl: 48h\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Geo.ASN.CacheTTL != 48*time.Hour {
		t.Fatalf("asn.cache_ttl = %v, want 48h", cfg2.Geo.ASN.CacheTTL)
	}

	if _, negErr := loadRaw(t, base+"  asn:\n    cache_ttl: -1s\n"); negErr == nil {
		t.Fatal("negative asn.cache_ttl must be rejected")
	}
}

// TestLoadCacheTTLDisableSemantics proves the pointer-presence semantics for the
// three cache TTLs: an unset value defaults, an explicit 0 is preserved (which
// the resolver / app.go treat as "disable"), and a negative value is rejected.
func TestLoadCacheTTLDisableSemantics(t *testing.T) {
	t.Parallel()

	// Unset -> defaults.
	def, err := writeConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if def.Resolver.CacheTTL == nil || *def.Resolver.CacheTTL != 30*time.Minute {
		t.Fatalf("resolver.cache_ttl default = %v", def.Resolver.CacheTTL)
	}
	if def.Resolver.CacheNegativeTTL == nil || *def.Resolver.CacheNegativeTTL != 10*time.Minute {
		t.Fatalf("resolver.cache_negative_ttl default = %v", def.Resolver.CacheNegativeTTL)
	}
	if def.DeadCache.TTL == nil || *def.DeadCache.TTL != 2*time.Hour {
		t.Fatalf("deadcache.ttl default = %v", def.DeadCache.TTL)
	}

	// Explicit 0 -> preserved (disable), not coerced back to the default.
	dis, err := writeConfig(t, "resolver:\n  cache_ttl: 0s\n  cache_negative_ttl: 0s\ndeadcache:\n  ttl: 0s\n")
	if err != nil {
		t.Fatal(err)
	}
	if dis.Resolver.CacheTTL == nil || *dis.Resolver.CacheTTL != 0 {
		t.Fatalf("explicit resolver.cache_ttl=0 must be preserved, got %v", dis.Resolver.CacheTTL)
	}
	if dis.Resolver.CacheNegativeTTL == nil || *dis.Resolver.CacheNegativeTTL != 0 {
		t.Fatalf("explicit resolver.cache_negative_ttl=0 must be preserved, got %v", dis.Resolver.CacheNegativeTTL)
	}
	if dis.DeadCache.TTL == nil || *dis.DeadCache.TTL != 0 {
		t.Fatalf("explicit deadcache.ttl=0 must be preserved, got %v", dis.DeadCache.TTL)
	}

	// Negative -> rejected, for each field independently.
	for _, block := range []string{
		"resolver:\n  cache_ttl: -1s\n",
		"resolver:\n  cache_negative_ttl: -1s\n",
		"deadcache:\n  ttl: -1h\n",
	} {
		if _, negErr := writeConfig(t, block); negErr == nil {
			t.Fatalf("negative TTL must be rejected: %q", block)
		}
	}
}

// TestLoadRejectsUnknownFilterType: a filters entry outside the known set
// must fail Load and name the offending filter type.
func TestLoadRejectsUnknownFilterType(t *testing.T) {
	t.Parallel()

	const base = geoPreamble
	yaml := base + "filters:\n  - type: bogus\n"
	_, err := loadRaw(t, yaml)
	if err == nil {
		t.Fatal("expected error for unknown filter type")
	}
	if !strings.Contains(err.Error(), "unknown type") || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error %q does not name the bad filter", err)
	}
}

// TestLoadMergesSourcesConfig: a sibling sources.yaml appends its subscription
// sources to the effective config, mirroring the private.yaml overlay merge.
func TestLoadMergesSourcesConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := geoPreamble + "subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n"
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := "subscriptions:\n  sources:\n    - name: b\n      url: https://b.example.com/s\n    - name: c\n      url: https://c.example.com/s\n"
	if err := os.WriteFile(filepath.Join(dir, "sources.yaml"), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Subscriptions.Sources); got != 3 {
		t.Fatalf("sources not merged: got %d, want 3: %+v", got, cfg.Subscriptions.Sources)
	}
	if cfg.Subscriptions.Sources[1].Name != "b" || cfg.Subscriptions.Sources[2].Name != "c" {
		t.Fatalf("sources overlay order wrong: %+v", cfg.Subscriptions.Sources)
	}
}

// TestValidateBodySource covers the inline-source validation branch: a Body
// source needs only a valid name (URL may be empty), a URL source still needs a
// public https URL, and a source with neither is rejected.
func TestValidateBodySource(t *testing.T) {
	t.Parallel()

	base := geoPreamble

	cases := []struct {
		name    string
		sources string
		wantErr bool
	}{
		{
			name:    "body source with empty url accepted",
			sources: "subscriptions:\n  sources:\n    - name: inline\n      body: dmxlc3M6Ly91QDEuMS4xLjE6NDQzI2E=\n",
			wantErr: false,
		},
		{
			name:    "url source with non-https url rejected",
			sources: "subscriptions:\n  sources:\n    - name: bad\n      url: http://insecure.example/s\n",
			wantErr: true,
		},
		{
			name:    "source with neither url nor body rejected",
			sources: "subscriptions:\n  sources:\n    - name: empty\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(base+tc.sources), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load(path)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestValidateInlineSourceShape loads the entry the crawler actually writes for
// its inline-node harvest: a body, no URL, and the mark that makes it prunable,
// which is only legal in private.yaml -- so this shape cannot be reached from
// TestValidateBodySource's curated fixtures.
func TestValidateInlineSourceShape(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := geoPreamble
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	priv := "subscriptions:\n  sources:\n    - name: inline\n      body: dmxlc3M6Ly91QDEuMS4xLjE6NDQzI2E=\n      managed: true\n"
	if err := os.WriteFile(filepath.Join(dir, "private.yaml"), []byte(priv), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the inline entry must load: %v", err)
	}
	src := cfg.Subscriptions.Sources[0]
	if src.Name != "inline" || !src.Managed || src.URL != "" || src.Feed != "" {
		t.Fatalf("inline entry decoded wrong: %+v", src)
	}
}

// TestValidateSourceHWID pins the accepted x-hwid shape. A value the panel
// refuses is worse than an absent one: the fetch still answers 200, carrying
// only the placeholder node, so no counter anywhere reports the loss. The field
// is equally refused on an inline body source, which fetches nothing at all.
func TestValidateSourceHWID(t *testing.T) {
	t.Parallel()

	const (
		urlLine  = "      url: https://a.example.com/s\n"
		bodyLine = "      body: dmxlc3M6Ly91QDEuMS4xLjE6NDQzI2E=\n"
		good     = "abcdef0123456789"
	)

	cases := []struct {
		name     string
		srcName  string
		entry    string
		wantHWID string
		wantErr  bool
	}{
		{name: "absent", srcName: "plain", entry: urlLine},
		{name: "sixteen chars", srcName: "sized-ok", entry: urlLine + "      hwid: " + good + "\n", wantHWID: good},
		{name: "nine chars", srcName: "too-short", entry: urlLine + "      hwid: abcdef012\n", wantErr: true},
		{name: "sixty-five chars", srcName: "too-long", entry: urlLine + "      hwid: " + strings.Repeat("a", 65) + "\n", wantErr: true},
		{name: "underscore", srcName: "bad-rune", entry: urlLine + "      hwid: abcdef_0123456789\n", wantErr: true},
		{name: "space", srcName: "bad-space", entry: urlLine + "      hwid: \"abcdef 0123456789\"\n", wantErr: true},
		{name: "inline body source", srcName: "inline", entry: bodyLine + "      hwid: " + good + "\n", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := writeConfig(t, "subscriptions:\n  sources:\n    - name: "+tc.srcName+"\n"+tc.entry)
			if tc.wantErr {
				assertHWIDRejected(t, err, tc.srcName)
				return
			}
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if got := cfg.Subscriptions.Sources[0].HWID; got != tc.wantHWID {
				t.Fatalf("hwid = %q, want %q", got, tc.wantHWID)
			}
		})
	}
}

func assertHWIDRejected(t *testing.T, err error, srcName string) {
	t.Helper()

	if err == nil {
		t.Fatal("expected the hwid to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), srcName) {
		t.Fatalf("error %q does not name the offending source %q", err, srcName)
	}
	if !strings.Contains(err.Error(), "hwid") {
		t.Fatalf("error %q does not name the offending key", err)
	}
}

// TestLoadRejectsPortlessResolverAddress: resolver.address is handed verbatim to
// net.Dialer for every lookup, so a value missing its port dials nothing and
// every node is dropped as a DNS failure -- with no error naming the key. Load
// must refuse it; a host:port and an omitted address load.
func TestLoadRejectsPortlessResolverAddress(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"1.1.1.1", "dns.example.com", ":53"} {
		_, err := writeConfig(t, "resolver:\n  address: "+addr+"\n")
		if err == nil {
			t.Fatalf("resolver.address %q must be rejected", addr)
		}
		if !strings.Contains(err.Error(), "resolver.address") {
			t.Fatalf("error %q does not name resolver.address", err)
		}
	}

	cfg, err := writeConfig(t, "resolver:\n  address: 1.1.1.1:53\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resolver.Address != "1.1.1.1:53" {
		t.Fatalf("address = %q, want 1.1.1.1:53", cfg.Resolver.Address)
	}
	if _, emptyErr := writeConfig(t, ""); emptyErr != nil {
		t.Fatalf("an omitted resolver.address keeps the system resolver: %v", emptyErr)
	}
}

// TestLoadRejectsUnknownKey: every setting of this service is a yaml key, so a
// typo must fail loudly instead of silently restoring the built-in default. The
// error names the offending key at every nesting depth.
func TestLoadRejectsUnknownKey(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ yaml, key string }{
		"top level":      {"bogus_block:\n  x: 1\n", "bogus_block"},
		"nested block":   {"subscriptions:\n  check:\n    max_avg_mss: 800\n", "max_avg_mss"},
		"list entry":     {"filters:\n  - type: bandwidth\n    min_mpbs: 9\n", "min_mpbs"},
		"resolver block": {"resolver:\n  cache_tt1: 1h\n", "cache_tt1"},
	}
	for name, tc := range cases {
		_, err := writeConfig(t, tc.yaml)
		if err == nil {
			t.Fatalf("%s: a misspelled key must be rejected", name)
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Fatalf("%s: error %q does not name %q", name, err, tc.key)
		}
	}
}

// TestLoadOverlayStrictness: the overlays decode strictly too (a key the overlay
// schema does not carry was silently dropped before, e.g. an interval written
// into sources.yaml where only sources are honoured), but an empty or
// comment-only overlay must still load -- an absent-equivalent overlay is a
// valid "no sources", not a parse failure.
func TestLoadOverlayStrictness(t *testing.T) {
	t.Parallel()

	const base = geoPreamble

	write := func(t *testing.T, sources, private string) (config.Config, error) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sources.yaml"), []byte(sources), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "private.yaml"), []byte(private), 0o644); err != nil {
			t.Fatal(err)
		}
		return config.Load(path)
	}

	cfg, err := write(t, "", "# no sources harvested yet\n")
	if err != nil {
		t.Fatalf("an empty and a comment-only overlay must load: %v", err)
	}
	if cfg.SubscriptionsEnabled() {
		t.Fatal("empty overlays must contribute no sources")
	}

	_, err = write(t, "subscriptions:\n  interval: 5m\n  sources:\n    - name: a\n      url: https://a.example.com/s\n", "")
	if err == nil {
		t.Fatal("a key the overlay schema does not carry must be rejected, not dropped")
	}
	if !strings.Contains(err.Error(), "interval") {
		t.Fatalf("error %q does not name the offending overlay key", err)
	}
}

// TestLoadRejectsManagedInGitTrackedSources: managed is the crawler's write
// authority over an entry -- it makes it prune-eligible -- so a git-tracked file
// may not claim it. Accepting it there would let one cycle delete a hand-added
// source, and the error has to name the file that must be edited. feed is the
// counterexample that keeps the rule about authority and not about crawler
// vocabulary: it only groups, so a curated entry may carry one.
func TestLoadRejectsManagedInGitTrackedSources(t *testing.T) {
	t.Parallel()

	const (
		base    = geoPreamble
		managed = "subscriptions:\n  sources:\n    - name: curated-one\n      url: https://a.example.com/s\n      managed: true\n"
	)

	for _, file := range []string{"config.yaml", "sources.yaml"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		content := base
		if file == "config.yaml" {
			content += managed
		} else if err := os.WriteFile(filepath.Join(dir, file), []byte(managed), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := config.Load(path)
		if err == nil {
			t.Fatalf("%s: managed: true must be rejected in a git-tracked file", file)
		}
		if !strings.Contains(err.Error(), "sources.yaml") || !strings.Contains(err.Error(), "curated-one") {
			t.Fatalf("%s: error %q names neither the curated file nor the source", file, err)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	curated := "subscriptions:\n  sources:\n    - name: curated-one\n      url: https://a.example.com/s\n      feed: somechannel\n"
	if err := os.WriteFile(filepath.Join(dir, "sources.yaml"), []byte(curated), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("a curated entry may name its feed: %v", err)
	}
	if cfg.Subscriptions.Sources[0].Feed != "somechannel" {
		t.Fatalf("curated feed not carried: %+v", cfg.Subscriptions.Sources[0])
	}
}

// TestLoadCarriesOwnershipFromPrivateOverlay: private.yaml is the crawler's own
// file, so managed: true is accepted there and must survive the strict decode
// together with the feed it was minted from. An entry without the key decodes to
// false, which is what shelters a hand-added source from the prune.
func TestLoadCarriesOwnershipFromPrivateOverlay(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := geoPreamble
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	priv := "subscriptions:\n  sources:\n    - name: seyedng-3631\n      url: https://a.example.com/s\n      managed: true\n      feed: seyedng\n" +
		"    - name: hand-added\n      url: https://b.example.com/s\n"
	if err := os.WriteFile(filepath.Join(dir, "private.yaml"), []byte(priv), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("private.yaml may declare managed: %v", err)
	}
	if len(cfg.Subscriptions.Sources) != 2 {
		t.Fatalf("sources not merged: %+v", cfg.Subscriptions.Sources)
	}
	if !cfg.Subscriptions.Sources[0].Managed {
		t.Fatalf("managed: true not carried: %+v", cfg.Subscriptions.Sources[0])
	}
	if cfg.Subscriptions.Sources[1].Managed {
		t.Fatalf("an absent managed key must decode to false: %+v", cfg.Subscriptions.Sources[1])
	}
	if cfg.Subscriptions.Sources[0].Feed != "seyedng" {
		t.Fatalf("feed not carried: %+v", cfg.Subscriptions.Sources[0])
	}
	if cfg.Subscriptions.Sources[1].Feed != "" {
		t.Fatalf("an absent feed key must decode empty: %+v", cfg.Subscriptions.Sources[1])
	}
}

// TestLoadValidatesProberParamsWithoutSources: the prober block must be checked
// on every load, not only when a source list happens to be non-empty. Every
// source here arrives from an overlay, so the old list-gated check let a bad
// value boot clean and then fail EVERY reload from the moment the crawler wrote
// its first source -- blaming private.yaml for a key in config.yaml.
func TestLoadValidatesProberParamsWithoutSources(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ yaml, want string }{
		"short interval":     {"subscriptions:\n  interval: 10s\n", "subscriptions.interval must be at least"},
		"max_fail >= rounds": {"subscriptions:\n  check:\n    rounds: 2\n    max_fail: 2\n", "max_fail"},
		"max_avg_ms below 1": {"subscriptions:\n  check:\n    max_avg_ms: -1\n", "max_avg_ms"},
		"bad status range":   {"subscriptions:\n  check:\n    expected_status: not-a-range\n", "expected_status"},
	}
	for name, tc := range cases {
		_, err := writeConfig(t, tc.yaml)
		if err == nil {
			t.Fatalf("%s: must be rejected with no sources configured", name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q does not contain %q", name, err, tc.want)
		}
	}
}

// TestLoadBlamesTheFileThatOwnsTheKey: a bad prober param in config.yaml must be
// reported as its own error even when a private.yaml overlay supplies the
// sources. Previously the check only ran after the overlay merge, so the message
// pointed at private.yaml, which was innocent.
func TestLoadBlamesTheFileThatOwnsTheKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := geoPreamble +
		"subscriptions:\n  interval: 10s\n"
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	priv := "subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n"
	if err := os.WriteFile(filepath.Join(dir, "private.yaml"), []byte(priv), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("a sub-minimum subscriptions.interval must fail the load")
	}
	if strings.Contains(err.Error(), "private config") {
		t.Fatalf("error %q blames private.yaml for a config.yaml key", err)
	}
}
