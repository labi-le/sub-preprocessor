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
	store         Blocklist
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
	store Blocklist,
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
		store:         store,
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

	entries := Merge(bodies)
	if len(entries) == 0 {
		c.logger.Warn().Msg("no entries merged; keeping previous stable list")
		return
	}

	res, err := c.prober.Probe(ctx, entriesPayload(entries))
	if err != nil {
		c.logger.Warn().Err(err).Msg("probe failed; keeping previous stable list")
		return
	}

	survivors := SelectSurvivors(entries, res, c.rounds, c.maxFail, c.maxAvgMs)
	survivors = c.geminiGate(ctx, entries, survivors)
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
			Tested:       len(res),
			Kept:         len(survivors),
		},
	})
	c.logger.Info().
		Int("sources_ok", len(bodies)).
		Int("merged", len(entries)).
		Int("kept", len(survivors)).
		Msg("stable list updated")
}

func entriesPayload(entries []Entry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		b.WriteString(e.Raw)
		b.WriteByte('\n')
	}

	return b.Bytes()
}

// geminiChecker is the optional Gemini capability of a Prober, asserted at run
// time so a plain Prober (e.g. the test fake) needs no Gemini methods.
type geminiChecker interface {
	GeminiEnabled() bool
	GeminiCheck(ctx context.Context, payload []byte) map[string]GeminiOutcome
}

// geminiGate drops survivors that cannot reach Gemini through their own node
// and records geo-blocked node hosts in the store (TTL) so later cycles skip
// them before probing. A survivor is kept only when the through-node API GET
// returns a body without the geo-block marker.
func (c *Checker) geminiGate(ctx context.Context, entries []Entry, survivors []Survivor) []Survivor {
	gc, ok := c.prober.(geminiChecker)
	if !ok || !gc.GeminiEnabled() {
		return survivors
	}

	subset := make([]Entry, 0, len(survivors))
	for _, s := range survivors {
		subset = append(subset, s.Entry)
	}
	outcomes := gc.GeminiCheck(ctx, entriesPayload(subset))
	if outcomes == nil {
		c.logger.Warn().Msg("gemini gate skipped: no outcomes")
		return survivors
	}

	kept := make([]Survivor, 0, len(survivors))
	var blocked, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case o.Blocked:
			blocked++
			if c.store != nil {
				if err := c.store.Block(o.Server); err != nil {
					c.logger.Warn().Err(err).Str("host", o.Server).Msg("geoblock write failed")
				}
			}
		case !o.Reachable:
			unreachable++
		default:
			kept = append(kept, s)
		}
	}
	c.logger.Info().Int("survivors", len(survivors)).Int("kept", len(kept)).
		Int("gemini_blocked", blocked).Int("gemini_unreachable", unreachable).Msg("gemini gate")
	return kept
}
