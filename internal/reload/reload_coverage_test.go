package reload_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/reload"
)

// Reload classification of every yaml leaf key. The reload pipeline applies a
// changed config through exactly one of these paths:
//
//	live-processor: reload.OptionsFromConfig -> preprocess.NewProcessor (or the
//	                Groups map in the same holder snapshot), rebuilt on every
//	                non-Equal reload.
//	live-worker:    stable.Controller.Apply, gated by one of the Changed()
//	                helpers in the reloader's subsAffected condition.
//	live-both:      consumed by both paths above.
//	live-other:     dedicated handler in Reloader.Reload (e.g. log.SetLevel).
//	restart-warned: consumed once at startup; the reloader logs a
//	                restart-required warning on change.
//
// Two tests enforce the table, and they check different things:
//
//   - TestReloadCoverageComplete walks config.Config and fails when a yaml leaf
//     is missing here, so a new field cannot ship unclassified.
//   - TestReloadClassificationMatchesBehaviour mutates each key in isolation and
//     asserts the recorded class is the path the reloader actually takes, so a
//     row cannot claim a reach it does not have. Table membership alone proved
//     nothing about behaviour: a key recorded live-both while the processor
//     discarded it kept the suite green.
//
// The table lives here rather than in internal/config because the behavioural
// half needs OptionsFromConfig, and one table with both guarantees beats two
// tables that drift.
const (
	liveProcessor = "live-processor"
	liveWorker    = "live-worker"
	liveBoth      = "live-both"
	liveOther     = "live-other"
	restartWarned = "restart-warned"
)

var reloadClassification = map[string]string{
	"log.level": liveOther,

	"server.listen":         restartWarned,
	"server.metrics_listen": restartWarned,

	"geo.geofeed.sources[].url":     liveProcessor,
	"geo.geofeed.sources[].type":    liveProcessor,
	"geo.geofeed.refresh_interval":  liveProcessor,
	"geo.dbip.url":                  liveProcessor,
	"geo.dbip.refresh_interval":     liveProcessor,
	"geo.registry.urls":             liveProcessor,
	"geo.registry.refresh_interval": liveProcessor,
	"geo.asn.timeout":               liveProcessor,
	"geo.asn.cache_ttl":             liveProcessor,

	"resolver.address":            liveProcessor,
	"resolver.timeout":            liveProcessor,
	"resolver.cache_ttl":          liveProcessor,
	"resolver.cache_negative_ttl": liveProcessor,

	// Only type/provider/deny_patterns reach the IP-stage chain. The remaining
	// filter keys configure through-node gates the processor never builds, so
	// they are worker-only however much they sit in the same list.
	"filters[].type":              liveBoth,
	"filters[].provider":          liveBoth,
	"filters[].deny_patterns":     liveBoth,
	"filters[].exclude_groups":    liveWorker,
	"filters[].exclude_countries": liveWorker,
	"filters[].min_mbps":          liveWorker,
	"filters[].test_url":          liveWorker,
	"filters[].timeout":           liveWorker,
	"filters[].concurrency":       liveWorker,
	"filters[].marker":            liveWorker,
	"filters[].model":             liveWorker,
	"filters[].endpoint":          liveWorker,
	"filters[].api_key":           liveWorker,
	"filters[].key_file":          liveWorker,
	"filters[].key_var":           liveWorker,
	"filters[].version":           liveWorker,

	"annotate[].tag":       liveBoth,
	"annotate[].providers": liveBoth,

	// groups reaches / through the holder snapshot rather than Options, and the
	// worker through GroupsChanged.
	"groups": liveBoth,

	"subscriptions.interval":              liveWorker,
	"subscriptions.sources[].name":        liveWorker,
	"subscriptions.sources[].url":         liveWorker,
	"subscriptions.sources[].body":        liveWorker,
	"subscriptions.check.rounds":          liveWorker,
	"subscriptions.check.timeout":         liveWorker,
	"subscriptions.check.max_fail":        liveWorker,
	"subscriptions.check.max_avg_ms":      liveWorker,
	"subscriptions.check.test_url":        liveWorker,
	"subscriptions.check.expected_status": liveWorker,
	"subscriptions.check.concurrency":     liveWorker,
	"subscriptions.check.source_timeout":  liveWorker,

	"geoblock.db_path":             restartWarned,
	"geoblock.ttl":                 restartWarned,
	"geoblock.gemini.endpoint":     liveWorker,
	"geoblock.gemini.model":        liveWorker,
	"geoblock.gemini.marker":       liveWorker,
	"geoblock.gemini.api_key":      liveWorker,
	"geoblock.gemini.key_file":     liveWorker,
	"geoblock.gemini.key_var":      liveWorker,
	"geoblock.gemini.timeout":      liveWorker,
	"geoblock.gemini.concurrency":  liveWorker,
	"geoblock.claude.endpoint":     liveWorker,
	"geoblock.claude.marker":       liveWorker,
	"geoblock.claude.version":      liveWorker,
	"geoblock.claude.timeout":      liveWorker,
	"geoblock.claude.concurrency":  liveWorker,
	"geoblock.chatgpt.endpoint":    liveWorker,
	"geoblock.chatgpt.marker":      liveWorker,
	"geoblock.chatgpt.timeout":     liveWorker,
	"geoblock.chatgpt.concurrency": liveWorker,
	"geoblock.tidal.endpoint":      liveWorker,
	"geoblock.tidal.timeout":       liveWorker,
	"geoblock.tidal.concurrency":   liveWorker,

	"deadcache.ttl": restartWarned,

	"fetch.timeout": liveProcessor,
}

// coverageBase is the config every behavioural mutation starts from. Every field
// a mutator touches holds a value the mutation actually changes, and the filters
// list carries one entry per shape the keys address (country, asn, bandwidth,
// gemini, claude) so a per-entry key can be mutated in isolation.
func coverageBase() config.Config {
	cfg := config.Config{
		Log: config.LogConfig{Level: "info"},
		Geo: config.GeoConfig{
			Geofeed: config.GeofeedConfig{
				Sources:         []geofeed.Source{{URL: "https://example.com/feed.csv", Type: "raw"}},
				RefreshInterval: new(24 * time.Hour),
			},
			DBIP: config.DBIPConfig{
				URL:             "https://example.com/db-{yyyy-mm}.csv.gz",
				RefreshInterval: new(24 * time.Hour),
			},
			Registry: config.RegistryConfig{
				URLs:            []string{"https://example.com/delegated"},
				RefreshInterval: new(24 * time.Hour),
			},
			ASN: config.ASNConfig{Timeout: 5 * time.Second, CacheTTL: 24 * time.Hour},
		},
		Filters: []config.FilterConfig{
			{
				Type: config.FilterCountry, Provider: config.ProviderGeofeed,
				ExcludeGroups: []string{"blocked"}, ExcludeCountries: []string{"RU"},
			},
			{Type: config.FilterASN, DenyPatterns: []string{"spammy"}},
			{
				Type: config.FilterBandwidth, MinMbps: new(5),
				TestURL: "https://speed.example.com/x", Timeout: 20 * time.Second, Concurrency: 4,
			},
			{
				Type: config.FilterGemini, Marker: "blocked-marker", Model: "flash",
				Endpoint: "https://gemini.example.com", APIKey: "key", KeyFile: "/run/key", KeyVar: "KEY",
			},
			{Type: config.FilterClaude, Version: "2023-06-01"},
		},
		Annotate: []config.AnnotateSpec{{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}}},
		Groups:   config.Groups{"blocked": {"RU"}},
		Subscriptions: config.SubscriptionsConfig{
			Interval: 30 * time.Minute,
			Check: config.CheckConfig{
				Rounds: 5, Timeout: 2 * time.Second, TestURL: "https://gstatic.example.com/generate_204",
				ExpectedStatus: "204", MaxFail: 1, MaxAvgMs: 1000,
				SourceTimeout: 2 * time.Minute, Concurrency: 16,
			},
			Sources: []config.SubscriptionSource{{Name: "alpha", URL: "https://alpha.example.com/s", Body: "seed"}},
		},
		GeoBlock: config.GeoBlockConfig{
			DBPath:  "/var/lib/geoblock.db",
			TTL:     720 * time.Hour,
			Gemini:  config.GeminiConfig{Endpoint: "https://gemini.example.com", Model: "flash", Marker: "m", APIKey: "k", KeyFile: "/f", KeyVar: "V", Timeout: 15 * time.Second, Concurrency: 8},
			Claude:  config.ClaudeConfig{Endpoint: "https://claude.example.com", Marker: "m", Version: "2023-06-01", Timeout: 15 * time.Second, Concurrency: 8},
			ChatGPT: config.ChatGPTConfig{Endpoint: "https://openai.example.com", Marker: "m", Timeout: 15 * time.Second, Concurrency: 8},
			Tidal:   config.TidalConfig{Endpoint: "https://tidal.example.com", Timeout: 15 * time.Second, Concurrency: 8},
		},
		DeadCache: config.DeadCacheConfig{TTL: new(2 * time.Hour)},
		Fetch:     config.FetchConfig{Timeout: 3 * time.Second},
	}
	cfg.Server.Listen = ":8080"
	cfg.Server.MetricsListen = ":9090"
	cfg.Resolver.Address = "1.1.1.1:53"
	cfg.Resolver.Timeout = 5 * time.Second
	cfg.Resolver.CacheTTL = new(30 * time.Minute)
	cfg.Resolver.CacheNegativeTTL = new(10 * time.Minute)

	return cfg
}

// keyMutators changes exactly one yaml key per entry, so the reload path it
// trips is attributable to that key alone. Filter indices follow coverageBase:
// 0 country, 1 asn, 2 bandwidth, 3 gemini, 4 claude.
var keyMutators = map[string]func(*config.Config){
	"log.level": func(c *config.Config) { c.Log.Level = "debug" },

	"server.listen":         func(c *config.Config) { c.Server.Listen = ":9999" },
	"server.metrics_listen": func(c *config.Config) { c.Server.MetricsListen = ":9998" },

	"geo.geofeed.sources[].url":  func(c *config.Config) { c.Geo.Geofeed.Sources[0].URL = "https://other.example.com/feed" },
	"geo.geofeed.sources[].type": func(c *config.Config) { c.Geo.Geofeed.Sources[0].Type = "gzip" },
	"geo.geofeed.refresh_interval": func(c *config.Config) {
		c.Geo.Geofeed.RefreshInterval = new(time.Hour)
	},
	"geo.dbip.url":              func(c *config.Config) { c.Geo.DBIP.URL = "https://other.example.com/db.csv.gz" },
	"geo.dbip.refresh_interval": func(c *config.Config) { c.Geo.DBIP.RefreshInterval = new(time.Hour) },
	"geo.registry.urls": func(c *config.Config) {
		c.Geo.Registry.URLs = []string{"https://other.example.com/delegated"}
	},
	"geo.registry.refresh_interval": func(c *config.Config) { c.Geo.Registry.RefreshInterval = new(time.Hour) },
	"geo.asn.timeout":               func(c *config.Config) { c.Geo.ASN.Timeout = 9 * time.Second },
	"geo.asn.cache_ttl":             func(c *config.Config) { c.Geo.ASN.CacheTTL = 48 * time.Hour },

	"resolver.address":            func(c *config.Config) { c.Resolver.Address = "9.9.9.9:53" },
	"resolver.timeout":            func(c *config.Config) { c.Resolver.Timeout = 9 * time.Second },
	"resolver.cache_ttl":          func(c *config.Config) { c.Resolver.CacheTTL = new(time.Hour) },
	"resolver.cache_negative_ttl": func(c *config.Config) { c.Resolver.CacheNegativeTTL = new(time.Minute) },

	"filters[].type":     func(c *config.Config) { c.Filters[0].Type = config.FilterASN },
	"filters[].provider": func(c *config.Config) { c.Filters[0].Provider = config.ProviderASN },
	"filters[].deny_patterns": func(c *config.Config) {
		c.Filters[1].DenyPatterns = []string{"other"}
	},
	"filters[].exclude_groups": func(c *config.Config) { c.Filters[0].ExcludeGroups = nil },
	"filters[].exclude_countries": func(c *config.Config) {
		c.Filters[0].ExcludeCountries = []string{"CN"}
	},
	"filters[].min_mbps":    func(c *config.Config) { c.Filters[2].MinMbps = new(9) },
	"filters[].test_url":    func(c *config.Config) { c.Filters[2].TestURL = "https://other.example.com/x" },
	"filters[].timeout":     func(c *config.Config) { c.Filters[2].Timeout = 30 * time.Second },
	"filters[].concurrency": func(c *config.Config) { c.Filters[2].Concurrency = 2 },
	"filters[].marker":      func(c *config.Config) { c.Filters[3].Marker = "other-marker" },
	"filters[].model":       func(c *config.Config) { c.Filters[3].Model = "pro" },
	"filters[].endpoint":    func(c *config.Config) { c.Filters[3].Endpoint = "https://other.example.com" },
	"filters[].api_key":     func(c *config.Config) { c.Filters[3].APIKey = "other-key" },
	"filters[].key_file":    func(c *config.Config) { c.Filters[3].KeyFile = "/run/other" },
	"filters[].key_var":     func(c *config.Config) { c.Filters[3].KeyVar = "OTHER" },
	"filters[].version":     func(c *config.Config) { c.Filters[4].Version = "2024-01-01" },

	"annotate[].tag": func(c *config.Config) { c.Annotate[0].Tag = config.TagASN },
	"annotate[].providers": func(c *config.Config) {
		c.Annotate[0].Providers = []string{config.ProviderDBIP}
	},

	"groups": func(c *config.Config) { c.Groups = config.Groups{"blocked": {"CN"}} },

	"subscriptions.interval": func(c *config.Config) { c.Subscriptions.Interval = time.Hour },
	"subscriptions.sources[].name": func(c *config.Config) {
		c.Subscriptions.Sources[0].Name = "beta"
	},
	"subscriptions.sources[].url": func(c *config.Config) {
		c.Subscriptions.Sources[0].URL = "https://beta.example.com/s"
	},
	"subscriptions.sources[].body":        func(c *config.Config) { c.Subscriptions.Sources[0].Body = "other" },
	"subscriptions.check.rounds":          func(c *config.Config) { c.Subscriptions.Check.Rounds = 3 },
	"subscriptions.check.timeout":         func(c *config.Config) { c.Subscriptions.Check.Timeout = time.Second },
	"subscriptions.check.max_fail":        func(c *config.Config) { c.Subscriptions.Check.MaxFail = 2 },
	"subscriptions.check.max_avg_ms":      func(c *config.Config) { c.Subscriptions.Check.MaxAvgMs = 800 },
	"subscriptions.check.test_url":        func(c *config.Config) { c.Subscriptions.Check.TestURL = "https://other.example.com/204" },
	"subscriptions.check.expected_status": func(c *config.Config) { c.Subscriptions.Check.ExpectedStatus = "200" },
	"subscriptions.check.concurrency":     func(c *config.Config) { c.Subscriptions.Check.Concurrency = 8 },
	"subscriptions.check.source_timeout":  func(c *config.Config) { c.Subscriptions.Check.SourceTimeout = 5 * time.Minute },

	"geoblock.db_path":             func(c *config.Config) { c.GeoBlock.DBPath = "/var/lib/other.db" },
	"geoblock.ttl":                 func(c *config.Config) { c.GeoBlock.TTL = time.Hour },
	"geoblock.gemini.endpoint":     func(c *config.Config) { c.GeoBlock.Gemini.Endpoint = "https://other.example.com" },
	"geoblock.gemini.model":        func(c *config.Config) { c.GeoBlock.Gemini.Model = "pro" },
	"geoblock.gemini.marker":       func(c *config.Config) { c.GeoBlock.Gemini.Marker = "other" },
	"geoblock.gemini.api_key":      func(c *config.Config) { c.GeoBlock.Gemini.APIKey = "other" },
	"geoblock.gemini.key_file":     func(c *config.Config) { c.GeoBlock.Gemini.KeyFile = "/other" },
	"geoblock.gemini.key_var":      func(c *config.Config) { c.GeoBlock.Gemini.KeyVar = "OTHER" },
	"geoblock.gemini.timeout":      func(c *config.Config) { c.GeoBlock.Gemini.Timeout = 30 * time.Second },
	"geoblock.gemini.concurrency":  func(c *config.Config) { c.GeoBlock.Gemini.Concurrency = 4 },
	"geoblock.claude.endpoint":     func(c *config.Config) { c.GeoBlock.Claude.Endpoint = "https://other.example.com" },
	"geoblock.claude.marker":       func(c *config.Config) { c.GeoBlock.Claude.Marker = "other" },
	"geoblock.claude.version":      func(c *config.Config) { c.GeoBlock.Claude.Version = "2024-01-01" },
	"geoblock.claude.timeout":      func(c *config.Config) { c.GeoBlock.Claude.Timeout = 30 * time.Second },
	"geoblock.claude.concurrency":  func(c *config.Config) { c.GeoBlock.Claude.Concurrency = 4 },
	"geoblock.chatgpt.endpoint":    func(c *config.Config) { c.GeoBlock.ChatGPT.Endpoint = "https://other.example.com" },
	"geoblock.chatgpt.marker":      func(c *config.Config) { c.GeoBlock.ChatGPT.Marker = "other" },
	"geoblock.chatgpt.timeout":     func(c *config.Config) { c.GeoBlock.ChatGPT.Timeout = 30 * time.Second },
	"geoblock.chatgpt.concurrency": func(c *config.Config) { c.GeoBlock.ChatGPT.Concurrency = 4 },
	"geoblock.tidal.endpoint":      func(c *config.Config) { c.GeoBlock.Tidal.Endpoint = "https://other.example.com" },
	"geoblock.tidal.timeout":       func(c *config.Config) { c.GeoBlock.Tidal.Timeout = 30 * time.Second },
	"geoblock.tidal.concurrency":   func(c *config.Config) { c.GeoBlock.Tidal.Concurrency = 4 },

	"deadcache.ttl": func(c *config.Config) { c.DeadCache.TTL = new(time.Hour) },

	"fetch.timeout": func(c *config.Config) { c.Fetch.Timeout = 7 * time.Second },
}

// TestReloadCoverageComplete asserts the classification table and the Config
// struct describe exactly the same set of yaml leaf keys, so a config field can
// never be added without deciding how it reloads.
func TestReloadCoverageComplete(t *testing.T) {
	t.Parallel()

	leaves := map[string]bool{}
	collectYAMLLeaves(reflect.TypeFor[config.Config](), "", leaves)

	for leaf := range leaves {
		if _, ok := reloadClassification[leaf]; !ok {
			t.Errorf("config key %q has no reload classification: decide how it reaches the running service (OptionsFromConfig, a Changed() gate, or a restart warning) and add it to reloadClassification", leaf)
		}
	}
	for leaf, class := range reloadClassification {
		if !leaves[leaf] {
			t.Errorf("reloadClassification lists %q (%s) but config.Config has no such yaml key; remove the stale row", leaf, class)
		}
	}
}

// TestReloadClassificationMatchesBehaviour changes one key at a time and asserts
// the recorded class is the path Reload actually takes for it: which of the
// holder inputs (Options + Groups) the edit rebuilds, whether it trips the
// subsAffected gate that re-applies the worker, and whether it only earns a
// restart warning. Without this, a row could claim any reach it liked.
func TestReloadClassificationMatchesBehaviour(t *testing.T) {
	t.Parallel()

	for key, class := range reloadClassification {
		if reach, ok := mutatedReach(t, key, class); ok {
			checkClassReach(t, key, class, reach)
		}
	}

	for key := range keyMutators {
		if _, ok := reloadClassification[key]; !ok {
			t.Errorf("keyMutators has a stale entry %q with no classification row", key)
		}
	}
}

// reachedPaths is which of the reloader's three paths one config edit trips.
type reachedPaths struct {
	proc    bool
	worker  bool
	restart bool
}

// mutatedReach edits key in isolation on a pristine config and measures where
// the edit lands. ok is false when the row could not be exercised at all — a
// failure in itself, since an unexercised row is free to claim any reach.
func mutatedReach(t *testing.T, key, class string) (reachedPaths, bool) {
	t.Helper()

	mutate, ok := keyMutators[key]
	if !ok {
		t.Errorf("%q (%s) has no mutator: add one so the classification is checked against behaviour, not just declared", key, class)

		return reachedPaths{}, false
	}
	base, changed := coverageBase(), coverageBase()
	mutate(&changed)
	if config.Equal(base, changed) {
		t.Errorf("%q: mutator changed nothing, so it proves nothing", key)

		return reachedPaths{}, false
	}

	return reachedPaths{
		proc:    requestPathAffected(base, changed),
		worker:  workerAffected(base, changed),
		restart: restartAffected(base, changed),
	}, true
}

// checkClassReach holds the measured reach against what the class promises.
// The three live-* classes say nothing about restart because the reloader warns
// off the same diffs it applies; only the two classes that claim no live reach
// at all are pinned on it.
func checkClassReach(t *testing.T, key, class string, reach reachedPaths) {
	t.Helper()

	proc, worker, restart := reach.proc, reach.worker, reach.restart
	switch class {
	case liveProcessor:
		if !proc || worker {
			t.Errorf("%q is %s but proc=%v worker=%v: a live-processor key must rebuild what / serves and leave the worker alone", key, class, proc, worker)
		}
	case liveWorker:
		if proc || !worker {
			t.Errorf("%q is %s but proc=%v worker=%v: a live-worker key must trip a subsAffected gate and not change the processor inputs", key, class, proc, worker)
		}
	case liveBoth:
		if !proc || !worker {
			t.Errorf("%q is %s but proc=%v worker=%v: a live-both key must reach both paths", key, class, proc, worker)
		}
	case liveOther:
		if proc || worker || restart {
			t.Errorf("%q is %s but proc=%v worker=%v restart=%v: a live-other key is handled by its own branch in Reload, not by these gates", key, class, proc, worker, restart)
		}
	case restartWarned:
		if proc || worker || !restart {
			t.Errorf("%q is %s but proc=%v worker=%v restart=%v: a restart-warned key must trip only a restart-warning diff", key, class, proc, worker, restart)
		}
	default:
		t.Errorf("%q: unknown classification %q", key, class)
	}
}

// requestPathAffected reports whether the edit changes either input the reloader
// swaps into the holder: the processor's Options or the Groups map served
// alongside it.
func requestPathAffected(base, changed config.Config) bool {
	// The compare stays structural on the whole Options value on purpose: any
	// future config-derived field then counts as a processor-input change
	// without this test being taught about it. deepequalerrors fires because
	// Options can reach an error through PreloadedASN's result cache, but both
	// operands come straight out of OptionsFromConfig, which leaves every
	// Preloaded* field nil — TestOptionsFromConfig locks that — so DeepEqual
	// never reaches an error value and its identity comparison cannot mislead.
	optsChanged := !reflect.DeepEqual( //nolint:govet // no error value is reachable; see above
		reload.OptionsFromConfig(base), reload.OptionsFromConfig(changed))
	return optsChanged || config.GroupsChanged(base, changed)
}

// workerAffected mirrors the reloader's subsAffected condition verbatim.
func workerAffected(base, changed config.Config) bool {
	return config.SubscriptionsChanged(base, changed) ||
		config.GroupsChanged(base, changed) ||
		config.FiltersChanged(base, changed) ||
		config.ProberChanged(base, changed) ||
		config.AnnotateChanged(base, changed)
}

// restartAffected covers the three diffs Reload only warns about.
func restartAffected(base, changed config.Config) bool {
	return config.ListenChanged(base, changed) ||
		config.MetricsListenChanged(base, changed) ||
		config.StoresChanged(base, changed)
}

// collectYAMLLeaves records the yaml path of every leaf field reachable from t.
// Traversal is total on purpose: pointers are dereferenced and slice/array/map
// element structs are followed, so no *T, []*T or map[string]T block can hide
// its keys behind one unclassified leaf. Untagged fields are not skipped either
// — yaml.v3 decodes an embedded struct's fields at the parent level and any
// other exported field under its lowercased name, so both are reachable keys.
func collectYAMLLeaves(t reflect.Type, prefix string, out map[string]bool) {
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "-" {
			continue
		}
		ft := derefType(f.Type)
		if tag == "" {
			if f.Anonymous && ft.Kind() == reflect.Struct {
				collectYAMLLeaves(ft, prefix, out)

				continue
			}
			tag = strings.ToLower(f.Name)
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		collectTypeLeaves(ft, path, out)
	}
}

// collectTypeLeaves records path itself, or the keys nested inside it when the
// type behind the key is an aggregate of structs.
func collectTypeLeaves(ft reflect.Type, path string, out map[string]bool) {
	switch {
	case ft == reflect.TypeFor[time.Duration]():
		// A Duration is an integer kind but a leaf value, not an aggregate.
		out[path] = true
	case ft.Kind() == reflect.Struct:
		collectYAMLLeaves(ft, path, out)
	case ft.Kind() == reflect.Slice, ft.Kind() == reflect.Array, ft.Kind() == reflect.Map:
		if elem := derefType(ft.Elem()); elem.Kind() == reflect.Struct {
			collectYAMLLeaves(elem, path+"[]", out)

			return
		}
		out[path] = true
	default:
		out[path] = true
	}
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t
}
