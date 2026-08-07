package stable

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/preprocess"
)

// Filterer is the worker's half of the preprocess processor: the per-node IP
// stage plus the annotator the publication runs. Declared locally to avoid an
// import cycle.
type Filterer interface {
	FilterNodes(ctx context.Context, req preprocess.FilterRequest) ([]preprocess.NodeResult, preprocess.Stats, error)
	// Annotator is nil when annotation is configured off; the cycle then
	// publishes merged lines verbatim.
	Annotator() preprocess.Annotator
}

// traceChecker is the prober's egress-measuring half. Unlike the through-node
// gates it answers with a measurement rather than a verdict, which is why it
// runs after the filter chain instead of inside it.
type traceChecker interface {
	TraceCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]TraceResult
}

// Blocklist records nodes that failed a through-node API check; declared
// locally to avoid importing geoblock. A nil Blocklist disables persistence.
// Prune is part of the contract for the same reason DeadCache has one: a TTL
// store that is only swept at process start grows for the whole uptime of a
// container that restarts monthly at best.
type Blocklist interface {
	Block(host string) error
	Prune() error
}

// DeadCache skips re-probing recently-dead nodes; a nil DeadCache disables it.
type DeadCache interface {
	Blocked(key string) bool
	Block(key string) error
	Prune() error
}

// CheckerSpec is the config-derived half of a Checker: everything a config
// reload can change. RunOnce reads it once per cycle, so a reload landing
// mid-cycle configures the next cycle instead of rewriting the running one.
type CheckerSpec struct {
	Sources       []config.SubscriptionSource
	Denied        filter.CountrySet
	Interval      time.Duration
	Rounds        int
	MaxFail       int
	MaxAvgMs      int
	SourceTimeout time.Duration
	Prober        Prober
	Filters       []NodeFilter
	// Trace turns on the per-node egress probe, from the annotate chain naming
	// the cloudflare provider. The probe still depends on the prober being able
	// to run it.
	Trace bool
}

// Checker periodically fetches sources through the preprocess pipeline, merges
// them, probes the nodes and publishes survivors to the holder. Everything a
// reload can change lives behind spec; the remaining fields are fixed for the
// process lifetime. That split is why a reload never restarts the worker: it
// swaps the spec and the in-flight cycle keeps running to publication.
type Checker struct {
	spec atomic.Pointer[CheckerSpec]
	// reload wakes Run when Reconfigure lands between cycles, so an Interval
	// change applies now instead of one full old period later. Capacity 1: a
	// poke sent during a cycle is drained on the next loop and is then a no-op.
	reload   chan struct{}
	filterer func() Filterer
	// store is the same Blocklist the apiFilters write through. The checker
	// holds it only to prune it once per cycle; it never writes to it.
	store  Blocklist
	dead   DeadCache
	holder *Holder
	// snapshotPath is where each publication is persisted so a restart serves
	// the last good list. Fixed for the process lifetime like the fields above
	// it: a reload swaps the spec, never this.
	snapshotPath string
	logger       zerolog.Logger
	reporter     Reporter
}

func NewChecker(
	spec CheckerSpec,
	filterer func() Filterer,
	store Blocklist,
	dead DeadCache,
	holder *Holder,
	snapshotPath string,
	logger zerolog.Logger,
	reporter Reporter,
) *Checker {
	c := &Checker{
		reload:       make(chan struct{}, 1),
		filterer:     filterer,
		store:        store,
		dead:         dead,
		holder:       holder,
		snapshotPath: snapshotPath,
		logger:       logger,
		reporter:     reporter,
	}
	c.spec.Store(&spec)

	return c
}

// Reconfigure swaps the settings the next cycle will use. A cycle already in
// flight keeps the spec it started with, so a reload never discards its work.
func (c *Checker) Reconfigure(spec CheckerSpec) {
	c.spec.Store(&spec)
	select {
	case c.reload <- struct{}{}:
	default: // a poke is already pending; Run will re-read the spec anyway
	}
}

// Run blocks: one cycle immediately, then one per interval, until ctx is done.
func (c *Checker) Run(ctx context.Context) {
	_ = c.RunOnce(ctx)

	interval := c.spec.Load().Interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.reload:
		case <-ticker.C:
			_ = c.RunOnce(ctx)
		}
		// Re-read after either wakeup: the reload may have arrived while a cycle
		// ran or while we sat idle between them.
		if next := c.spec.Load().Interval; next != interval {
			interval = next
			ticker.Reset(interval)
		}
	}
}

// RunOnce executes a single check cycle. The spec is read once, up front: a
// reload landing mid-cycle must not change the parameters this cycle is already
// running under. On any failure the previously published snapshot is kept
// untouched; a probe error (including context cancellation) is returned and
// aborts the cycle before any cache write.
func (c *Checker) RunOnce(ctx context.Context) error {
	start := time.Now()
	spec := c.spec.Load()
	// One processor for the whole cycle: the IP stage that judged an address
	// and the annotator that describes it must be the same one, even if a
	// reload swaps the snapshot mid-cycle.
	svc := c.filterer()
	bodies, sourceReports := c.fetchSources(ctx, spec, svc)

	entries := Merge(bodies)
	if len(entries) == 0 {
		c.logger.Warn().Msg("no entries merged; keeping previous stable list")
		c.reportError()
		return nil
	}

	probe, skipped, ok := c.filterDead(entries)
	if !ok {
		c.reportError()
		return nil
	}

	c.logger.Info().Int("nodes", len(probe)).Int("dead_skipped", skipped).
		Int("rounds", spec.Rounds).Msg("probing merged nodes")
	res, err := spec.Prober.Probe(ctx, entriesPayload(probe))
	if err != nil {
		c.logger.Warn().Err(err).Msg("probe failed; keeping previous stable list")
		c.reportError()
		return fmt.Errorf("probe: %w", err)
	}
	if err = ctx.Err(); err != nil {
		c.logger.Warn().Err(err).Msg("cycle cancelled after probe; keeping previous stable list")
		c.reportError()
		return fmt.Errorf("cycle cancelled after probe: %w", err)
	}

	c.recordDead(probe, res)

	survivors := SelectSurvivors(probe, res, spec.Rounds, spec.MaxFail, spec.MaxAvgMs)
	survivors, filterReports, trace := c.filterAndMeasureEgress(ctx, spec, survivors)
	c.pruneCaches()
	if err = ctx.Err(); err != nil {
		c.logger.Warn().Err(err).Msg("cycle cancelled during node filters; keeping previous stable list")
		c.reportError()
		return fmt.Errorf("cycle cancelled during node filters: %w", err)
	}
	if len(survivors) == 0 {
		c.logger.Warn().Msg("no survivors; keeping previous stable list")
		c.reportError()
		return nil
	}

	// Before observe, not merely before Store: this is what fills
	// Survivor.Country, which the gauges below then count.
	ann := svc.Annotator()
	payload := BuildPayload(ctx, ann, survivors)
	trace.Moved = movedCount(ctx, ann, survivors)

	snap := &Snapshot{
		Payload:   payload,
		UpdatedAt: time.Now(),
		Stats: Stats{
			SourcesOK:    len(bodies),
			SourcesTotal: len(spec.Sources),
			Merged:       len(entries),
			Tested:       len(probe),
			Kept:         len(survivors),
		},
	}
	c.holder.Store(snap)
	// After the in-memory publication and never gating it: a snapshot that
	// cannot be written costs the NEXT restart its head start, and nothing at
	// all in this cycle, so it is a warning rather than an error return.
	if saveErr := SaveSnapshot(c.snapshotPath, snap); saveErr != nil {
		c.logger.Warn().Err(saveErr).Str("path", c.snapshotPath).
			Msg("persisting the stable snapshot failed; the list is published in memory only")
	}
	c.logger.Info().
		Int("sources_ok", len(bodies)).
		Int("merged", len(entries)).
		Int("dead_skipped", skipped).
		Int("probed", len(probe)).
		Int("kept", len(survivors)).
		Msg("stable list updated")

	c.observe(CycleReport{
		SourcesOK:     len(bodies),
		SourcesTotal:  len(spec.Sources),
		Merged:        len(entries),
		DeadSkipped:   skipped,
		Probed:        len(probe),
		Kept:          len(survivors),
		GeoUnknown:    geoUnknownCount(survivors),
		KeptCountries: keptCountries(survivors),
		Duration:      time.Since(start),
		Sources:       sourceReports,
		Filters:       filterReports,
		KeptSpeeds:    keptSpeeds(survivors),
		Trace:         trace,
	})

	return nil
}

// reportError records a cycle that did not publish a new list (hard error or
// soft skip). observe records a published cycle. Both are no-ops without a
// Reporter.
func (c *Checker) reportError() {
	if c.reporter != nil {
		c.reporter.ObserveError()
	}
}

func (c *Checker) observe(r CycleReport) {
	if c.reporter != nil {
		c.reporter.Observe(r)
	}
}

// keptSpeeds collects the measured Mbps of kept nodes for the speed histogram.
// It is empty when no bandwidth filter ran (every Mbps is then zero).
func keptSpeeds(survivors []Survivor) []int {
	speeds := make([]int, 0, len(survivors))
	for _, s := range survivors {
		if s.Mbps > 0 {
			speeds = append(speeds, s.Mbps)
		}
	}
	return speeds
}

// geoUnknownCount counts published nodes whose annotation resolved no country
// (the [GEO:??] tag), for the coverage gauge.
func geoUnknownCount(survivors []Survivor) int {
	n := 0
	for _, s := range survivors {
		if s.Country == countryUnknown {
			n++
		}
	}
	return n
}

// keptCountries counts published nodes per resolved country ("" and "??" are
// excluded: covered by annotation-off and the geo-unknown gauge respectively).
func keptCountries(survivors []Survivor) map[string]int {
	m := make(map[string]int)
	for _, s := range survivors {
		if s.Country != "" && s.Country != countryUnknown {
			m[s.Country]++
		}
	}
	return m
}

// movedCount counts published nodes the trace actually MOVED: those whose
// final country differs from the one the offline chain gives for the RESOLVED
// address — the tag the node would have carried had it never answered. That
// offline verdict is never published and nothing remembers it, so the chain is
// re-run with an empty Egress, for the traced nodes alone; an untraced node's
// published tag already IS the offline answer.
func movedCount(ctx context.Context, ann preprocess.Annotator, survivors []Survivor) int {
	if ann == nil {
		return 0
	}
	r := renderer{ann: ann}
	moved := 0
	for i := range survivors {
		s := &survivors[i]
		if !s.Egress.Valid() {
			continue
		}
		if _, offline, ok := r.render(ctx, s.Raw, preprocess.AnnotateRequest{IP: s.Entry.IP}); ok &&
			countryString(offline) != s.Country {
			moved++
		}
	}

	return moved
}

// filterAndMeasureEgress runs the through-node NodeFilter chain over the
// survivors and then asks the ones still standing where their traffic actually
// leaves from. It parses the survivor set into proxies ONCE (regardless of
// filter count) and shares them across every filter and the trace, which select
// the subset for their current (narrowed) survivors by node label. The checker
// owns the proxies' lifecycle: they are closed exactly once at the end
// (deferred), and nothing else closes them. Probe parses and closes its own
// full set independently. Parsing is skipped entirely when there is nothing to
// run or no survivor; a parse failure logs and passes survivors through
// unchanged (matching the previous per-filter skip-on-no-proxies behavior).
//
// The trace runs LAST, on the final survivor set, because its answer only
// annotates: measuring a node a later gate drops would spend a request on a
// tag nobody sees.
func (c *Checker) filterAndMeasureEgress(
	ctx context.Context, spec *CheckerSpec, survivors []Survivor,
) ([]Survivor, []FilterReport, TraceReport) {
	tracer, canTrace := spec.Prober.(traceChecker)
	if spec.Trace && !canTrace {
		c.logger.Warn().Msg("cloudflare annotation requested but prober lacks trace support; skipping")
	}
	trace := spec.Trace && canTrace
	if (len(spec.Filters) == 0 && !trace) || len(survivors) == 0 {
		return survivors, nil, TraceReport{}
	}

	entries := make([]Entry, len(survivors))
	for i, s := range survivors {
		entries[i] = s.Entry
	}
	proxies, err := spec.Prober.ParseProxies(entriesPayload(entries))
	if err != nil {
		c.logger.Warn().Err(err).Msg("node filters: parsing survivors failed; skipping filters")
		return survivors, nil, TraceReport{}
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	// entryLabel, not px.Name(): a mierus:// survivor parses into one proxy per
	// configured port, and the filters below look this map up by Entry.Label.
	// Every port is kept, in mihomo's emission order, because collapsing them
	// here would hand the filters an arbitrary port: a node whose last port is
	// filtered on our egress would be measured unreachable and dropped even
	// though the probe selected it on a live one. The checks fold the
	// OUTCOMES instead (see betterAPIOutcome, betterBandwidthOutcome).
	byLabel := make(map[string][]mihomo.Proxy, len(proxies))
	for _, px := range proxies {
		label := entryLabel(px)
		byLabel[label] = append(byLabel[label], px)
	}
	reports := make([]FilterReport, 0, len(spec.Filters))
	for _, f := range spec.Filters {
		var rep FilterReport
		survivors, rep = f.apply(ctx, survivors, byLabel)
		reports = append(reports, rep)
	}
	if !trace {
		return survivors, reports, TraceReport{}
	}

	return survivors, reports, applyTrace(ctx, tracer, survivors, byLabel)
}

// applyTrace records what each survivor reported about its own egress. It
// drops nothing: a node whose trace fails keeps its place and is annotated
// from the offline chain instead.
func applyTrace(
	ctx context.Context, tracer traceChecker, survivors []Survivor, byLabel map[string][]mihomo.Proxy,
) TraceReport {
	traced := tracer.TraceCheck(ctx, filterSubset(survivors, byLabel))
	var rep TraceReport
	for i := range survivors {
		res, ok := traced[survivors[i].Label]
		if !ok {
			rep.Unanswered++

			continue
		}
		rep.Answered++
		survivors[i].Egress = preprocess.Egress{IP: res.IP, Country: res.Country}
	}

	return rep
}

// fetchSources fetches every configured source concurrently through the
// preprocess pipeline and returns the successfully-filtered node sets in
// configuration order, so Merge's first-source-wins dedupe is deterministic.
func (c *Checker) fetchSources(
	ctx context.Context, spec *CheckerSpec, svc Filterer,
) ([]SourceBody, []SourceReport) {
	type result struct {
		body  SourceBody
		stats preprocess.Stats
		err   error
	}

	const fetchConcurrency = 16
	sem := make(chan struct{}, fetchConcurrency)
	results := make([]result, len(spec.Sources))

	var wg sync.WaitGroup
	for i, src := range spec.Sources {
		sem <- struct{}{} // bound goroutine creation, not just execution
		wg.Add(1)
		go func(i int, src config.SubscriptionSource) {
			defer wg.Done()
			defer func() { <-sem }()

			c.logger.Debug().Str("source", src.Name).Msg("fetching source")
			sourceCtx, cancel := context.WithTimeout(ctx, spec.SourceTimeout)
			defer cancel()

			// The worker imposes no allow-list: the configured country filters
			// only ever exclude, and an exclusion must not drop a node whose IP
			// no geo source covers.
			req := preprocess.FilterRequest{
				SubscriptionURL:  fetch.SubscriptionURL(src.URL),
				AllowedCountries: filter.All(),
				DeniedCountries:  spec.Denied,
			}
			if src.Body != "" {
				// Inline source: filter the pasted payload directly, no fetch.
				req.SubscriptionURL = ""
				req.Body = []byte(src.Body)
			}
			nodes, stats, err := svc.FilterNodes(sourceCtx, req)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			results[i] = result{
				body:  SourceBody{Name: src.Name, Nodes: nodes},
				stats: stats,
			}
		}(i, src)
	}
	wg.Wait()

	bodies := make([]SourceBody, 0, len(spec.Sources))
	reports := make([]SourceReport, 0, len(spec.Sources))
	for i, r := range results {
		if r.err != nil {
			c.logger.Warn().Str("source", spec.Sources[i].Name).Err(r.err).Msg("source fetch failed")
			continue
		}
		bodies = append(bodies, r.body)
		reports = append(reports, SourceReport{
			Name:         spec.Sources[i].Name,
			Total:        r.stats.Total,
			Kept:         r.stats.Kept,
			DNSDrop:      r.stats.DNSDrop,
			GeoDrop:      r.stats.GeoDrop,
			ASNDrop:      r.stats.ASNDrop,
			GeoBlockDrop: r.stats.GeoBlockDrop,
			IPv6Drop:     r.stats.IPv6Drop,
			Unsupported:  r.stats.Unsupported,
		})
	}
	return bodies, reports
}

// filterDead drops the nodes the dead cache saw fail a recent probe, so the
// probe only re-tests live/unknown ones. It returns the nodes to probe, how
// many it skipped, and false when nothing remains to probe (caller keeps the
// previous list).
//
// A through-node filter's verdict is deliberately absent here: the checks whose
// verdict is worth reusing write the geoblock store instead, and preprocess
// drops those hosts a whole stage earlier, before the node is ever merged.
func (c *Checker) filterDead(entries []Entry) (probe []Entry, deadSkipped int, ok bool) {
	probe = make([]Entry, 0, len(entries))
	for _, e := range entries {
		if c.dead != nil && c.dead.Blocked(e.Addr) {
			deadSkipped++
			continue
		}
		probe = append(probe, e)
	}
	if len(probe) == 0 {
		c.logger.Warn().Int("dead_skipped", deadSkipped).
			Msg("every merged node was recently ruled out; keeping previous stable list")
		return nil, deadSkipped, false
	}
	return probe, deadSkipped, true
}

// recordDead caches nodes that returned no successful probe so later cycles
// skip them.
func (c *Checker) recordDead(probe []Entry, res map[string]ProbeResult) {
	if c.dead == nil {
		return
	}
	for _, e := range probe {
		if _, ok := res[e.Label]; !ok {
			_ = c.dead.Block(e.Addr)
		}
	}
}

// pruneCaches sheds expired entries from every TTL cache this cycle wrote to.
// One call site on purpose: the geoblock store spent its life pruned only at
// process start precisely because its cleanup lived nowhere in the cycle.
func (c *Checker) pruneCaches() {
	if c.dead != nil {
		_ = c.dead.Prune()
	}
	if c.store != nil {
		_ = c.store.Prune()
	}
}

func entriesPayload(entries []Entry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		b.WriteString(e.Raw)
		b.WriteByte('\n')
	}

	return b.Bytes()
}
