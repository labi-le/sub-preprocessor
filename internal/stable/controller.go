package stable

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/filter"
)

// Controller starts and stops the background subscription checker in
// response to configuration changes.
type Controller struct {
	baseCtx  context.Context
	holder   *Holder
	filterer func() Filterer
	store    Blocklist
	dead     DeadCache
	logger   zerolog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

func NewController(ctx context.Context, holder *Holder, filterer func() Filterer, store Blocklist, dead DeadCache, logger zerolog.Logger) *Controller {
	return &Controller{baseCtx: ctx, holder: holder, filterer: filterer, store: store, dead: dead, logger: logger}
}

// Apply stops any running checker and starts a new one when cfg has
// subscription sources configured.
func (c *Controller) Apply(cfg config.Config) error {
	c.Stop()
	if !cfg.SubscriptionsEnabled() {
		return nil
	}

	subs := cfg.Subscriptions
	allowed := filter.All()
	excluded := filter.ParseAllowed(subs.ExcludeCountries...)
	for _, group := range subs.ExcludeGroups {
		for _, code := range cfg.Groups[group] {
			excluded.Add(code)
		}
	}
	allowed.Exclude(excluded)

	geminiKey, keyErr := cfg.GeoBlock.Gemini.APIKeyResolved()
	if keyErr != nil {
		c.logger.Warn().Err(keyErr).Msg("gemini key unavailable; geo-block check disabled")
	}
	prober, err := NewMihomoProber(subs.Check, cfg.GeoBlock.Gemini, geminiKey, c.logger)
	if err != nil {
		return fmt.Errorf("create prober: %w", err)
	}
	filters := buildNodeFilters(subs.Check.Filters, prober, c.store, c.logger)

	checker := NewChecker(
		subs.Sources,
		allowed,
		subs.Interval,
		subs.Check.Rounds,
		subs.Check.MaxFail,
		subs.Check.MaxAvgMs,
		subs.Check.SourceTimeout,
		c.filterer,
		prober,
		filters,
		c.dead,
		c.holder,
		c.logger,
	)

	ctx, cancel := context.WithCancel(c.baseCtx)
	done := make(chan struct{})
	c.cancel = cancel
	c.done = done
	go func() {
		defer close(done)
		checker.Run(ctx)
	}()
	c.logger.Info().Int("sources", len(subs.Sources)).Dur("interval", subs.Interval).Msg("subscription checker started")

	return nil
}

// Stop cancels the running checker, if any, and waits for it to exit.
func (c *Controller) Stop() {
	if c.cancel == nil {
		return
	}
	c.cancel()
	<-c.done
	c.cancel = nil
	c.done = nil
}
