package stable

import (
	"context"
	"fmt"
	"net/netip"
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

// probedAdapterSource is the optional half of a Prober that keeps the adapter
// objects its last successful Probe built alive past the call, so the egress
// stage can consume them by label instead of converting and re-parsing the
// survivors' byte-identical raw. Asserted like traceChecker, so the base
// Prober contract is unchanged and a Prober without the capability falls back
// to ParseProxies in filterAndMeasureEgress.
//
// Taking transfers ownership: the taker MUST Close each proxy exactly once. A
// Prober that retained adapters and then errored or was cancelled closes them
// itself before returning, so a nil take means nothing is owed.
type probedAdapterSource interface {
	TakeProbedAdapters() []mihomo.Proxy
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
//
// The block is keyed on the endpoint's server:port AND the address the IP
// stage resolved for it that cycle: a hostname re-pointed to a different
// address is a different server for the cache's purposes, and skipping it
// unprobed would keep a healthy node out of the list for the whole jittered
// [3h, 4.5h) TTL (see LogicCycle#2).
//
// Neither method may retain addr. The checker passes Entry.Addr, a view into
// Merge's key arena; the bytes stay valid, but a retained view pins its whole
// 1 KiB block (see keyArena) for the jittered [3h, 4.5h) a DeadCache holds an
// entry, so an implementation that remembers the key MUST remember a copy.
// Asking the caller to allocate instead cost one string per merged node —
// 36342 of them per cycle at the 2026-08-15 production shape — to spare the
// far smaller set that actually probes dead. ip is a value and needs no copy.
type DeadCache interface {
	Blocked(addr string, ip netip.Addr) bool
	Block(addr string, ip netip.Addr) error
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
	fetchedAt := time.Now()

	entries := Merge(bodies)
	mergedAt := time.Now()
	if len(entries) == 0 {
		c.logger.Warn().Msg("no entries merged; keeping previous stable list")
		c.reportError()
		return nil
	}

	probe, skipped, ok := c.filterDead(entries)
	deadFilteredAt := time.Now()
	if !ok {
		c.reportError()
		return nil
	}

	c.logger.Info().Int("nodes", len(probe)).Int("dead_skipped", skipped).
		Int("rounds", spec.Rounds).Msg("probing merged nodes")
	res, err := spec.Prober.Probe(ctx, entriesPayload(probe))
	probedAt := time.Now()
	if err != nil {
		c.logger.Warn().Err(err).Msg("probe failed; keeping previous stable list")
		c.reportError()
		return fmt.Errorf("probe: %w", err)
	}
	// A capable prober kept its adapters open past Probe for the egress stage
	// below. Take them before any further exit; the deferred close pays
	// exactly once whatever path the rest of the cycle takes — cancellation,
	// zero survivors, an egress stage that never runs, or a normal
	// publication.
	probed := takeProbedAdapters(spec.Prober)
	defer closeProxies(probed)
	if err = ctx.Err(); err != nil {
		c.logger.Warn().Err(err).Msg("cycle cancelled after probe; keeping previous stable list")
		c.reportError()
		return fmt.Errorf("cycle cancelled after probe: %w", err)
	}

	// Read while it still describes THIS probe: PrecheckAbsent afterwards would
	// be indistinguishable from a prober that runs no pre-check.
	precheck := precheckReportOf(spec.Prober)

	c.recordDead(probe, res)

	survivors := SelectSurvivors(probe, res, spec.Rounds, spec.MaxFail, spec.MaxAvgMs)
	selectedAt := time.Now()
	survivors, filterReports, trace, gemini := c.filterAndMeasureEgress(ctx, spec, survivors, sourceReports, probed)
	filteredAt := time.Now()
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

	publishingAt, publishedAt, err := c.annotateAndPublish(ctx, svc, spec, survivors, len(bodies), len(entries), len(probe), skipped, &trace)
	if err != nil {
		return err
	}

	c.observe(CycleReport{
		SourcesOK:     len(bodies),
		SourcesTotal:  len(spec.Sources),
		Merged:        len(entries),
		DeadSkipped:   skipped,
		Probed:        len(probe),
		Kept:          len(survivors),
		ProbeStages:   probeStages(probe, res),
		Precheck:      precheck,
		GeoUnknown:    geoUnknownCount(survivors),
		KeptCountries: keptCountries(survivors),
		Duration:      time.Since(start),
		Phases: CyclePhases{
			Fetch:      fetchedAt.Sub(start),
			Merge:      mergedAt.Sub(fetchedAt),
			DeadFilter: deadFilteredAt.Sub(mergedAt),
			Probe:      probedAt.Sub(deadFilteredAt),
			Egress:     filteredAt.Sub(selectedAt),
			Publish:    publishedAt.Sub(publishingAt),
		},
		Sources:         sourceReports,
		Filters:         filterReports,
		KeptSpeeds:      keptSpeeds(survivors),
		KeptLatenciesMs: keptLatencies(survivors),
		Trace:           trace,
		Gemini:          gemini,
	})

	return nil
}

// annotateAndPublish is the publish tail of RunOnce, split out as one step so
// the cancellation verdict cannot separate from the write it guards: the list
// is only swapped and persisted after a ctx gate that resolved the same chain
// BuildPayload just ran, so a cycle cancelled mid-render never commits its
// degraded payload. It returns the window it occupied; observe folds that into
// the Publish phase, having run on the other side of the return.
func (c *Checker) annotateAndPublish(
	ctx context.Context, svc Filterer, spec *CheckerSpec,
	survivors []Survivor, sourcesOK, merged, tested, skipped int, trace *TraceReport,
) (time.Time, time.Time, error) {
	publishingAt := time.Now()
	// Before observe, not merely before Store: this is what fills
	// Survivor.Country, which the gauges below then count.
	ann := svc.Annotator()
	payload := BuildPayload(ctx, ann, survivors)
	// A cancellation that lands inside BuildPayload must not commit its output:
	// a chain member that honours ctx resolves nothing, so the payload would
	// publish with every tag degraded. movedCount would burn the same cancelled
	// chain, so it is skipped too.
	if err := ctx.Err(); err != nil {
		c.logger.Warn().Err(err).Msg("cycle cancelled during publish; keeping previous stable list")
		c.reportError()
		return publishingAt, time.Time{}, fmt.Errorf("cycle cancelled during publish: %w", err)
	}
	trace.Moved = movedCount(ctx, ann, survivors)

	// Only ever len()-ed from here down, which needs the length word alone:
	// the three pools are already collected, and a full-slice read added here
	// pins them across the probe (checker_retention_test.go).
	c.publish(payload, Stats{
		SourcesOK:    sourcesOK,
		SourcesTotal: len(spec.Sources),
		Merged:       merged,
		Tested:       tested,
		Kept:         len(survivors),
	}, skipped)
	return publishingAt, time.Now(), nil
}

// publish swaps the new list in, persists it and logs the cycle. The save runs
// AFTER the in-memory publication and never gates it: a snapshot that cannot be
// written costs the NEXT restart its head start and nothing at all in this
// cycle, so it is a warning rather than an error.
func (c *Checker) publish(payload []byte, stats Stats, deadSkipped int) {
	snap := &Snapshot{Payload: payload, UpdatedAt: time.Now(), Stats: stats}
	c.holder.Store(snap)
	if saveErr := SaveSnapshot(c.snapshotPath, snap); saveErr != nil {
		c.logger.Warn().Err(saveErr).Str("path", c.snapshotPath).
			Msg("persisting the stable snapshot failed; the list is published in memory only")
	}
	c.logger.Info().
		Int("sources_ok", stats.SourcesOK).
		Int("merged", stats.Merged).
		Int("dead_skipped", deadSkipped).
		Int("probed", stats.Tested).
		Int("kept", stats.Kept).
		Msg("stable list updated")
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

// keptLatencies collects the mean probe delay of kept nodes for the latency
// histogram: the value SelectSurvivors filtered and sorted on, which until now
// reached no log and no metric.
func keptLatencies(survivors []Survivor) []int {
	latencies := make([]int, 0, len(survivors))
	for _, s := range survivors {
		latencies = append(latencies, s.MeanMs)
	}
	return latencies
}

// precheckReporter is the optional half of a Prober that runs the TCP
// reachability pre-check, asserted like the traceChecker capability. A Prober
// without one reports PrecheckAbsent, which renders as no series rather than as
// a pre-check that condemned nobody.
type precheckReporter interface {
	PrecheckReport() PrecheckReport
}

func precheckReportOf(p Prober) PrecheckReport {
	reporter, ok := p.(precheckReporter)
	if !ok {
		return PrecheckReport{}
	}

	return reporter.PrecheckReport()
}

// probeStages counts the probed set by how far each probe got. It walks the
// probed entries rather than the result map so the counts sum to len(probe): a
// label the prober never named indexes to StageUnknown instead of vanishing,
// which keeps the ratio against stable_probed_nodes closed.
func probeStages(probe []Entry, res map[string]ProbeResult) map[ProbeStage]int {
	stages := make(map[ProbeStage]int)
	for _, e := range probe {
		stages[res[e.Label].Stage]++
	}

	return stages
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

// takeProbedAdapters takes the adapters a capable prober retained for the
// egress stage (see probedAdapterSource), or returns nil for a prober without
// the capability.
func takeProbedAdapters(prober Prober) []mihomo.Proxy {
	src, capable := prober.(probedAdapterSource)
	if !capable {
		return nil
	}
	return src.TakeProbedAdapters()
}

// closeProxies closes each proxy exactly once; a nil or empty slice closes
// nothing.
func closeProxies(proxies []mihomo.Proxy) {
	for _, px := range proxies {
		_ = px.Close()
	}
}

// parseEgressProxies parses the survivor set for a prober without the
// retention capability. The caller owns the returned proxies' close.
func parseEgressProxies(spec *CheckerSpec, kept []Survivor) ([]mihomo.Proxy, error) {
	entries := make([]Entry, len(kept))
	for i, s := range kept {
		entries[i] = s.Entry
	}
	proxies, err := spec.Prober.ParseProxies(entriesPayload(entries))
	if err != nil {
		return nil, fmt.Errorf("parse egress proxies: %w", err)
	}
	return proxies, nil
}

// proxiesByLabel groups proxies by the label their Entry folds onto — entryLabel,
// not px.Name(): a mierus:// survivor parses into one proxy per configured port,
// and the filters look this map up by Entry.Label. Every port is kept, in
// mihomo's emission order, because collapsing them here would hand the filters
// an arbitrary port: a node whose last port is filtered on our egress would be
// measured unreachable and dropped even though the probe selected it on a live
// one. The checks fold the OUTCOMES instead (see betterAPIOutcome,
// betterBandwidthOutcome).
//
// Only labels the survivor set carries are kept — the filters and the trace
// never look up any other — which matters because the retained path hands over
// adapters for the WHOLE live probe set, and an entry per dropped label would
// cost this stage a slice allocation per probed-but-dropped node.
func proxiesByLabel(survivors []Survivor, proxies []mihomo.Proxy) map[string][]mihomo.Proxy {
	wanted := make(map[string]struct{}, len(survivors))
	for _, s := range survivors {
		wanted[s.Label] = struct{}{}
	}
	byLabel := make(map[string][]mihomo.Proxy, len(survivors))
	for _, px := range proxies {
		label := entryLabel(px)
		if _, ok := wanted[label]; !ok {
			continue
		}
		byLabel[label] = append(byLabel[label], px)
	}
	return byLabel
}

// filterAndMeasureEgress runs the through-node NodeFilter chain over the
// survivors and then asks the ones still standing where their traffic actually
// leaves from. The proxies are shared across every filter and the trace, which
// select the subset for their current (narrowed) survivors by node label; each
// is built exactly once and closed exactly once. A prober with the
// probedAdapterSource capability hands its adapters over via probed, which the
// probe built for the WHOLE live set; RunOnce took them and owns their close,
// so nothing is closed here. A prober without the capability is parsed here —
// the survivor set alone — and THIS scope owns that parse's close, deferred so
// every exit accounts for itself. Parsing is skipped entirely when there is
// nothing to run or no survivor; a parse failure logs and passes survivors
// through unchanged (matching the previous per-filter skip-on-no-proxies
// behavior).
//
// The trace runs LAST, on the final survivor set, because its answer only
// annotates: measuring a node a later gate drops would spend a request on a
// tag nobody sees.
//
// The fourth return is the gemini gate's account of itself, empty unless that
// gate is in the chain. It rides beside the FilterReports rather than inside
// one because the nodes it counts are KEPT, and a FilterReport's only
// per-reason field is Dropped.
//
// The per-source stage counts are folded in here rather than in RunOnce because
// this is the only scope that holds both sides of the narrowing at once: tested
// is what the probe passed over, kept is what the chain left. Deferred so every
// exit accounts for itself: the early return -- nothing to filter or no
// survivor -- the ParseProxies failure, the normal exits, and a panic
// unwinding through. A cancelled check is not an exit here at all: the filters
// test ctx.Err() inside apply and hand their survivors back to the loop below.
func (c *Checker) filterAndMeasureEgress(
	ctx context.Context, spec *CheckerSpec, tested []Survivor, sources []SourceReport, probed []mihomo.Proxy,
) (kept []Survivor, reports []FilterReport, tr TraceReport, gemini GeminiReport) {
	defer func() { c.countSourceStages(sources, tested, kept) }()
	kept = tested
	tracer, canTrace := spec.Prober.(traceChecker)
	if spec.Trace && !canTrace {
		c.logger.Warn().Msg("cloudflare annotation requested but prober lacks trace support; skipping")
	}
	trace := spec.Trace && canTrace
	if (len(spec.Filters) == 0 && !trace) || len(kept) == 0 {
		return kept, nil, TraceReport{}, GeminiReport{}
	}

	proxies := probed
	if proxies == nil {
		// A prober without the retention capability: parse the survivor set
		// here and own those proxies, exactly as before the capability existed.
		var err error
		proxies, err = parseEgressProxies(spec, kept)
		if err != nil {
			c.logger.Warn().Err(err).Msg("node filters: parsing survivors failed; skipping filters")
			return kept, nil, TraceReport{}, GeminiReport{}
		}
		defer closeProxies(proxies)
	}

	byLabel := proxiesByLabel(kept, proxies)
	reports = make([]FilterReport, 0, len(spec.Filters))
	for _, f := range spec.Filters {
		var rep FilterReport
		kept, rep = f.apply(ctx, kept, byLabel)
		reports = append(reports, rep)
		// Optional capability, read exactly like spec.Prober.(traceChecker)
		// above: only the gemini gate can answer before its own verdict
		// exists, so only it accounts for what it could not verify.
		if gf, ok := f.(*geminiFilter); ok {
			gemini = gf.verification()
		}
	}
	if !trace {
		return kept, reports, TraceReport{}, gemini
	}

	return kept, reports, applyTrace(ctx, tracer, kept, byLabel), gemini
}

// countSourceStages fills the two post-merge per-source counts. One index
// serves both passes and sourceOfLabel returns a substring, so the only
// allocation is that one map -- 13.6kB at 349 sources, flat in the node count,
// zero per node. The CPU is not free at ~11ns a lookup, but tested is the
// PROBE's survivor set, not the merged pool: measured 2026-08-15 it held 376
// nodes here and 853 on the second instance (retired 2026-08-26), so the two
// passes cost under 20us once per cycle. The map is sized by the source count,
// which is why it dwarfs them.
//
// A label attributing to no configured source has nowhere to land, so the
// shortfall is logged rather than dropped in silence: the per-source columns
// would otherwise stop summing to the cycle's totals with every gauge still
// looking plausible. Only the tested pass reports it, the filtered set being a
// subset of it.
func (c *Checker) countSourceStages(reports []SourceReport, tested, filtered []Survivor) {
	byName := make(map[string]int, len(reports))
	for i := range reports {
		byName[reports[i].Name] = i
	}
	unattributed := 0
	for _, s := range tested {
		i, ok := byName[sourceOfLabel(s.Label)]
		if !ok {
			unattributed++

			continue
		}
		reports[i].Tested++
	}
	for _, s := range filtered {
		if i, ok := byName[sourceOfLabel(s.Label)]; ok {
			reports[i].Filtered++
		}
	}
	if unattributed > 0 {
		c.logger.Warn().Int("nodes", unattributed).
			Msg("survivors attributed to no configured source; per-source stage counts understate")
	}
}

// applyTrace records what each survivor reported about its own egress. It
// drops nothing: a node whose trace fails keeps its place and is annotated
// from the offline chain instead.
//
// Only this function marks the report TraceRan: reaching it means the trace
// stage actually issued its checks, which every other exit of
// filterAndMeasureEgress leaves at the zero value.
func applyTrace(
	ctx context.Context, tracer traceChecker, survivors []Survivor, byLabel map[string][]mihomo.Proxy,
) TraceReport {
	traced := tracer.TraceCheck(ctx, filterSubset(survivors, byLabel))
	rep := TraceReport{State: TraceRan}
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
		wg.Go(func() {
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
				HWID:             src.HWID,
			}
			if src.Body != "" {
				// Inline source: filter the pasted payload directly, no fetch.
				req.SubscriptionURL = ""
				req.HWID = ""
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
		})
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
			Managed:      spec.Sources[i].Managed,
			Feed:         spec.Sources[i].Feed,
			Total:        r.stats.Total,
			Valid:        r.stats.Kept,
			DNSDrop:      r.stats.DNSDrop,
			GeoDrop:      r.stats.GeoDrop,
			CIDRDrop:     r.stats.CIDRDrop,
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
//
// The cap is len(entries), not the kept count: at production shape (36342
// merged, 9503 kept) an exact fit would save 2.4MB of live heap and cost a
// second Blocked pass over the whole pool, measured at +1.7ms.
func (c *Checker) filterDead(entries []Entry) (probe []Entry, deadSkipped int, ok bool) {
	probe = make([]Entry, 0, len(entries))
	for _, e := range entries {
		if c.dead != nil && c.dead.Blocked(e.Addr, e.IP) {
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
//
// The write carries the same plausibility breaker as the pre-check
// (breakerTrips): when nearly every probed node failed, the fault is likelier
// our egress than the pool, and committing that verdict would freeze the
// published list for deadcache.ttl after the network recovers. The verdict
// fails open instead, exactly as filterReachable's does.
func (c *Checker) recordDead(probe []Entry, res map[string]ProbeResult) {
	if c.dead == nil {
		return
	}
	blocked := 0
	for _, e := range probe {
		// Zero successes, not absence: the prober emits an entry per label and
		// a label reads zero only when every port failed (foldProbeResults
		// folds best-of-ports), so an absence test would silently stop
		// blocking anything the moment the map is fully populated.
		if r, ok := res[e.Label]; !ok || r.Successes == 0 {
			blocked++
		}
	}
	if breakerTrips(blocked, len(probe)) {
		c.logger.Warn().Int("blocked", blocked).Int("probed", len(probe)).
			Int("threshold_pct", precheckBreakerPercent).
			Msg("nearly every probed node failed; treating the verdict as unreliable and keeping the dead cache unchanged")
		return
	}
	for _, e := range probe {
		if r, ok := res[e.Label]; !ok || r.Successes == 0 {
			_ = c.dead.Block(e.Addr, e.IP)
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

// entriesPayload sizes the probe payload in one pass, as BuildPayload does: a
// bytes.Buffer grows to the next power of two, so at production shape it copied
// 2MB through 13 dead intermediates and handed the probe 46KB of slack to hold
// for the whole run.
func entriesPayload(entries []Entry) []byte {
	total := 0
	for i := range entries {
		total += len(entries[i].Raw) + 1
	}
	out := make([]byte, 0, total)
	for i := range entries {
		out = append(out, entries[i].Raw...)
		out = append(out, '\n')
	}

	return out
}
