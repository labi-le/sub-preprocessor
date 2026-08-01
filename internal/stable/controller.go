package stable

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// Controller owns the background subscription checker: it starts one on the
// first enabling config and reconfigures it on every later one. Only shutdown
// stops it.
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

// Apply hands cfg to the running checker, or starts one when none is running.
// The prober and node filters are built first, so a failed construction leaves
// the previous configuration in place. A reload NEVER restarts a live worker:
// it swaps the spec the next cycle reads, letting the cycle in flight run to
// publication instead of being cancelled and losing its whole probe pass.
//
// An empty merged source list is refused rather than obeyed once a worker runs.
// Every source of this deployment comes from an overlay, so an empty list is
// nearly always a missing or truncated sources.yaml/private.yaml, and stopping
// the worker on it would cancel the cycle in flight and freeze /stable.txt on
// its last publication with nothing behind it. Keeping the previous spec live
// costs a stale source list until the config is fixed, and a deliberate disable
// is a restart (or an explicitly empty list at startup, which never starts one).
func (c *Controller) Apply(cfg config.Config) error {
	if !cfg.SubscriptionsEnabled() {
		if c.checker != nil {
			c.logger.Warn().Msg("reloaded config has no subscription sources; keeping the running worker on its previous sources (check the sources.yaml/private.yaml overlays)")
		}

		return nil
	}

	subs := cfg.Subscriptions
	denied := cfg.DeniedCountries()

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
		case config.FilterGeoTrace:
			geo.GeoTrace = spec.GeoTrace
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
		Denied:        denied,
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

	checker := NewChecker(spec, c.filterer, c.store, c.dead, c.holder, c.logger, c.reporter)
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
// the shutdown-only hard path; a config reload goes through Apply, which
// reconfigures instead of cancelling.
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
