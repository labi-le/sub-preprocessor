package reload

import (
	"context"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/server"
)

// Applier restarts the stable subscription worker from a validated config.
// Implemented by *stable.Controller.
type Applier interface {
	Apply(cfg config.Config) error
}

// Reloader rebuilds the processing pipeline from a config file on demand and
// atomically swaps it into the server Holder. On any load, validation, or build
// error it logs the failure and keeps the previously applied settings (the
// holder is never mutated on a failed reload).
//
// currentCfg and currentProc track the last successfully applied state;
// currentProcCfg records the config that BUILT currentProc (they diverge when
// ctl.Apply fails: the processor swap stays, the config commit does not). All
// three are mutated only from the single watcher goroutine that drives Reload,
// so they need no additional locking.
type Reloader struct {
	path           string
	holder         *server.Holder
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
	holder *server.Holder,
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

// Reload loads the config from disk and, if it changed and is valid, builds a
// new Processor and atomically swaps it into the holder. Geofeed data (lookup +
// LoadedAt) is carried over when geofeed.sources are unchanged, avoiding a
// re-download. Any error keeps the previously applied settings.
func (r *Reloader) Reload(ctx context.Context) {
	newCfg, err := config.Load(r.path)
	if err != nil {
		r.logger.Error().Err(err).Str("path", r.path).
			Msg("config reload failed; keeping previous settings")
		return
	}

	if config.Equal(r.currentCfg, newCfg) {
		r.logger.Debug().Msg("config unchanged; skipping reload")
		return
	}

	opts := OptionsFromConfig(newCfg)
	opts.Blocklist = r.blocklist
	// Diff against the config that built currentProc (not currentCfg): after a
	// failed Apply the two diverge, and carrying geofeed data across the wrong
	// source set would serve stale countries.
	if !config.GeofeedSourcesChanged(r.currentProcCfg, newCfg) {
		lookup, at := r.currentProc.GeofeedState()
		opts.PreloadedGeofeed = lookup
		opts.PreloadedLoadedAt = at
	}

	newProc, err := preprocess.NewProcessor(ctx, r.logger, opts)
	if err != nil {
		r.logger.Error().Err(err).
			Msg("building processor from new config failed; keeping previous settings")
		return
	}

	if levelErr := log.SetLevel(newCfg.Log.Level); levelErr != nil {
		r.logger.Warn().Err(levelErr).Str("level", newCfg.Log.Level).
			Msg("invalid log level in reloaded config; keeping current level")
	}

	if config.ListenChanged(r.currentCfg, newCfg) {
		r.logger.Warn().
			Str("old", r.currentCfg.Server.Listen).
			Str("new", newCfg.Server.Listen).
			Msg("server.listen change requires restart; ignoring")
	}

	if config.StoresChanged(r.currentCfg, newCfg) {
		r.logger.Warn().
			Msg("geoblock.db_path/geoblock.ttl/deadcache.ttl change requires restart; stores are built once at startup")
	}

	r.holder.Store(&server.Snapshot{Svc: newProc, Groups: newCfg.Groups})

	// The stable worker derives its allow set and through-node filters from the
	// unified filters list, plus subscriptions, groups, the geoblock prober
	// settings (gemini/claude), and the annotate list (baked into the bandwidth
	// [SPD:] tag), so a change to any of them re-applies it; unrelated config
	// edits must leave it running.
	subsAffected := config.SubscriptionsChanged(r.currentCfg, newCfg) ||
		config.GroupsChanged(r.currentCfg, newCfg) ||
		config.FiltersChanged(r.currentCfg, newCfg) ||
		config.ProberChanged(r.currentCfg, newCfg) ||
		config.AnnotateChanged(r.currentCfg, newCfg)
	applied := true
	if r.ctl != nil && subsAffected {
		if applyErr := r.ctl.Apply(newCfg); applyErr != nil {
			applied = false
			r.logger.Error().Err(applyErr).
				Msg("applying subscriptions config failed; stable worker keeps previous settings")
		} else {
			r.logger.Info().Msg("subscriptions config applied")
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
