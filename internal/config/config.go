package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"gopkg.in/yaml.v3"
)

const (
	defaultTimeout  = 5 * time.Second
	defaultLogLevel = "info"

	defaultSubsInterval    = 30 * time.Minute
	minSubsInterval        = time.Minute
	defaultCheckRounds     = 5
	defaultCheckRoundPause = 3 * time.Second
	defaultCheckTimeout    = 2 * time.Second
	defaultCheckTestURL    = "https://www.gstatic.com/generate_204"
	defaultCheckStatus     = "204"
	defaultCheckMaxAvgMs   = 1000
	defaultCheckConcurr    = 16
)

var sourceNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

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
	ASN           ASNConfig           `yaml:"asn"`
	Workflow      WorkflowConfig      `yaml:"workflow"`
	Groups        Groups              `yaml:"groups"`
	Subscriptions SubscriptionsConfig `yaml:"subscriptions"`
}

type SubscriptionsConfig struct {
	Interval         time.Duration        `yaml:"interval"`
	ExcludeCountries []string             `yaml:"exclude_countries"`
	ExcludeGroups    []string             `yaml:"exclude_groups"`
	Check            CheckConfig          `yaml:"check"`
	Sources          []SubscriptionSource `yaml:"sources"`
}

type CheckConfig struct {
	Rounds         int           `yaml:"rounds"`
	RoundPause     time.Duration `yaml:"round_pause"`
	Timeout        time.Duration `yaml:"timeout"`
	TestURL        string        `yaml:"test_url"`
	ExpectedStatus string        `yaml:"expected_status"`
	MaxFail        int           `yaml:"max_fail"`
	MaxAvgMs       int           `yaml:"max_avg_ms"`
	Concurrency    int           `yaml:"concurrency"`
}

type SubscriptionSource struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

func (cfg Config) SubscriptionsEnabled() bool {
	return len(cfg.Subscriptions.Sources) > 0
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
	cfg.Subscriptions.applyDefaults()
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
	if err := cfg.Subscriptions.Validate(cfg.Groups); err != nil {
		return err
	}
	return nil
}

func (s *SubscriptionsConfig) applyDefaults() {
	if s.Interval == 0 {
		s.Interval = defaultSubsInterval
	}
	c := &s.Check
	if c.Rounds == 0 {
		c.Rounds = defaultCheckRounds
	}
	if c.RoundPause == 0 {
		c.RoundPause = defaultCheckRoundPause
	}
	if c.Timeout == 0 {
		c.Timeout = defaultCheckTimeout
	}
	if c.TestURL == "" {
		c.TestURL = defaultCheckTestURL
	}
	if c.ExpectedStatus == "" {
		c.ExpectedStatus = defaultCheckStatus
	}
	if c.MaxAvgMs == 0 {
		c.MaxAvgMs = defaultCheckMaxAvgMs
	}
	if c.Concurrency == 0 {
		c.Concurrency = defaultCheckConcurr
	}
}

func (s *SubscriptionsConfig) Validate(groups Groups) error {
	if len(s.Sources) == 0 {
		return nil
	}
	if s.Interval < minSubsInterval {
		return fmt.Errorf("subscriptions.interval must be at least %v", minSubsInterval)
	}
	if err := s.Check.validate(); err != nil {
		return err
	}
	for _, c := range s.ExcludeCountries {
		if err := validateCountryCode("subscriptions.exclude_countries", c); err != nil {
			return err
		}
	}
	for _, g := range s.ExcludeGroups {
		if _, ok := groups[g]; !ok {
			return fmt.Errorf("subscriptions.exclude_groups: unknown group %q", g)
		}
	}
	seen := make(map[string]struct{}, len(s.Sources))
	for _, src := range s.Sources {
		if !sourceNameRe.MatchString(src.Name) {
			return fmt.Errorf("subscriptions.sources: invalid name %q", src.Name)
		}
		if _, dup := seen[src.Name]; dup {
			return fmt.Errorf("subscriptions.sources: duplicate name %q", src.Name)
		}
		seen[src.Name] = struct{}{}
		if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(src.URL)); err != nil {
			return fmt.Errorf("subscriptions.sources.%s: %w", src.Name, err)
		}
	}
	return nil
}

func (c *CheckConfig) validate() error {
	if c.Rounds < 1 {
		return errors.New("subscriptions.check.rounds must be at least 1")
	}
	if c.Concurrency < 1 {
		return errors.New("subscriptions.check.concurrency must be at least 1")
	}
	if c.Timeout <= 0 || c.RoundPause < 0 {
		return errors.New("subscriptions.check: timeout must be positive, round_pause non-negative")
	}
	if c.MaxFail < 0 || c.MaxFail >= c.Rounds {
		return errors.New("subscriptions.check.max_fail must be within [0, rounds)")
	}
	if c.MaxAvgMs < 1 {
		return errors.New("subscriptions.check.max_avg_ms must be at least 1")
	}
	return nil
}

func SubscriptionsChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Subscriptions, newCfg.Subscriptions)
}

func GroupsChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Groups, newCfg.Groups)
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

func Equal(a, b Config) bool {
	return reflect.DeepEqual(a, b)
}

func GeofeedSourcesChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Geofeed.Sources, newCfg.Geofeed.Sources)
}

func ListenChanged(old, newCfg Config) bool {
	return old.Server.Listen != newCfg.Server.Listen
}
