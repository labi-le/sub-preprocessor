package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/metrics"
	"domains.lst/sub-preprocessor/internal/stable"
)

func render(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestMetricsObserveRender(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.Observe(stable.CycleReport{
		SourcesOK: 130, SourcesTotal: 145,
		Merged: 21000, DeadSkipped: 20000, Probed: 966, Kept: 165,
		Duration: 90 * time.Second,
		Sources: []stable.SourceReport{
			{Name: "mifa", Total: 100, Kept: 20, DNSDrop: 5, GeoDrop: 71, GeoBlockDrop: 4},
		},
		Filters: []stable.FilterReport{
			{Name: "claude", In: 474, Kept: 387, Dropped: map[string]int{"blocked": 7, "unreachable": 80}},
			{Name: "bandwidth", In: 387, Kept: 165, Dropped: map[string]int{"slow": 49, "unreachable": 173}},
		},
		KeptSpeeds: []int{3, 7, 30, 120},
		GeoUnknown: 3,
	})

	out := render(t, m)
	// Labels render in sorted key order: filter<reason, but reason<source.
	wants := []string{
		"# TYPE stable_kept_nodes gauge",
		"stable_kept_nodes 165",
		"stable_merged_nodes 21000",
		"stable_sources_ok 130",
		"stable_cycles_total 1",
		"stable_cycle_failures_total 0",
		`stable_filter_kept_nodes{filter="bandwidth"} 165`,
		`stable_filter_dropped_nodes{filter="bandwidth",reason="slow"} 49`,
		`stable_filter_dropped_nodes{filter="claude",reason="blocked"} 7`,
		`stable_source_kept_nodes{source="mifa"} 20`,
		`stable_source_dropped_nodes{reason="geo",source="mifa"} 71`,
		`stable_kept_speed_mbps_bucket{le="5"} 1`,
		`stable_kept_speed_mbps_bucket{le="10"} 2`,
		`stable_kept_speed_mbps_bucket{le="+Inf"} 4`,
		"stable_kept_speed_mbps_count 4",
		"stable_kept_speed_mbps_sum 160",
		"stable_geo_unknown_nodes 3",
		"stable_kept_speed_min_mbps 3",
		"stable_kept_speed_max_mbps 120",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func TestMetricsObserveError(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.ObserveError()
	m.ObserveError()

	out := render(t, m)
	if !strings.Contains(out, "stable_cycle_failures_total 2") {
		t.Errorf("failures not counted:\n%s", out)
	}
	if !strings.Contains(out, "stable_cycles_total 2") {
		t.Errorf("total must include failures:\n%s", out)
	}
}

func TestMetricsEmptyRender(t *testing.T) {
	t.Parallel()

	out := render(t, metrics.New())
	if !strings.Contains(out, "stable_cycles_total 0") {
		t.Errorf("counters must render before the first cycle:\n%s", out)
	}
	if strings.Contains(out, "\nstable_kept_nodes ") {
		t.Errorf("no cycle gauges must render before the first Observe:\n%s", out)
	}
}
