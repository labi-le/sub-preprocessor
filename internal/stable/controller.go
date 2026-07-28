package stable

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/filter"
)

// Controller owns the background subscription checker: it starts one on the
// first enabling config, reconfigures it on every later one, and stops it only
// on shutdown or when the source list empties.
//
// checker/cancel/done are mutated without locking because callers are
// serialized: app.Run performs the first Apply before starting the config
// watcher, every later Apply comes from that single watcher goroutine, and the
// deferred Stop is registered so LIFO joins the watcher first. Calling Apply
// from a second goroutine would race and could leave two workers running.
type Controller struct {
	baseCtx  context.Context
	holder   *Holder
	filterer func() Filterer
	store    Blocklist
	dead     DeadCache
	logger   zerolog.Logger
	reporter Reporter

	checker *Checker
	cancel  context.CancelFunc
	done    chan struct{}
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

// Apply hands cfg to the running checker, or starts one when none is running.
// The prober and node filters are built first, so a failed construction leaves
// the previous configuration in place. A reload NEVER restarts a live worker:
// it swaps the spec the next cycle reads, letting the cycle in flight run to
// publication instead of being cancelled and losing its whole probe pass.
func (c *Controller) Apply(cfg config.Config) error {
	if !cfg.SubscriptionsEnabled() {
		c.Stop()
		return nil
	}

	subs := cfg.Subscriptions
	allowed := allowedCountries(cfg)

	// The gemini/claude/chatgpt/tidal prober params default to the geoblock and
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
		case config.FilterTidal:
			geo.Tidal = spec.Tidal
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

	spec := CheckerSpec{
		Sources:       subs.Sources,
		Allowed:       allowed,
		Interval:      subs.Interval,
		Rounds:        subs.Check.Rounds,
		MaxFail:       subs.Check.MaxFail,
		MaxAvgMs:      subs.Check.MaxAvgMs,
		SourceTimeout: subs.Check.SourceTimeout,
		Prober:        prober,
		Filters:       filters,
	}

	if c.checker != nil {
		c.checker.Reconfigure(spec)
		c.logger.Info().Int("sources", len(subs.Sources)).Dur("interval", subs.Interval).
			Msg("subscription checker reconfigured")

		return nil
	}

	checker := NewChecker(spec, c.filterer, c.dead, c.holder, c.logger, c.reporter)
	ctx, cancel := context.WithCancel(c.baseCtx)
	done := make(chan struct{})
	c.checker = checker
	c.cancel = cancel
	c.done = done
	go func() {
		defer close(done)
		checker.Run(ctx)
	}()
	c.logger.Info().Int("sources", len(subs.Sources)).Dur("interval", subs.Interval).Msg("subscription checker started")

	return nil
}

// Stop cancels the running checker, if any, and waits for it to exit. This is
// the hard path, used on shutdown and when the source list empties; a config
// reload goes through Apply, which reconfigures instead of cancelling.
func (c *Controller) Stop() {
	if c.cancel == nil {
		return
	}
	c.cancel()
	<-c.done
	c.cancel = nil
	c.done = nil
	c.checker = nil
}
