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
	PreloadedGeofeed    geofeed.CountryLookup
	PreloadedLoadedAt   time.Time
	Blocklist           Blocklist
	FetchTimeout        time.Duration
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
	needsASN := false
	for _, spec := range opts.IPFilters {
		if spec.Type == config.FilterASN || (spec.Type == config.FilterCountry && spec.Provider == config.ProviderASN) {
			needsASN = true
		}
	}
	for _, a := range opts.Annotate {
		if a.Provider == config.ProviderASN {
			needsASN = true
		}
	}

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

	var asnProv geo.Provider
	if asnR != nil {
		asnProv = geo.NewASN(asnR)
	}
	p.annotator = newAnnotator(opts.Annotate, geo.NewGeofeed(p.snapshotLookup), asnProv)

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

func FormatStats(stats Stats) string {
	return fmt.Sprintf("done: total=%d kept=%d dns_drop=%d geo_drop=%d asn_drop=%d geoblock_drop=%d unsupported=%d",
		stats.Total, stats.Kept, stats.DNSDrop, stats.GeoDrop, stats.ASNDrop, stats.GeoBlockDrop, stats.Unsupported)
}
