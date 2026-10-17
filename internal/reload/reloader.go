package reload

import (
	"context"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/preprocess"
)

// Applier hands a validated config to the stable subscription worker.
// Implemented by *stable.Controller.
type Applier interface {
	// Apply stages cfg on the running worker and reports whether the worker's
	// policy now comes from it. A refused config (no subscription sources)
	// returns false with a nil error: the worker keeps its previous generation,
	// and the caller must neither commit the config nor publish its processor.
	Apply(cfg config.Config) (bool, error)
	// UseProcessor publishes p as the processor the worker's next cycle reads
	// without rebuilding the worker policy, which is what a reload that
	// changed only processor-owned settings needs. It takes the concrete
	// processor because that is what the reloader rebuilt; stable.Filterer is
	// the worker-side view of the same value.
	UseProcessor(p *preprocess.Processor)
}

// Reloader rebuilds the processing pipeline from a config file on demand and
// publishes the replacement to the stable worker as one generation: the
// processor and, when the worker policy changed, the spec travel together
// through ctl, while the Holder records the rebuilt processor the controller
// resolves for Apply. On any load, validation, or build error it logs the
// failure and keeps the previously applied settings (the holder is never
// mutated on a failed reload).
//
// currentCfg records the config whose policy the worker actually runs: it is
// committed only after Apply reports the policy staged, so a REFUSED config
// (empty source list) or a failed Apply leaves it on the previous one and the
// next reload diffs against what is really running. currentProcCfg records the
// config that BUILT currentProc; the two diverge when Apply fails or refuses.
// All three are mutated only from the single watcher goroutine that drives
// Reload, so they need no additional locking.
type Reloader struct {
	path           string
	holder         *Holder
	logger         zerolog.Logger
	currentCfg     config.Config
	currentProc    *preprocess.Processor
	currentProcCfg config.Config
	ctl            Applier
	blocklist      preprocess.Blocklist
}

// NewReloader creates a Reloader seeded with the settings already applied at
// startup (cfg + proc) so the first Reload can diff against them.
func NewReloader(
	path string,
	holder *Holder,
	logger zerolog.Logger,
	cfg config.Config,
	proc *preprocess.Processor,
	ctl Applier,
	blocklist preprocess.Blocklist,
) *Reloader {
	return &Reloader{
		path:           path,
		holder:         holder,
		logger:         logger,
		currentCfg:     cfg,
		currentProc:    proc,
		currentProcCfg: cfg,
		ctl:            ctl,
		blocklist:      blocklist,
	}
}

// carryLoadedState hands the outgoing processor's downloads and warm caches to
// its replacement, each guarded by its own diff. Every guard reads
// currentProcCfg rather than currentCfg: after a failed ctl.Apply the two
// diverge, and carrying data across the wrong source set would serve stale
// answers. A zero state -- the provider or the cidr filter absent from the
// current processor -- leaves the preload unset.
//
// The DNS and Cymru caches are the only carried state whose value is the cache
// itself rather than a download: dropping them re-resolves every host and node
// IP on the next cycle, so their configured TTLs would never be reached under
// the crawler's hourly private.yaml rewrite.
func (r *Reloader) carryLoadedState(opts *preprocess.Options, newCfg config.Config) {
	if !config.GeofeedSourcesChanged(r.currentProcCfg, newCfg) {
		opts.PreloadedGeofeed = r.currentProc.GeofeedState()
	}
	if !config.DBIPChanged(r.currentProcCfg, newCfg) {
		opts.PreloadedDBIP = r.currentProc.DBIPState()
	}
	if !config.RegistryChanged(r.currentProcCfg, newCfg) {
		opts.PreloadedRegistry = r.currentProc.RegistryState()
	}
	if !config.CIDRFiltersChanged(r.currentProcCfg, newCfg) {
		opts.PreloadedCIDR = r.currentProc.CIDRState()
	}
	if !config.ResolverChanged(r.currentProcCfg, newCfg) {
		opts.PreloadedResolver = r.currentProc.ResolverState()
	}
	if !config.ASNChanged(r.currentProcCfg, newCfg) {
		opts.PreloadedASN = r.currentProc.ASNState()
	}
}

// Reload loads the config from disk and, if it changed and is valid, builds a
// new Processor and publishes it with the worker policy it was built from.
// Geofeed state (the lookup, its load time and the retry schedule in flight)
// is carried over when geofeed.sources are unchanged, avoiding a re-download;
// dbip/registry state is carried the same way when its config block is
// unchanged, as is the cidr allow-list when its filter entry is unchanged, as
// are the DNS and ASN resolvers (with their caches) when resolver.*/geo.asn.*
// are unchanged. Any error keeps the previously applied settings.
func (r *Reloader) Reload(ctx context.Context) {
	newCfg, err := config.Load(r.path)
	if err != nil {
		r.logger.Error().Err(err).Str("path", r.path).
			Msg("config reload failed; keeping previous settings")
		return
	}

	// Both must match for the reload to be genuinely redundant. After a failed
	// ctl.Apply the two diverge: currentProc serves the new file while
	// currentCfg still records the old one, so reverting the file to the old one
	// would hit an Equal(currentCfg) fast path and leave the holder serving the
	// rejected config — the revert is exactly what an operator reaches for after
	// that error.
	if config.Equal(r.currentCfg, newCfg) && config.Equal(r.currentProcCfg, newCfg) {
		r.logger.Debug().Msg("config unchanged; skipping reload")
		return
	}

	opts := OptionsFromConfig(newCfg)
	opts.Blocklist = r.blocklist
	r.carryLoadedState(&opts, newCfg)

	newProc, err := preprocess.NewProcessor(ctx, r.logger, opts)
	if err != nil {
		r.logger.Error().Err(err).
			Msg("building processor from new config failed; keeping previous settings")
		return
	}

	// SetLevel cannot fail here: config.Load already ran Validate, which
	// rejects any level zerolog.ParseLevel refuses — the very call SetLevel
	// wraps — so a bad level fails the reload at Load and keeps the previous
	// processor before this line is ever reached. A non-nil error would mean
	// the load gate was bypassed, which is worth an error, not a warn.
	if levelErr := log.SetLevel(newCfg.Log.Level); levelErr != nil {
		r.logger.Error().Err(levelErr).Str("level", newCfg.Log.Level).
			Msg("log level failed to parse after config.Validate accepted it; keeping current level")
	}

	if config.ListenChanged(r.currentCfg, newCfg) {
		r.logger.Warn().
			Str("old", r.currentCfg.Server.Listen).
			Str("new", newCfg.Server.Listen).
			Msg("server.listen change requires restart; ignoring")
	}

	if config.MetricsListenChanged(r.currentCfg, newCfg) {
		r.logger.Warn().
			Str("old", r.currentCfg.Server.MetricsListen).
			Str("new", newCfg.Server.MetricsListen).
			Msg("server.metrics_listen change requires restart; metrics listener is started once at startup")
	}

	if config.StoresChanged(r.currentCfg, newCfg) {
		r.logger.Warn().
			Msg("geoblock.db_path/geoblock.ttl/deadcache.ttl/subscriptions.snapshot_path change requires restart; stores are built once at startup")
	}

	r.holder.Store(newProc)

	// The stable worker derives its allow set and through-node filters from the
	// unified filters list, plus subscriptions, groups, the through-node prober
	// settings (gemini/claude/chatgpt/tidal under geoblock, cloudflare under
	// geo), and the annotate list (which decides the published tags and whether
	// the egress trace runs at all), so a change to any of them re-applies it;
	// unrelated config edits must leave it running.
	subsAffected := config.SubscriptionsChanged(r.currentCfg, newCfg) ||
		config.GroupsChanged(r.currentCfg, newCfg) ||
		config.FiltersChanged(r.currentCfg, newCfg) ||
		config.ProberChanged(r.currentCfg, newCfg) ||
		config.AnnotateChanged(r.currentCfg, newCfg)
	applied := true
	if r.ctl != nil {
		if !subsAffected {
			// Policy unchanged: swap only the processor, still as one
			// generation, so a cycle can never pair the old deny-set with the
			// new lookup.
			r.ctl.UseProcessor(newProc)
		} else if staged, applyErr := r.ctl.Apply(newCfg); applyErr != nil {
			applied = false
			r.logger.Error().Err(applyErr).
				Msg("applying subscriptions config failed; stable worker keeps previous settings")
			// The worker policy is not committed, but the rebuilt processor
			// still reaches the worker: retaining it across a failed Apply is
			// the documented tradeoff (see the Reloader doc), and the retry
			// republishes the pair.
			r.ctl.UseProcessor(newProc)
		} else if !staged {
			// Refused (no subscription sources): the worker keeps its previous
			// generation. Publishing this config's processor would pair it with
			// the older policy, so neither half is published and the config is
			// not committed -- the next reload diffs against the policy that is
			// actually running and retries the Apply.
			applied = false
		} else {
			// Apply only swaps the checker's spec and pokes it; Run starts a
			// cycle on the ticker alone, so the new settings are live from the
			// NEXT scheduled cycle, not from this log line.
			r.logger.Info().Msg("subscriptions config staged; the stable worker takes it up at its next scheduled cycle")
		}
	}

	r.currentProc = newProc
	r.currentProcCfg = newCfg
	if !applied {
		// Do not commit newCfg: keeping the old config means a re-save of the
		// same file diffs as changed and retries ctl.Apply instead of hitting
		// the config.Equal fast path. The processor/holder swap above
		// intentionally stays applied — it already serves the new settings and
		// rebuilding it on the retry is harmless.
		return
	}
	r.currentCfg = newCfg
	r.logger.Info().Msg("config reloaded")
}
