package app

import (
	"context"
	"fmt"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/preprocess"
	serverpkg "domains.lst/sub-preprocessor/internal/server"
)

const defaultConfigPath = "./config.yaml"
const shutdownTimeout = 3 * time.Second

func Run(ctx context.Context) error {
	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := log.InitLogger(cfg.Log.Level)
	logger.Info().Str("level", cfg.Log.Level).Msg("logger initialized")

	svc, err := preprocess.NewProcessor(ctx, logger, cfg.Geofeed.Sources, cfg.Geofeed.RefreshInterval, cfg.Resolver.Timeout, cfg.Resolver.Address, cfg.ASN.Timeout, cfg.ASN.DenyPatterns, cfg.Workflow.Stages)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	srv := serverpkg.New(logger, cfg.Server.Listen, svc)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Listen()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		logger.Info().Msg("shutting down")
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			return fmt.Errorf("server shutdown: %w", shutdownErr)
		}
		logger.Info().Msg("shutdown complete")
		return nil
	case listenErr := <-errCh:
		return listenErr
	}
}
