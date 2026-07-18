package config_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/config"
)

// Reload classification of every yaml leaf key. The reload pipeline applies a
// changed config through exactly one of these paths:
//
//	live-processor: reload.OptionsFromConfig -> preprocess.NewProcessor, rebuilt
//	                on every non-Equal reload.
//	live-worker:    stable.Controller.Apply, gated by one of the Changed()
//	                helpers in the reloader's subsAffected condition.
//	live-both:      consumed by both paths above.
//	live-other:     dedicated handler in Reloader.Reload (e.g. log.SetLevel).
//	restart-warned: consumed once at startup; the reloader logs a
//	                restart-required warning on change.
//	validation-only: never read at runtime (e.g. the retired annotate
//	                "provider" key kept solely to reject with a rename error).
//
// TestReloadCoverageComplete walks config.Config and fails when a yaml leaf is
// missing here — classify every new config field (and give it a reload path or
// a restart warning) before shipping it.
const (
	liveProcessor  = "live-processor"
	liveWorker     = "live-worker"
	liveBoth       = "live-both"
	liveOther      = "live-other"
	restartWarned  = "restart-warned"
	validationOnly = "validation-only"
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

	"filters[].type":              liveBoth,
	"filters[].provider":          liveBoth,
	"filters[].exclude_groups":    liveBoth,
	"filters[].exclude_countries": liveBoth,
	"filters[].deny_patterns":     liveBoth,
	"filters[].min_mbps":          liveBoth,
	"filters[].test_url":          liveBoth,
	"filters[].timeout":           liveBoth,
	"filters[].concurrency":       liveBoth,
	"filters[].marker":            liveBoth,
	"filters[].model":             liveBoth,
	"filters[].endpoint":          liveBoth,
	"filters[].api_key":           liveBoth,
	"filters[].key_file":          liveBoth,
	"filters[].key_var":           liveBoth,
	"filters[].version":           liveBoth,

	"annotate[].tag":       liveBoth,
	"annotate[].providers": liveBoth,
	"annotate[].provider":  validationOnly,

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

	"geoblock.db_path":            restartWarned,
	"geoblock.ttl":                restartWarned,
	"geoblock.gemini.endpoint":    liveWorker,
	"geoblock.gemini.model":       liveWorker,
	"geoblock.gemini.marker":      liveWorker,
	"geoblock.gemini.api_key":     liveWorker,
	"geoblock.gemini.key_file":    liveWorker,
	"geoblock.gemini.key_var":     liveWorker,
	"geoblock.gemini.timeout":     liveWorker,
	"geoblock.gemini.concurrency": liveWorker,
	"geoblock.claude.endpoint":    liveWorker,
	"geoblock.claude.marker":      liveWorker,
	"geoblock.claude.version":     liveWorker,
	"geoblock.claude.timeout":     liveWorker,
	"geoblock.claude.concurrency": liveWorker,

	"deadcache.ttl": restartWarned,

	"fetch.timeout": liveProcessor,
}

// TestReloadCoverageComplete asserts the classification table and the Config
// struct describe exactly the same set of yaml leaf keys, so a config field
// can never be added without deciding how it reloads.
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

// collectYAMLLeaves records the yaml path of every leaf field reachable from t.
// Structs recurse with their yaml tag; slices of structs recurse with a "[]"
// marker; everything else (scalars, durations, string slices, maps) is a leaf.
func collectYAMLLeaves(t reflect.Type, prefix string, out map[string]bool) {
	for f := range t.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		ft := f.Type
		switch {
		case ft == reflect.TypeFor[time.Duration]():
			out[path] = true
		case ft.Kind() == reflect.Struct:
			collectYAMLLeaves(ft, path, out)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			collectYAMLLeaves(ft.Elem(), path+"[]", out)
		default:
			out[path] = true
		}
	}
}

// Guard against typos in the classification values themselves.
func TestReloadClassificationValues(t *testing.T) {
	t.Parallel()
	valid := map[string]bool{
		liveProcessor: true, liveWorker: true, liveBoth: true,
		liveOther: true, restartWarned: true, validationOnly: true,
	}
	for leaf, class := range reloadClassification {
		if !valid[class] {
			t.Errorf("%s: unknown classification %q", leaf, class)
		}
	}
}
