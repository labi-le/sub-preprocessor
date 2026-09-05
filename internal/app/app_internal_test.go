package app

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/metrics"
	"domains.lst/sub-preprocessor/internal/stable"
)

const metricsProbeTimeout = 2 * time.Second

// TestStartMetricsReportsBindFailure pins the contract that a metrics bind
// failure reaches the caller. It used to be swallowed by a detached goroutine
// while startup logged "metrics listener started" anyway, leaving the service
// running with no observability.
func TestStartMetricsReportsBindFailure(t *testing.T) {
	t.Parallel()

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer func() { _ = held.Close() }()

	srv, err := startMetrics(t.Context(), held.Addr().String(), metrics.New(), zerolog.Nop())
	if err == nil {
		_ = srv.Close()
		t.Fatal("binding an occupied port must be reported as an error")
	}
	if srv != nil {
		t.Fatal("no server may be handed back when the bind failed")
	}
}

// TestStartMetricsServesAfterBind covers the happy path end to end: the
// listener is bound before the function returns, so the resolved address is
// scrapeable immediately.
func TestStartMetricsServesAfterBind(t *testing.T) {
	t.Parallel()

	srv, err := startMetrics(t.Context(), "127.0.0.1:0", metrics.New(), zerolog.Nop())
	if err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	defer func() { _ = srv.Close() }()

	client := &http.Client{Timeout: metricsProbeTimeout}
	resp, err := client.Get("http://" + srv.Addr + "/metrics")
	if err != nil {
		t.Fatalf("scrape %s: %v", srv.Addr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read scrape: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "stable_cycles_total") {
		t.Fatalf("scrape did not render the metric set: %q", body)
	}
}

// TestRestoreStableList pins the startup half of snapshot persistence: with a
// readable file and a source list to power a worker, the holder is already
// published before the server starts, and without a usable file it stays empty
// so /stable.txt answers 503 exactly as it did before the feature existed.
func TestRestoreStableList(t *testing.T) {
	t.Parallel()

	saved := &stable.Snapshot{
		Payload:   []byte("vless://u@1.1.1.1:443#alpha-001\n"),
		UpdatedAt: time.Date(2026, 8, 7, 13, 53, 57, 0, time.UTC),
		Stats:     stable.Stats{SourcesOK: 2, SourcesTotal: 3, Merged: 68266, Tested: 400, Kept: 1},
	}
	path := filepath.Join(t.TempDir(), "stable.json")
	if err := stable.SaveSnapshot(path, saved); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	restored := restoreStableList(configWithSnapshotPath(path), zerolog.Nop()).Load()

	if restored == nil {
		t.Fatal("the persisted list was not published; /stable.txt would answer 503 for a whole cycle after a restart")
	}
	if string(restored.Payload) != string(saved.Payload) {
		t.Errorf("payload:\ngot  %q\nwant %q", restored.Payload, saved.Payload)
	}
	if restored.Stats != saved.Stats {
		t.Errorf("stats: got %+v want %+v", restored.Stats, saved.Stats)
	}
	// The restart must not reset the age the X-Stable-Stats header reports.
	if !restored.UpdatedAt.Equal(saved.UpdatedAt) {
		t.Errorf("updated_at: got %v want %v", restored.UpdatedAt, saved.UpdatedAt)
	}

	for name, cfg := range map[string]config.Config{
		"persistence off": configWithSnapshotPath(""),
		"no file yet":     configWithSnapshotPath(filepath.Join(t.TempDir(), "absent.json")),
		"malformed file":  configWithSnapshotPath(writeFile(t, "{\"payload\":\"vless")),
	} {
		if h := restoreStableList(cfg, zerolog.Nop()); h.Load() != nil {
			t.Errorf("%s: the holder must stay empty so /stable.txt answers 503", name)
		}
	}
}

// TestRestoreStableListSkipsSnapshotWhenDisabled pins the ordering behind the
// deliberate disable: Apply starts no worker for an explicitly empty source
// list, so the restore that precedes it must not seed the holder either. A
// previous run's snapshot would then answer 200 on /stable.txt for the whole
// process lifetime with no worker to ever replace it, silently bypassing the
// 503-on-cold-start an empty holder answers.
func TestRestoreStableListSkipsSnapshotWhenDisabled(t *testing.T) {
	t.Parallel()

	saved := &stable.Snapshot{
		Payload:   []byte("vless://u@192.0.2.1:443#foo\n"),
		UpdatedAt: time.Date(2026, 8, 7, 13, 53, 57, 0, time.UTC),
	}
	path := filepath.Join(t.TempDir(), "stable.json")
	if err := stable.SaveSnapshot(path, saved); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	var cfg config.Config
	cfg.Subscriptions.SnapshotPath = path
	if cfg.SubscriptionsEnabled() {
		t.Fatal("fixture must model an empty merged source list")
	}

	if h := restoreStableList(cfg, zerolog.Nop()); h.Load() != nil {
		t.Fatal("a disabled worker must not serve the previous run's snapshot; /stable.txt must answer 503")
	}
}

// configWithSnapshotPath builds a config whose worker will actually run (one
// source) alongside the given snapshot path, so restoreStableList reaches the
// snapshot file rather than stopping at the enable gate.
func configWithSnapshotPath(path string) config.Config {
	var cfg config.Config
	cfg.Subscriptions.SnapshotPath = path
	cfg.Subscriptions.Sources = []config.SubscriptionSource{{Name: "seed", URL: "https://example.com/stable"}}

	return cfg
}

func writeFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "stable.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}
