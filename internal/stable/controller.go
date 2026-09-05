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
	// snapshotPath is handed to the checker when one is first started. It is
	// startup-only for the same reason store and dead are: config.StoresChanged
	// warns on an edit instead of a reload re-applying it.
	snapshotPath string
	logger       zerolog.Logger
	reporter     Reporter

	checker *Checker
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewController(
	ctx context.Context,
	holder *Holder,
	filterer func() Filterer,
	store Blocklist,
	dead DeadCache,
	snapshotPath string,
	logger zerolog.Logger,
	reporter Reporter,
) *Controller {
	return &Controller{
		baseCtx: ctx, holder: holder, filterer: filterer, store: store, dead: dead,
		snapshotPath: snapshotPath, logger: logger, reporter: reporter,
	}
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
	geminiArmed := false
	for _, spec := range nodeSpecs {
		names = append(names, spec.Type)
		switch spec.Type {
		case config.FilterGemini:
			geo.Gemini = spec.Gemini
			geminiArmed = true
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

	// A configured gemini gate with no usable key must fail, not boot silently
	// off (see resolveGeminiKey); the unarmed base is left unread.
	geminiKey, err := resolveGeminiKey(geo.Gemini, geminiArmed)
	if err != nil {
		return err
	}
	prober, err := NewMihomoProber(subs.Check, bandwidth, geo, cfg.Geo.Cloudflare, geminiKey, c.logger)
	if err != nil {
		return fmt.Errorf("create prober: %w", err)
	}
	filters := buildNodeFilters(names, prober, c.store, c.logger)

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
		Trace:         cfg.AnnotateUsesProvider(config.ProviderCloudflare),
	}

	if c.checker != nil {
		c.checker.Reconfigure(spec)
		c.logger.Info().Int("sources", len(subs.Sources)).Dur("interval", subs.Interval).
			Msg("subscription checker reconfigured; the new settings apply from its next scheduled cycle")

		return nil
	}

	checker := NewChecker(spec, c.filterer, c.store, c.dead, c.holder, c.snapshotPath, c.logger, c.reporter)
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

// resolveGeminiKey returns the key an ARMED gemini gate will check with, or an
// error when it cannot be resolved: the check is the only gate reading a
// location verdict, and a disabled one keeps every geo-blocked node while its
// metrics read "disabled". config validation already refuses an armed entry
// that declares no key material; this is the second half, where the declared
// key_file is actually read -- a missing mount or missing var refuses boot and
// fails the reload instead of degrading the gate. An unarmed base resolves to
// no key and is not read at all: no filter entry, no key needed, no file I/O
// on every reload.
func resolveGeminiKey(g config.GeminiConfig, armed bool) (string, error) {
	if !armed {
		return "", nil
	}
	key, err := g.APIKeyResolved()
	switch {
	case err != nil:
		return "", fmt.Errorf("gemini gate is configured but its key cannot be resolved: %w", err)
	case key == "":
		return "", fmt.Errorf("gemini gate is configured but resolves no key: set api_key, or a key_file (%q) holding key_var %s, on the filter entry or geoblock.gemini", g.KeyFile, g.KeyVar)
	}
	return key, nil
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
