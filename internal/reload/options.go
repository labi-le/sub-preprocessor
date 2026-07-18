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
// The Preloaded* fields are intentionally left unset: callers (the reloader)
// decide whether to carry over geofeed/dbip/registry state, while the startup
// path leaves them zero so NewProcessor performs the initial fetches.
func OptionsFromConfig(cfg config.Config) preprocess.Options {
	return preprocess.Options{
		GeofeedSources:      cfg.Geo.Geofeed.Sources,
		RefreshInterval:     cfg.Geo.Geofeed.RefreshInterval,
		DNSTimeout:          cfg.Resolver.Timeout,
		DNSAddress:          cfg.Resolver.Address,
		DNSCacheTTL:         *cfg.Resolver.CacheTTL,
		DNSCacheNegativeTTL: *cfg.Resolver.CacheNegativeTTL,
		ASNTimeout:          cfg.Geo.ASN.Timeout,
		ASNCacheTTL:         cfg.Geo.ASN.CacheTTL,
		IPFilters:           cfg.IPFilterSpecs(),
		Annotate:            cfg.Annotate,
		DBIP:                cfg.Geo.DBIP,
		Registry:            cfg.Geo.Registry,
		FetchTimeout:        cfg.Fetch.Timeout,
	}
}
