// Package metrics renders stable-worker cycle stats in the Prometheus text
// exposition format. It deliberately avoids the prometheus/client_golang
// dependency: the metric set is small, and this module's
// `google.golang.org/protobuf => metacubex/protobuf-go` replace makes pulling
// the client's protobuf-based exposition path a risk not worth taking.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"domains.lst/sub-preprocessor/internal/stable"
)

// speedBuckets are the cumulative upper bounds (Mbps) for the kept-node speed
// histogram.
var speedBuckets = []float64{5, 10, 25, 50, 100, 250, 500}

const (
	labelFilter = "filter"
	labelSource = "source"
	labelReason = "reason"
)

// Metrics holds the latest cycle report plus lifetime counters and renders them
// on scrape. It satisfies stable.Reporter. Use New; the zero value is not ready.
type Metrics struct {
	mu           sync.RWMutex
	last         *stable.CycleReport
	lastAt       time.Time
	cyclesTotal  int64
	cyclesFailed int64
}

func New() *Metrics { return &Metrics{} }

// Observe records a published cycle.
func (m *Metrics) Observe(r stable.CycleReport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = &r
	m.lastAt = time.Now()
	m.cyclesTotal++
}

// ObserveError records a cycle that did not publish a new list; it still counts
// toward the total so failures/total is a valid ratio.
func (m *Metrics) ObserveError() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cyclesTotal++
	m.cyclesFailed++
}

// Handler serves the metrics in Prometheus text format. It renders into a
// buffer under a read lock, then writes, so a slow scrape never blocks Observe.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var buf bytes.Buffer
		m.writeMetrics(&buf)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	})
}

func (m *Metrics) writeMetrics(w io.Writer) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	counter(w, "stable_cycles_total", "Stable cycles attempted (published + failed).", m.cyclesTotal)
	counter(w, "stable_cycle_failures_total", "Stable cycles that did not publish a new list.", m.cyclesFailed)

	if m.last == nil {
		return
	}
	r := m.last

	gauge(w, "stable_sources_ok", "Sources that returned a usable body last cycle.", float64(r.SourcesOK))
	gauge(w, "stable_sources_total", "Sources configured.", float64(r.SourcesTotal))
	gauge(w, "stable_merged_nodes", "Unique nodes after merge/dedupe.", float64(r.Merged))
	gauge(w, "stable_dead_skipped_nodes", "Nodes skipped before probing: a recent probe found them dead.", float64(r.DeadSkipped))
	gauge(w, "stable_probed_nodes", "Nodes latency-probed.", float64(r.Probed))
	gauge(w, "stable_kept_nodes", "Nodes published to /stable.txt.", float64(r.Kept))
	gauge(w, "stable_geo_unknown_nodes", "Published nodes whose GEO tag is [GEO:??]: no annotation provider resolved a country.", float64(r.GeoUnknown))
	writeTrace(w, r.Trace)
	writeGemini(w, r.Gemini)
	if len(r.KeptCountries) > 0 {
		help(w, "stable_kept_country_nodes", "gauge", "Published nodes per resolved country (last cycle).")
		for _, c := range sortedKeys(r.KeptCountries) {
			sample(w, "stable_kept_country_nodes", map[string]string{"country": c}, float64(r.KeptCountries[c]))
		}
	}
	gauge(w, "stable_cycle_duration_seconds", "Wall time of the last cycle.", r.Duration.Seconds())
	gauge(w, "stable_last_success_timestamp_seconds", "Unix time of the last published cycle.", float64(m.lastAt.Unix()))

	writeFilters(w, r.Filters)
	writeSources(w, r.Sources)
	writeHistogram(w, "stable_kept_speed_mbps", "Download speed (Mbps) of kept nodes.", r.KeptSpeeds)
	if len(r.KeptSpeeds) > 0 {
		gauge(w, "stable_kept_speed_min_mbps", "Slowest kept node's measured speed last cycle.", float64(slices.Min(r.KeptSpeeds)))
		gauge(w, "stable_kept_speed_max_mbps", "Fastest kept node's measured speed last cycle.", float64(slices.Max(r.KeptSpeeds)))
	}
}

func writeFilters(w io.Writer, filters []stable.FilterReport) {
	help(w, "stable_filter_in_nodes", "gauge", "Survivors entering each through-node filter.")
	for _, f := range filters {
		sample(w, "stable_filter_in_nodes", map[string]string{labelFilter: f.Name}, float64(f.In))
	}
	help(w, "stable_filter_kept_nodes", "gauge", "Survivors kept by each through-node filter.")
	for _, f := range filters {
		sample(w, "stable_filter_kept_nodes", map[string]string{labelFilter: f.Name}, float64(f.Kept))
	}
	help(w, "stable_filter_dropped_nodes", "gauge", "Survivors dropped by each through-node filter, by reason.")
	for _, f := range filters {
		for _, reason := range sortedKeys(f.Dropped) {
			sample(w, "stable_filter_dropped_nodes", map[string]string{labelFilter: f.Name, labelReason: reason}, float64(f.Dropped[reason]))
		}
	}
}

// writeTrace renders the cloudflare annotation stage. It is not a filter and has
// no FilterReport: the trace drops nothing, so answered+unanswered is simply
// the published list split by whether the node told us where it exits.
func writeTrace(w io.Writer, t stable.TraceReport) {
	gauge(w, "stable_trace_answered_nodes", "Published nodes that reported their own egress through cdn-cgi/trace; their tags describe that address.", float64(t.Answered))
	gauge(w, "stable_trace_unanswered_nodes", "Published nodes whose trace did not complete: kept and tagged from the offline chain alone, never dropped.", float64(t.Unanswered))
	gauge(w, "stable_trace_moved_nodes", "Answered nodes exiting from a country other than the one the offline chain places their resolved address in: how often that chain would have tagged the wrong country.", float64(t.Moved))
}

// writeGemini renders the gemini gate's self-account. These nodes are KEPT, so
// none of it belongs in stable_filter_dropped_nodes; a reader of that series
// must be able to keep reading it as "thrown away".
//
// The three gate states render distinguishably, which is the whole point:
// absent emits NOTHING, matching the way a filter that never ran emits no drop
// series; configured-but-keyless emits enabled=0 with both counts at zero; a
// gate that ran emits enabled=1. An explicit 0 rather than absence for the
// keyless case is deliberate -- absence cannot be pinned on any single cause
// (no scrape, no cycle published yet, no gemini filter in the chain, or one
// configured that never reached its check: buildNodeFilters dropped it for
// want of Gemini support on the prober, or filterAndMeasureEgress bailed
// before the chain when ParseProxies failed) -- and mistaking a dead gate for
// a healthy one is exactly the failure this metric exists to make visible.
func writeGemini(w io.Writer, g stable.GeminiReport) {
	if g.State == stable.GeminiGateAbsent {
		return
	}
	var enabled float64
	if g.State == stable.GeminiGateRan {
		enabled = 1
	}
	gauge(w, "stable_gemini_gate_enabled",
		"1 when the gemini gate ran last cycle, 0 when it is configured as a filter but was skipped for want of a usable API key -- it then checked nothing and every survivor passed through unverified.",
		enabled)
	gauge(w, "stable_gemini_gate_checks",
		"Gemini API responses the gate classified last cycle: the denominator for stable_gemini_gate_unverified_checks. One per proxy that ANSWERED, so it is neither the node count nor stable_filter_in_nodes{filter=\"gemini\"} -- a mierus:// node contributes one per answering port, not one per configured port. A proxy that never answered is in neither term here, and reaches stable_filter_dropped_nodes{filter=\"gemini\",reason=\"unreachable\"} only when EVERY proxy of its node was unreachable: that reason is counted per SURVIVOR, off an outcome already folded best-of-ports, so one live port keeps the node and its dead siblings are counted nowhere.",
		float64(g.Checks))
	gauge(w, "stable_gemini_gate_unverified_checks",
		"Responses that told the gate nothing about the egress location: the API rejected the request before evaluating it (401/403/404/429, or 400 API_KEY_INVALID -- a rotated, restricted or quota-exhausted key). These nodes were KEPT and published unverified; this is not a drop, not a block and not a request error.",
		float64(g.Unverified))
}

func writeSources(w io.Writer, sources []stable.SourceReport) {
	help(w, "stable_source_nodes_total", "gauge", "Nodes each source yielded before filtering last cycle.")
	for _, s := range sources {
		sample(w, "stable_source_nodes_total", map[string]string{labelSource: s.Name}, float64(s.Total))
	}
	help(w, "stable_source_kept_nodes", "gauge", "Nodes each source contributed after preprocess filtering.")
	for _, s := range sources {
		sample(w, "stable_source_kept_nodes", map[string]string{labelSource: s.Name}, float64(s.Kept))
	}
	help(w, "stable_source_dropped_nodes", "gauge", "Nodes each source dropped in preprocess, by reason (reason=unsupported counts unparseable input lines, which are not in stable_source_nodes_total).")
	for _, s := range sources {
		reasons := []struct {
			reason string
			n      int
		}{
			{"dns", s.DNSDrop}, {"geo", s.GeoDrop}, {"asn", s.ASNDrop},
			{"geoblock", s.GeoBlockDrop}, {"ipv6", s.IPv6Drop},
			{"unsupported", s.Unsupported},
		}
		for _, d := range reasons {
			sample(w, "stable_source_dropped_nodes", map[string]string{labelSource: s.Name, labelReason: d.reason}, float64(d.n))
		}
	}
}

func writeHistogram(w io.Writer, name, helpText string, values []int) {
	help(w, name, "histogram", helpText)
	counts := make([]int, len(speedBuckets))
	var sum float64
	for _, v := range values {
		sum += float64(v)
		for i, ub := range speedBuckets {
			if float64(v) <= ub {
				counts[i]++ // le buckets are cumulative: a value counts in every bound it is under
			}
		}
	}
	for i, ub := range speedBuckets {
		fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, formatFloat(ub), counts[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, len(values))
	fmt.Fprintf(w, "%s_sum %s\n", name, formatFloat(sum))
	fmt.Fprintf(w, "%s_count %d\n", name, len(values))
}

func gauge(w io.Writer, name, helpText string, v float64) {
	help(w, name, "gauge", helpText)
	sample(w, name, nil, v)
}

func counter(w io.Writer, name, helpText string, v int64) {
	help(w, name, "counter", helpText)
	sample(w, name, nil, float64(v))
}

func help(w io.Writer, name, typ, helpText string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, helpText, name, typ)
}

func sample(w io.Writer, name string, labels map[string]string, v float64) {
	if len(labels) == 0 {
		fmt.Fprintf(w, "%s %s\n", name, formatFloat(v))
		return
	}
	fmt.Fprintf(w, "%s{%s} %s\n", name, formatLabels(labels), formatFloat(v))
}

func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labels[k]))
		b.WriteByte('"')
	}
	return b.String()
}

// invalidLabelValue replaces a label value that is not valid UTF-8.
const invalidLabelValue = "invalid_utf8"

// labelValueEscaper escapes exactly the three characters the text format
// reserves in a label value. It is not a place to be creative: Prometheus's
// parser accepts only \\, \" and \n after a backslash and fails the whole
// scrape on any other escape sequence
// (prometheus/common expfmt.TextParser.readTokenAsLabelValue). A carriage
// return needs no escape — the parser copies it through — so it is left alone
// rather than turned into an invalid \r.
var labelValueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// escapeLabelValue renders s as a Prometheus text-format label value: valid
// UTF-8 with the reserved characters escaped.
//
// Validity is checked, not assumed. A label value can originate outside this
// service — with annotate disabled the pipeline republishes upstream node
// names verbatim, and the country label is read out of a [GEO:xx] tag inside
// such a name — and an invalid byte fails the label value
// (prometheus/common model.LabelValue.IsValid is utf8.ValidString), taking the
// ENTIRE scrape and every other metric down with it. Replacing the value is
// strictly better than reproducing it faithfully; this is the last line of
// defence, not the first.
func escapeLabelValue(s string) string {
	if !utf8.ValidString(s) {
		return invalidLabelValue
	}
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return labelValueEscaper.Replace(s)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
