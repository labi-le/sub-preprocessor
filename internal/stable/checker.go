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

// Checker periodically fetches sources through the preprocess pipeline,
// merges them, probes the nodes and publishes survivors to the holder.
type Checker struct {
	sources  []config.SubscriptionSource
	allowed  filter.CountrySet
	interval time.Duration
	rounds   int
	maxFail  int
	maxAvgMs int
	filterer func() Filterer
	prober   Prober
	holder   *Holder
	logger   zerolog.Logger
}

func NewChecker(
	sources []config.SubscriptionSource,
	allowed filter.CountrySet,
	interval time.Duration,
	rounds, maxFail, maxAvgMs int,
	filterer func() Filterer,
	prober Prober,
	holder *Holder,
	logger zerolog.Logger,
) *Checker {
	return &Checker{
		sources:  sources,
		allowed:  allowed,
		interval: interval,
		rounds:   rounds,
		maxFail:  maxFail,
		maxAvgMs: maxAvgMs,
		filterer: filterer,
		prober:   prober,
		holder:   holder,
		logger:   logger,
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
	bodies := make([]SourceBody, 0, len(c.sources))
	for _, src := range c.sources {
		var buf bytes.Buffer
		buf.Grow(sourceBufSize)
		if _, err := svc.Filter(ctx, &buf, preprocess.FilterRequest{
			SubscriptionURL:  fetch.SubscriptionURL(src.URL),
			AllowedCountries: c.allowed,
		}); err != nil {
			c.logger.Warn().Str("source", src.Name).Err(err).Msg("source fetch failed")

			continue
		}
		bodies = append(bodies, SourceBody{Name: src.Name, Body: append([]byte(nil), buf.Bytes()...)})
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
