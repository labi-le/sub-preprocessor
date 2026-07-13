package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n      type: gzip\n")
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
	if len(cfg.Geofeed.Sources) != 1 {
		t.Fatalf("unexpected sources count: %d", len(cfg.Geofeed.Sources))
	}
	if cfg.Geofeed.Sources[0].Type != "gzip" {
		t.Fatalf("unexpected source type: %q", cfg.Geofeed.Sources[0].Type)
	}
	if cfg.Geofeed.RefreshInterval != 0 {
		t.Fatalf("unexpected refresh interval default: %v", cfg.Geofeed.RefreshInterval)
	}
	if len(cfg.Workflow.Stages) != 2 || cfg.Workflow.Stages[0] != "geofeed" || cfg.Workflow.Stages[1] != "asn" {
		t.Fatalf("unexpected default workflow stages: %v", cfg.Workflow.Stages)
	}
}

func TestLoadRejectsMissingGeofeedType(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n")
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
	content := []byte("geofeed:\n  refresh_interval: 24h\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n      type: gzip\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geofeed.RefreshInterval != 24*time.Hour {
		t.Fatalf("unexpected refresh interval: %v", cfg.Geofeed.RefreshInterval)
	}
}

func TestLoadGroups(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n      type: gzip\ngroups:\n  nordics:\n    - FI\n    - SE\n    - NO\n    - DK\n  baltics:\n    - EE\n    - LV\n    - LT\n")
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
	content := []byte("geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n      type: gzip\ngroups:\n  invalid:\n    - XYZ\n")
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
	content := []byte("geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n      type: gzip\ngroups:\n  \"\":\n    - FI\n")
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
	cfgA := config.Config{Geofeed: config.GeofeedConfig{Sources: []geofeed.Source{src}}}
	cfgB := cfgA
	if config.GeofeedSourcesChanged(cfgA, cfgB) {
		t.Fatal("identical sources should not be changed")
	}
	cfgB.Geofeed.Sources = append(cfgB.Geofeed.Sources, geofeed.Source{URL: "https://other.com/feed.csv", Type: "gzip"})
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
	base := "geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n      type: gzip\ngroups:\n  geo_blocked: [RU, IR]\n"
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
	if cfg.Resolver.CacheTTL != 30*time.Minute {
		t.Fatalf("cache_ttl: %v", cfg.Resolver.CacheTTL)
	}
	if cfg.Resolver.CacheNegativeTTL != 10*time.Minute {
		t.Fatalf("cache_negative_ttl: %v", cfg.Resolver.CacheNegativeTTL)
	}
}

func TestLoadResolverCacheExplicit(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "resolver:\n  cache_ttl: 1h\n  cache_negative_ttl: 5m\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resolver.CacheTTL != time.Hour {
		t.Fatalf("cache_ttl: %v", cfg.Resolver.CacheTTL)
	}
	if cfg.Resolver.CacheNegativeTTL != 5*time.Minute {
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
  exclude_countries: [CN]
  exclude_groups: [geo_blocked]
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
	if s.Interval != 15*time.Minute || len(s.ExcludeCountries) != 1 || len(s.ExcludeGroups) != 1 {
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
		"unknown group":   "subscriptions:\n  exclude_groups: [nope]\n  sources:\n    - name: a\n      url: https://a.example.com/s\n",
		"bad country":     "subscriptions:\n  exclude_countries: [RUS]\n  sources:\n    - name: a\n      url: https://a.example.com/s\n",
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
	c.ASN.DenyPatterns = []string{"changed"}
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
