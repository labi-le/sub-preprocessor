package app

import (
	"context"
	"fmt"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	serverpkg "domains.lst/sub-preprocessor/internal/server"
)

const defaultConfigPath = "./config.yaml"

func Run(ctx context.Context) error {
	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	sources := make([]geofeed.Source, 0, len(cfg.Geofeed.Sources))
	for _, source := range cfg.Geofeed.Sources {
		sources = append(sources, geofeed.Source{URL: source.URL, Type: source.Type})
	}

	svc, err := preprocess.NewService(ctx, sources, cfg.Geofeed.RefreshInterval, cfg.Resolver.Timeout, cfg.Resolver.StrictDNS)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	app := serverpkg.New(cfg.Server.Listen, svc)
	if listenErr := app.Listen(); listenErr != nil {
		return fmt.Errorf("server listen: %w", listenErr)
	}
	return nil
}
