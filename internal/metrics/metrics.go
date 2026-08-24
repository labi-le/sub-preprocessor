// Package metrics renders stable-worker cycle stats in the Prometheus text
// exposition format. It deliberately avoids the prometheus/client_golang
// dependency: the metric set is small, and this module's
// `google.golang.org/protobuf => metacubex/protobuf-go` replace makes pulling
// the client's protobuf-based exposition path a risk not worth taking.
package metrics

import (
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

// latencyBuckets are the cumulative upper bounds (ms) for the kept-node
// latency histogram.
//
// Invariant: this ladder carries a bound equal to EVERY instance's shipped
// check.max_avg_ms, and changing one ships its bucket in the same commit.
// Instances share a Prometheus and differ only by job, so a gate that falls
// between two bounds is invisible on the panel — which is the one question
// the metric exists to answer; TestLatencyBucketsCoverShippedGates checks it
// against the shipped config files. 1000 is defaultCheckMaxAvgMs, the gate a
// config omitting the key runs on. The tail runs past every gate only so that
// raising a max_avg_ms into it stays a one-file edit -- SelectSurvivors drops
// everything above the gate, so those bounds always equal +Inf.
var latencyBuckets = []float64{100, 250, 500, 800, 1000, 1500, 3000, 4000, 6000, 12000}

const (
	labelFilter = "filter"
	labelSource = "source"
	// labelFeed is "feed" and not "group": group is also a PromQL aggregation
	// operator, so `sum by (group)` reads as a keyword.
	labelFeed   = "feed"
	labelOwner  = "owner"
	labelReason = "reason"
	labelPhase  = "phase"
	labelStage  = "stage"
)

// Who owns a source: the crawler mints and prunes the managed ones,
// everything else is hand-added to the config.
const (
	ownerCrawler = "crawler"
	ownerCurated = "curated"
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

// Handler serves the metrics in Prometheus text format. It snapshots the cycle
// report under a read lock and renders outside it, so neither a slow scrape nor
// the whole exposition sits between Observe and the lock.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.writeMetrics(w)
	})
}

func (m *Metrics) writeMetrics(dst io.Writer) {
	// Snapshotted rather than rendered under the lock: Observe publishes a
	// fresh *CycleReport and never mutates a published one.
	m.mu.RLock()
	cyclesTotal, cyclesFailed, r, lastAt := m.cyclesTotal, m.cyclesFailed, m.last, m.lastAt
	m.mu.RUnlock()

	w := newExposition(dst)
	defer w.flush()

	counter(w, "stable_cycles_total", "Stable cycles attempted (published + failed).", cyclesTotal)
	counter(w, "stable_cycle_failures_total", "Stable cycles that did not publish a new list.", cyclesFailed)

	if r == nil {
		return
	}

	gauge(w, "stable_sources_ok", "Sources that returned a usable body last cycle.", float64(r.SourcesOK))
	gauge(w, "stable_sources_total", "Sources configured.", float64(r.SourcesTotal))
	gauge(w, "stable_merged_nodes", "Unique nodes after merge/dedupe.", float64(r.Merged))
	gauge(w, "stable_dead_skipped_nodes", "Nodes skipped before probing: a recent probe found them dead.", float64(r.DeadSkipped))
	gauge(w, "stable_probed_nodes", "Nodes latency-probed.", float64(r.Probed))
	writeProbeStages(w, r.ProbeStages)
	writePrecheck(w, r.Precheck)
	gauge(w, "stable_kept_nodes", "Nodes published to /stable.txt.", float64(r.Kept))
	gauge(w, "stable_geo_unknown_nodes", "Published nodes whose GEO tag is [GEO:??]: no annotation provider resolved a country.", float64(r.GeoUnknown))
	writeTrace(w, r.Trace)
	writeGemini(w, r.Gemini)
	if len(r.KeptCountries) > 0 {
		help(w, "stable_kept_country_nodes", "gauge", "Published nodes per resolved country (last cycle).")
		for _, c := range sortedKeys(r.KeptCountries) {
			sampleEsc(w, "stable_kept_country_nodes", "country", c, float64(r.KeptCountries[c]))
		}
	}
	gauge(w, "stable_cycle_duration_seconds", "Wall time of the last cycle.", r.Duration.Seconds())
	writePhases(w, r.Phases)
	gauge(w, "stable_last_success_timestamp_seconds", "Unix time of the last published cycle.", float64(lastAt.Unix()))

	writeFilters(w, r.Filters)
	writeSources(w, r.Sources)
	writeHistogram(w, "stable_kept_speed_mbps", "Download speed (Mbps) of kept nodes.", r.KeptSpeeds, speedBuckets)
	if len(r.KeptSpeeds) > 0 {
		gauge(w, "stable_kept_speed_min_mbps", "Slowest kept node's measured speed last cycle.", float64(slices.Min(r.KeptSpeeds)))
		gauge(w, "stable_kept_speed_max_mbps", "Fastest kept node's measured speed last cycle.", float64(slices.Max(r.KeptSpeeds)))
	}
	writeHistogram(w, "stable_kept_latency_ms", "Mean probe delay (ms) of kept nodes.", r.KeptLatenciesMs, latencyBuckets)
	if len(r.KeptLatenciesMs) > 0 {
		gauge(w, "stable_kept_latency_min_ms", "Fastest kept node's mean probe delay last cycle.", float64(slices.Min(r.KeptLatenciesMs)))
		gauge(w, "stable_kept_latency_max_ms", "Slowest kept node's mean probe delay last cycle: how close the published list runs to max_avg_ms.", float64(slices.Max(r.KeptLatenciesMs)))
	}
}

func writeFilters(w *exposition, filters []stable.FilterReport) {
	help(w, "stable_filter_in_nodes", "gauge", "Survivors entering each through-node filter.")
	for _, f := range filters {
		sampleEsc(w, "stable_filter_in_nodes", labelFilter, f.Name, float64(f.In))
	}
	help(w, "stable_filter_kept_nodes", "gauge", "Survivors kept by each through-node filter.")
	for _, f := range filters {
		sampleEsc(w, "stable_filter_kept_nodes", labelFilter, f.Name, float64(f.Kept))
	}
	help(w, "stable_filter_dropped_nodes", "gauge", "Survivors dropped by each through-node filter, by reason.")
	lbl := make([]byte, 0, labelScratch)
	for _, f := range filters {
		for _, reason := range sortedKeys(f.Dropped) {
			lbl = appendLabelEsc(lbl[:0], labelFilter, f.Name)
			lbl = append(lbl, ',')
			lbl = appendLabelEsc(lbl, labelReason, reason)
			sample(w, "stable_filter_dropped_nodes", lbl, float64(f.Dropped[reason]))
		}
	}
}

// writePhases renders where the cycle's wall time went. One metric with a
// phase label, not six names: the panel stacks them, and a stack wants one
// series selector.
//
// The phases sum to LESS than stable_cycle_duration_seconds, and the panel
// says so -- the steps between stages are in no phase. A cycle that aborted
// renders nothing here at all: reportError reaches ObserveError, which leaves
// m.last alone, so every gauge on this page -- these included -- keeps
// describing the last cycle that PUBLISHED. Only stable_cycles_total and
// stable_cycle_failures_total move on a failure.
func writePhases(w *exposition, p stable.CyclePhases) {
	help(w, "stable_cycle_phase_duration_seconds", "gauge",
		"Wall time of each phase of the last published cycle. The phases sum to slightly less than stable_cycle_duration_seconds: the steps between them (dead-cache write, survivor selection, cache prune, report assembly) belong to no phase. phase=\"probe\" is the whole Prober.Probe call -- payload parsing, then the TCP reachability pre-check at its own concurrency outside check.concurrency, then the URL-test rounds -- so check.timeout * check.rounds bounds only its last part; the condemned count in the probe-outcome stage breakdown is what shows the pre-check's share. phase=\"egress\" is the through-node filters and the cdn-cgi/trace measurement, which also do per-node network work.")
	// Pipeline order, not sorted: the panel legend reads as the funnel.
	for _, ph := range []struct {
		name string
		d    time.Duration
	}{
		{"fetch", p.Fetch},
		{"merge", p.Merge},
		{"dead_filter", p.DeadFilter},
		{"probe", p.Probe},
		{"egress", p.Egress},
		{"publish", p.Publish},
	} {
		sampleLit(w, "stable_cycle_phase_duration_seconds", labelPhase, ph.name, ph.d.Seconds())
	}
}

// writeProbeStages splits the probed set by how far each node's probe got. The
// four known stages render even at zero -- "nobody failed at fetch" is an
// answer -- while stage="unknown" appears only when the prober left nodes
// unclassified, since a permanent zero there says nothing.
//
// A nil map renders nothing: the fold reaching CycleReport is the hand-off that
// can be dropped, and an absent series says that, where four zeros beside a
// non-zero stable_probed_nodes would read as a cycle that probed nobody.
func writeProbeStages(w *exposition, stages map[stable.ProbeStage]int) {
	if len(stages) == 0 {
		return
	}
	help(w, "stable_probe_outcome_nodes", "gauge",
		"Probed nodes by how far the probe got, summing to stable_probed_nodes. stage=\"condemned\" never spent a URL test: the reachability pre-check proved the server accepts no TCP connection. It reads 0 both for a pre-check that condemned nobody and for one whose breaker discarded its verdict -- stable_precheck_trusted tells those apart -- and an endpoint whose name did not resolve is never condemned, since a failed lookup proves nothing. stage=\"connect\" merges transport and crypto failures deliberately -- mihomo's vless adapter renders its dial error with %s, and vless dominates every pool this worker reads, so no error inspection can separate them. stage=\"fetch\" got a tunnel and failed the GET through it. stage=\"passed\" answered at least one round. This counts NODES, folded best-of-ports: it cannot express what share of ATTEMPTS burned the full check.timeout.")
	// Progress order, unknown last: it is a defect indicator, not a stage.
	for _, s := range []stable.ProbeStage{
		stable.StageCondemned, stable.StageConnect, stable.StageFetch, stable.StagePassed,
	} {
		sampleEsc(w, "stable_probe_outcome_nodes", labelStage, s.String(), float64(stages[s]))
	}
	if n := stages[stable.StageUnknown]; n > 0 {
		sampleEsc(w, "stable_probe_outcome_nodes", labelStage, stable.StageUnknown.String(), float64(n))
	}
}

// writePrecheck renders the reachability pre-check's self-account. A tripped
// breaker discards its verdict and URL-tests everything, which leaves
// stable_probe_outcome_nodes{stage="condemned"} at 0 -- byte-identical to a
// pre-check that ran and condemned nobody. Same failure mode as the gemini
// gate's, so the same shape of fix: the trusted flag renders an explicit 0
// there, an explicit 1 when the verdict was used, and NOTHING when no
// pre-check ran at all.
//
// The counts are ENDPOINTS, not nodes: one distinct server:port is dialled
// once where a multi-port mierus:// node is several, so they never match the
// condemned node count.
func writePrecheck(w *exposition, p stable.PrecheckReport) {
	if p.State == stable.PrecheckAbsent {
		return
	}
	var trusted float64
	if p.State == stable.PrecheckRan {
		trusted = 1
	}
	gauge(w, "stable_precheck_trusted",
		"1 when the TCP reachability pre-check's verdict was used last cycle, 0 when its breaker rejected it: refused at least 95% of the endpoints it judged, so the verdict was DISCARDED, every node was URL-tested and stable_probe_outcome_nodes{stage=\"condemned\"} reads 0 exactly as it does for a pre-check that condemned nobody. stable_precheck_refused_endpoints is then the only trace of what was thrown away.",
		trusted)
	gauge(w, "stable_precheck_dialled_endpoints",
		"Distinct server:port endpoints the pre-check dialled last cycle: its own denominator. Endpoints, not nodes -- a multi-port mierus:// node is several, and a node whose adapter reaches its server over UDP (hysteria2/tuic/mieru, or vless xhttp-over-QUIC) is dialled not at all.",
		float64(p.Dialled))
	gauge(w, "stable_precheck_refused_endpoints",
		"Endpoints the pre-check proved unreachable: a refused or black-holed SYN on every attempt. Condemned unprobed and dead-cached while stable_precheck_trusted is 1; discarded when it is 0. A healthy egress measured ~59% of dialled endpoints here.",
		float64(p.Refused))
	gauge(w, "stable_precheck_unresolved_endpoints",
		"Endpoints whose name never became an address inside the dial budget (a *net.DNSError, timeout included). The pre-check proved nothing about them, so they were URL-tested rather than condemned: mihomo's own dial resolves through a stale-serving cache where this path pays an uncached lookup per attempt. ~5% of dialled endpoints in the measured mix; a spike is a resolver fault, not a pool fault.",
		float64(p.Unresolved))
}

// writeTrace renders the cloudflare annotation stage. It is not a filter and has
// no FilterReport: the trace drops nothing, so answered+unanswered is simply
// the published list split by whether the node told us where it exits.
func writeTrace(w *exposition, t stable.TraceReport) {
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
func writeGemini(w *exposition, g stable.GeminiReport) {
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

// dropReasons is the order the drop family has always rendered in; reason is
// the only label that varies within a source.
var dropReasons = [...]string{"dns", "geo", "cidr", "asn", "geoblock", "ipv6", "unsupported"}

func writeSources(w *exposition, sources []stable.SourceReport) {
	// The four count families below carry one identical label set per source,
	// but Prometheus wants a family's samples contiguous, so they cannot share
	// a loop. One arena plus one offset per source costs two allocations where
	// a []string of rendered label sets cost one per source.
	labels := make([]byte, 0, sourceLabelBytes(sources))
	ends := make([]int, len(sources))
	for i, s := range sources {
		owner := ownerCurated
		if s.Managed {
			owner = ownerCrawler
		}
		feed := sourceFeed(s)
		// Sorted as the label map was: feed, owner, source.
		labels = appendLabelEsc(labels, labelFeed, feed)
		labels = append(labels, ',')
		labels = appendLabel(labels, labelOwner, owner)
		labels = append(labels, ',')
		labels = appendLabelEsc(labels, labelSource, s.Name)
		ends[i] = len(labels)
	}

	help(w, "stable_source_nodes_total", "gauge", "Nodes each source yielded before filtering last cycle. source is the verbatim config key; owner and feed are the entry's own fields, never re-derived from the name -- owner=crawler when the crawler minted the entry and curated when it was hand-added, feed the channel it was minted from, or the name itself where no channel was recorded.")
	for i, s := range sources {
		sample(w, "stable_source_nodes_total", sourceLabels(labels, ends, i), float64(s.Total))
	}
	help(w, "stable_source_valid_nodes", "gauge", "Nodes each source contributed after preprocess filtering. Counted per source BEFORE Merge dedupes across sources, so a node two sources both yield is counted in both.")
	for i, s := range sources {
		sample(w, "stable_source_valid_nodes", sourceLabels(labels, ends, i), float64(s.Valid))
	}
	help(w, "stable_source_tested_nodes", "gauge", "Nodes each source contributed that survived the URL test. Measured after the merge, so stable_source_valid_nodes minus this is NOT the probe failures. Merge first drops what will not re-parse, what is a placeholder, what an earlier source already yielded at the same server:port, and what cannot carry its label; then the dead-node cache skips every merged node whose recent probe failed, and only the remainder is URL-tested. The duplicate term is the largest of the three -- a node two sources both yield counts in both of their valid gauges and in neither's later ones -- and the dead-node skip is merely the largest term no per-source series exposes: read stable_dead_skipped_nodes against stable_merged_nodes for it.")
	for i, s := range sources {
		sample(w, "stable_source_tested_nodes", sourceLabels(labels, ends, i), float64(s.Tested))
	}
	help(w, "stable_source_published_nodes", "gauge", "Nodes each source contributed that survived every through-node filter and reached the published list.")
	for i, s := range sources {
		sample(w, "stable_source_published_nodes", sourceLabels(labels, ends, i), float64(s.Filtered))
	}
	// The drop family keeps source+reason alone: 7 of every 11 per-source
	// samples are drops (3507 of 5511 at 501 sources), and nothing consumes
	// drops by owner. Before these labels the family was 265324 exposition
	// bytes (prod :9091, 2026-08-18 00:58 +0300).
	help(w, "stable_source_dropped_nodes", "gauge", "Nodes each source dropped in preprocess, by reason (reason=unsupported counts unparseable input lines, which are not in stable_source_nodes_total). It deliberately carries no feed or owner labels: nothing reads drops by owner.")
	lbl, src := make([]byte, 0, labelScratch), make([]byte, 0, labelScratch)
	for _, s := range sources {
		counts := [len(dropReasons)]int{
			s.DNSDrop, s.GeoDrop, s.CIDRDrop, s.ASNDrop,
			s.GeoBlockDrop, s.IPv6Drop, s.Unsupported,
		}
		// reason sorts before source, and only the name needs escaping: the
		// seven reasons are literals.
		src = appendLabelEsc(src[:0], labelSource, s.Name)
		for i, reason := range dropReasons {
			lbl = appendLabel(lbl[:0], labelReason, reason)
			lbl = append(lbl, ',')
			lbl = append(lbl, src...)
			sample(w, "stable_source_dropped_nodes", lbl, float64(counts[i]))
		}
	}
}

func sourceLabels(arena []byte, ends []int, i int) []byte {
	if i == 0 {
		return arena[:ends[0]]
	}
	return arena[ends[i-1]:ends[i]]
}

// sourceFeed is the feed label's value. An entry with no recorded channel names
// itself -- typically the mint that saw no origin, though ownership does not
// enter it: a curated entry may set feed too.
func sourceFeed(s stable.SourceReport) string {
	if s.Feed != "" {
		return s.Feed
	}
	return s.Name
}

// sourceLabelBytes estimates the arena: a source costs its name, its feed and
// labelSyntaxBytes. It is not a ceiling -- a name carrying a byte
// appendLabelValue escapes, or one that is not valid UTF-8, renders wider than
// it measures (`a"b` measures 39 and renders 41, "\xff" measures 35 and renders
// 57 -- 2026-08-18). The regrow is safe because ends holds lengths, not
// pointers into the arena.
// Only 4096 B of BenchmarkWriteSources' 12288 B drop is this estimate; the rest
// is the fixture's shorter names (docs/guides/benchmarks.md).
func sourceLabelBytes(sources []stable.SourceReport) int {
	n := 0
	for _, s := range sources {
		n += len(s.Name) + len(sourceFeed(s)) + labelSyntaxBytes
	}
	return n
}

var leInf = []byte(`le="+Inf"`)

func writeHistogram(w *exposition, name, helpText string, values []int, buckets []float64) {
	help(w, name, "histogram", helpText)
	counts := make([]int, len(buckets))
	var sum float64
	for _, v := range values {
		sum += float64(v)
		for i, ub := range buckets {
			if float64(v) <= ub {
				counts[i]++ // le buckets are cumulative: a value counts in every bound it is under
			}
		}
	}
	le := make([]byte, 0, labelScratch)
	for i, ub := range buckets {
		le = append(le[:0], `le="`...)
		le = appendFloat(le, ub)
		le = append(le, '"')
		intSample(w, name, "_bucket", le, counts[i])
	}
	intSample(w, name, "_bucket", leInf, len(values))
	floatSample(w, name, "_sum", nil, sum)
	intSample(w, name, "_count", nil, len(values))
}

// exposition is the scrape's render buffer. The writers below append into it
// instead of calling fmt.Fprintf per sample: boxing every argument into an
// []any and heap-copying the string headers accounted for 73% of the objects a
// 501-source scrape allocated (24912 allocs/op).
type exposition struct {
	w io.Writer
	b []byte
}

// flushAt caps the buffer, so peak footprint is one bound rather than the whole
// ~400 kB exposition; bufSlack covers the longest burst between two flush
// points, help()'s HELP+TYPE pair for stable_probe_outcome_nodes at 941 B
// (measured on testdata/exposition.golden, 2026-08-18). Neither bufSlack nor
// labelScratch is a ceiling -- a config-supplied source or filter name renders
// past both -- but growing them by append is correct, merely one allocation.
const (
	flushAt      = 32 << 10
	bufSlack     = 4 << 10
	labelScratch = 128
)

func newExposition(w io.Writer) *exposition {
	return &exposition{w: w, b: make([]byte, 0, flushAt+bufSlack)}
}

func (e *exposition) endLine() {
	e.b = append(e.b, '\n')
	if len(e.b) >= flushAt {
		e.flush()
	}
}

// flush drops a write error exactly as fmt.Fprintf did: a scrape that hung up
// mid-render leaves nobody to report it to.
func (e *exposition) flush() {
	if len(e.b) == 0 {
		return
	}
	_, _ = e.w.Write(e.b)
	e.b = e.b[:0]
}

func gauge(w *exposition, name, helpText string, v float64) {
	help(w, name, "gauge", helpText)
	sample(w, name, nil, v)
}

func counter(w *exposition, name, helpText string, v int64) {
	help(w, name, "counter", helpText)
	intSample(w, name, "", nil, int(v))
}

func help(w *exposition, name, typ, helpText string) {
	w.b = append(w.b, "# HELP "...)
	w.b = append(w.b, name...)
	w.b = append(w.b, ' ')
	w.b = append(w.b, helpText...)
	w.b = append(w.b, "\n# TYPE "...)
	w.b = append(w.b, name...)
	w.b = append(w.b, ' ')
	w.b = append(w.b, typ...)
	w.endLine()
}

func sample(w *exposition, name string, labels []byte, v float64) {
	floatSample(w, name, "", labels, v)
}

// sampleLit takes its label value verbatim, for the values this file spells
// out; sampleEsc escapes one that arrived from elsewhere.
func sampleLit(w *exposition, name, key, value string, v float64) {
	w.b = append(w.b, name...)
	w.b = append(w.b, '{')
	w.b = appendLabel(w.b, key, value)
	w.b = append(w.b, '}', ' ')
	w.b = appendFloat(w.b, v)
	w.endLine()
}

func sampleEsc(w *exposition, name, key, value string, v float64) {
	w.b = append(w.b, name...)
	w.b = append(w.b, '{')
	w.b = appendLabelEsc(w.b, key, value)
	w.b = append(w.b, '}', ' ')
	w.b = appendFloat(w.b, v)
	w.endLine()
}

func floatSample(w *exposition, name, suffix string, labels []byte, v float64) {
	w.b = appendSampleHead(w.b, name, suffix, labels)
	w.b = appendFloat(w.b, v)
	w.endLine()
}

// intSample exists because a count must not go through the float path: 'g'
// would render 12345678 as 1.2345678e+07.
func intSample(w *exposition, name, suffix string, labels []byte, n int) {
	w.b = appendSampleHead(w.b, name, suffix, labels)
	w.b = strconv.AppendInt(w.b, int64(n), intBase)
	w.endLine()
}

// A sample's number format is wire format, so it lives in one place: 'g' at
// shortest round-trip precision, exactly what strconv.FormatFloat rendered
// before. labelSyntaxBytes is the quotes, equals signs, commas and owner value
// of a source's three label pairs.
const (
	floatVerb        = 'g'
	floatPrec        = -1
	floatBits        = 64
	intBase          = 10
	labelSyntaxBytes = 33
)

func appendFloat(dst []byte, v float64) []byte {
	return strconv.AppendFloat(dst, v, floatVerb, floatPrec, floatBits)
}

func appendSampleHead(dst []byte, name, suffix string, labels []byte) []byte {
	dst = append(dst, name...)
	dst = append(dst, suffix...)
	if len(labels) > 0 {
		dst = append(dst, '{')
		dst = append(dst, labels...)
		dst = append(dst, '}')
	}
	return append(dst, ' ')
}

func appendLabel(dst []byte, key, value string) []byte {
	dst = append(dst, key...)
	dst = append(dst, '=', '"')
	dst = append(dst, value...)
	return append(dst, '"')
}

func appendLabelEsc(dst []byte, key, value string) []byte {
	dst = append(dst, key...)
	dst = append(dst, '=', '"')
	dst = appendLabelValue(dst, value)
	return append(dst, '"')
}

// invalidLabelValue replaces a label value that is not valid UTF-8.
const invalidLabelValue = "invalid_utf8"

// appendLabelValue appends s as a Prometheus text-format label value: valid
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
//
// The escapes are exactly the three the text format reserves and no more:
// Prometheus's parser accepts only \\, \" and \n after a backslash and fails
// the whole scrape on any other escape sequence
// (prometheus/common expfmt.TextParser.readTokenAsLabelValue). A carriage
// return is copied through, so escaping it would invent an invalid \r.
func appendLabelValue(dst []byte, s string) []byte {
	if !utf8.ValidString(s) {
		return append(dst, invalidLabelValue...)
	}
	if !strings.ContainsAny(s, "\\\"\n") {
		return append(dst, s...)
	}
	for i := range len(s) {
		switch c := s[i]; c {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '"':
			dst = append(dst, '\\', '"')
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
