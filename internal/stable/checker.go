package stable

import (
	"bytes"
	"context"
	"time"

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
	}
}

// Run blocks: one cycle immediately, then one per interval, until ctx is done.
func (c *Checker) Run(ctx context.Context) {
	c.RunOnce(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.RunOnce(ctx)
		}
	}
}

// RunOnce executes a single check cycle. On any failure the previously
// published snapshot is kept untouched.
func (c *Checker) RunOnce(ctx context.Context) {
	bodies := c.fetchSources(ctx)

	entries := Merge(bodies)
	if len(entries) == 0 {
		c.logger.Warn().Msg("no entries merged; keeping previous stable list")
		return
	}

	probe, deadSkipped, ok := c.filterDead(entries)
	if !ok {
		return
	}

	c.logger.Info().Int("nodes", len(probe)).Int("dead_skipped", deadSkipped).
		Int("rounds", c.rounds).Msg("probing merged nodes")
	res, err := c.prober.Probe(ctx, entriesPayload(probe))
	if err != nil {
		c.logger.Warn().Err(err).Msg("probe failed; keeping previous stable list")
		return
	}

	c.recordDead(probe, res)

	survivors := SelectSurvivors(probe, res, c.rounds, c.maxFail, c.maxAvgMs)
	for _, f := range c.filters {
		survivors = f.apply(ctx, probe, survivors)
	}
	if len(survivors) == 0 {
		c.logger.Warn().Msg("no survivors; keeping previous stable list")
		return
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
}

// fetchSources fetches every configured source concurrently through the
// preprocess pipeline and returns the successfully-filtered bodies.
func (c *Checker) fetchSources(ctx context.Context) []SourceBody {
	svc := c.filterer()

	type result struct {
		name string
		body SourceBody
		err  error
	}

	const fetchConcurrency = 16
	sem := make(chan struct{}, fetchConcurrency)
	results := make(chan result, len(c.sources))

	for _, src := range c.sources {
		go func(src config.SubscriptionSource) {
			sem <- struct{}{}
			defer func() { <-sem }()

			c.logger.Debug().Str("source", src.Name).Msg("fetching source")
			sourceCtx, cancel := context.WithTimeout(ctx, c.sourceTimeout)
			defer cancel()

			var buf bytes.Buffer
			buf.Grow(sourceBufSize)
			_, err := svc.Filter(sourceCtx, &buf, preprocess.FilterRequest{
				SubscriptionURL:  fetch.SubscriptionURL(src.URL),
				AllowedCountries: c.allowed,
			})
			if err != nil {
				results <- result{name: src.Name, err: err}
				return
			}
			results <- result{name: src.Name, body: SourceBody{Name: src.Name, Body: append([]byte(nil), buf.Bytes()...)}}
		}(src)
	}

	bodies := make([]SourceBody, 0, len(c.sources))
	for range c.sources {
		r := <-results
		if r.err != nil {
			c.logger.Warn().Str("source", r.name).Err(r.err).Msg("source fetch failed")
			continue
		}
		bodies = append(bodies, r.body)
	}
	return bodies
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
