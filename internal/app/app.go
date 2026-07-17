package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geoblock"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/metrics"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/reload"
	serverpkg "domains.lst/sub-preprocessor/internal/server"
	"domains.lst/sub-preprocessor/internal/stable"
)

const defaultConfigPath = "./config/config.yaml"
const shutdownTimeout = 3 * time.Second
const metricsReadHeaderTimeout = 5 * time.Second

func startMetrics(addr string, m *metrics.Metrics, logger zerolog.Logger) *http.Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error().Err(err).Msg("metrics listener error")
		}
	}()
	logger.Info().Str("addr", addr).Msg("metrics listener started")
	return srv
}

// buildStores constructs the optional geoblock store and dead-node cache from
// config. Both are nil when their feature is disabled; the caller owns the
// store's Close and wires the nil-able Blocklist/DeadCache interfaces.
func buildStores(cfg config.Config, logger zerolog.Logger) (*geoblock.Store, *stable.DeadSet, error) {
	var (
		gbStore *geoblock.Store
		dcache  *stable.DeadSet
	)
	if cfg.GeoBlock.DBPath != "" {
		store, err := geoblock.Open(cfg.GeoBlock.DBPath, cfg.GeoBlock.TTL)
		if err != nil {
			return nil, nil, fmt.Errorf("open geoblock store: %w", err)
		}
		gbStore = store
		logger.Info().Str("db", cfg.GeoBlock.DBPath).Int("blocked", store.Count()).Msg("geoblock store")
	}

	if *cfg.DeadCache.TTL > 0 {
		dcache = stable.NewDeadSet(*cfg.DeadCache.TTL)
		logger.Info().Dur("ttl", *cfg.DeadCache.TTL).Msg("dead-node cache (in-memory)")
	}

	return gbStore, dcache, nil
}

// buildProcessor wires processor options from config and constructs the
// preprocess service.
func buildProcessor(ctx context.Context, cfg config.Config, logger zerolog.Logger, pblock preprocess.Blocklist) (*preprocess.Processor, error) {
	opts := reload.OptionsFromConfig(cfg)
	opts.Blocklist = pblock
	svc, err := preprocess.NewProcessor(ctx, logger, opts)
	if err != nil {
		return nil, fmt.Errorf("create service: %w", err)
	}
	return svc, nil
}

// buildWatcher wires the config reloader and its filesystem watcher.
func buildWatcher(cfg config.Config, logger zerolog.Logger, holder *serverpkg.Holder, svc *preprocess.Processor, ctl *stable.Controller, pblock preprocess.Blocklist) (*reload.Watcher, error) {
	reloader := reload.NewReloader(defaultConfigPath, holder, logger, cfg, svc, ctl, pblock)
	watcher, err := reload.NewWatcher(defaultConfigPath, reloader.Reload, logger)
	if err != nil {
		return nil, fmt.Errorf("create config watcher: %w", err)
	}
	return watcher, nil
}

func Run(ctx context.Context) error {
	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := log.InitLogger(cfg.Log.Level)
	logger.Info().Str("level", cfg.Log.Level).Msg("logger initialized")

	gbStore, deadSet, err := buildStores(cfg, logger)
	if err != nil {
		return err
	}
	var (
		pblock preprocess.Blocklist
		sblock stable.Blocklist
		dcache stable.DeadCache
	)
	if gbStore != nil {
		defer func() { _ = gbStore.Close() }()
		pblock, sblock = gbStore, gbStore
	}
	if deadSet != nil {
		dcache = deadSet
	}

	svc, err := buildProcessor(ctx, cfg, logger, pblock)
	if err != nil {
		return err
	}

	holder := serverpkg.NewHolder(&serverpkg.Snapshot{Svc: svc, Groups: cfg.Groups})
	stableHolder := stable.NewHolder()
	m := metrics.New()
	ctl := stable.NewController(ctx, stableHolder, func() stable.Filterer {
		return holder.Load().Svc
	}, sblock, dcache, logger, m)
	if applyErr := ctl.Apply(cfg); applyErr != nil {
		return fmt.Errorf("start stable subscriptions worker: %w", applyErr)
	}
	defer ctl.Stop()

	srv := serverpkg.New(logger, cfg.Server.Listen, holder, stableHolder)

	if metricsSrv := startMetrics(cfg.Server.MetricsListen, m, logger); metricsSrv != nil {
		defer func() { _ = metricsSrv.Close() }()
	}

	watcher, err := buildWatcher(cfg, logger, holder, svc, ctl, pblock)
	if err != nil {
		return err
	}

	// Watcher runs under a derived context; the deferred cancel+join is
	// registered AFTER the ctl.Stop/gbStore.Close defers so (LIFO) the watcher
	// is joined FIRST on every return path — an in-flight Reload→ctl.Apply can
	// never race controller/store teardown.
	watcherCtx, cancelWatcher := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		if watchErr := watcher.Run(watcherCtx); watchErr != nil {
			logger.Error().Err(watchErr).Msg("config watcher error")
		}
	}()
	defer func() {
		cancelWatcher()
		<-watcherDone
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Listen()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		logger.Info().Msg("shutting down")
		shutdownErr := srv.Shutdown(shutdownCtx)
		<-watcherDone
		if shutdownErr != nil {
			return fmt.Errorf("server shutdown: %w", shutdownErr)
		}
		logger.Info().Msg("shutdown complete")
		return nil
	case listenErr := <-errCh:
		return listenErr
	}
}
