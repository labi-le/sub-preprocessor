package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeSourcesOverlayRefusesTheMarkItLoads pins the curated-ownership rule to
// the file rather than to Load's statement order. The list handed in already
// holds a legitimate managed entry -- the shape a git-tracked overlay merged
// AFTER private.yaml would see, where the merged-list pass runs with the mark
// allowed and would let the overlay's own mark through. The refusal must come
// from the merge itself, must name the overlay and the offending entry, must not
// blame the entry that was already there, and must land before the append.
func TestMergeSourcesOverlayRefusesTheMarkItLoads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	overlay := "subscriptions:\n  sources:\n    - name: curated-one\n      url: https://a.example.com/s\n      managed: true\n"
	if err := os.WriteFile(filepath.Join(dir, sourcesOverlayFile), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	var cfg Config
	cfg.Subscriptions.Sources = []SubscriptionSource{
		{Name: "seyedng-3631", URL: "https://b.example.com/s", Managed: true, Feed: "seyedng"},
	}

	err := mergeSourcesOverlay(dir, &cfg)
	if err == nil {
		t.Fatal("the overlay's own mark must be refused wherever the merge happens")
	}
	if !strings.Contains(err.Error(), sourcesOverlayFile) || !strings.Contains(err.Error(), "curated-one") {
		t.Fatalf("error %q names neither the overlay nor the offending entry", err)
	}
	if strings.Contains(err.Error(), "seyedng-3631") {
		t.Fatalf("error %q blames an entry the overlay did not contribute", err)
	}
	if len(cfg.Subscriptions.Sources) != 1 {
		t.Fatalf("a refused overlay must contribute nothing: %+v", cfg.Subscriptions.Sources)
	}
}
