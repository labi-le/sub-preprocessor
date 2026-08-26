package metrics_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/metrics"
	"domains.lst/sub-preprocessor/internal/stable"
)

const latencyBucketPrefix = `stable_kept_latency_ms_bucket{le="`

// exposedLatencyBounds reads the ladder back out of a scrape rather than off
// the package variable: the exposition is the form a gate has to be visible in
// for the panel to answer "how close is the list running to max_avg_ms".
func exposedLatencyBounds(t *testing.T) []float64 {
	t.Helper()

	m := metrics.New()
	m.Observe(stable.CycleReport{})

	var bounds []float64
	for line := range strings.SplitSeq(render(t, m), "\n") {
		rest, isBucket := strings.CutPrefix(line, latencyBucketPrefix)
		if !isBucket {
			continue
		}
		le, _, _ := strings.Cut(rest, `"`)
		if le == "+Inf" {
			continue
		}
		v, err := strconv.ParseFloat(le, 64)
		if err != nil {
			t.Fatalf("unparseable bucket bound %q: %v", le, err)
		}
		bounds = append(bounds, v)
	}
	// Without this a renamed metric would pass the invariant vacuously.
	if len(bounds) == 0 {
		t.Fatalf("no %s… buckets in the exposition", latencyBucketPrefix)
	}
	return bounds
}

// shippedConfigDirs are the config directories the repo ships. The invariant is
// about what ships, so the test reads those files, not fixtures.
var shippedConfigDirs = []string{"config"}

// TestLatencyBucketsCoverShippedGates enforces the latencyBuckets invariant
// against the configs themselves, so moving a max_avg_ms off the ladder fails
// here instead of leaving that gate invisible between two bounds.
func TestLatencyBucketsCoverShippedGates(t *testing.T) {
	t.Parallel()

	for _, dir := range shippedConfigDirs {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join("..", "..", dir, "config.yaml")
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			gate := float64(cfg.Subscriptions.Check.MaxAvgMs)
			if bounds := exposedLatencyBounds(t); !slices.Contains(bounds, gate) {
				t.Errorf("%s: subscriptions.check.max_avg_ms = %v has no equal bound in the ladder %v",
					path, gate, bounds)
			}
		})
	}
}

// TestLatencyBucketsCoverTheDefaultGate covers the gate no shipped config
// exercises. The shipped config sets max_avg_ms explicitly, so Load never
// applies defaultCheckMaxAvgMs on that path and the bound justified by it is
// the one bound deletable with the suite green — exactly the invisible gate
// this metric exists to prevent, waiting for the first config that omits the
// key.
func TestLatencyBucketsCoverTheDefaultGate(t *testing.T) {
	t.Parallel()
	// A shipped config with exactly one line removed, so the fixture stays
	// valid under every other validator and differs only in the key at issue.
	src, err := os.ReadFile(filepath.Join("..", "..", "config", "config.yaml"))
	if err != nil {
		t.Fatalf("read shipped config: %v", err)
	}
	var kept []string
	for line := range strings.SplitSeq(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "max_avg_ms:") {
			continue
		}
		kept = append(kept, line)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if writeErr := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600); writeErr != nil {
		t.Fatalf("write fixture: %v", writeErr)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	// Guard the STRIP, not the loaded value: Load can never return 0 (normalize
	// substitutes the default, validate rejects below 1), so a zero check is dead
	// code. The reachable vacuity is a strip that removes nothing — rewrite the
	// key as `"max_avg_ms": 800` and the fixture silently pins the shipped gate.
	if len(kept) == len(strings.Split(string(src), "\n")) {
		t.Fatal("fixture strip removed no line; the test would pin the shipped gate, not the default")
	}
	gate := float64(cfg.Subscriptions.Check.MaxAvgMs)
	if bounds := exposedLatencyBounds(t); !slices.Contains(bounds, gate) {
		t.Errorf("defaulted max_avg_ms = %v has no equal bound in the ladder %v", gate, bounds)
	}
}

// TestLatencyBucketsAreStrictlyIncreasing guards the hazard the invariant
// invites: writeHistogram emits one line per element with no dedupe, so
// appending a bound the ladder already carries renders two identical le=
// series and Prometheus rejects the whole scrape as a duplicate sample.
func TestLatencyBucketsAreStrictlyIncreasing(t *testing.T) {
	t.Parallel()

	bounds := exposedLatencyBounds(t)
	for i := 1; i < len(bounds); i++ {
		if bounds[i] <= bounds[i-1] {
			t.Errorf("bound %d (%v) does not exceed its predecessor (%v): %v",
				i, bounds[i], bounds[i-1], bounds)
		}
	}
}
