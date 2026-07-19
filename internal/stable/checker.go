package stable

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/preprocess"
)

const sourceBufSize = 4096

// Filterer matches server.Filterer; declared locally to avoid an import cycle.
type Filterer interface {
	Filter(ctx context.Context, b *bytes.Buffer, req preprocess.FilterRequest) (preprocess.Stats, error)
}

// Blocklist records nodes that failed the Gemini check; declared locally to
// avoid importing geoblock. A nil Blocklist disables persistence.
type Blocklist interface {
	Block(host string) error
}

// DeadCache skips re-probing recently-dead nodes; a nil DeadCache disables it.
type DeadCache interface {
	Blocked(key string) bool
	Block(key string) error
	Prune() error
}

// Checker periodically fetches sources through the preprocess pipeline,
// merges them, probes the nodes and publishes survivors to the holder.
type Checker struct {
	sources       []config.SubscriptionSource
	allowed       filter.CountrySet
	interval      time.Duration
	rounds        int
	maxFail       int
	maxAvgMs      int
	sourceTimeout time.Duration
	filterer      func() Filterer
	prober        Prober
	filters       []NodeFilter
	dead          DeadCache
	holder        *Holder
	logger        zerolog.Logger
	reporter      Reporter
}

func NewChecker(
	sources []config.SubscriptionSource,
	allowed filter.CountrySet,
	interval time.Duration,
	rounds, maxFail, maxAvgMs int,
	sourceTimeout time.Duration,
	filterer func() Filterer,
	prober Prober,
	filters []NodeFilter,
	dead DeadCache,
	holder *Holder,
	logger zerolog.Logger,
	reporter Reporter,
) *Checker {
	return &Checker{
		sources:       sources,
		allowed:       allowed,
		interval:      interval,
		rounds:        rounds,
		maxFail:       maxFail,
		maxAvgMs:      maxAvgMs,
		sourceTimeout: sourceTimeout,
		filterer:      filterer,
		prober:        prober,
		filters:       filters,
		dead:          dead,
		holder:        holder,
		logger:        logger,
		reporter:      reporter,
	}
}

// Run blocks: one cycle immediately, then one per interval, until ctx is done.
func (c *Checker) Run(ctx context.Context) {
	_ = c.RunOnce(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.RunOnce(ctx)
		}
	}
}

// RunOnce executes a single check cycle. On any failure the previously
// published snapshot is kept untouched; a probe error (including context
// cancellation) is returned and aborts the cycle before any cache write.
func (c *Checker) RunOnce(ctx context.Context) error {
	start := time.Now()
	bodies, sourceReports := c.fetchSources(ctx)

	entries := Merge(bodies)
	if len(entries) == 0 {
		c.logger.Warn().Msg("no entries merged; keeping previous stable list")
		c.reportError()
		return nil
	}

	probe, deadSkipped, ok := c.filterDead(entries)
	if !ok {
		c.reportError()
		return nil
	}

	c.logger.Info().Int("nodes", len(probe)).Int("dead_skipped", deadSkipped).
		Int("rounds", c.rounds).Msg("probing merged nodes")
	res, err := c.prober.Probe(ctx, entriesPayload(probe))
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

	survivors := SelectSurvivors(probe, res, c.rounds, c.maxFail, c.maxAvgMs)
	survivors, filterReports := c.applyFilters(ctx, survivors)
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

	c.holder.Store(&Snapshot{
		Payload:   BuildPayload(survivors),
		UpdatedAt: time.Now(),
		Stats: Stats{
			SourcesOK:    len(bodies),
			SourcesTotal: len(c.sources),
			Merged:       len(entries),
			Tested:       len(probe),
			Kept:         len(survivors),
		},
	})
	c.logger.Info().
		Int("sources_ok", len(bodies)).
		Int("merged", len(entries)).
		Int("dead_skipped", deadSkipped).
		Int("probed", len(probe)).
		Int("kept", len(survivors)).
		Msg("stable list updated")

	c.observe(CycleReport{
		SourcesOK:     len(bodies),
		SourcesTotal:  len(c.sources),
		Merged:        len(entries),
		DeadSkipped:   deadSkipped,
		Probed:        len(probe),
		Kept:          len(survivors),
		GeoUnknown:    geoUnknownCount(survivors),
		KeptCountries: keptCountries(survivors),
		Duration:      time.Since(start),
		Sources:       sourceReports,
		Filters:       filterReports,
		KeptSpeeds:    keptSpeeds(survivors),
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
		if s.Country == "??" {
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
		if s.Country != "" && s.Country != "??" {
			m[s.Country]++
		}
	}
	return m
}

// applyFilters runs the through-node NodeFilter chain over the survivors. It
// parses the survivor set into proxies ONCE (regardless of filter count) and
// shares them across every filter, which selects the subset for its current
// (narrowed) survivors by node label. The checker owns the proxies' lifecycle:
// they are closed exactly once after the chain (deferred), and no filter closes
// them. Probe parses and closes its own full set independently. Parsing is
// skipped entirely when there is no filter or no survivor; a parse failure logs
// and passes survivors through unchanged (matching the previous per-filter
// skip-on-no-proxies behavior).
func (c *Checker) applyFilters(ctx context.Context, survivors []Survivor) ([]Survivor, []FilterReport) {
	if len(c.filters) == 0 || len(survivors) == 0 {
		return survivors, nil
	}

	entries := make([]Entry, len(survivors))
	for i, s := range survivors {
		entries[i] = s.Entry
	}
	proxies, err := c.prober.ParseProxies(entriesPayload(entries))
	if err != nil {
		c.logger.Warn().Err(err).Msg("node filters: parsing survivors failed; skipping filters")
		return survivors, nil
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	byLabel := make(map[string]mihomo.Proxy, len(proxies))
	for _, px := range proxies {
		byLabel[px.Name()] = px
	}
	reports := make([]FilterReport, 0, len(c.filters))
	for _, f := range c.filters {
		var rep FilterReport
		survivors, rep = f.apply(ctx, survivors, byLabel)
		reports = append(reports, rep)
	}

	return survivors, reports
}

// fetchSources fetches every configured source concurrently through the
// preprocess pipeline and returns the successfully-filtered bodies in
// configuration order, so Merge's first-source-wins dedupe is deterministic.
func (c *Checker) fetchSources(ctx context.Context) ([]SourceBody, []SourceReport) {
	svc := c.filterer()

	type result struct {
		body  SourceBody
		stats preprocess.Stats
		err   error
	}

	const fetchConcurrency = 16
	sem := make(chan struct{}, fetchConcurrency)
	results := make([]result, len(c.sources))

	var wg sync.WaitGroup
	for i, src := range c.sources {
		sem <- struct{}{} // bound goroutine creation, not just execution
		wg.Add(1)
		go func(i int, src config.SubscriptionSource) {
			defer wg.Done()
			defer func() { <-sem }()

			c.logger.Debug().Str("source", src.Name).Msg("fetching source")
			sourceCtx, cancel := context.WithTimeout(ctx, c.sourceTimeout)
			defer cancel()

			var buf bytes.Buffer
			buf.Grow(sourceBufSize)
			req := preprocess.FilterRequest{
				SubscriptionURL:  fetch.SubscriptionURL(src.URL),
				AllowedCountries: c.allowed,
			}
			if src.Body != "" {
				// Inline source: filter the pasted payload directly, no fetch.
				req = preprocess.FilterRequest{Body: []byte(src.Body), AllowedCountries: c.allowed}
			}
			stats, err := svc.Filter(sourceCtx, &buf, req)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			results[i] = result{
				body:  SourceBody{Name: src.Name, Body: append([]byte(nil), buf.Bytes()...)},
				stats: stats,
			}
		}(i, src)
	}
	wg.Wait()

	bodies := make([]SourceBody, 0, len(c.sources))
	reports := make([]SourceReport, 0, len(c.sources))
	for i, r := range results {
		if r.err != nil {
			c.logger.Warn().Str("source", c.sources[i].Name).Err(r.err).Msg("source fetch failed")
			continue
		}
		bodies = append(bodies, r.body)
		reports = append(reports, SourceReport{
			Name:         c.sources[i].Name,
			Total:        r.stats.Total,
			Kept:         r.stats.Kept,
			DNSDrop:      r.stats.DNSDrop,
			GeoDrop:      r.stats.GeoDrop,
			ASNDrop:      r.stats.ASNDrop,
			GeoBlockDrop: r.stats.GeoBlockDrop,
			Unsupported:  r.stats.Unsupported,
		})
	}
	return bodies, reports
}

// filterDead drops nodes a recent cycle marked dead so the probe only re-tests
// live/unknown nodes. It returns the nodes to probe, how many were skipped, and
// false when nothing remains to probe (caller keeps the previous list).
func (c *Checker) filterDead(entries []Entry) (probe []Entry, deadSkipped int, ok bool) {
	if c.dead == nil {
		return entries, 0, true
	}
	probe = make([]Entry, 0, len(entries))
	for _, e := range entries {
		if c.dead.Blocked(e.Addr) {
			deadSkipped++
			continue
		}
		probe = append(probe, e)
	}
	if len(probe) == 0 {
		c.logger.Warn().Int("dead_skipped", deadSkipped).Msg("all merged nodes recently dead; keeping previous stable list")
		return nil, deadSkipped, false
	}
	return probe, deadSkipped, true
}

// recordDead caches nodes that returned no successful probe so later cycles
// skip them, then prunes expired entries.
func (c *Checker) recordDead(probe []Entry, res map[string]ProbeResult) {
	if c.dead == nil {
		return
	}
	for _, e := range probe {
		if _, ok := res[e.Label]; !ok {
			_ = c.dead.Block(e.Addr)
		}
	}
	_ = c.dead.Prune()
}

func entriesPayload(entries []Entry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		b.WriteString(e.Raw)
		b.WriteByte('\n')
	}

	return b.Bytes()
}
