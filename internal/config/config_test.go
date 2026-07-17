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

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n")
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
	if cfg.Geo.Geofeed.RefreshInterval != 0 {
		t.Fatalf("unexpected refresh interval default: %v", cfg.Geo.Geofeed.RefreshInterval)
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

func TestLoadGeofeedRefreshInterval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geo:\n  geofeed:\n    refresh_interval: 24h\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geo.Geofeed.RefreshInterval != 24*time.Hour {
		t.Fatalf("unexpected refresh interval: %v", cfg.Geo.Geofeed.RefreshInterval)
	}
}

func TestLoadGroups(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\ngroups:\n  nordics:\n    - FI\n    - SE\n    - NO\n    - DK\n  baltics:\n    - EE\n    - LV\n    - LT\n")
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
	content := []byte("geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\ngroups:\n  invalid:\n    - XYZ\n")
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
	content := []byte("geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\ngroups:\n  \"\":\n    - FI\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for empty group name")
	}
}

func TestEqual(t *testing.T) {
	cfgA := config.Config{Server: struct {
		Listen string `yaml:"listen"`
	}{Listen: ":8080"}}
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

func TestListenChanged(t *testing.T) {
	cfgA := config.Config{Server: struct {
		Listen string `yaml:"listen"`
	}{Listen: ":8080"}}
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
	base := "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\ngroups:\n  geo_blocked: [RU, IR]\n"
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

	const base = "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"
	const subs = "subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n"
	cases := map[string]struct {
		yaml    string
		wantErr string
	}{
		"negative gemini concurrency":   {base + "geoblock:\n  gemini:\n    concurrency: -1\n", "geoblock.gemini.concurrency"},
		"negative claude concurrency":   {base + "geoblock:\n  claude:\n    concurrency: -1\n", "geoblock.claude.concurrency"},
		"negative gemini timeout":       {base + "geoblock:\n  gemini:\n    timeout: -1s\n", "geoblock.gemini.timeout"},
		"negative claude timeout":       {base + "geoblock:\n  claude:\n    timeout: -1s\n", "geoblock.claude.timeout"},
		"negative geoblock ttl":         {base + "geoblock:\n  ttl: -1h\n", "geoblock.ttl"},
		"negative resolver timeout":     {base + "resolver:\n  timeout: -1s\n", "resolver.timeout"},
		"negative asn timeout":          {"geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n  asn:\n    timeout: -1s\n", "geo.asn.timeout"},
		"negative fetch timeout":        {base + "fetch:\n  timeout: -1s\n", "fetch.timeout"},
		"negative deadcache ttl":        {base + "deadcache:\n  ttl: -1h\n", "deadcache.ttl"},
		"negative geofeed refresh":      {"geo:\n  geofeed:\n    refresh_interval: -1m\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n", "geo.geofeed.refresh_interval"},
		"negative subs interval":        {base + "subscriptions:\n  interval: -1m\n", "subscriptions.interval"},
		"negative check timeout":        {base + "subscriptions:\n  check:\n    timeout: -1s\n", "subscriptions.check.timeout"},
		"negative check source timeout": {base + "subscriptions:\n  check:\n    source_timeout: -1s\n", "subscriptions.check.source_timeout"},
		"unknown filter type":           {base + "filters:\n  - type: bogus\n", `unknown type "bogus"`},
		"bad log level":                 {base + "log:\n  level: verbose\n", "log.level"},
		"bad expected status":           {base + subs + "  check:\n    expected_status: not-a-range\n", "expected_status"},
		"non-http test url":             {base + subs + "  check:\n    test_url: ftp://example.com/generate_204\n", "test_url"},
		"hostless test url":             {base + subs + "  check:\n    test_url: ./relative\n", "test_url"},
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
		"geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"+
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

func TestLoadMergesPrivateConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\nsubscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n"
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
	base := "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"
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
	const base = "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"
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

	const base = "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"
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

	const base = "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"
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
	base := "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\nsubscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n"
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

	base := "geo:\n  geofeed:\n    sources:\n      - url: https://example.com/geofeed.csv.gz\n        type: gzip\n"

	cases := []struct {
		name    string
		sources string
		wantErr bool
	}{
		{
			name:    "body source with empty url accepted",
			sources: "subscriptions:\n  sources:\n    - name: tg-inline\n      body: dmxlc3M6Ly91QDEuMS4xLjE6NDQzI2E=\n",
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
