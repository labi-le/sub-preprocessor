package reload

import (
	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/preprocess"
)

// OptionsFromConfig maps a loaded config.Config to preprocess.Options. It is the
// single source of truth for this mapping, mirroring the bootstrap mapping in
// internal/app so the startup path and the reload path build processors
// identically.
//
// The PreloadedGeofeed / PreloadedLoadedAt fields are intentionally left unset:
// callers (the reloader) decide whether to carry over geofeed state, while the
// startup path leaves them zero so NewProcessor performs the initial fetch.
func OptionsFromConfig(cfg config.Config) preprocess.Options {
	return preprocess.Options{
		GeofeedSources:      cfg.Geofeed.Sources,
		RefreshInterval:     cfg.Geofeed.RefreshInterval,
		DNSTimeout:          cfg.Resolver.Timeout,
		DNSAddress:          cfg.Resolver.Address,
		DNSCacheTTL:         cfg.Resolver.CacheTTL,
		DNSCacheNegativeTTL: cfg.Resolver.CacheNegativeTTL,
		ASNTimeout:          cfg.ASN.Timeout,
		ASNDenyPatterns:     cfg.ASN.DenyPatterns,
		WorkflowStages:      cfg.Workflow.Stages,
	}
}
