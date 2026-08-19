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
			{Name: "mifa", Total: 133, Valid: 20, Tested: 14, Filtered: 9, DNSDrop: 5, GeoDrop: 71, CIDRDrop: 33, GeoBlockDrop: 4},
		},
		Filters: []stable.FilterReport{
			{Name: "claude", In: 474, Kept: 387, Dropped: map[string]int{"blocked": 7, "unreachable": 80}},
			{Name: "bandwidth", In: 387, Kept: 165, Dropped: map[string]int{"slow": 49, "unreachable": 173}},
		},
		Trace:         stable.TraceReport{Answered: 157, Unanswered: 8, Moved: 47},
		Gemini:        stable.GeminiReport{State: stable.GeminiGateRan, Checks: 306, Unverified: 22},
		Precheck:      stable.PrecheckReport{State: stable.PrecheckRan, Dialled: 1907, Refused: 1119, Unresolved: 99},
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
		"# HELP stable_trace_answered_nodes Published nodes that reported their own egress through cdn-cgi/trace; their tags describe that address.",
		"# TYPE stable_trace_answered_nodes gauge",
		"stable_trace_answered_nodes 157",
		"# TYPE stable_trace_unanswered_nodes gauge",
		"stable_trace_unanswered_nodes 8",
		"# TYPE stable_trace_moved_nodes gauge",
		"stable_trace_moved_nodes 47",
		// Distinct values per stage: a swap of two of the three reads fails here.
		`stable_source_nodes_total{feed="mifa",owner="curated",source="mifa"} 133`,
		`stable_source_valid_nodes{feed="mifa",owner="curated",source="mifa"} 20`,
		`stable_source_tested_nodes{feed="mifa",owner="curated",source="mifa"} 14`,
		`stable_source_published_nodes{feed="mifa",owner="curated",source="mifa"} 9`,
		`stable_source_dropped_nodes{reason="geo",source="mifa"} 71`,
		`stable_source_dropped_nodes{reason="cidr",source="mifa"} 33`,
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
		"# TYPE stable_gemini_gate_enabled gauge",
		"stable_gemini_gate_enabled 1",
		"stable_gemini_gate_checks 306",
		"stable_gemini_gate_unverified_checks 22",
		"# TYPE stable_precheck_trusted gauge",
		"stable_precheck_trusted 1",
		"stable_precheck_dialled_endpoints 1907",
		"stable_precheck_refused_endpoints 1119",
		"stable_precheck_unresolved_endpoints 99",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	// The trace is an annotation stage, not a gate. Its counters must never
	// reach the filter series, where a reader would read 47 nodes moved as 47
	// nodes thrown away.
	if strings.Contains(out, `filter="cloudflare"`) {
		t.Errorf("cloudflare is no longer a filter:\n%s", out)
	}
	// Same rule, for the same reason, on the gemini gate: its 22 unverified
	// nodes were KEPT and published, so nothing about them may render as a
	// drop reason on a panel that means "thrown away".
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "stable_filter_dropped_nodes{") && strings.HasSuffix(line, " 22") {
			t.Errorf("the gemini unverified count reached a drop series: %q", line)
		}
	}
	if strings.Contains(out, `reason="unverified"`) {
		t.Errorf("unverified is not a drop reason:\n%s", out)
	}
}

// TestMetricsSourceOwnerLabels pins what the dashboard groups on. Both labels
// are the entry's own fields, and no label is derived by decomposing a name --
// the feed fallback copies it whole -- so a crawled source keeps its channel
// row even when its name carries a post id, a collision shard or no channel at
// all, and a hand-added source shaped exactly like a minted one --
// seyedng-4102 below -- still renders curated and unfolded.
func TestMetricsSourceOwnerLabels(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.Observe(stable.CycleReport{
		Sources: []stable.SourceReport{
			{
				Name: "genliberty-3631", Managed: true, Feed: "genliberty",
				Total: 41, Valid: 32, Tested: 23, Filtered: 14, GeoDrop: 5,
			},
			{
				Name: "genliberty-3631-7e9b21", Managed: true, Feed: "genliberty",
				Total: 11, Valid: 10, Tested: 9, Filtered: 8,
			},
			{Name: "96c4d7c7a7", Managed: true, Total: 3, Valid: 3, Tested: 2, Filtered: 1},
			{Name: "flat447", Total: 61, Valid: 52, Tested: 43, Filtered: 34, DNSDrop: 6},
			{Name: "seyedng-4102", Total: 7, Valid: 6, Tested: 5, Filtered: 4},
		},
	})

	out := render(t, m)
	for _, want := range []string{
		`stable_source_nodes_total{feed="genliberty",owner="crawler",source="genliberty-3631"} 41`,
		`stable_source_valid_nodes{feed="genliberty",owner="crawler",source="genliberty-3631"} 32`,
		`stable_source_tested_nodes{feed="genliberty",owner="crawler",source="genliberty-3631"} 23`,
		`stable_source_published_nodes{feed="genliberty",owner="crawler",source="genliberty-3631"} 14`,
		`stable_source_nodes_total{feed="genliberty",owner="crawler",source="genliberty-3631-7e9b21"} 11`,
		`stable_source_nodes_total{feed="96c4d7c7a7",owner="crawler",source="96c4d7c7a7"} 3`,
		`stable_source_nodes_total{feed="flat447",owner="curated",source="flat447"} 61`,
		`stable_source_valid_nodes{feed="flat447",owner="curated",source="flat447"} 52`,
		`stable_source_tested_nodes{feed="flat447",owner="curated",source="flat447"} 43`,
		`stable_source_published_nodes{feed="flat447",owner="curated",source="flat447"} 34`,
		`stable_source_nodes_total{feed="seyedng-4102",owner="curated",source="seyedng-4102"} 7`,
		`stable_source_dropped_nodes{reason="geo",source="genliberty-3631"} 5`,
		`stable_source_dropped_nodes{reason="dns",source="flat447"} 6`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Drops are 7 of every 11 per-source samples and nothing reads them by
	// owner, so the family stays at source+reason on purpose.
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(line, "stable_source_dropped_nodes{") {
			continue
		}
		if strings.Contains(line, "feed=") || strings.Contains(line, "owner=") {
			t.Errorf("drop series carries a per-source label it must not: %q", line)
		}
	}
}

// TestMetricsGeminiGateStatesRenderApart is the point of the whole series. A
// gate skipped for want of a key checks NOTHING and passes every survivor
// through, so from the drops panel it is indistinguishable from a gate that
// found nothing wrong -- which is how a rotated key stayed invisible for a
// full production day. So the keyless gate renders an explicit zero rather
// than nothing: absence says only "no gemini gate ran", which has four causes
// -- nothing scraped yet, no cycle published yet, no gemini filter in the
// chain, or one configured that never reached its check (buildNodeFilters
// dropped it with a WARN for want of Gemini support on the prober, or
// ParseProxies failed and the whole through-node stage was skipped, publishing
// every survivor UNFILTERED) -- and only the first two are benign.
func TestMetricsGeminiGateStatesRenderApart(t *testing.T) {
	t.Parallel()

	rendered := map[stable.GeminiGateState]string{}
	for _, tc := range []struct {
		name string
		rep  stable.GeminiReport
		want []string
		deny []string
	}{
		{
			name: "absent",
			rep:  stable.GeminiReport{},
			deny: []string{"stable_gemini_gate_"},
		},
		{
			name: "configured but keyless",
			rep:  stable.GeminiReport{State: stable.GeminiGateSkipped},
			want: []string{
				"stable_gemini_gate_enabled 0",
				"stable_gemini_gate_checks 0",
				"stable_gemini_gate_unverified_checks 0",
			},
		},
		{
			name: "ran and verified everything",
			rep:  stable.GeminiReport{State: stable.GeminiGateRan, Checks: 285},
			want: []string{
				"stable_gemini_gate_enabled 1",
				"stable_gemini_gate_checks 285",
				"stable_gemini_gate_unverified_checks 0",
			},
		},
	} {
		m := metrics.New()
		m.Observe(stable.CycleReport{Gemini: tc.rep})
		out := render(t, m)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: missing %q in:\n%s", tc.name, w, out)
			}
		}
		for _, d := range tc.deny {
			if strings.Contains(out, d) {
				t.Errorf("%s: must render no %q series:\n%s", tc.name, d, out)
			}
		}
		rendered[tc.rep.State] = geminiLines(out)
	}

	// The assertion the three cases exist for: no two states may look alike.
	for a, ra := range rendered {
		for b, rb := range rendered {
			if a != b && ra == rb {
				t.Errorf("gate states %d and %d render identically as %q", a, b, ra)
			}
		}
	}
}

// geminiLines extracts the gemini gate samples (not the HELP/TYPE headers,
// which are constant) so two renderings can be compared for equality.
func geminiLines(out string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "stable_gemini_gate_") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestMetricsPrecheckStatesRenderApart is the gemini gate's lesson on the
// pre-check: a breaker that discarded the verdict condemns nobody, so
// stage="condemned" reads 0 for it exactly as it does for a pre-check that
// found every server reachable, and only these series tell the two apart. A
// prober that runs no pre-check emits nothing at all, matching how a filter
// that never ran emits no drop series.
func TestMetricsPrecheckStatesRenderApart(t *testing.T) {
	t.Parallel()

	rendered := map[stable.PrecheckState]string{}
	for _, tc := range []struct {
		name string
		rep  stable.PrecheckReport
		want []string
	}{
		{
			name: "absent",
			rep:  stable.PrecheckReport{},
		},
		{
			name: "tripped, verdict discarded",
			rep:  stable.PrecheckReport{State: stable.PrecheckTripped, Dialled: 1907, Refused: 1889},
			want: []string{
				"stable_precheck_trusted 0",
				"stable_precheck_dialled_endpoints 1907",
				"stable_precheck_refused_endpoints 1889",
				"stable_precheck_unresolved_endpoints 0",
			},
		},
		{
			name: "ran and condemned nobody",
			rep:  stable.PrecheckReport{State: stable.PrecheckRan, Dialled: 1907},
			want: []string{
				"stable_precheck_trusted 1",
				"stable_precheck_dialled_endpoints 1907",
				"stable_precheck_refused_endpoints 0",
			},
		},
	} {
		m := metrics.New()
		m.Observe(stable.CycleReport{
			Precheck: tc.rep,
			Probed:   1907,
			ProbeStages: map[stable.ProbeStage]int{
				stable.StageUnknown: 0, stable.StageCondemned: 0,
				stable.StageConnect: 0, stable.StageFetch: 0, stable.StagePassed: 1907,
			},
		})
		out := render(t, m)
		// The series a tripped breaker leaves indistinguishable, pinned as the
		// reason these gauges exist.
		if !strings.Contains(out, `stable_probe_outcome_nodes{stage="condemned"} 0`) {
			t.Fatalf("%s: fixture must condemn nobody:\n%s", tc.name, out)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: missing %q in:\n%s", tc.name, w, out)
			}
		}
		// Sample lines, not a substring search: the probe-stage HELP text names
		// stable_precheck_trusted, and an absent pre-check may still be
		// mentioned there.
		if len(tc.want) == 0 && precheckLines(out) != "" {
			t.Errorf("%s: must render no pre-check sample:\n%s", tc.name, out)
		}
		rendered[tc.rep.State] = precheckLines(out)
	}

	for a, ra := range rendered {
		for b, rb := range rendered {
			if a != b && ra == rb {
				t.Errorf("pre-check states %d and %d render identically as %q", a, b, ra)
			}
		}
	}
}

// precheckLines extracts the pre-check samples so two renderings can be
// compared for equality; the HELP/TYPE headers are constant.
func precheckLines(out string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "stable_precheck_") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
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

// TestMetricsCyclePhasesRender gives every phase a distinct value, so a
// rendering that pairs a label with the wrong field fails here rather than
// sending an operator to optimise the wrong stage. The values are the shape
// production shows: probe dominates, the rest share the remainder.
func TestMetricsCyclePhasesRender(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.Observe(stable.CycleReport{
		Kept:     149,
		Duration: 1204*time.Second + 900*time.Millisecond,
		Phases: stable.CyclePhases{
			Fetch:      61 * time.Second,
			Merge:      1500 * time.Millisecond,
			DeadFilter: 250 * time.Millisecond,
			Probe:      1084 * time.Second,
			Egress:     52 * time.Second,
			Publish:    3 * time.Second,
		},
	})

	out := render(t, m)
	wants := []string{
		"# TYPE stable_cycle_phase_duration_seconds gauge",
		`stable_cycle_phase_duration_seconds{phase="fetch"} 61`,
		`stable_cycle_phase_duration_seconds{phase="merge"} 1.5`,
		`stable_cycle_phase_duration_seconds{phase="dead_filter"} 0.25`,
		`stable_cycle_phase_duration_seconds{phase="probe"} 1084`,
		`stable_cycle_phase_duration_seconds{phase="egress"} 52`,
		`stable_cycle_phase_duration_seconds{phase="publish"} 3`,
		"stable_cycle_duration_seconds 1204.9",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	// Six phases, one metric: a seventh line means a label was duplicated or a
	// phase rendered twice, which double-counts the stack.
	if got := strings.Count(out, "stable_cycle_phase_duration_seconds{"); got != 6 {
		t.Errorf("phase samples = %d, want 6:\n%s", got, out)
	}

	// An aborted cycle publishes no phases of its own: ObserveError leaves
	// m.last alone, so the breakdown keeps describing the last PUBLISHED
	// cycle. A zeroed stack would read as a cycle that did nothing.
	before := phaseLines(out)
	m.ObserveError()
	after := render(t, m)
	if got := phaseLines(after); got != before {
		t.Errorf("a failed cycle rewrote the phase breakdown:\ngot  %s\nwant %s", got, before)
	}
	if !strings.Contains(after, "stable_cycle_failures_total 1") {
		t.Errorf("the failure itself must still count:\n%s", after)
	}
}

func phaseLines(out string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "stable_cycle_phase_duration_seconds{") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	return b.String()
}

// TestMetricsProbeStagesRender pairs every stage with a distinct count, so a
// label wired to the wrong key fails here. The shape is the measured one: most
// of the probed set is condemned by the reachability pre-check, a minority
// passes.
func TestMetricsProbeStagesRender(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.Observe(stable.CycleReport{
		Probed: 1898,
		ProbeStages: map[stable.ProbeStage]int{
			stable.StageCondemned: 1118,
			stable.StageConnect:   612,
			stable.StageFetch:     19,
			stable.StagePassed:    149,
			stable.StageUnknown:   0, // classified every probed node
		},
	})

	out := render(t, m)
	wants := []string{
		"# TYPE stable_probe_outcome_nodes gauge",
		`stable_probe_outcome_nodes{stage="condemned"} 1118`,
		`stable_probe_outcome_nodes{stage="connect"} 612`,
		`stable_probe_outcome_nodes{stage="fetch"} 19`,
		`stable_probe_outcome_nodes{stage="passed"} 149`,
		"stable_probed_nodes 1898",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	// Nothing classified as unknown, so that series must be absent rather than
	// a permanent zero.
	if strings.Contains(out, `stable_probe_outcome_nodes{stage="unknown"}`) {
		t.Errorf("an empty unknown bucket must not render:\n%s", out)
	}
}

// TestMetricsProbeStagesRenderZerosAndUnknown pins the two halves of the
// absent/zero decision. A stage that nobody reached is an answer and renders 0;
// a report carrying no fold at all renders NOTHING, because four zeros beside a
// non-zero stable_probed_nodes would read as a cycle that probed nobody rather
// than as the dropped hand-off it is. Unknown renders only when the prober left
// nodes unclassified.
func TestMetricsProbeStagesRenderZerosAndUnknown(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.Observe(stable.CycleReport{
		Probed: 7,
		ProbeStages: map[stable.ProbeStage]int{
			stable.StageCondemned: 0,
			stable.StageConnect:   0,
			stable.StageFetch:     0,
			stable.StagePassed:    3,
			stable.StageUnknown:   4,
		},
	})

	out := render(t, m)
	wants := []string{
		`stable_probe_outcome_nodes{stage="condemned"} 0`,
		`stable_probe_outcome_nodes{stage="connect"} 0`,
		`stable_probe_outcome_nodes{stage="fetch"} 0`,
		`stable_probe_outcome_nodes{stage="passed"} 3`,
		`stable_probe_outcome_nodes{stage="unknown"} 4`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}

	dropped := metrics.New()
	dropped.Observe(stable.CycleReport{Probed: 7})
	if got := render(t, dropped); strings.Contains(got, "stable_probe_outcome_nodes") {
		t.Errorf("a report with no fold must render no stage series:\n%s", got)
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

// TestMetricsKeptLatencyRender pins the whole ladder, not just the samples, so
// that dropping a bound is caught here. The threshold half of the invariant --
// that every shipped max_avg_ms has a bound -- is enforced by
// TestLatencyBucketsCoverShippedGates, which reads the config files this test
// never touches. The 5700 sample exercises the tail; no shipped config sets a
// gate that high, so a survivor cannot carry it in production.
func TestMetricsKeptLatencyRender(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.Observe(stable.CycleReport{Kept: 5, KeptLatenciesMs: []int{80, 250, 640, 900, 5700}})

	out := render(t, m)
	want := `# HELP stable_kept_latency_ms Mean probe delay (ms) of kept nodes.
# TYPE stable_kept_latency_ms histogram
stable_kept_latency_ms_bucket{le="100"} 1
stable_kept_latency_ms_bucket{le="250"} 2
stable_kept_latency_ms_bucket{le="500"} 2
stable_kept_latency_ms_bucket{le="800"} 3
stable_kept_latency_ms_bucket{le="1000"} 4
stable_kept_latency_ms_bucket{le="1500"} 4
stable_kept_latency_ms_bucket{le="3000"} 4
stable_kept_latency_ms_bucket{le="4000"} 4
stable_kept_latency_ms_bucket{le="6000"} 5
stable_kept_latency_ms_bucket{le="12000"} 5
stable_kept_latency_ms_bucket{le="+Inf"} 5
stable_kept_latency_ms_sum 7570
stable_kept_latency_ms_count 5
`
	if !strings.Contains(out, want) {
		t.Errorf("latency histogram:\ngot:\n%s\nwant it to contain:\n%s", out, want)
	}
	for _, w := range []string{
		"stable_kept_latency_min_ms 80",
		"stable_kept_latency_max_ms 5700",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	// The speed buckets must not have been reused: 5 and 10 Mbps are not
	// latency bounds, and every kept node would land in the bottom bucket.
	if strings.Contains(out, `stable_kept_latency_ms_bucket{le="5"}`) {
		t.Errorf("latency histogram is using the speed buckets:\n%s", out)
	}
}

// TestMetricsKeptLatencyGaugesGuarded mirrors the speed pair: with no kept
// nodes the min/max gauges must be absent rather than render 0, which would
// read as a cycle that published instantaneous nodes.
func TestMetricsKeptLatencyGaugesGuarded(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.Observe(stable.CycleReport{})

	out := render(t, m)
	for _, deny := range []string{"stable_kept_latency_min_ms", "stable_kept_latency_max_ms"} {
		if strings.Contains(out, deny) {
			t.Errorf("%s must not render for an empty cycle:\n%s", deny, out)
		}
	}
	if !strings.Contains(out, "stable_kept_latency_ms_count 0") {
		t.Errorf("the histogram itself renders unconditionally:\n%s", out)
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
