package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"gopkg.in/yaml.v3"
)

const defaultTimeout = 5 * time.Second

const (
	WorkflowFailFirst = "fail_first"
	WorkflowAll       = "all"
)

var defaultWorkflowStages = []string{"geo", "asn"}

type WorkflowConfig struct {
	Stages    []string `yaml:"stages"`
	Algorithm string   `yaml:"algorithm"`
}

type Config struct {
	Server struct {
		Listen string `yaml:"listen"`
	} `yaml:"server"`
	Geofeed struct {
		Sources         []geofeed.Source `yaml:"sources"`
		RefreshInterval time.Duration    `yaml:"refresh_interval"`
	} `yaml:"geofeed"`
	Resolver struct {
		Address string        `yaml:"address"`
		Timeout time.Duration `yaml:"timeout"`
	} `yaml:"resolver"`
	ASN      ASNConfig      `yaml:"asn"`
	Workflow WorkflowConfig `yaml:"workflow"`
}

type ASNConfig struct {
	DenyPatterns []string      `yaml:"deny_patterns"`
	Timeout      time.Duration `yaml:"timeout"`
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
	if cfg.ASN.Timeout == 0 {
		cfg.ASN.Timeout = defaultTimeout
	}
	if len(cfg.Workflow.Stages) == 0 {
		cfg.Workflow.Stages = defaultWorkflowStages
	}
	if cfg.Workflow.Algorithm == "" {
		cfg.Workflow.Algorithm = WorkflowFailFirst
	}
	if cfg.Workflow.Algorithm != WorkflowFailFirst && cfg.Workflow.Algorithm != WorkflowAll {
		return Config{}, fmt.Errorf("unknown workflow algorithm: %q (must be %q or %q)", cfg.Workflow.Algorithm, WorkflowFailFirst, WorkflowAll)
	}
	if errValidate := validateGeofeedSources(cfg.Geofeed.Sources); errValidate != nil {
		return Config{}, errValidate
	}

	return cfg, nil
}

func validateGeofeedSources(sources []geofeed.Source) error {
	if len(sources) == 0 {
		return errors.New("geofeed.sources must contain at least one source")
	}
	for i := range sources {
		sources[i].URL = strings.TrimSpace(sources[i].URL)
		if sources[i].URL == "" {
			return errors.New("geofeed.sources.url must not be empty")
		}
		if sources[i].Type == "" {
			return errors.New("geofeed.sources.type must not be empty")
		}
		if errValidate := fetch.ValidateFileType(sources[i].Type); errValidate != nil {
			return fmt.Errorf("validate source type: %w", errValidate)
		}
	}
	return nil
}
