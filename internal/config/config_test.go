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
