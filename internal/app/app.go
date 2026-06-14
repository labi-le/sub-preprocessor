package app

import (
	"context"
	"fmt"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/preprocess"
	serverpkg "domains.lst/sub-preprocessor/internal/server"
)

const defaultConfigPath = "./config.yaml"
const shutdownTimeout = 10 * time.Second

func Run(ctx context.Context) error {
	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	svc, err := preprocess.NewProcessor(ctx, cfg.Geofeed.Sources, cfg.Geofeed.RefreshInterval, cfg.Resolver.Timeout, cfg.Resolver.Address, cfg.ASN.Timeout, cfg.ASN.DenyPatterns, cfg.Workflow.Stages, cfg.Workflow.Algorithm)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	srv := serverpkg.New(cfg.Server.Listen, svc)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Listen()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			return fmt.Errorf("server shutdown: %w", shutdownErr)
		}
		return nil
	case listenErr := <-errCh:
		return listenErr
	}
}
