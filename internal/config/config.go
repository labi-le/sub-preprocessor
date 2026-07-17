package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"github.com/metacubex/mihomo/common/utils"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

const (
	defaultTimeout          = 5 * time.Second
	defaultLogLevel         = "info"
	defaultDNSCacheTTL      = 30 * time.Minute
	defaultDNSNegativeCache = 10 * time.Minute
	defaultASNCacheTTL      = 24 * time.Hour

	defaultSubsInterval      = 30 * time.Minute
	minSubsInterval          = time.Minute
	defaultCheckRounds       = 5
	defaultCheckTimeout      = 2 * time.Second
	defaultCheckTestURL      = "https://www.gstatic.com/generate_204"
	defaultCheckStatus       = "204"
	defaultCheckMaxAvgMs     = 1000
	defaultCheckConcurr      = 16
	defaultBandwidthTestURL  = "https://speed.cloudflare.com/__down?bytes=2000000"
	defaultBandwidthMinMbps  = 5
	defaultBandwidthTimeout  = 20 * time.Second
	defaultBandwidthConcurr  = 4
	defaultSourceTimeout     = 120 * time.Second
	defaultDeadCacheTTL      = 2 * time.Hour
	defaultFetchTimeout      = 3 * time.Second
	defaultGeoBlockTTL       = 720 * time.Hour
	defaultGeminiEndpoint    = "https://generativelanguage.googleapis.com"
	defaultGeminiModel       = "gemini-2.0-flash"
	defaultGeminiMarker      = "User location is not supported for the API use"
	defaultGeminiKeyVar      = "LITELLM_GOOGLE_API_KEY"
	defaultGeminiTimeout     = 15 * time.Second
	defaultGeminiConcurrency = 8
	defaultClaudeEndpoint    = "https://api.anthropic.com"
	defaultClaudeMarker      = "Request not allowed"
	defaultClaudeVersion     = "2023-06-01"
	defaultClaudeTimeout     = 15 * time.Second
	defaultClaudeConcurrency = 8
)

// Unified filter types, provider names, and annotation tags. The single
// filters list selects IP-stage (country/asn, run per-node in preprocess) and
// through-node filters (gemini/claude/bandwidth, run post-probe in stable);
// which physical stage a type lands in is an implementation detail, not config.
const (
	FilterCountry   = "country"
	FilterASN       = "asn"
	FilterGemini    = "gemini"
	FilterClaude    = "claude"
	FilterBandwidth = "bandwidth"

	ProviderGeofeed = "geofeed"
	ProviderASN     = "asn"

	TagGEO = "GEO"
	TagIP  = "IP"
	TagASN = "ASN"
)

var sourceNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

type LogConfig struct {
	Level string `yaml:"level"`
}

type Config struct {
	Log    LogConfig `yaml:"log"`
	Server struct {
		Listen        string `yaml:"listen"`
		MetricsListen string `yaml:"metrics_listen"`
	} `yaml:"server"`
	Geo      GeoConfig `yaml:"geo"`
	Resolver struct {
		Address string        `yaml:"address"`
		Timeout time.Duration `yaml:"timeout"`
		// CacheTTL / CacheNegativeTTL are pointers so an unset value defaults
		// (nil -> defaultDNSCacheTTL / defaultDNSNegativeCache) while an
		// explicit 0 is preserved and means "disable that cache" (resolver.New
		// treats a zero TTL as disable).
		CacheTTL         *time.Duration `yaml:"cache_ttl"`
		CacheNegativeTTL *time.Duration `yaml:"cache_negative_ttl"`
	} `yaml:"resolver"`
	// Filters is the unified, ordered filter list. See IPFilterSpecs and
	// NodeFilterSpecs for how the two builders (preprocess / stable) consume it.
	Filters []FilterConfig `yaml:"filters"`
	// Annotate is the ordered tag list applied to node names (both / and
	// /stable.txt). An empty list disables annotation.
	Annotate      []AnnotateSpec      `yaml:"annotate"`
	Groups        Groups              `yaml:"groups"`
	Subscriptions SubscriptionsConfig `yaml:"subscriptions"`
	GeoBlock      GeoBlockConfig      `yaml:"geoblock"`
	DeadCache     DeadCacheConfig     `yaml:"deadcache"`
	Fetch         FetchConfig         `yaml:"fetch"`
}

// GeoConfig groups the geo provider settings shared by the country/asn filters
// and by annotation: the geofeed IP->country lookup and the Team-Cymru ASN
// resolver.
type GeoConfig struct {
	Geofeed GeofeedConfig `yaml:"geofeed"`
	ASN     ASNConfig     `yaml:"asn"`
}

// FilterConfig is one entry in the unified filters list. Type selects which
// filter to build; the remaining fields are type-specific:
//   - country: Provider (geofeed|asn), ExcludeGroups, ExcludeCountries
//   - asn:     DenyPatterns
//   - bandwidth: MinMbps, TestURL, Timeout, Concurrency
//   - gemini/claude: selectors; prober params come from geoblock.{gemini,claude}
//     and may be overridden per-entry (Marker/Model/Endpoint/Key*/Timeout/
//     Concurrency for gemini; Marker/Endpoint/Version/Timeout/Concurrency for
//     claude).
type FilterConfig struct {
	Type string `yaml:"type"`

	// country / asn (IP-stage, preprocess)
	Provider         string   `yaml:"provider"`
	ExcludeGroups    []string `yaml:"exclude_groups"`
	ExcludeCountries []string `yaml:"exclude_countries"`
	DenyPatterns     []string `yaml:"deny_patterns"`

	// bandwidth (through-node, stable). MinMbps is a pointer so an unset value
	// defaults to defaultBandwidthMinMbps while an explicit 0 means "no floor".
	MinMbps     *int          `yaml:"min_mbps"`
	TestURL     string        `yaml:"test_url"`
	Timeout     time.Duration `yaml:"timeout"`
	Concurrency int           `yaml:"concurrency"`

	// gemini/claude optional overrides (fall back to geoblock.{gemini,claude}).
	Marker   string `yaml:"marker"`
	Model    string `yaml:"model"`
	Endpoint string `yaml:"endpoint"`
	APIKey   string `yaml:"api_key"`
	KeyFile  string `yaml:"key_file"`
	KeyVar   string `yaml:"key_var"`
	Version  string `yaml:"version"`
}

// AnnotateSpec is one entry in the ordered annotation tag list. Provider is
// required for GEO and ASN (geofeed|asn) and unused for IP.
type AnnotateSpec struct {
	Tag      string `yaml:"tag"`
	Provider string `yaml:"provider"`
}

// IPFilterSpec is a parsed IP-stage (per-node, preprocess) filter derived from
// the unified filters list.
type IPFilterSpec struct {
	Type             string
	Provider         string
	ExcludeGroups    []string
	ExcludeCountries []string
	DenyPatterns     []string
}

// NodeFilterSpec is a parsed through-node (post-probe, stable) filter derived
// from the unified filters list. The gemini/claude configs are already merged
// over the geoblock defaults; bandwidth carries the entry's params.
type NodeFilterSpec struct {
	Type      string
	Bandwidth BandwidthConfig
	Gemini    GeminiConfig
	Claude    ClaudeConfig
}

// IPFilterSpecs returns the IP-stage filters (country/asn) in config order.
func (cfg *Config) IPFilterSpecs() []IPFilterSpec {
	var specs []IPFilterSpec
	for _, f := range cfg.Filters {
		switch f.Type {
		case FilterCountry:
			specs = append(specs, IPFilterSpec{
				Type:             FilterCountry,
				Provider:         f.Provider,
				ExcludeGroups:    f.ExcludeGroups,
				ExcludeCountries: f.ExcludeCountries,
			})
		case FilterASN:
			specs = append(specs, IPFilterSpec{
				Type:         FilterASN,
				Provider:     ProviderASN,
				DenyPatterns: f.DenyPatterns,
			})
		}
	}
	return specs
}

// NodeFilterSpecs returns the through-node filters (gemini/claude/bandwidth) in
// config order.
func (cfg *Config) NodeFilterSpecs() []NodeFilterSpec {
	var specs []NodeFilterSpec
	for _, f := range cfg.Filters {
		switch f.Type {
		case FilterGemini:
			specs = append(specs, NodeFilterSpec{Type: FilterGemini, Gemini: f.mergedGemini(cfg.GeoBlock.Gemini)})
		case FilterClaude:
			specs = append(specs, NodeFilterSpec{Type: FilterClaude, Claude: f.mergedClaude(cfg.GeoBlock.Claude)})
		case FilterBandwidth:
			specs = append(specs, NodeFilterSpec{Type: FilterBandwidth, Bandwidth: f.bandwidthConfig()})
		}
	}
	return specs
}

func (f FilterConfig) mergedGemini(base GeminiConfig) GeminiConfig {
	if f.Endpoint != "" {
		base.Endpoint = f.Endpoint
	}
	if f.Model != "" {
		base.Model = f.Model
	}
	if f.Marker != "" {
		base.Marker = f.Marker
	}
	if f.APIKey != "" {
		base.APIKey = f.APIKey
	}
	if f.KeyFile != "" {
		base.KeyFile = f.KeyFile
	}
	if f.KeyVar != "" {
		base.KeyVar = f.KeyVar
	}
	if f.Timeout != 0 {
		base.Timeout = f.Timeout
	}
	if f.Concurrency != 0 {
		base.Concurrency = f.Concurrency
	}
	return base
}

func (f FilterConfig) mergedClaude(base ClaudeConfig) ClaudeConfig {
	if f.Endpoint != "" {
		base.Endpoint = f.Endpoint
	}
	if f.Marker != "" {
		base.Marker = f.Marker
	}
	if f.Version != "" {
		base.Version = f.Version
	}
	if f.Timeout != 0 {
		base.Timeout = f.Timeout
	}
	if f.Concurrency != 0 {
		base.Concurrency = f.Concurrency
	}
	return base
}

func (f FilterConfig) bandwidthConfig() BandwidthConfig {
	return BandwidthConfig{
		TestURL:     f.TestURL,
		MinMbps:     f.MinMbps,
		Timeout:     f.Timeout,
		Concurrency: f.Concurrency,
	}
}

type SubscriptionsConfig struct {
	Interval time.Duration        `yaml:"interval"`
	Check    CheckConfig          `yaml:"check"`
	Sources  []SubscriptionSource `yaml:"sources"`
}

// CheckConfig holds the URL-test (latency) prober params only. The through-node
// filters (gemini/claude/bandwidth) and their params live in the top-level
// filters list, not here.
type CheckConfig struct {
	Rounds         int           `yaml:"rounds"`
	Timeout        time.Duration `yaml:"timeout"`
	TestURL        string        `yaml:"test_url"`
	ExpectedStatus string        `yaml:"expected_status"`
	MaxFail        int           `yaml:"max_fail"`
	MaxAvgMs       int           `yaml:"max_avg_ms"`
	SourceTimeout  time.Duration `yaml:"source_timeout"`
	Concurrency    int           `yaml:"concurrency"`
}

// BandwidthConfig configures the through-node download-speed gate (the
// "bandwidth" filter). MinMbps is a pointer so an unset value defaults to
// defaultBandwidthMinMbps while an explicit 0 means "no speed floor" (annotate
// + drop-unreachable only).
type BandwidthConfig struct {
	TestURL     string        `yaml:"test_url"`
	MinMbps     *int          `yaml:"min_mbps"`
	Timeout     time.Duration `yaml:"timeout"`
	Concurrency int           `yaml:"concurrency"`
}

type SubscriptionSource struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url,omitempty"`
	// Body carries an inline subscription payload (base64 or raw newline-joined
	// URIs) in place of a fetched URL. When set, the source is filtered directly
	// without any HTTP fetch. Used by the crawler's inline-node harvest.
	Body string `yaml:"body,omitempty"`
}

type privateConfig struct {
	Subscriptions struct {
		Sources []SubscriptionSource `yaml:"sources"`
	} `yaml:"subscriptions"`
}

func (cfg *Config) SubscriptionsEnabled() bool {
	return len(cfg.Subscriptions.Sources) > 0
}

type GeofeedConfig struct {
	Sources         []geofeed.Source `yaml:"sources"`
	RefreshInterval time.Duration    `yaml:"refresh_interval"`
}

type Groups map[string][]string

type ASNConfig struct {
	Timeout  time.Duration `yaml:"timeout"`
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// GeoBlockConfig configures the per-node geo-block list: a SQLite TTL store of
// node hosts that failed a through-node API reachability check (Gemini, Claude).
type GeoBlockConfig struct {
	DBPath string        `yaml:"db_path"`
	TTL    time.Duration `yaml:"ttl"`
	Gemini GeminiConfig  `yaml:"gemini"`
	Claude ClaudeConfig  `yaml:"claude"`
}

// DeadCacheConfig configures the in-memory short-TTL cache of nodes that failed
// the stable probe, so later cycles skip re-probing them (see stable.DeadSet;
// keyed by server:port, not persisted).
type DeadCacheConfig struct {
	// TTL is a pointer so an unset value defaults (nil -> defaultDeadCacheTTL)
	// while an explicit 0 is preserved and means "disable the dead-node cache"
	// (app.go gates the DeadSet on TTL > 0).
	TTL *time.Duration `yaml:"ttl"`
}

func (d *DeadCacheConfig) applyDefaults() {
	if d.TTL == nil {
		ttl := defaultDeadCacheTTL
		d.TTL = &ttl
	}
}

// FetchConfig configures the HTTP client used to download subscription bodies.
// Timeout bounds how long a single subscription fetch may wait before failing,
// so an unresponsive source is abandoned quickly instead of stalling a cycle.
type FetchConfig struct {
	Timeout time.Duration `yaml:"timeout"`
}

func (f *FetchConfig) applyDefaults() {
	if f.Timeout == 0 {
		f.Timeout = defaultFetchTimeout
	}
}

// GeminiConfig configures the through-node Gemini reachability check run during
// the stable probe: a real API GET whose body reveals a geo-block, which
// mihomo's HEAD-only URLTest cannot detect.
type GeminiConfig struct {
	Endpoint    string        `yaml:"endpoint"`
	Model       string        `yaml:"model"`
	Marker      string        `yaml:"marker"`
	APIKey      string        `yaml:"api_key"`
	KeyFile     string        `yaml:"key_file"`
	KeyVar      string        `yaml:"key_var"`
	Timeout     time.Duration `yaml:"timeout"`
	Concurrency int           `yaml:"concurrency"`
}

// ClaudeConfig configures the through-node Anthropic API reachability check.
// Anthropic geo-blocks before authentication (HTTP 403 "Request not allowed"),
// so no API key is needed: a keyless GET /v1/models from an allowed region
// returns an authentication error instead of the block marker.
type ClaudeConfig struct {
	Endpoint    string        `yaml:"endpoint"`
	Marker      string        `yaml:"marker"`
	Version     string        `yaml:"version"`
	Timeout     time.Duration `yaml:"timeout"`
	Concurrency int           `yaml:"concurrency"`
}

func (g *GeoBlockConfig) applyDefaults() {
	if g.TTL == 0 {
		g.TTL = defaultGeoBlockTTL
	}
	gm := &g.Gemini
	if gm.Endpoint == "" {
		gm.Endpoint = defaultGeminiEndpoint
	}
	if gm.Model == "" {
		gm.Model = defaultGeminiModel
	}
	if gm.Marker == "" {
		gm.Marker = defaultGeminiMarker
	}
	if gm.KeyVar == "" {
		gm.KeyVar = defaultGeminiKeyVar
	}
	if gm.Timeout == 0 {
		gm.Timeout = defaultGeminiTimeout
	}
	if gm.Concurrency == 0 {
		gm.Concurrency = defaultGeminiConcurrency
	}
	cl := &g.Claude
	if cl.Endpoint == "" {
		cl.Endpoint = defaultClaudeEndpoint
	}
	if cl.Marker == "" {
		cl.Marker = defaultClaudeMarker
	}
	if cl.Version == "" {
		cl.Version = defaultClaudeVersion
	}
	if cl.Timeout == 0 {
		cl.Timeout = defaultClaudeTimeout
	}
	if cl.Concurrency == 0 {
		cl.Concurrency = defaultClaudeConcurrency
	}
}

// APIKeyResolved returns the inline api_key, or the value of key_var read from
// key_file (an env-style KEY=VALUE file, e.g. the agenix secret). Empty without
// error when neither is set, which disables the Gemini check.
func (g GeminiConfig) APIKeyResolved() (string, error) {
	if g.APIKey != "" {
		return g.APIKey, nil
	}
	if g.KeyFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(g.KeyFile)
	if err != nil {
		return "", fmt.Errorf("gemini key_file: %w", err)
	}
	prefix := g.KeyVar + "="
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("gemini key_file %q: %s not found", g.KeyFile, g.KeyVar)
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
	if cfg.Server.MetricsListen == "" {
		cfg.Server.MetricsListen = ":9090"
	}
	if cfg.Resolver.Timeout == 0 {
		cfg.Resolver.Timeout = defaultTimeout
	}
	if cfg.Resolver.CacheTTL == nil {
		ttl := defaultDNSCacheTTL
		cfg.Resolver.CacheTTL = &ttl
	}
	if cfg.Resolver.CacheNegativeTTL == nil {
		ttl := defaultDNSNegativeCache
		cfg.Resolver.CacheNegativeTTL = &ttl
	}
	if cfg.Geo.ASN.Timeout == 0 {
		cfg.Geo.ASN.Timeout = defaultTimeout
	}
	if cfg.Geo.ASN.CacheTTL == 0 {
		cfg.Geo.ASN.CacheTTL = defaultASNCacheTTL
	}
	cfg.applyFilterDefaults()
	cfg.Subscriptions.applyDefaults()
	cfg.GeoBlock.applyDefaults()
	cfg.DeadCache.applyDefaults()
	cfg.Fetch.applyDefaults()

	// Merge the tracked sources.yaml overlay BEFORE validation so the appended
	// sources are validated together with the rest of the config.
	if err := mergeSourcesOverlay(filepath.Dir(path), &cfg); err != nil {
		return Config{}, err
	}
	if errValidate := cfg.Validate(); errValidate != nil {
		return Config{}, errValidate
	}

	privatePath := filepath.Join(filepath.Dir(path), "private.yaml")
	privBytes, readErr := os.ReadFile(privatePath)
	switch {
	case readErr == nil:
		var priv privateConfig
		if unmarshalErr := yaml.Unmarshal(privBytes, &priv); unmarshalErr != nil {
			return Config{}, fmt.Errorf("unmarshal private config: %w", unmarshalErr)
		}
		cfg.Subscriptions.Sources = append(cfg.Subscriptions.Sources, priv.Subscriptions.Sources...)
		if validateErr := cfg.Subscriptions.Validate(); validateErr != nil {
			return Config{}, fmt.Errorf("private config: %w", validateErr)
		}
	case errors.Is(readErr, fs.ErrNotExist):
		// No private overlay to merge.
	default:
		// A permission or I/O error must fail loudly: silently skipping the
		// overlay would drop the crawler-managed sources from the output.
		return Config{}, fmt.Errorf("read private config: %w", readErr)
	}

	return cfg, nil
}

// mergeSourcesOverlay appends subscription sources from a sibling sources.yaml
// (curated sources kept out of config.yaml) to cfg. A missing file is fine; a
// read or parse error fails loudly, mirroring the private.yaml overlay so a
// permission/I/O problem never silently drops the curated sources.
func mergeSourcesOverlay(dir string, cfg *Config) error {
	b, err := os.ReadFile(filepath.Join(dir, "sources.yaml"))
	switch {
	case err == nil:
		var overlay privateConfig
		if unmarshalErr := yaml.Unmarshal(b, &overlay); unmarshalErr != nil {
			return fmt.Errorf("unmarshal sources config: %w", unmarshalErr)
		}
		cfg.Subscriptions.Sources = append(cfg.Subscriptions.Sources, overlay.Subscriptions.Sources...)
	case errors.Is(err, fs.ErrNotExist):
		// No sources overlay to merge.
	default:
		return fmt.Errorf("read sources config: %w", err)
	}
	return nil
}

// applyFilterDefaults coerces per-entry filter and annotation defaults so a
// value that loads is guaranteed to build.
func (cfg *Config) applyFilterDefaults() {
	for i := range cfg.Filters {
		f := &cfg.Filters[i]
		switch f.Type {
		case FilterCountry:
			if f.Provider == "" {
				f.Provider = ProviderGeofeed
			}
		case FilterBandwidth:
			applyBandwidthDefaults(f)
		}
	}
	for i := range cfg.Annotate {
		applyAnnotateDefaults(&cfg.Annotate[i])
	}
}

func applyBandwidthDefaults(f *FilterConfig) {
	if f.TestURL == "" {
		f.TestURL = defaultBandwidthTestURL
	}
	if f.MinMbps == nil {
		f.MinMbps = new(defaultBandwidthMinMbps)
	}
	if f.Timeout == 0 {
		f.Timeout = defaultBandwidthTimeout
	}
	if f.Concurrency == 0 {
		f.Concurrency = defaultBandwidthConcurr
	}
}

func applyAnnotateDefaults(a *AnnotateSpec) {
	if a.Provider != "" {
		return
	}
	switch a.Tag {
	case TagGEO:
		a.Provider = ProviderGeofeed
	case TagASN:
		a.Provider = ProviderASN
	}
}

func (cfg *Config) Validate() error {
	if cfg.Log.Level != "" {
		if _, err := zerolog.ParseLevel(cfg.Log.Level); err != nil {
			return fmt.Errorf("log.level: %w", err)
		}
	}
	if err := cfg.validateNonNegative(); err != nil {
		return err
	}
	if err := cfg.GeoBlock.validate(); err != nil {
		return err
	}
	if err := cfg.Geo.Geofeed.Validate(); err != nil {
		return err
	}
	if err := cfg.Groups.Validate(); err != nil {
		return err
	}
	if err := cfg.validateFilters(); err != nil {
		return err
	}
	if err := cfg.validateAnnotate(); err != nil {
		return err
	}
	if err := cfg.Subscriptions.Validate(); err != nil {
		return err
	}
	return nil
}

// validateNonNegative rejects negative durations. The three cache TTLs are
// pointers (nil-checked) because an explicit 0 is valid and means "disable".
func (cfg *Config) validateNonNegative() error {
	if cfg.Resolver.Timeout < 0 {
		return errors.New("resolver.timeout must not be negative")
	}
	if cfg.Resolver.CacheTTL != nil && *cfg.Resolver.CacheTTL < 0 {
		return errors.New("resolver.cache_ttl must not be negative")
	}
	if cfg.Resolver.CacheNegativeTTL != nil && *cfg.Resolver.CacheNegativeTTL < 0 {
		return errors.New("resolver.cache_negative_ttl must not be negative")
	}
	if cfg.Geo.ASN.Timeout < 0 {
		return errors.New("geo.asn.timeout must not be negative")
	}
	if cfg.Geo.ASN.CacheTTL < 0 {
		return errors.New("geo.asn.cache_ttl must not be negative")
	}
	if cfg.Fetch.Timeout < 0 {
		return errors.New("fetch.timeout must not be negative")
	}
	if cfg.DeadCache.TTL != nil && *cfg.DeadCache.TTL < 0 {
		return errors.New("deadcache.ttl must not be negative")
	}
	if cfg.Geo.Geofeed.RefreshInterval < 0 {
		return errors.New("geo.geofeed.refresh_interval must not be negative")
	}
	if cfg.Subscriptions.Interval < 0 {
		return errors.New("subscriptions.interval must not be negative")
	}
	if cfg.Subscriptions.Check.Timeout < 0 {
		return errors.New("subscriptions.check.timeout must not be negative")
	}
	if cfg.Subscriptions.Check.SourceTimeout < 0 {
		return errors.New("subscriptions.check.source_timeout must not be negative")
	}
	return nil
}

// validateFilters rejects unknown filter types and type-specific bad values.
func (cfg *Config) validateFilters() error {
	for i, f := range cfg.Filters {
		if err := cfg.validateFilter(i, f); err != nil {
			return err
		}
	}
	return nil
}

func (cfg *Config) validateFilter(i int, f FilterConfig) error {
	switch f.Type {
	case FilterCountry:
		return cfg.validateCountryFilter(i, f)
	case FilterASN:
		return validateASNFilter(i, f)
	case FilterGemini, FilterClaude:
		return validateAPIFilter(i, f)
	case FilterBandwidth:
		return f.validateBandwidth(i)
	default:
		return fmt.Errorf("filters[%d]: unknown type %q (must be %q, %q, %q, %q or %q)",
			i, f.Type, FilterCountry, FilterASN, FilterGemini, FilterClaude, FilterBandwidth)
	}
}

func (cfg *Config) validateCountryFilter(i int, f FilterConfig) error {
	if f.Provider != ProviderGeofeed && f.Provider != ProviderASN {
		return fmt.Errorf("filters[%d]: country provider must be %q or %q, got %q", i, ProviderGeofeed, ProviderASN, f.Provider)
	}
	for _, c := range f.ExcludeCountries {
		if err := validateCountryCode(fmt.Sprintf("filters[%d].exclude_countries", i), c); err != nil {
			return err
		}
	}
	for _, g := range f.ExcludeGroups {
		if _, ok := cfg.Groups[g]; !ok {
			return fmt.Errorf("filters[%d].exclude_groups: unknown group %q", i, g)
		}
	}
	return nil
}

func validateASNFilter(i int, f FilterConfig) error {
	for _, p := range f.DenyPatterns {
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("filters[%d].deny_patterns: invalid regexp %q: %w", i, p, err)
		}
	}
	return nil
}

func validateAPIFilter(i int, f FilterConfig) error {
	if f.Timeout < 0 {
		return fmt.Errorf("filters[%d].timeout must not be negative", i)
	}
	if f.Concurrency < 0 {
		return fmt.Errorf("filters[%d].concurrency must not be negative", i)
	}
	return nil
}

func (f FilterConfig) validateBandwidth(i int) error {
	if f.MinMbps != nil && *f.MinMbps < 0 {
		return fmt.Errorf("filters[%d].min_mbps must not be negative", i)
	}
	if f.Timeout <= 0 {
		return fmt.Errorf("filters[%d].timeout must be positive", i)
	}
	if f.Concurrency < 1 {
		return fmt.Errorf("filters[%d].concurrency must be at least 1", i)
	}
	if f.TestURL != "" {
		// Egresses THROUGH the proxy node, so host-side SSRF rules don't apply;
		// only require a well-formed absolute http(s) URL.
		u, err := url.Parse(f.TestURL)
		if err != nil {
			return fmt.Errorf("filters[%d].test_url: %w", i, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("filters[%d].test_url: must be an absolute http(s) URL, got %q", i, f.TestURL)
		}
	}
	return nil
}

// validateAnnotate rejects unknown tags and missing/invalid providers.
func (cfg *Config) validateAnnotate() error {
	for i, a := range cfg.Annotate {
		switch a.Tag {
		case TagGEO, TagASN:
			if a.Provider != ProviderGeofeed && a.Provider != ProviderASN {
				return fmt.Errorf("annotate[%d]: tag %s provider must be %q or %q, got %q", i, a.Tag, ProviderGeofeed, ProviderASN, a.Provider)
			}
		case TagIP:
			// No provider needed.
		default:
			return fmt.Errorf("annotate[%d]: unknown tag %q (must be %q, %q or %q)", i, a.Tag, TagGEO, TagIP, TagASN)
		}
	}
	return nil
}

// validate rejects values that would panic or misbehave downstream: a negative
// concurrency reaches make(chan struct{}, n) in the prober workers, and
// negative timeouts/TTLs bypass the ==0 default guards.
func (g *GeoBlockConfig) validate() error {
	if g.TTL < 0 {
		return errors.New("geoblock.ttl must not be negative")
	}
	if g.Gemini.Timeout < 0 {
		return errors.New("geoblock.gemini.timeout must not be negative")
	}
	if g.Gemini.Concurrency < 0 {
		return errors.New("geoblock.gemini.concurrency must not be negative")
	}
	if g.Claude.Timeout < 0 {
		return errors.New("geoblock.claude.timeout must not be negative")
	}
	if g.Claude.Concurrency < 0 {
		return errors.New("geoblock.claude.concurrency must not be negative")
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
	if c.Timeout == 0 {
		c.Timeout = defaultCheckTimeout
	}
	if c.TestURL == "" {
		c.TestURL = defaultCheckTestURL
	}
	if c.ExpectedStatus == "" {
		c.ExpectedStatus = defaultCheckStatus
	}
	if c.SourceTimeout == 0 {
		c.SourceTimeout = defaultSourceTimeout
	}
	if c.MaxAvgMs == 0 {
		c.MaxAvgMs = defaultCheckMaxAvgMs
	}
	if c.Concurrency == 0 {
		c.Concurrency = defaultCheckConcurr
	}
}

func (s *SubscriptionsConfig) Validate() error {
	if len(s.Sources) == 0 {
		return nil
	}
	if s.Interval < minSubsInterval {
		return fmt.Errorf("subscriptions.interval must be at least %v", minSubsInterval)
	}
	if err := s.Check.validate(); err != nil {
		return err
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
		// A Body source carries an inline payload and needs no URL; a source
		// with neither Body nor a valid public https URL is rejected here.
		if strings.TrimSpace(src.Body) == "" {
			if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(src.URL)); err != nil {
				return fmt.Errorf("subscriptions.sources.%s: %w", src.Name, err)
			}
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
	if c.Timeout <= 0 {
		return errors.New("subscriptions.check.timeout must be positive")
	}
	if c.SourceTimeout <= 0 {
		return errors.New("subscriptions.check.source_timeout must be positive")
	}
	if c.MaxFail < 0 || c.MaxFail >= c.Rounds {
		return errors.New("subscriptions.check.max_fail must be within [0, rounds)")
	}
	if c.MaxAvgMs < 1 {
		return errors.New("subscriptions.check.max_avg_ms must be at least 1")
	}
	// Same parser the prober uses (stable.NewMihomoProber), so a value that
	// loads is guaranteed to build — zero drift between Load and Apply.
	if _, err := utils.NewUnsignedRanges[uint16](c.ExpectedStatus); err != nil {
		return fmt.Errorf("subscriptions.check.expected_status %q: %w", c.ExpectedStatus, err)
	}
	if c.TestURL != "" {
		// The URL test egresses THROUGH the remote proxy node, so host-side
		// SSRF rules don't apply; only require a well-formed http(s) URL.
		u, err := url.Parse(c.TestURL)
		if err != nil {
			return fmt.Errorf("subscriptions.check.test_url: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("subscriptions.check.test_url: must be an absolute http(s) URL, got %q", c.TestURL)
		}
	}
	return nil
}

func SubscriptionsChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Subscriptions, newCfg.Subscriptions)
}

func GroupsChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Groups, newCfg.Groups)
}

// FiltersChanged reports whether the unified filters list differs. Both the
// preprocess processor (IP-stage chain + allow-set inputs) and the stable
// worker (allow set + through-node filters) derive from it, so the reloader
// rebuilds/re-applies when it changes.
func FiltersChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Filters, newCfg.Filters)
}

// ProberChanged reports whether the through-node prober settings (gemini/claude)
// differ; the stable worker must be re-applied when they do. The store-only
// geoblock fields (db_path, ttl) are covered by StoresChanged instead.
func ProberChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.GeoBlock.Gemini, newCfg.GeoBlock.Gemini) ||
		!reflect.DeepEqual(old.GeoBlock.Claude, newCfg.GeoBlock.Claude)
}

// StoresChanged reports whether a setting baked into the stores built once at
// startup changed: geoblock.db_path / geoblock.ttl (SQLite blocklist) or
// deadcache.ttl. Such a change requires a restart to take effect.
func StoresChanged(old, newCfg Config) bool {
	return old.GeoBlock.DBPath != newCfg.GeoBlock.DBPath ||
		old.GeoBlock.TTL != newCfg.GeoBlock.TTL ||
		!reflect.DeepEqual(old.DeadCache.TTL, newCfg.DeadCache.TTL)
}

// AnnotateChanged reports whether the annotate tag list differs. The processor
// bakes it into the per-node [GEO]/[IP]/[ASN] tags and the stable worker into
// the bandwidth [SPD:] tag, so the reloader must rebuild/re-apply when it
// changes; otherwise the published annotation stays stale.
func AnnotateChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Annotate, newCfg.Annotate)
}

func (g *GeofeedConfig) Validate() error {
	if len(g.Sources) == 0 {
		return errors.New("geo.geofeed.sources must contain at least one source")
	}
	for i := range g.Sources {
		g.Sources[i].URL = strings.TrimSpace(g.Sources[i].URL)
		if g.Sources[i].URL == "" {
			return errors.New("geo.geofeed.sources.url must not be empty")
		}
		if g.Sources[i].Type == "" {
			return errors.New("geo.geofeed.sources.type must not be empty")
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
	return !reflect.DeepEqual(old.Geo.Geofeed.Sources, newCfg.Geo.Geofeed.Sources)
}

func ListenChanged(old, newCfg Config) bool {
	return old.Server.Listen != newCfg.Server.Listen
}
