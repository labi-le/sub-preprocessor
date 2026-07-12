package reload_test

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
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
const baseGeofeedYAML = "geofeed:\n" +
	"  sources:\n" +
	"    - url: https://127.0.0.1:1/geofeed\n" +
	"      type: raw\n"

// stubLookup is a no-op geofeed.CountryLookup used to preload a Processor so it
// never fetches geofeed data during tests.
type stubLookup struct{}

func (stubLookup) LookupCountry(_ netip.Addr) geofeed.CountryCode {
	return geofeed.CountryCode{'N', 'L'}
}

// failingFilterer satisfies stable.Filterer and always errors, guaranteeing the
// stable worker never performs network fetches from reload tests.
type failingFilterer struct{}

func (failingFilterer) Filter(
	_ context.Context,
	_ *bytes.Buffer,
	_ preprocess.FilterRequest,
) (preprocess.Stats, error) {
	return preprocess.Stats{}, errors.New("stub filterer: no network in tests")
}

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
	ctl *stable.Controller,
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
	opts.PreloadedGeofeed = stubLookup{}
	opts.PreloadedLoadedAt = loadedAt
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
	cfg.Geofeed.Sources = []geofeed.Source{{URL: "https://example.com/feed.csv", Type: "raw"}}
	cfg.Geofeed.RefreshInterval = 7 * time.Minute
	cfg.Resolver.Address = "9.9.9.9:53"
	cfg.Resolver.Timeout = 3 * time.Second
	cfg.ASN.Timeout = 4 * time.Second
	cfg.ASN.DenyPatterns = []string{"^AS1234 ", "spammy"}
	cfg.Workflow.Stages = []string{"geofeed", "asn"}

	opts := reload.OptionsFromConfig(cfg)

	if !slices.Equal(opts.GeofeedSources, cfg.Geofeed.Sources) {
		t.Errorf("GeofeedSources: got %v want %v", opts.GeofeedSources, cfg.Geofeed.Sources)
	}
	if opts.RefreshInterval != cfg.Geofeed.RefreshInterval {
		t.Errorf("RefreshInterval: got %v want %v", opts.RefreshInterval, cfg.Geofeed.RefreshInterval)
	}
	if opts.DNSTimeout != cfg.Resolver.Timeout {
		t.Errorf("DNSTimeout: got %v want %v", opts.DNSTimeout, cfg.Resolver.Timeout)
	}
	if opts.DNSAddress != cfg.Resolver.Address {
		t.Errorf("DNSAddress: got %q want %q", opts.DNSAddress, cfg.Resolver.Address)
	}
	if opts.ASNTimeout != cfg.ASN.Timeout {
		t.Errorf("ASNTimeout: got %v want %v", opts.ASNTimeout, cfg.ASN.Timeout)
	}
	if !slices.Equal(opts.ASNDenyPatterns, cfg.ASN.DenyPatterns) {
		t.Errorf("ASNDenyPatterns: got %v want %v", opts.ASNDenyPatterns, cfg.ASN.DenyPatterns)
	}
	if !slices.Equal(opts.WorkflowStages, cfg.Workflow.Stages) {
		t.Errorf("WorkflowStages: got %v want %v", opts.WorkflowStages, cfg.Workflow.Stages)
	}
	if opts.PreloadedGeofeed != nil {
		t.Error("OptionsFromConfig must not set PreloadedGeofeed")
	}
	if !opts.PreloadedLoadedAt.IsZero() {
		t.Error("OptionsFromConfig must not set PreloadedLoadedAt")
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
		{"invalid asn regex (AC9)", baseGeofeedYAML + "asn:\n  deny_patterns:\n    - \"(\"\n"},
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
	writeConfig(t, path, baseGeofeedYAML+"asn:\n  deny_patterns:\n    - \"^AS1234 \"\n")
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
	lookup, at := newProc.GeofeedState()
	if lookup == nil {
		t.Fatal("geofeed lookup must be carried over (non-nil)")
	}
	if !at.Equal(loadedAt) {
		t.Fatalf("AC8: LoadedAt must be carried over: got %v want %v", at, loadedAt)
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
	_, at := newProc.GeofeedState()
	if !at.Equal(loadedAt) {
		t.Fatalf("AC12: first reload must carry LoadedAt: got %v want %v", at, loadedAt)
	}
	if len(after.Groups["nordics"]) != 2 {
		t.Fatalf("AC12: new groups must be applied in the swapped snapshot, got %v", after.Groups)
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
		func() stable.Filterer { return failingFilterer{} }, nil, nil, zerolog.Nop())
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
		func() stable.Filterer { return failingFilterer{} }, nil, nil, zerolog.Nop())
	t.Cleanup(ctl.Stop)

	loadedAt := time.Now().Add(-time.Hour)
	r, holder, path := setupReloader(t, logger, loadedAt, ctl)

	before := holder.Load()
	logBuf.Reset()

	writeConfig(t, path, baseGeofeedYAML+"asn:\n  deny_patterns:\n    - \"^AS9999 \"\n")
	r.Reload(t.Context())

	if holder.Load() == before {
		t.Fatal("valid asn edit must still swap the snapshot")
	}
	if logs := logBuf.String(); strings.Contains(logs, "subscriptions config applied") {
		t.Fatalf("unrelated change must not re-apply subscriptions, got:\n%s", logs)
	}
}
