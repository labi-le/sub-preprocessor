package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"github.com/metacubex/mihomo/common/utils"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

const (
	defaultTimeout          = 5 * time.Second
	schemeHTTPS             = "https"
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
	defaultChatGPTEndpoint   = "https://api.openai.com"
	// The error code OpenAI returns on the compliance endpoint when it refuses
	// the caller's egress; the accepted answer carries no such code.
	defaultChatGPTMarker      = "unsupported_country"
	defaultChatGPTTimeout     = 15 * time.Second
	defaultChatGPTConcurrency = 8
	defaultTidalEndpoint      = "https://api.tidal.com"
	defaultTidalTimeout       = 15 * time.Second
	defaultTidalConcurrency   = 8
	// Cloudflare's trace endpoint answers key=value lines from the edge the
	// request actually reached, so it reports the egress rather than a
	// registration. Concurrency matches the other through-node checks.
	defaultGeoTraceEndpoint    = "https://cloudflare.com/cdn-cgi/trace"
	defaultGeoTraceTimeout     = 15 * time.Second
	defaultGeoTraceConcurrency = 8

	// Free downloadable IP->country databases for the annotate provider chain.
	// {yyyy-mm} in the dbip URL expands to the current UTC month at fetch time.
	defaultDBIPURL      = "https://download.db-ip.com/free/dbip-country-lite-{yyyy-mm}.csv.gz"
	defaultGeoDBRefresh = 24 * time.Hour
)

// Unified filter types, provider names, and annotation tags. The single
// filters list selects IP-stage (country/asn, run per-node in preprocess) and
// through-node filters (gemini/claude/chatgpt/tidal/bandwidth, run post-probe
// in stable); which physical stage a type lands in is an implementation
// detail, not config.
const (
	FilterCountry   = "country"
	FilterASN       = "asn"
	FilterGemini    = "gemini"
	FilterClaude    = "claude"
	FilterChatGPT   = "chatgpt"
	FilterTidal     = "tidal"
	FilterBandwidth = "bandwidth"

	// ProviderGeoTrace answers from what the node reported about its own egress
	// (Cloudflare's cdn-cgi/trace, run by the stable worker after the probes).
	// The on-demand GET / path has no post-probe stage, so there it always
	// misses and the chain falls through to the offline providers below.
	ProviderGeoTrace = "geotrace"
	ProviderGeofeed  = "geofeed"
	ProviderDBIP     = "dbip"
	ProviderRegistry = "registry"
	ProviderASN      = "asn"

	TagGEO = "GEO"
)

var sourceNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// The five RIR delegated-extended-latest files together cover the full
// allocated IP space with registration countries.
var defaultRegistryURLs = []string{
	"https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-extended-latest",
	"https://ftp.apnic.net/stats/apnic/delegated-apnic-extended-latest",
	"https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest",
	"https://ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-extended-latest",
	"https://ftp.afrinic.net/stats/afrinic/delegated-afrinic-extended-latest",
}

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
// and by annotation: the geofeed IP->country lookup, the DB-IP and RIR-registry
// downloadable databases, and the Team-Cymru ASN resolver.
type GeoConfig struct {
	Geofeed  GeofeedConfig  `yaml:"geofeed"`
	DBIP     DBIPConfig     `yaml:"dbip"`
	Registry RegistryConfig `yaml:"registry"`
	ASN      ASNConfig      `yaml:"asn"`
}

// FilterConfig is one entry in the unified filters list. Type selects which
// filter to build; the remaining fields are type-specific:
//   - country: Provider (geofeed|asn); ExcludeGroups/ExcludeCountries, which
//     are worker-only (see below)
//   - asn:     DenyPatterns
//   - bandwidth: MinMbps, TestURL, Timeout, Concurrency
//   - gemini/claude/chatgpt/tidal: selectors; prober params come from
//     geoblock.{gemini,claude,chatgpt,tidal} and may be overridden
//     per-entry (Marker/Model/Endpoint/Key*/Timeout/Concurrency for gemini;
//     Marker/Endpoint/Version/Timeout/Concurrency for claude;
//     Marker/Endpoint/Timeout/Concurrency for chatgpt;
//     Endpoint/Timeout/Concurrency for tidal).
//
// A field a type's merge does not read is silently ignored for that type: a
// tidal entry honours only Endpoint/Timeout/Concurrency, so a Marker or Model
// written beside it changes nothing.
type FilterConfig struct {
	Type string `yaml:"type"`

	// country / asn. Provider and DenyPatterns build the IP-stage chain in
	// preprocess; ExcludeGroups/ExcludeCountries do NOT reach it -- they are
	// read only by the stable worker, through Config.DeniedCountries, so on the
	// on-demand GET / path the country constraint comes from the query params
	// alone.
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

	// gemini/claude/chatgpt/tidal overrides (fall back to the geoblock
	// sub-block).
	Marker   string `yaml:"marker"`
	Model    string `yaml:"model"`
	Endpoint string `yaml:"endpoint"`
	APIKey   string `yaml:"api_key"`
	KeyFile  string `yaml:"key_file"`
	KeyVar   string `yaml:"key_var"`
	Version  string `yaml:"version"`
}

// AnnotateSpec is one entry in the ordered annotation tag list. GEO is the only
// tag the loader accepts — validateAnnotate's default arm rejects everything
// else, IP and ASN included, both retired once nothing consumed them — and it
// is provider-backed: Providers is its ordered lookup chain, first provider
// that answers wins, and it must not be empty. Omitting it in YAML is still
// fine, because applyAnnotateDefaults runs before Validate and fills GEO with
// geofeed; the emptiness check only bites a Config built in code.
//
// One accepted tag does not make the LIST redundant: entries are rendered in
// order and repeat freely (two GEO entries with different chains publish two
// tags, and Annotate reports the leftmost that resolved), and an empty list
// disables annotation outright — a distinct mode, not a milder one, since the
// annotator then goes nil and nothing strips upstream tags either.
//
// The retired single-provider "provider" key needs no field here: the strict
// decode in decodeStrict rejects it, and every future rename with it.
type AnnotateSpec struct {
	Tag       string   `yaml:"tag"`
	Providers []string `yaml:"providers"`
}

// IPFilterSpec is a parsed IP-stage (per-node, preprocess) filter derived from
// the unified filters list. It carries only what preprocess consumes; the
// configured country exclusions are the worker's input and travel through
// Config.DeniedCountries instead.
type IPFilterSpec struct {
	Type         string
	Provider     string
	DenyPatterns []string
}

// NodeFilterSpec is a parsed through-node (post-probe, stable) filter derived
// from the unified filters list. The API configs are already merged
// over the geoblock defaults; bandwidth carries the entry's params.
type NodeFilterSpec struct {
	Type      string
	Bandwidth BandwidthConfig
	Gemini    GeminiConfig
	Claude    ClaudeConfig
	ChatGPT   ChatGPTConfig
	Tidal     TidalConfig
}

// IPFilterSpecs returns the IP-stage filters (country/asn) in config order.
func (cfg *Config) IPFilterSpecs() []IPFilterSpec {
	var specs []IPFilterSpec
	for _, f := range cfg.Filters {
		switch f.Type {
		case FilterCountry:
			specs = append(specs, IPFilterSpec{
				Type:     FilterCountry,
				Provider: f.Provider,
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

// DeniedCountries builds the stable worker's country deny-set: every code the
// country filter entries exclude, directly or through a group. It is a deny-set
// rather than the complement allow-set so a node whose IP no geo source covers
// is kept rather than dropped for being unplaceable.
//
// This is the only consumer of filters[].exclude_groups /
// filters[].exclude_countries: preprocess never sees them, so a change here
// takes effect on /stable.txt only.
func (cfg *Config) DeniedCountries() filter.CountrySet {
	denied := filter.CountrySet{}
	for _, f := range cfg.Filters {
		if f.Type != FilterCountry {
			continue
		}
		for _, code := range f.ExcludeCountries {
			denied.Add(code)
		}
		for _, group := range f.ExcludeGroups {
			for _, code := range cfg.Groups[group] {
				denied.Add(code)
			}
		}
	}
	return denied
}

// AnnotateUsesProvider reports whether any annotate tag's chain names the given
// provider. The stable worker asks for ProviderGeoTrace: the trace probe costs
// one request through every survivor, so it is only worth running when a tag
// can actually render its answer.
func (cfg *Config) AnnotateUsesProvider(name string) bool {
	for _, a := range cfg.Annotate {
		if slices.Contains(a.Providers, name) {
			return true
		}
	}
	return false
}

// NodeFilterSpecs returns the through-node filters (gemini/claude/chatgpt/
// tidal/bandwidth) in config order.
func (cfg *Config) NodeFilterSpecs() []NodeFilterSpec {
	var specs []NodeFilterSpec
	for _, f := range cfg.Filters {
		switch f.Type {
		case FilterGemini:
			specs = append(specs, NodeFilterSpec{Type: FilterGemini, Gemini: f.mergedGemini(cfg.GeoBlock.Gemini)})
		case FilterClaude:
			specs = append(specs, NodeFilterSpec{Type: FilterClaude, Claude: f.mergedClaude(cfg.GeoBlock.Claude)})
		case FilterChatGPT:
			specs = append(specs, NodeFilterSpec{Type: FilterChatGPT, ChatGPT: f.mergedChatGPT(cfg.GeoBlock.ChatGPT)})
		case FilterTidal:
			specs = append(specs, NodeFilterSpec{Type: FilterTidal, Tidal: f.mergedTidal(cfg.GeoBlock.Tidal)})
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

func (f FilterConfig) mergedChatGPT(base ChatGPTConfig) ChatGPTConfig {
	if f.Endpoint != "" {
		base.Endpoint = f.Endpoint
	}
	if f.Marker != "" {
		base.Marker = f.Marker
	}
	if f.Timeout != 0 {
		base.Timeout = f.Timeout
	}
	if f.Concurrency != 0 {
		base.Concurrency = f.Concurrency
	}
	return base
}

func (f FilterConfig) mergedTidal(base TidalConfig) TidalConfig {
	if f.Endpoint != "" {
		base.Endpoint = f.Endpoint
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
// filters (gemini/claude/chatgpt/tidal/bandwidth) and their params
// live in the top-level filters list, not here.
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

// GeofeedConfig configures the published-geofeed IP->country sources.
// RefreshInterval is a pointer so an unset value defaults (nil ->
// defaultGeoDBRefresh) while an explicit 0 is preserved and means "load once,
// never refresh" (preprocess treats a non-positive interval as disable). Before
// that distinction existed, omitting the key froze the geofeed for the whole
// process lifetime. The dbip/registry siblings spell 0 differently -- see
// DBIPConfig.applyDefaults.
type GeofeedConfig struct {
	Sources         []geofeed.Source `yaml:"sources"`
	RefreshInterval *time.Duration   `yaml:"refresh_interval"`
}

func (g *GeofeedConfig) applyDefaults() {
	if g.RefreshInterval == nil {
		g.RefreshInterval = new(defaultGeoDBRefresh)
	}
}

// DBIPConfig configures the DB-IP Country Lite IP->country database download
// (annotate provider "dbip"). The literal {yyyy-mm} placeholder in URL expands
// to the current UTC month at fetch time.
type DBIPConfig struct {
	URL string `yaml:"url"`
	// RefreshInterval is a pointer only so all three geo blocks decode alike;
	// nil and an explicit 0 mean the same thing here, unlike GeofeedConfig.
	RefreshInterval *time.Duration `yaml:"refresh_interval"`
}

// applyDefaults reads an explicit 0 as "use the default", which is what it
// meant before RefreshInterval became a pointer; taking it as "load once, never
// refresh" would silently flip the behaviour of a config already in the field.
// Disabling has no legitimate use for these two either: both defaults are
// moving targets (DB-IP's {yyyy-mm} URL rotates monthly, the RIR
// delegated-extended files are rewritten daily), so a frozen copy only rots,
// and refreshing rarely is already expressible as a long interval. It also keeps
// preprocess's geoDB out of a dead end -- with a non-positive interval
// staleLocked returns false before it consults retryAt, so the retry a failed
// initial download arms could never fire and the provider would stay empty for
// the process lifetime while the startup log promises one. A negative value
// still reaches Validate, so a typo stays loud.
func (d *DBIPConfig) applyDefaults() {
	if d.URL == "" {
		d.URL = defaultDBIPURL
	}
	if d.RefreshInterval == nil || *d.RefreshInterval == 0 {
		d.RefreshInterval = new(defaultGeoDBRefresh)
	}
}

func (d *DBIPConfig) validate() error {
	return validateDownloadURL("geo.dbip.url", d.URL)
}

// RegistryConfig configures the RIR delegated-extended IP->country database
// downloads (annotate provider "registry"), one URL per RIR.
type RegistryConfig struct {
	URLs []string `yaml:"urls"`
	// RefreshInterval carries DBIPConfig.RefreshInterval's semantics.
	RefreshInterval *time.Duration `yaml:"refresh_interval"`
}

// applyDefaults treats an explicit 0 as the default for the reasons spelled out
// on DBIPConfig.applyDefaults.
func (r *RegistryConfig) applyDefaults() {
	if len(r.URLs) == 0 {
		r.URLs = slices.Clone(defaultRegistryURLs)
	}
	if r.RefreshInterval == nil || *r.RefreshInterval == 0 {
		r.RefreshInterval = new(defaultGeoDBRefresh)
	}
}

func (r *RegistryConfig) validate() error {
	for i, u := range r.URLs {
		if err := validateDownloadURL(fmt.Sprintf("geo.registry.urls[%d]", i), u); err != nil {
			return err
		}
	}
	return nil
}

// validateDownloadURL requires an absolute https URL with a host. The literal
// {yyyy-mm} month placeholder is substituted before parsing so it never trips
// the URL parser.
func validateDownloadURL(name, raw string) error {
	u, err := url.Parse(strings.ReplaceAll(raw, "{yyyy-mm}", "2000-01"))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if u.Scheme != schemeHTTPS || u.Host == "" {
		return fmt.Errorf("%s: must be an absolute https URL, got %q", name, raw)
	}
	return nil
}

type Groups map[string][]string

type ASNConfig struct {
	Timeout  time.Duration `yaml:"timeout"`
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// GeoBlockConfig configures the per-node geo-block list -- a SQLite TTL store of
// node hosts that failed a through-node API reachability check (Gemini, Claude,
// ChatGPT) -- plus the base params of every through-node check. Tidal's
// params live here for uniformity even though the tidal filter deliberately
// never writes to the store: its verdict is a bare status code, a far weaker
// signal than the explicit refusal markers the other checks match, so a
// transient CDN error or rate-limit would otherwise evict the host from every
// endpoint for the store's whole TTL. Geotrace's live here for the same
// uniformity and never reach the store either -- it gates nothing, it only
// answers the annotate chain.
type GeoBlockConfig struct {
	DBPath   string         `yaml:"db_path"`
	TTL      time.Duration  `yaml:"ttl"`
	Gemini   GeminiConfig   `yaml:"gemini"`
	Claude   ClaudeConfig   `yaml:"claude"`
	ChatGPT  ChatGPTConfig  `yaml:"chatgpt"`
	Tidal    TidalConfig    `yaml:"tidal"`
	GeoTrace GeoTraceConfig `yaml:"geotrace"`
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

// ChatGPTConfig configures the through-node OpenAI reachability check. It needs
// no API key: the compliance endpoint refuses an unsupported egress with HTTP
// 403 and code "unsupported_country" before any credential is involved. The
// refusal tracks the egress, not the node's IP country -- nodes in supported
// countries are refused too, which is the point of checking through the node.
type ChatGPTConfig struct {
	Endpoint    string        `yaml:"endpoint"`
	Marker      string        `yaml:"marker"`
	Timeout     time.Duration `yaml:"timeout"`
	Concurrency int           `yaml:"concurrency"`
}

// TidalConfig configures the through-node Tidal reachability check. It needs no
// API key: GET /v1/country answers 200 {"countryCode":"XX"} for an egress Tidal
// accepts, and from one it refuses the request never reaches the API at all --
// CloudFront answers 403 with an HTML error page (measured from a Russian
// egress).
//
// The check deliberately does NOT compare that country against Tidal's list of
// markets. That list gates where a subscription can be BOUGHT; an existing
// subscriber streams fine from a country Tidal merely does not sell in (Tidal's
// terms key on residence, and its help centre documents no travel restriction
// at all). Only a hard refusal means the node is unusable, so only that drops
// it.
type TidalConfig struct {
	Endpoint    string        `yaml:"endpoint"`
	Timeout     time.Duration `yaml:"timeout"`
	Concurrency int           `yaml:"concurrency"`
}

// GeoTraceConfig configures the through-node egress lookup behind the
// "geotrace" annotate provider. It is no gate: it drops nothing and only tells
// the annotate chain where the node's traffic actually leaves from.
//
// The offline providers cannot know that. They place the address our resolver
// returned for the node's hostname, and 41% of the named hosts measured in the
// pool sit in Cloudflare's shared anycast ranges, which terminate in many
// countries at once -- so the tag described Cloudflare's registration while the
// traffic left from the origin (a node tagged CA exiting in Germany).
type GeoTraceConfig struct {
	Endpoint    string        `yaml:"endpoint"`
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
	cg := &g.ChatGPT
	if cg.Endpoint == "" {
		cg.Endpoint = defaultChatGPTEndpoint
	}
	if cg.Marker == "" {
		cg.Marker = defaultChatGPTMarker
	}
	if cg.Timeout == 0 {
		cg.Timeout = defaultChatGPTTimeout
	}
	if cg.Concurrency == 0 {
		cg.Concurrency = defaultChatGPTConcurrency
	}
	td := &g.Tidal
	if td.Endpoint == "" {
		td.Endpoint = defaultTidalEndpoint
	}
	if td.Timeout == 0 {
		td.Timeout = defaultTidalTimeout
	}
	if td.Concurrency == 0 {
		td.Concurrency = defaultTidalConcurrency
	}
	applyGeoTraceDefaults(&g.GeoTrace)
}

func applyGeoTraceDefaults(gt *GeoTraceConfig) {
	if gt.Endpoint == "" {
		gt.Endpoint = defaultGeoTraceEndpoint
	}
	if gt.Timeout == 0 {
		gt.Timeout = defaultGeoTraceTimeout
	}
	if gt.Concurrency == 0 {
		gt.Concurrency = defaultGeoTraceConcurrency
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

// decodeStrict decodes one YAML document into dst, rejecting any key the target
// type does not declare. Every setting of this service is a YAML key, so a
// non-strict decode makes a typo indistinguishable from an omission — and an
// omission silently means the built-in default: `max_avg_ms` mistyped restores
// the 1000ms cap over the tuned 800 with no signal anywhere.
//
// It is applied to the overlays too: their three-field source shape is a
// contract the crawler already validates before writing (crawl.validatePrivate),
// and an unknown key there would be a source the crawler meant to qualify and
// the service silently read plain.
//
// An empty or comment-only document yields io.EOF from Decode with dst
// untouched; that is not an error, because an absent-equivalent overlay is a
// valid "no sources".
func decodeStrict(name string, b []byte, dst any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("unmarshal %s: %w", name, err)
	}

	return nil
}

func Load(path string) (Config, error) {
	b, errRead := os.ReadFile(path)
	if errRead != nil {
		return Config{}, fmt.Errorf("read config file: %w", errRead)
	}

	var cfg Config
	if errUnmarshal := decodeStrict("config", b, &cfg); errUnmarshal != nil {
		return Config{}, errUnmarshal
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
	cfg.Geo.Geofeed.applyDefaults()
	cfg.Geo.DBIP.applyDefaults()
	cfg.Geo.Registry.applyDefaults()
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
		if unmarshalErr := decodeStrict("private config", privBytes, &priv); unmarshalErr != nil {
			return Config{}, unmarshalErr
		}
		cfg.Subscriptions.Sources = append(cfg.Subscriptions.Sources, priv.Subscriptions.Sources...)
		if validateErr := cfg.Subscriptions.validateSources(); validateErr != nil {
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
		if unmarshalErr := decodeStrict("sources config", b, &overlay); unmarshalErr != nil {
			return unmarshalErr
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
	if len(a.Providers) == 0 && a.Tag == TagGEO {
		a.Providers = []string{ProviderGeofeed}
	}
}

func (cfg *Config) Validate() error {
	if cfg.Log.Level != "" {
		if _, err := zerolog.ParseLevel(cfg.Log.Level); err != nil {
			return fmt.Errorf("log.level: %w", err)
		}
	}
	if err := validateResolverAddress(cfg.Resolver.Address); err != nil {
		return err
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
	if err := cfg.Geo.DBIP.validate(); err != nil {
		return err
	}
	if err := cfg.Geo.Registry.validate(); err != nil {
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

// validateNonNegative rejects negative durations. The cache TTLs and the three
// geo refresh intervals are pointers (nil-checked) because an explicit 0 is
// valid and means "disable".
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
	if cfg.Geo.Geofeed.RefreshInterval != nil && *cfg.Geo.Geofeed.RefreshInterval < 0 {
		return errors.New("geo.geofeed.refresh_interval must not be negative")
	}
	if cfg.Geo.DBIP.RefreshInterval != nil && *cfg.Geo.DBIP.RefreshInterval < 0 {
		return errors.New("geo.dbip.refresh_interval must not be negative")
	}
	if cfg.Geo.Registry.RefreshInterval != nil && *cfg.Geo.Registry.RefreshInterval < 0 {
		return errors.New("geo.registry.refresh_interval must not be negative")
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

// validateResolverAddress requires an explicit host:port -- the string
// net.Dialer receives verbatim for every DNS query. A portless value dials
// nothing, so every lookup fails and every node is dropped as a DNS failure:
// a total outage produced by one missing ":53", previously accepted silently by
// both startup and reload. An empty address keeps the system resolver.
func validateResolverAddress(addr string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("resolver.address %q: %w (want host:port, e.g. 1.1.1.1:53)", addr, err)
	}
	if host == "" || port == "" {
		return fmt.Errorf("resolver.address %q: must be host:port, e.g. 1.1.1.1:53", addr)
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
	case FilterGemini, FilterClaude, FilterChatGPT, FilterTidal:
		return validateAPIFilter(i, f)
	case FilterBandwidth:
		return f.validateBandwidth(i)
	default:
		return fmt.Errorf("filters[%d]: unknown type %q (must be one of: %s)", i, f.Type,
			strings.Join([]string{
				FilterCountry, FilterASN, FilterGemini, FilterClaude,
				FilterChatGPT, FilterTidal, FilterBandwidth,
			}, ", "))
	}
}

func (cfg *Config) validateCountryFilter(i int, f FilterConfig) error {
	if f.Provider != ProviderGeofeed && f.Provider != ProviderASN {
		return fmt.Errorf("filters[%d]: country provider must be %q or %q, got %q", i, ProviderGeofeed, ProviderASN, f.Provider)
	}
	if err := validateCountryList(fmt.Sprintf("filters[%d].exclude_countries", i), f.ExcludeCountries); err != nil {
		return err
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

func validateCountryList(name string, codes []string) error {
	for _, c := range codes {
		if err := validateCountryCode(name, c); err != nil {
			return err
		}
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
		if (u.Scheme != "http" && u.Scheme != schemeHTTPS) || u.Host == "" {
			return fmt.Errorf("filters[%d].test_url: must be an absolute http(s) URL, got %q", i, f.TestURL)
		}
	}
	return nil
}

// validateAnnotate rejects unknown tags and invalid provider chains.
func (cfg *Config) validateAnnotate() error {
	for i, a := range cfg.Annotate {
		if a.Tag != TagGEO {
			return fmt.Errorf("annotate[%d]: unknown tag %q (must be %q)", i, a.Tag, TagGEO)
		}
		if len(a.Providers) == 0 {
			return fmt.Errorf("annotate[%d]: tag %s requires at least one provider", i, a.Tag)
		}
		if err := validateProviderChain(i, a.Providers); err != nil {
			return err
		}
	}
	return nil
}

func validateProviderChain(i int, providers []string) error {
	for j, p := range providers {
		switch p {
		case ProviderGeoTrace, ProviderGeofeed, ProviderDBIP, ProviderRegistry, ProviderASN:
		default:
			return fmt.Errorf("annotate[%d]: unknown provider %q (must be %q, %q, %q, %q or %q)",
				i, p, ProviderGeoTrace, ProviderGeofeed, ProviderDBIP, ProviderRegistry, ProviderASN)
		}
		if slices.Contains(providers[:j], p) {
			return fmt.Errorf("annotate[%d]: duplicate provider %q", i, p)
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
	if g.ChatGPT.Timeout < 0 {
		return errors.New("geoblock.chatgpt.timeout must not be negative")
	}
	if g.ChatGPT.Concurrency < 0 {
		return errors.New("geoblock.chatgpt.concurrency must not be negative")
	}
	if g.Tidal.Timeout < 0 {
		return errors.New("geoblock.tidal.timeout must not be negative")
	}
	if g.Tidal.Concurrency < 0 {
		return errors.New("geoblock.tidal.concurrency must not be negative")
	}
	if g.GeoTrace.Timeout < 0 {
		return errors.New("geoblock.geotrace.timeout must not be negative")
	}
	if g.GeoTrace.Concurrency < 0 {
		return errors.New("geoblock.geotrace.concurrency must not be negative")
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

// Validate checks the prober parameters unconditionally, then the merged source
// list. The parameters are independent of where sources come from, and in this
// deployment every source arrives from an overlay -- gating their validation on
// a non-empty list meant a bad subscriptions.interval in config.yaml booted
// clean and then failed EVERY reload from the moment the crawler wrote the
// first source.
func (s *SubscriptionsConfig) Validate() error {
	if s.Interval < minSubsInterval {
		return fmt.Errorf("subscriptions.interval must be at least %v", minSubsInterval)
	}
	if err := s.Check.validate(); err != nil {
		return err
	}
	return s.validateSources()
}

// validateSources checks the merged source list alone. Load re-runs it after
// merging an overlay, so the error it blames on that overlay is always about
// entries the overlay actually contributed.
func (s *SubscriptionsConfig) validateSources() error {
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
		if (u.Scheme != "http" && u.Scheme != schemeHTTPS) || u.Host == "" {
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

// ProberChanged reports whether the through-node prober settings
// (gemini/claude/chatgpt/tidal/geotrace) differ; the stable worker must be
// re-applied when they do. The store-only geoblock fields (db_path, ttl) are
// covered by StoresChanged instead.
func ProberChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.GeoBlock.Gemini, newCfg.GeoBlock.Gemini) ||
		!reflect.DeepEqual(old.GeoBlock.Claude, newCfg.GeoBlock.Claude) ||
		!reflect.DeepEqual(old.GeoBlock.ChatGPT, newCfg.GeoBlock.ChatGPT) ||
		!reflect.DeepEqual(old.GeoBlock.Tidal, newCfg.GeoBlock.Tidal) ||
		!reflect.DeepEqual(old.GeoBlock.GeoTrace, newCfg.GeoBlock.GeoTrace)
}

// StoresChanged reports whether a setting baked into the stores built once at
// startup changed: geoblock.db_path / geoblock.ttl (SQLite blocklist) or
// deadcache.ttl. Such a change requires a restart to take effect.
func StoresChanged(old, newCfg Config) bool {
	return old.GeoBlock.DBPath != newCfg.GeoBlock.DBPath ||
		old.GeoBlock.TTL != newCfg.GeoBlock.TTL ||
		!reflect.DeepEqual(old.DeadCache.TTL, newCfg.DeadCache.TTL)
}

// AnnotateChanged reports whether the annotate tag list differs. Both consumers
// bake it in: the processor renders the per-node [GEO:] tags from it, and
// the stable worker builds published names once per cycle, prepending its own
// [SPD:] prefix to those same tags. So the reloader must rebuild/re-apply when
// the list changes; otherwise the published annotation stays stale.
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
		if err := validateCountryList("groups."+name, countries); err != nil {
			return err
		}
	}
	return nil
}

func validateCountryCode(name, c string) error {
	c = strings.TrimSpace(c)
	if len(c) != 2 { //nolint:mnd // ISO 3166-1 alpha-2 country code length
		return fmt.Errorf("%s: invalid country code %q", name, c)
	}
	if !isASCIILetter(c[0]) || !isASCIILetter(c[1]) {
		return fmt.Errorf("%s: invalid country code %q", name, c)
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

// DBIPChanged / RegistryChanged report whether the corresponding downloadable
// database settings differ; the reloader carries over the loaded lookup when
// they don't, avoiding a multi-MB re-download per reload.
func DBIPChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Geo.DBIP, newCfg.Geo.DBIP)
}

func RegistryChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Geo.Registry, newCfg.Geo.Registry)
}

// ResolverChanged / ASNChanged report whether the DNS or Cymru resolver
// settings differ. The reloader carries the live resolver — and with it its
// warm cache — across a rebuild when they don't, so the configured cache_ttl
// can actually be reached instead of being reset on every reload.
//
// Both compare the whole block rather than the address/URL alone: the timeout
// and the TTLs are baked into the resolver at construction, so carrying one
// across a cache_ttl edit would silently keep serving the old TTL.
func ResolverChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Resolver, newCfg.Resolver)
}

func ASNChanged(old, newCfg Config) bool {
	return !reflect.DeepEqual(old.Geo.ASN, newCfg.Geo.ASN)
}

func ListenChanged(old, newCfg Config) bool {
	return old.Server.Listen != newCfg.Server.Listen
}

// MetricsListenChanged reports a change to server.metrics_listen. The metrics
// listener is started once in app.Run and never re-applied, so a change
// requires a restart.
func MetricsListenChanged(old, newCfg Config) bool {
	return old.Server.MetricsListen != newCfg.Server.MetricsListen
}
