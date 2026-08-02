package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
			{Name: "geotrace", In: 165, Kept: 165, Dropped: map[string]int{}, Notes: map[string]int{"corrected": 47, "unanswered": 8}},
		},
		KeptSpeeds:    []int{3, 7, 30, 120},
		GeoUnknown:    3,
		KeptCountries: map[string]int{"NL": 40, "FI": 12},
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
		"# TYPE stable_filter_notes gauge",
		`stable_filter_notes{filter="geotrace",note="corrected"} 47`,
		`stable_filter_notes{filter="geotrace",note="unanswered"} 8`,
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
		`stable_kept_country_nodes{country="FI"} 12`,
		`stable_kept_country_nodes{country="NL"} 40`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	// The notes are a separate series precisely so the drops chart stays a
	// drops chart: a geotrace counter leaking into stable_filter_dropped_nodes
	// would read as 47 published nodes thrown away.
	for _, note := range []string{"corrected", "unanswered"} {
		if strings.Contains(out, `stable_filter_dropped_nodes{filter="geotrace",reason="`+note+`"}`) {
			t.Errorf("note %q must not render as a drop reason:\n%s", note, out)
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

// TestMetricsLabelValuesStayParseable pins RUNTIME-7. A label value can reach
// the exposition from outside this service: with annotate disabled the pipeline
// republishes upstream node names verbatim and stable derives the country label
// from a [GEO:xx] tag inside such a name. Prometheus rejects a label value that
// is not valid UTF-8 and fails on any escape sequence other than \\, \" and \n,
// and either failure takes the WHOLE scrape down, not just the offending line.
func TestMetricsLabelValuesStayParseable(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.Observe(stable.CycleReport{
		Kept: 1,
		KeptCountries: map[string]int{
			"\xff\xfe":  1, // invalid UTF-8: must be replaced, not escaped
			"a\rb":      2, // carriage return: legal raw, and \r is NOT a legal escape
			"q\"z":      3, // reserved: double quote
			"back\\sla": 4, // reserved: backslash
			"two\nline": 5, // reserved: line feed
		},
	})

	out := render(t, m)
	if !utf8.ValidString(out) {
		t.Fatal("exposition is not valid UTF-8")
	}
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(line, "stable_kept_country_nodes{") {
			continue
		}
		// Exactly two unescaped double quotes delimit the single label value,
		// and every backslash starts an escape the parser knows.
		if got := unescapedQuotes(line); got != 2 {
			t.Errorf("line %q has %d unescaped quotes, want 2", line, got)
		}
		if bad, ok := badEscape(line); !ok {
			t.Errorf("line %q carries escape sequence %q, which Prometheus rejects", line, bad)
		}
	}
	for _, want := range []string{
		`stable_kept_country_nodes{country="invalid_utf8"} 1`,
		`stable_kept_country_nodes{country="q\"z"} 3`,
		`stable_kept_country_nodes{country="back\\sla"} 4`,
		`stable_kept_country_nodes{country="two\nline"} 5`,
		// A carriage return is left verbatim on purpose: the parser copies it
		// through, while \r would be an escape sequence it rejects.
		"stable_kept_country_nodes{country=\"a\rb\"} 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A line feed must never reach the exposition raw: it would end the line.
	if strings.Contains(out, "two\nline") {
		t.Errorf("unescaped newline in label value:\n%s", out)
	}
}

// unescapedQuotes counts the double quotes that are not preceded by an escaping
// backslash — the delimiters a text-format parser splits a label value on.
func unescapedQuotes(s string) int {
	n, escaped := 0, false
	for i := range len(s) {
		switch {
		case escaped:
			escaped = false
		case s[i] == '\\':
			escaped = true
		case s[i] == '"':
			n++
		}
	}
	return n
}

// badEscape reports the first escape sequence Prometheus's text parser would
// reject (it accepts only \\, \" and \n), or ok=true when there is none.
func badEscape(s string) (seq string, ok bool) {
	for i := 0; i < len(s)-1; i++ {
		if s[i] != '\\' {
			continue
		}
		switch s[i+1] {
		case '\\', '"', 'n':
			i++ // consume the escaped byte so `\\` does not read as two escapes
		default:
			return s[i : i+2], false
		}
	}
	return "", true
}
