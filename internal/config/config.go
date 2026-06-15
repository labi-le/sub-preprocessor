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

const (
	defaultTimeout  = 5 * time.Second
	defaultLogLevel = "info"
)

type WorkflowConfig struct {
	Stages []string `yaml:"stages"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type Config struct {
	Log    LogConfig `yaml:"log"`
	Server struct {
		Listen string `yaml:"listen"`
	} `yaml:"server"`
	Geofeed  GeofeedConfig `yaml:"geofeed"`
	Resolver struct {
		Address string        `yaml:"address"`
		Timeout time.Duration `yaml:"timeout"`
	} `yaml:"resolver"`
	ASN      ASNConfig      `yaml:"asn"`
	Workflow WorkflowConfig `yaml:"workflow"`
	Groups   Groups         `yaml:"groups"`
}

type GeofeedConfig struct {
	Sources         []geofeed.Source `yaml:"sources"`
	RefreshInterval time.Duration    `yaml:"refresh_interval"`
}

type Groups map[string][]string

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

	if cfg.Log.Level == "" {
		cfg.Log.Level = defaultLogLevel
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
		cfg.Workflow.Stages = []string{"geofeed", "asn"}
	}
	if errValidate := cfg.Validate(); errValidate != nil {
		return Config{}, errValidate
	}

	return cfg, nil
}

func (cfg *Config) Validate() error {
	if err := cfg.Geofeed.Validate(); err != nil {
		return err
	}
	if err := cfg.Groups.Validate(); err != nil {
		return err
	}
	return nil
}

func (g *GeofeedConfig) Validate() error {
	if len(g.Sources) == 0 {
		return errors.New("geofeed.sources must contain at least one source")
	}
	for i := range g.Sources {
		g.Sources[i].URL = strings.TrimSpace(g.Sources[i].URL)
		if g.Sources[i].URL == "" {
			return errors.New("geofeed.sources.url must not be empty")
		}
		if g.Sources[i].Type == "" {
			return errors.New("geofeed.sources.type must not be empty")
		}
		if errValidate := fetch.ValidateFileType(g.Sources[i].Type); errValidate != nil {
			return fmt.Errorf("validate source type: %w", errValidate)
		}
	}
	return nil
}

func (g Groups) Validate() error {
	for name, countries := range g {
		if name == "" {
			return errors.New("groups: group name must not be empty")
		}
		if len(countries) == 0 {
			return fmt.Errorf("groups.%s: must contain at least one country", name)
		}
		for _, c := range countries {
			if err := validateCountryCode(name, c); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateCountryCode(name, c string) error {
	c = strings.TrimSpace(c)
	if len(c) != 2 { //nolint:mnd // ISO 3166-1 alpha-2 country code length
		return fmt.Errorf("groups.%s: invalid country code %q", name, c)
	}
	if !isASCIILetter(c[0]) || !isASCIILetter(c[1]) {
		return fmt.Errorf("groups.%s: invalid country code %q", name, c)
	}
	return nil
}

func isASCIILetter(b byte) bool {
	return ('A' <= b && b <= 'Z') || ('a' <= b && b <= 'z')
}
