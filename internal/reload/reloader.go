package reload

import (
	"context"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/server"
	"domains.lst/sub-preprocessor/internal/stable"
)

// Reloader rebuilds the processing pipeline from a config file on demand and
// atomically swaps it into the server Holder. On any load, validation, or build
// error it logs the failure and keeps the previously applied settings (the
// holder is never mutated on a failed reload).
//
// currentCfg and currentProc track the last successfully applied state. They are
// mutated only from the single watcher goroutine that drives Reload, so they
// need no additional locking.
type Reloader struct {
	path        string
	holder      *server.Holder
	logger      zerolog.Logger
	currentCfg  config.Config
	currentProc *preprocess.Processor
	ctl         *stable.Controller
	blocklist   preprocess.Blocklist
}

// NewReloader creates a Reloader seeded with the settings already applied at
// startup (cfg + proc) so the first Reload can diff against them.
func NewReloader(
	path string,
	holder *server.Holder,
	logger zerolog.Logger,
	cfg config.Config,
	proc *preprocess.Processor,
	ctl *stable.Controller,
	blocklist preprocess.Blocklist,
) *Reloader {
	return &Reloader{
		path:        path,
		holder:      holder,
		logger:      logger,
		currentCfg:  cfg,
		currentProc: proc,
		ctl:         ctl,
		blocklist:   blocklist,
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
	if !config.GeofeedSourcesChanged(r.currentCfg, newCfg) {
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

	r.holder.Store(&server.Snapshot{Svc: newProc, Groups: newCfg.Groups})

	// The stable worker derives its filter set from both sections, so either
	// change requires a restart; unrelated config edits must leave it running.
	subsAffected := config.SubscriptionsChanged(r.currentCfg, newCfg) ||
		config.GroupsChanged(r.currentCfg, newCfg)
	if r.ctl != nil && subsAffected {
		if applyErr := r.ctl.Apply(newCfg); applyErr != nil {
			r.logger.Error().Err(applyErr).
				Msg("applying subscriptions config failed; stable worker stopped")
		} else {
			r.logger.Info().Msg("subscriptions config applied")
		}
	}

	r.currentCfg = newCfg
	r.currentProc = newProc
	r.logger.Info().Msg("config reloaded")
}
