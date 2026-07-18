package preprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geo"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/resolver"
	"domains.lst/sub-preprocessor/internal/subscription"
	"github.com/rs/zerolog"
)

type Options struct {
	GeofeedSources      []geofeed.Source
	RefreshInterval     time.Duration
	DNSTimeout          time.Duration
	DNSAddress          string
	DNSCacheTTL         time.Duration
	DNSCacheNegativeTTL time.Duration
	ASNTimeout          time.Duration
	ASNCacheTTL         time.Duration
	IPFilters           []config.IPFilterSpec
	Annotate            []config.AnnotateSpec
	DBIP                config.DBIPConfig
	Registry            config.RegistryConfig
	PreloadedGeofeed    geofeed.CountryLookup
	PreloadedLoadedAt   time.Time
	// PreloadedDBIP / PreloadedRegistry carry an already-loaded database (and
	// its load time) across config reloads, mirroring PreloadedGeofeed. They
	// are used only when the matching provider is referenced by Annotate.
	PreloadedDBIP             geofeed.CountryLookup
	PreloadedDBIPLoadedAt     time.Time
	PreloadedRegistry         geofeed.CountryLookup
	PreloadedRegistryLoadedAt time.Time
	Blocklist                 Blocklist
	FetchTimeout              time.Duration
}

type FilterRequest struct {
	SubscriptionURL  fetch.SubscriptionURL
	AllowedCountries filter.CountrySet
	// Body, when non-empty, is an inline subscription payload filtered directly
	// without any HTTP fetch. It is normalized with the same base64-tolerant
	// decoder used for fetched subscriptions. Takes precedence over SubscriptionURL.
	Body []byte
}

// Blocklist reports whether a node host is currently geo-blocked (failed the
// Gemini reachability check). Satisfied by *geoblock.Store; nil disables it.
type Blocklist interface {
	Blocked(host string) bool
}

type Processor struct {
	mu              sync.RWMutex
	reloadMu        sync.Mutex
	logger          zerolog.Logger
	countryLookup   geofeed.CountryLookup
	sources         []geofeed.Source
	loadedAt        time.Time
	refreshInterval time.Duration
	resolver        *resolver.Resolver
	filters         []Filter
	blocklist       Blocklist
	annotator       *annotator
	fetchTimeout    time.Duration
	// dbip/registry are the lazily-built in-memory geo databases; nil when no
	// annotate entry references them (no download, no refresh goroutine).
	dbip     *geoDB
	registry *geoDB
}

// geoDB holds one downloadable in-memory IP->country database (dbip or
// registry) under the same mutex discipline as the processor's geofeed state:
// mu guards lookup/loadedAt, reloadMu serializes background refreshes.
type geoDB struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex
	name     string
	lookup   geofeed.CountryLookup
	loadedAt time.Time
	interval time.Duration
	load     func(ctx context.Context) ([]geofeed.Range, error)
}

type Stats struct {
	Total        int
	Kept         int
	DNSDrop      int
	GeoDrop      int
	ASNDrop      int
	GeoBlockDrop int
	Unsupported  int
}

// PipelineContext holds request-scoped state shared across the processing pipeline.
type PipelineContext struct {
	Buffer   *bytes.Buffer
	Lookup   geofeed.CountryLookup
	Allowed  filter.CountrySet
	Resolved map[string][]netip.Addr
	Stats    *Stats
	// Scratch is a per-request buffer reused across nodes so filters can
	// compact IPs in place without dirtying the cached Resolved slices.
	Scratch []netip.Addr
	// addrScratch backs the single-address slice returned for bare-IP servers,
	// avoiding a per-node heap allocation. It is overwritten each node and must
	// be consumed (copied into Scratch) before the next node runs.
	addrScratch [1]netip.Addr
	IsFirstNode bool
	tagBuf      bytes.Buffer
}

// providerNeeds reports which lazily-built geo backends the configured IP
// filters and annotate chains reference.
func providerNeeds(opts Options) (needsASN, wantDBIP, wantRegistry bool) {
	for _, spec := range opts.IPFilters {
		if spec.Type == config.FilterASN || (spec.Type == config.FilterCountry && spec.Provider == config.ProviderASN) {
			needsASN = true
		}
	}
	for _, a := range opts.Annotate {
		for _, prov := range a.Providers {
			switch prov {
			case config.ProviderASN:
				needsASN = true
			case config.ProviderDBIP:
				wantDBIP = true
			case config.ProviderRegistry:
				wantRegistry = true
			}
		}
	}
	return needsASN, wantDBIP, wantRegistry
}

func NewProcessor(ctx context.Context, logger zerolog.Logger, opts Options) (*Processor, error) {
	initLog := log.Op(logger, "processor.New")

	var (
		lookup   geofeed.CountryLookup
		loadedAt time.Time
	)
	if opts.PreloadedGeofeed != nil {
		initLog.Info().Msg("using preloaded geofeed lookup")
		lookup = opts.PreloadedGeofeed
		loadedAt = opts.PreloadedLoadedAt
	} else {
		initLog.Info().Int("sources", len(opts.GeofeedSources)).Msg("loading geofeed")
		entries, err := geofeed.LoadAll(ctx, opts.GeofeedSources, initLog)
		if err != nil {
			return nil, fmt.Errorf("load geofeed: %w", err)
		}
		initLog.Info().Int("entries", len(entries)).Msg("geofeed loaded")
		lookup = geofeed.NewLookup(entries)
		loadedAt = time.Now()
	}
	needsASN, wantDBIP, wantRegistry := providerNeeds(opts)

	var asnR *asn.Resolver
	if needsASN {
		asnR = asn.New(opts.ASNTimeout, opts.ASNCacheTTL)
	}

	filters, errBuild := buildFilters(opts.IPFilters, asnR)
	if errBuild != nil {
		return nil, errBuild
	}

	p := &Processor{
		logger:          logger,
		countryLookup:   lookup,
		sources:         append([]geofeed.Source(nil), opts.GeofeedSources...),
		loadedAt:        loadedAt,
		refreshInterval: opts.RefreshInterval,
		resolver:        resolver.New(opts.DNSTimeout, opts.DNSAddress, opts.DNSCacheTTL, opts.DNSCacheNegativeTTL),
		blocklist:       opts.Blocklist,
		fetchTimeout:    opts.FetchTimeout,
		filters:         filters,
	}
	if wantDBIP {
		url := opts.DBIP.URL
		p.dbip = newGeoDB(ctx, initLog, config.ProviderDBIP, opts.DBIP.RefreshInterval,
			opts.PreloadedDBIP, opts.PreloadedDBIPLoadedAt,
			func(ctx context.Context) ([]geofeed.Range, error) {
				return geofeed.LoadDBIP(ctx, url, logger)
			})
	}
	if wantRegistry {
		urls := append([]string(nil), opts.Registry.URLs...)
		p.registry = newGeoDB(ctx, initLog, config.ProviderRegistry, opts.Registry.RefreshInterval,
			opts.PreloadedRegistry, opts.PreloadedRegistryLoadedAt,
			func(ctx context.Context) ([]geofeed.Range, error) {
				return geofeed.LoadRegistry(ctx, urls, logger)
			})
	}

	// The annotator receives only the providers that were actually built: the
	// lazy rule above guarantees every name referenced by opts.Annotate is
	// present, so a miss inside newAnnotator can only be a wiring bug.
	providers := map[string]geo.Provider{
		config.ProviderGeofeed: geo.NewLookupProvider(config.ProviderGeofeed, p.snapshotLookup),
	}
	if asnR != nil {
		providers[config.ProviderASN] = geo.NewASN(asnR)
	}
	if p.dbip != nil {
		providers[config.ProviderDBIP] = geo.NewLookupProvider(config.ProviderDBIP, p.dbip.snapshot)
	}
	if p.registry != nil {
		providers[config.ProviderRegistry] = geo.NewLookupProvider(config.ProviderRegistry, p.registry.snapshot)
	}
	p.annotator = newAnnotator(logger, opts.Annotate, providers)

	return p, nil
}

func (p *Processor) Filter(ctx context.Context, b *bytes.Buffer, req FilterRequest) (Stats, error) {
	label := string(req.SubscriptionURL)
	if len(req.Body) > 0 {
		label = "inline"
	}
	requestLog := p.logger.With().Str("url", label).Logger()
	start := time.Now()

	lookup := p.currentEntries(ctx)
	p.maybeRefreshGeoDBs(ctx)

	allowed := req.AllowedCountries
	isEmpty := true
	for _, v := range allowed {
		if v != 0 {
			isEmpty = false
			break
		}
	}
	if isEmpty {
		return Stats{}, errors.New("no allowed countries provided")
	}

	var body []byte
	if len(req.Body) > 0 {
		// Inline source: normalize the pasted payload with the same
		// base64-tolerant decoder used for fetched bodies; no HTTP fetch.
		body = subscription.Normalize(req.Body)
	} else {
		fetchCtx := ctx
		if p.fetchTimeout > 0 {
			var cancelFetch context.CancelFunc
			fetchCtx, cancelFetch = context.WithTimeout(ctx, p.fetchTimeout)
			defer cancelFetch()
		}
		loaded, errLoad := subscription.Load(fetchCtx, req.SubscriptionURL)
		if errLoad != nil {
			return Stats{}, fmt.Errorf("load subscription: %w", errLoad)
		}
		body = loaded
	}

	stats := Stats{}

	resolved := p.resolver.GetResolvedMap()
	defer p.resolver.PutResolvedMap(resolved)

	pctx := &PipelineContext{
		Buffer:      b,
		Lookup:      lookup,
		Allowed:     allowed,
		Resolved:    resolved,
		Stats:       &stats,
		IsFirstNode: true,
	}

	if err := p.processBody(ctx, body, pctx); err != nil {
		return stats, err
	}

	if stats.Total == 0 {
		return stats, errors.New("no supported URI nodes found")
	}

	requestLog.Info().
		Int("total", stats.Total).
		Int("kept", stats.Kept).
		Int("dns_drop", stats.DNSDrop).
		Int("geo_drop", stats.GeoDrop).
		Int("asn_drop", stats.ASNDrop).
		Int("geoblock_drop", stats.GeoBlockDrop).
		Int("unsupported", stats.Unsupported).
		Dur("latency", time.Since(start)).
		Msg("subscription processed")

	return stats, nil
}

// processBody parses the subscription body and runs each node through the
// pipeline. It returns the context error when the request was cancelled so a
// truncated node list is never served as success.
func (p *Processor) processBody(ctx context.Context, body []byte, pctx *PipelineContext) error {
	subscription.Parse(body, func(node subscription.Node) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		p.processNode(ctx, node, pctx)
		return true
	})
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("preprocess cancelled: %w", err)
	}
	return nil
}

// resolveNode returns the IPv4 addresses for a node's server. Bare IPs are
// handled inline without touching the resolver cache: the address is written
// into pctx.addrScratch (no allocation) and returned directly, since a bare IP
// needs no DNS and the caller copies the result into pctx.Scratch before the
// next node. Hostnames go through the DNS resolver and are memoized in
// pctx.Resolved with an isolated copy so cached resolver slices are never
// aliased into request-local state.
func (p *Processor) resolveNode(ctx context.Context, server string, pctx *PipelineContext) []netip.Addr {
	if ips, attempted := pctx.Resolved[server]; attempted {
		return ips
	}
	// Bare IPs skip DNS, the cache, and the request map: re-parsing on repeat
	// is allocation-free, so no memoization is needed.
	if addr, err := netip.ParseAddr(server); err == nil {
		if !addr.Is4() {
			return nil
		}
		pctx.addrScratch[0] = addr
		return pctx.addrScratch[:1]
	}
	ips, resolveErr := p.resolver.Resolve(ctx, server)
	if resolveErr != nil || len(ips) == 0 {
		pctx.Resolved[server] = []netip.Addr{}
		return nil
	}
	// Isolate the per-request map from the resolver cache: the copy guarantees
	// request-local code never mutates or aliases a cached resolver slice.
	pctx.Resolved[server] = append([]netip.Addr(nil), ips...)
	return pctx.Resolved[server]
}

func (p *Processor) processNode(ctx context.Context, node subscription.Node, pctx *PipelineContext) {
	pctx.Stats.Total++
	select {
	case <-ctx.Done():
		return
	default:
	}
	if node.Server == "" || node.Port == "" {
		pctx.Stats.Unsupported++
		return
	}
	if p.blocklist != nil && p.blocklist.Blocked(node.Server) {
		pctx.Stats.GeoBlockDrop++
		return
	}

	cached := p.resolveNode(ctx, node.Server, pctx)
	if len(cached) == 0 {
		pctx.Stats.DNSDrop++
		return
	}
	// Hand filters a scratch copy: they compact in place, and the cached
	// slice must stay pristine for later nodes on the same server.
	pctx.Scratch = append(pctx.Scratch[:0], cached...)
	ips := pctx.Scratch

	for _, f := range p.filters {
		ips = f.Process(ctx, ips, pctx)
		if len(ips) == 0 {
			return
		}
	}

	if !pctx.IsFirstNode {
		pctx.Buffer.WriteByte('\n')
	}
	pctx.IsFirstNode = false
	if p.annotator != nil {
		p.annotator.annotate(ctx, pctx.Buffer, &pctx.tagBuf, node, ips[0])
	} else {
		pctx.Buffer.WriteString(node.Raw)
	}
	pctx.Stats.Kept++
}

// snapshotLookup returns the processor's current geofeed lookup under the read
// lock. It backs the annotator's geofeed provider so per-node GEO annotation
// reflects background reloads instead of a captured snapshot.
//
//nolint:ireturn // returns the CountryLookup interface for the geo.Provider getter
func (p *Processor) snapshotLookup() geofeed.CountryLookup {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.countryLookup
}

//nolint:ireturn // pre-existing: returns interface for flexibility
func (p *Processor) currentEntries(ctx context.Context) geofeed.CountryLookup {
	p.mu.RLock()
	lookup := p.countryLookup
	needsReload := p.shouldReloadGeofeedLocked(time.Now())
	p.mu.RUnlock()

	if needsReload {
		if p.reloadMu.TryLock() {
			bgCtx := context.WithoutCancel(ctx)
			go func() {
				defer p.reloadMu.Unlock()
				p.doReload(bgCtx)
			}()
		}
	}

	return lookup
}

func (p *Processor) doReload(ctx context.Context) {
	entries, err := geofeed.LoadAll(ctx, p.sources, p.logger)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.loadedAt = time.Now()

	if err != nil {
		p.logger.Error().Err(err).Msg("background geofeed reload failed, keeping stale data")
		return
	}

	p.countryLookup = geofeed.NewLookup(entries)
	p.logger.Info().Int("entries", len(entries)).Msg("geofeed reloaded in background")
}

// shouldReloadGeofeedLocked reports whether the geofeed is stale. Callers must
// hold p.mu (read or write).
func (p *Processor) shouldReloadGeofeedLocked(now time.Time) bool {
	if p.refreshInterval <= 0 {
		return false
	}
	if p.loadedAt.IsZero() {
		return true
	}
	return now.Sub(p.loadedAt) >= p.refreshInterval
}

//nolint:ireturn // returns the countryLookup interface so callers can carry geofeed state across reloads
func (p *Processor) GeofeedState() (geofeed.CountryLookup, time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.countryLookup, p.loadedAt
}

//nolint:ireturn // returns the CountryLookup interface so callers can carry dbip state across reloads
func (p *Processor) DBIPState() (geofeed.CountryLookup, time.Time) {
	if p.dbip == nil {
		return nil, time.Time{}
	}
	return p.dbip.state()
}

//nolint:ireturn // returns the CountryLookup interface so callers can carry registry state across reloads
func (p *Processor) RegistryState() (geofeed.CountryLookup, time.Time) {
	if p.registry == nil {
		return nil, time.Time{}
	}
	return p.registry.state()
}

// maybeRefreshGeoDBs opportunistically refreshes the built geo databases on
// the request path, the same trigger point as the geofeed refresh in
// currentEntries.
func (p *Processor) maybeRefreshGeoDBs(ctx context.Context) {
	if p.dbip != nil {
		p.dbip.maybeRefresh(ctx, p.logger)
	}
	if p.registry != nil {
		p.registry.maybeRefresh(ctx, p.logger)
	}
}

// newGeoDB builds the state for one lazily-referenced geo database. A
// preloaded lookup (reload carry-over) is used as-is; otherwise the initial
// load runs inline but, unlike geofeed, a failure only WARNs and starts with
// an empty lookup and a zero loadedAt (so the next request-triggered refresh
// retries): startup must never depend on a third-party database mirror.
func newGeoDB(
	ctx context.Context,
	logger zerolog.Logger,
	name string,
	interval time.Duration,
	preloaded geofeed.CountryLookup,
	preloadedAt time.Time,
	load func(ctx context.Context) ([]geofeed.Range, error),
) *geoDB {
	db := &geoDB{name: name, interval: interval, load: load}
	if preloaded != nil {
		logger.Info().Str("db", name).Msg("using preloaded geo database")
		db.lookup = preloaded
		db.loadedAt = preloadedAt
		return db
	}
	logger.Info().Str("db", name).Msg("loading geo database")
	ranges, err := load(ctx)
	if err != nil {
		logger.Warn().Err(err).Str("db", name).
			Msg("initial geo database load failed; starting empty, retrying on next refresh")
		db.lookup = geofeed.NewRangeLookup(nil)
		return db
	}
	db.lookup = geofeed.NewRangeLookup(ranges)
	db.loadedAt = time.Now()
	logger.Info().Str("db", name).Int("ranges", len(ranges)).Msg("geo database loaded")
	return db
}

// snapshot returns the current lookup under the read lock; it backs the
// provider getter so per-node lookups reflect background refreshes.
//
//nolint:ireturn // returns the CountryLookup interface for the geo.Provider getter
func (db *geoDB) snapshot() geofeed.CountryLookup {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.lookup
}

//nolint:ireturn // returns the CountryLookup interface so callers can carry state across reloads
func (db *geoDB) state() (geofeed.CountryLookup, time.Time) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.lookup, db.loadedAt
}

// maybeRefresh kicks an opportunistic background reload when the database is
// stale, mirroring the geofeed TryLock pattern in currentEntries.
func (db *geoDB) maybeRefresh(ctx context.Context, logger zerolog.Logger) {
	db.mu.RLock()
	stale := db.staleLocked(time.Now())
	db.mu.RUnlock()
	if !stale {
		return
	}
	if db.reloadMu.TryLock() {
		bgCtx := context.WithoutCancel(ctx)
		go func() {
			defer db.reloadMu.Unlock()
			db.doReload(bgCtx, logger)
		}()
	}
}

// staleLocked reports whether the database needs a reload. Callers must hold
// db.mu (read or write). A zero loadedAt (failed initial load) is always
// stale so the next trigger retries.
func (db *geoDB) staleLocked(now time.Time) bool {
	if db.interval <= 0 {
		return false
	}
	if db.loadedAt.IsZero() {
		return true
	}
	return now.Sub(db.loadedAt) >= db.interval
}

func (db *geoDB) doReload(ctx context.Context, logger zerolog.Logger) {
	ranges, err := db.load(ctx)

	db.mu.Lock()
	defer db.mu.Unlock()

	// Stamp even on failure (geofeed doReload pattern) so a broken mirror is
	// retried once per interval, not on every request.
	db.loadedAt = time.Now()

	if err != nil {
		logger.Error().Err(err).Str("db", db.name).
			Msg("background geo database reload failed, keeping stale data")
		return
	}

	db.lookup = geofeed.NewRangeLookup(ranges)
	logger.Info().Str("db", db.name).Int("ranges", len(ranges)).Msg("geo database reloaded in background")
}

func FormatStats(stats Stats) string {
	return fmt.Sprintf("done: total=%d kept=%d dns_drop=%d geo_drop=%d asn_drop=%d geoblock_drop=%d unsupported=%d",
		stats.Total, stats.Kept, stats.DNSDrop, stats.GeoDrop, stats.ASNDrop, stats.GeoBlockDrop, stats.Unsupported)
}
