package config

import (
	"errors"
	"os"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"gopkg.in/yaml.v3"
)

type GeofeedSource struct {
	URL  string         `yaml:"url"`
	Type fetch.FileType `yaml:"type"`
}

type Config struct {
	Server struct {
		Listen string `yaml:"listen"`
	} `yaml:"server"`
	Geofeed struct {
		Sources         []GeofeedSource `yaml:"sources"`
		RefreshInterval time.Duration   `yaml:"refresh_interval"`
	} `yaml:"geofeed"`
	Resolver struct {
		Timeout   time.Duration `yaml:"timeout"`
		StrictDNS bool          `yaml:"strict_dns"`
	} `yaml:"resolver"`
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}

	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8080"
	}
	if cfg.Resolver.Timeout == 0 {
		cfg.Resolver.Timeout = 5 * time.Second
	}
	if len(cfg.Geofeed.Sources) == 0 {
		return Config{}, errors.New("geofeed.sources must contain at least one source")
	}
	for i := range cfg.Geofeed.Sources {
		source := &cfg.Geofeed.Sources[i]
		source.URL = strings.TrimSpace(source.URL)
		if source.URL == "" {
			return Config{}, errors.New("geofeed.sources.url must not be empty")
		}
		if source.Type == "" {
			return Config{}, errors.New("geofeed.sources.type must not be empty")
		}
		if err := fetch.ValidateFileType(source.Type); err != nil {
			return Config{}, err
		}
	}

	return cfg, nil
}
