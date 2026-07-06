package app

import (
	"context"
	"fmt"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
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

	svc, err := preprocess.NewProcessor(ctx, logger, reload.OptionsFromConfig(cfg))
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	holder := serverpkg.NewHolder(&serverpkg.Snapshot{Svc: svc, Groups: cfg.Groups})
	stableHolder := stable.NewHolder()
	srv := serverpkg.New(logger, cfg.Server.Listen, holder, stableHolder)

	reloader := reload.NewReloader(defaultConfigPath, holder, logger, cfg, svc)
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
