package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"gopkg.in/yaml.v3"
)

const defaultTimeout = 5 * time.Second

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
	b, errRead := os.ReadFile(path)
	if errRead != nil {
		return Config{}, fmt.Errorf("read config file: %w", errRead)
	}

	var cfg Config
	if errUnmarshal := yaml.Unmarshal(b, &cfg); errUnmarshal != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", errUnmarshal)
	}

	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8080"
	}
	if cfg.Resolver.Timeout == 0 {
		cfg.Resolver.Timeout = defaultTimeout
	}
	if len(cfg.Geofeed.Sources) == 0 {
		return Config{}, errors.New("geofeed.sources must contain at least one source")
	}
	for i := range cfg.Geofeed.Sources {
		cfg.Geofeed.Sources[i].URL = strings.TrimSpace(cfg.Geofeed.Sources[i].URL)
		if cfg.Geofeed.Sources[i].URL == "" {
			return Config{}, errors.New("geofeed.sources.url must not be empty")
		}
		if cfg.Geofeed.Sources[i].Type == "" {
			return Config{}, errors.New("geofeed.sources.type must not be empty")
		}
		if errValidate := fetch.ValidateFileType(cfg.Geofeed.Sources[i].Type); errValidate != nil {
			return Config{}, fmt.Errorf("validate source type: %w", errValidate)
		}
	}

	return cfg, nil
}
