package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
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
// specs (country/asn) in config order, dropping the through-node types.
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
		{Type: config.FilterCountry, Provider: config.ProviderGeofeed, ExcludeGroups: []string{"geo_blocked"}, ExcludeCountries: []string{"CN"}},
		{Type: config.FilterASN, Provider: config.ProviderASN, DenyPatterns: []string{"spammy"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IPFilterSpecs()=%+v, want %+v", got, want)
	}
}

// TestNodeFilterSpecsSplit proves the through-node types (gemini/claude/
// bandwidth) split out in order, with gemini/claude merged over the geoblock
// defaults and bandwidth carrying its entry params.
func TestNodeFilterSpecsSplit(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		GeoBlock: config.GeoBlockConfig{
			Gemini: config.GeminiConfig{Endpoint: "https://gemini.base", Marker: "base-marker", Model: "base-model", Timeout: 15 * time.Second, Concurrency: 8},
			Claude: config.ClaudeConfig{Endpoint: "https://claude.base", Marker: "base-claude", Version: "2023-06-01", Timeout: 15 * time.Second, Concurrency: 8},
		},
		Filters: []config.FilterConfig{
			{Type: config.FilterCountry, Provider: config.ProviderGeofeed},
			{Type: config.FilterClaude, Marker: "override-claude"},
			{Type: config.FilterBandwidth, MinMbps: new(9), TestURL: "https://speed/x", Timeout: 30 * time.Second, Concurrency: 2},
			{Type: config.FilterGemini, Model: "override-model"},
		},
	}

	got := cfg.NodeFilterSpecs()
	if len(got) != 3 {
		t.Fatalf("NodeFilterSpecs() len=%d, want 3", len(got))
	}
	if got[0].Type != config.FilterClaude || got[1].Type != config.FilterBandwidth || got[2].Type != config.FilterGemini {
		t.Fatalf("order = %s,%s,%s", got[0].Type, got[1].Type, got[2].Type)
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

// TestLoadAnnotateDefaultsAndValidation covers annotate provider defaulting and
// tag/provider validation.
func TestLoadAnnotateDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	cfg, err := loadYAML(t, geoBase+"annotate:\n  - tag: GEO\n  - tag: IP\n  - tag: ASN\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Annotate) != 3 {
		t.Fatalf("annotate len=%d, want 3", len(cfg.Annotate))
	}
	if cfg.Annotate[0].Provider != config.ProviderGeofeed {
		t.Fatalf("GEO provider default = %q, want geofeed", cfg.Annotate[0].Provider)
	}
	if cfg.Annotate[2].Provider != config.ProviderASN {
		t.Fatalf("ASN provider default = %q, want asn", cfg.Annotate[2].Provider)
	}

	rejects := map[string]struct {
		yaml    string
		wantErr string
	}{
		"unknown tag":      {geoBase + "annotate:\n  - tag: SPD\n", "unknown tag"},
		"geo bad provider": {geoBase + "annotate:\n  - tag: GEO\n    provider: bogus\n", "provider must be"},
	}
	for name, tc := range rejects {
		if _, rejErr := loadYAML(t, tc.yaml); rejErr == nil {
			t.Fatalf("%s: expected error", name)
		} else if !strings.Contains(rejErr.Error(), tc.wantErr) {
			t.Fatalf("%s: error %q does not contain %q", name, rejErr, tc.wantErr)
		}
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
