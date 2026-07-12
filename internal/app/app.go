package app

import (
	"context"
	"fmt"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geoblock"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/reload"
	serverpkg "domains.lst/sub-preprocessor/internal/server"
	"domains.lst/sub-preprocessor/internal/stable"
)

const defaultConfigPath = "./config/config.yaml"
const shutdownTimeout = 3 * time.Second

func Run(ctx context.Context) error {
	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := log.InitLogger(cfg.Log.Level)
	logger.Info().Str("level", cfg.Log.Level).Msg("logger initialized")

	var (
		gbStore *geoblock.Store
		pblock  preprocess.Blocklist
		sblock  stable.Blocklist
	)
	if cfg.GeoBlock.DBPath != "" {
		gbStore, err = geoblock.Open(cfg.GeoBlock.DBPath, cfg.GeoBlock.TTL)
		if err != nil {
			return fmt.Errorf("open geoblock store: %w", err)
		}
		defer func() { _ = gbStore.Close() }()
		pblock, sblock = gbStore, gbStore
		logger.Info().Str("db", cfg.GeoBlock.DBPath).Int("blocked", gbStore.Count()).Msg("geoblock store")
	}

	var dcache stable.DeadCache
	if cfg.DeadCache.TTL > 0 {
		dcache = stable.NewDeadSet(cfg.DeadCache.TTL)
		logger.Info().Dur("ttl", cfg.DeadCache.TTL).Msg("dead-node cache (in-memory)")
	}

	opts := reload.OptionsFromConfig(cfg)
	opts.Blocklist = pblock
	svc, err := preprocess.NewProcessor(ctx, logger, opts)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	holder := serverpkg.NewHolder(&serverpkg.Snapshot{Svc: svc, Groups: cfg.Groups})
	stableHolder := stable.NewHolder()
	ctl := stable.NewController(ctx, stableHolder, func() stable.Filterer {
		return holder.Load().Svc
	}, sblock, dcache, logger)
	if applyErr := ctl.Apply(cfg); applyErr != nil {
		return fmt.Errorf("start stable subscriptions worker: %w", applyErr)
	}
	defer ctl.Stop()

	srv := serverpkg.New(logger, cfg.Server.Listen, holder, stableHolder)

	reloader := reload.NewReloader(defaultConfigPath, holder, logger, cfg, svc, ctl, pblock)
	watcher, err := reload.NewWatcher(defaultConfigPath, reloader.Reload, logger)
	if err != nil {
		return fmt.Errorf("create config watcher: %w", err)
	}

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		if watchErr := watcher.Run(ctx); watchErr != nil {
			logger.Error().Err(watchErr).Msg("config watcher error")
		}
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
