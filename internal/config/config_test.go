package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n      type: gzip\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
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
}

func TestLoadRejectsMissingGeofeedType(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
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

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Geofeed.RefreshInterval != 24*time.Hour {
		t.Fatalf("unexpected refresh interval: %v", cfg.Geofeed.RefreshInterval)
	}
}
