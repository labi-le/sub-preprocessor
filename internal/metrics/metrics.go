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
	gauge(w, "stable_dead_skipped_nodes", "Nodes skipped by the dead-node cache.", float64(r.DeadSkipped))
	gauge(w, "stable_probed_nodes", "Nodes latency-probed.", float64(r.Probed))
	gauge(w, "stable_kept_nodes", "Nodes published to /stable.txt.", float64(r.Kept))
	gauge(w, "stable_geo_unknown_nodes", "Published nodes whose GEO tag is [GEO:??]: no annotation provider resolved a country.", float64(r.GeoUnknown))
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

func writeSources(w io.Writer, sources []stable.SourceReport) {
	help(w, "stable_source_nodes_total", "gauge", "Nodes each source yielded before filtering last cycle.")
	for _, s := range sources {
		sample(w, "stable_source_nodes_total", map[string]string{labelSource: s.Name}, float64(s.Total))
	}
	help(w, "stable_source_kept_nodes", "gauge", "Nodes each source contributed after preprocess filtering.")
	for _, s := range sources {
		sample(w, "stable_source_kept_nodes", map[string]string{labelSource: s.Name}, float64(s.Kept))
	}
	help(w, "stable_source_dropped_nodes", "gauge", "Nodes each source dropped in preprocess, by reason.")
	for _, s := range sources {
		reasons := []struct {
			reason string
			n      int
		}{
			{"dns", s.DNSDrop}, {"geo", s.GeoDrop}, {"asn", s.ASNDrop},
			{"geoblock", s.GeoBlockDrop}, {"unsupported", s.Unsupported},
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

// escapeLabelValue escapes the three characters the text format reserves in a
// label value: backslash, double-quote, and newline.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
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
