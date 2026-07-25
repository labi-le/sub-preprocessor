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
	reporter Reporter

	cancel context.CancelFunc
	done   chan struct{}
}

func NewController(ctx context.Context, holder *Holder, filterer func() Filterer, store Blocklist, dead DeadCache, logger zerolog.Logger, reporter Reporter) *Controller {
	return &Controller{baseCtx: ctx, holder: holder, filterer: filterer, store: store, dead: dead, logger: logger, reporter: reporter}
}

// allowedCountries builds the worker's country allow-set: every code except
// those the country filter entries exclude, directly or through a group.
func allowedCountries(cfg config.Config) filter.CountrySet {
	allowed := filter.All()
	excluded := filter.CountrySet{}
	for _, spec := range cfg.IPFilterSpecs() {
		if spec.Type != config.FilterCountry {
			continue
		}
		for _, code := range spec.ExcludeCountries {
			excluded.Add(code)
		}
		for _, group := range spec.ExcludeGroups {
			for _, code := range cfg.Groups[group] {
				excluded.Add(code)
			}
		}
	}
	allowed.Exclude(excluded)
	return allowed
}

// Apply stops any running checker and starts a new one when cfg has
// subscription sources configured. The prober and node filters are built
// before the old worker is stopped, so a failed construction leaves the
// previous checker running.
func (c *Controller) Apply(cfg config.Config) error {
	if !cfg.SubscriptionsEnabled() {
		c.Stop()
		return nil
	}

	subs := cfg.Subscriptions
	allowed := allowedCountries(cfg)

	// The gemini/claude/chatgpt prober params default to the geoblock block and
	// are overridden per-entry by the merged NodeFilterSpec; bandwidth params come
	// entirely from its filter entry.
	nodeSpecs := cfg.NodeFilterSpecs()
	geo := cfg.GeoBlock
	var bandwidth config.BandwidthConfig
	names := make([]string, 0, len(nodeSpecs))
	for _, spec := range nodeSpecs {
		names = append(names, spec.Type)
		switch spec.Type {
		case config.FilterGemini:
			geo.Gemini = spec.Gemini
		case config.FilterClaude:
			geo.Claude = spec.Claude
		case config.FilterChatGPT:
			geo.ChatGPT = spec.ChatGPT
		case config.FilterBandwidth:
			bandwidth = spec.Bandwidth
		}
	}

	geminiKey, keyErr := geo.Gemini.APIKeyResolved()
	if keyErr != nil {
		c.logger.Warn().Err(keyErr).Msg("gemini key unavailable; geo-block check disabled")
	}
	prober, err := NewMihomoProber(subs.Check, bandwidth, geo, geminiKey, c.logger)
	if err != nil {
		return fmt.Errorf("create prober: %w", err)
	}
	annotate := len(cfg.Annotate) > 0
	filters := buildNodeFilters(names, prober, c.store, annotate, c.logger)

	c.Stop()
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
		c.reporter,
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
