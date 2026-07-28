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

const (
	// reloadRetryInterval is how soon a background geo reload retries after a
	// failure or a refused swap. The configured refresh interval is a freshness
	// budget for good data (24h in production); reusing it as the retry delay
	// would pin the service to a broken snapshot for a day.
	reloadRetryInterval = 5 * time.Minute

	// maxRetryBackoffShift caps the doubling in retryDelay so the shift cannot
	// overflow; the refresh interval clamps the result long before this.
	maxRetryBackoffShift = 12

	// minSwapRatio is the smallest fraction of the live database a background
	// reload may come back with. A source can answer 200 with a truncated body,
	// which reports no error at all, so relative size is the only signal left
	// that the swap would blackhole most lookups.
	minSwapRatio = 0.5
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
	// PreloadedResolver / PreloadedASN carry the live DNS and Cymru caches
	// across a config reload. Both bake their timeouts and TTLs in at
	// construction, so a caller must only pass them on when the whole
	// resolver.* / geo.asn.* block is unchanged (config.ResolverChanged /
	// config.ASNChanged); otherwise the new knobs would be silently ignored.
	PreloadedResolver *resolver.Resolver
	PreloadedASN      *asn.Resolver
}

type FilterRequest struct {
	SubscriptionURL fetch.SubscriptionURL
	// AllowedCountries is the allow-list; the full set (filter.All()) means the
	// caller asked for none. DeniedCountries is the deny-list, matched only
	// against a positively resolved country, so an IP no geo source covers is
	// never dropped by an exclusion alone.
	AllowedCountries filter.CountrySet
	DeniedCountries  filter.CountrySet
	// Body, when non-empty, is an inline subscription payload filtered directly
	// without any HTTP fetch. It is normalized with the same base64-tolerant
	// decoder used for fetched subscriptions. Takes precedence over SubscriptionURL.
	Body []byte
}

// Blocklist reports whether a node host is currently geo-blocked (failed a
// through-node API reachability check). Satisfied by *geoblock.Store; nil
// disables it.
type Blocklist interface {
	Blocked(host string) bool
}

type Processor struct {
	mu            sync.RWMutex
	reloadMu      sync.Mutex
	logger        zerolog.Logger
	countryLookup geofeed.CountryLookup
	loadedAt      time.Time
	// retryAt, when non-zero, is the authoritative next-attempt time after a
	// reload that failed or was refused. loadedAt keeps pointing at the last
	// GOOD data (callers carry it across config reloads), so without retryAt a
	// failure would either read as fresh for a full interval or as stale on
	// every single request. reloadFailures counts consecutive failures and
	// drives the backoff in retryDelay.
	retryAt         time.Time
	reloadFailures  int
	refreshInterval time.Duration
	resolver        *resolver.Resolver
	// asnResolver is retained (beyond the filters and annotator that use it)
	// only so ASNState can hand its warm cache to the next processor; nil
	// when no filter or annotate chain needs ASN data.
	asnResolver  *asn.Resolver
	filters      []Filter
	blocklist    Blocklist
	annotator    *annotator
	fetchTimeout time.Duration
	// loadEntries fetches and parses the configured geofeed sources, reporting
	// how many of them failed. It is a field, like geoDB.load, so reload tests
	// can drive doReload without network access.
	loadEntries func(ctx context.Context) (entries []geofeed.Entry, failed int, err error)
	// dbip/registry are the lazily-built in-memory geo databases; nil when no
	// annotate entry references them (no download, no refresh goroutine).
	dbip     *geoDB
	registry *geoDB
}

// geoDB holds one downloadable in-memory IP->country database (dbip or
// registry) under the same mutex discipline as the processor's geofeed state:
// mu guards lookup/loadedAt/retryAt, reloadMu serializes background refreshes.
type geoDB struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex
	name     string
	lookup   geofeed.CountryLookup
	loadedAt time.Time
	// retryAt mirrors Processor.retryAt: the next-attempt gate after a failed
	// or refused reload, with reloadFailures driving its backoff.
	retryAt        time.Time
	reloadFailures int
	interval       time.Duration
	load           func(ctx context.Context) (ranges []geofeed.Range, failed int, err error)
}

// Stats counts one Filter call. Total is the nodes the parser produced, and
// Kept plus every Drop reason sums back to it. Unsupported sits outside that
// identity on purpose: it counts URI-shaped input lines the parser refused, so
// they never became nodes and were never in Total.
type Stats struct {
	Total        int
	Kept         int
	DNSDrop      int
	GeoDrop      int
	ASNDrop      int
	GeoBlockDrop int
	// IPv6Drop counts nodes whose server is an IP literal this pipeline cannot
	// use. Resolution, filtering and annotation are IPv4-only, so a v6 literal
	// is refused before any lookup; booking it as DNSDrop (as it once was)
	// reported a name-resolution fault that never happened.
	IPv6Drop int
	// Unsupported counts lines that contained "://" but did not parse as a
	// node — a truncated body, a corrupt vmess payload, an HTML page. Not part
	// of Total; see the type comment.
	Unsupported int
}

// PipelineContext holds request-scoped state shared across the processing pipeline.
type PipelineContext struct {
	Buffer *bytes.Buffer
	// Lookup is the country-resolution chain the geofeed country filter judges
	// nodes with: every in-memory country database this process loaded, tried
	// in order. It is the same set of databases the GEO annotation resolves
	// against, so the verdict and the published [GEO:xx] tag agree; see
	// countryChain.
	Lookup   geofeed.CountryLookup
	Allowed  filter.CountrySet
	Denied   filter.CountrySet
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
		retryAt  time.Time
	)
	if opts.PreloadedGeofeed != nil {
		initLog.Info().Msg("using preloaded geofeed lookup")
		lookup = opts.PreloadedGeofeed
		loadedAt = opts.PreloadedLoadedAt
	} else {
		initLog.Info().Int("sources", len(opts.GeofeedSources)).Msg("loading geofeed")
		entries, failed, err := geofeed.LoadAll(ctx, opts.GeofeedSources, initLog)
		if err != nil {
			return nil, fmt.Errorf("load geofeed: %w", err)
		}
		if failed > 0 {
			// Startup takes a partial feed because there is nothing better to
			// keep, but must not wait a whole refresh interval to complete it.
			delay := retryDelay(0, opts.RefreshInterval)
			retryAt = time.Now().Add(delay)
			initLog.Warn().Int("sources_failed", failed).Int("entries", len(entries)).
				Dur("retry_in", delay).Msg("initial geofeed load is partial; retrying shortly")
		}
		initLog.Info().Int("entries", len(entries)).Msg("geofeed loaded")
		lookup = geofeed.NewLookup(entries)
		loadedAt = time.Now()
	}
	needsASN, wantDBIP, wantRegistry := providerNeeds(opts)

	// A carried-over resolver keeps its warm cache; the reloader only passes
	// one on when the matching config block is unchanged, so the knobs baked
	// into it still describe the live config. A carry is ignored when nothing
	// references ASN data, so the annotator is not handed a dead provider.
	var asnR *asn.Resolver
	if needsASN {
		if asnR = opts.PreloadedASN; asnR == nil {
			asnR = asn.New(opts.ASNTimeout, opts.ASNCacheTTL)
		}
	}

	dnsR := opts.PreloadedResolver
	if dnsR == nil {
		dnsR = resolver.New(opts.DNSTimeout, opts.DNSAddress, opts.DNSCacheTTL, opts.DNSCacheNegativeTTL)
	}

	filters, errBuild := buildFilters(opts.IPFilters, asnR)
	if errBuild != nil {
		return nil, errBuild
	}

	sources := append([]geofeed.Source(nil), opts.GeofeedSources...)
	p := &Processor{
		logger:          logger,
		countryLookup:   lookup,
		loadedAt:        loadedAt,
		retryAt:         retryAt,
		refreshInterval: opts.RefreshInterval,
		resolver:        dnsR,
		blocklist:       opts.Blocklist,
		fetchTimeout:    opts.FetchTimeout,
		filters:         filters,
		asnResolver:     asnR,
		loadEntries: func(ctx context.Context) ([]geofeed.Entry, int, error) {
			return geofeed.LoadAll(ctx, sources, logger)
		},
	}
	if wantDBIP {
		url := opts.DBIP.URL
		p.dbip = newGeoDB(ctx, initLog, config.ProviderDBIP, *opts.DBIP.RefreshInterval,
			opts.PreloadedDBIP, opts.PreloadedDBIPLoadedAt,
			func(ctx context.Context) ([]geofeed.Range, int, error) {
				// One source: any failure is a total failure, never a partial.
				ranges, err := geofeed.LoadDBIP(ctx, url, logger)
				if err != nil {
					return nil, 0, fmt.Errorf("load dbip: %w", err)
				}
				return ranges, 0, nil
			})
	}
	if wantRegistry {
		urls := append([]string(nil), opts.Registry.URLs...)
		p.registry = newGeoDB(ctx, initLog, config.ProviderRegistry, *opts.Registry.RefreshInterval,
			opts.PreloadedRegistry, opts.PreloadedRegistryLoadedAt,
			func(ctx context.Context) ([]geofeed.Range, int, error) {
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

	p.maybeRefreshGeoDBs(ctx)
	lookup := p.countryChain(ctx)

	allowed := req.AllowedCountries
	if filter.IsEmpty(allowed) {
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
		Denied:      req.DeniedCountries,
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
		Int("ipv6_drop", stats.IPv6Drop).
		Int("unsupported", stats.Unsupported).
		Dur("latency", time.Since(start)).
		Msg("subscription processed")

	return stats, nil
}

// maxSubscriptionNodes caps how many parseable nodes one Filter call accepts.
// Nodes are resolved serially with a per-hostname DNS timeout, so an unbounded
// node list turns a single request into hours of lookups; the 10 MiB fetch cap
// holds roughly 380k minimal node URIs. The ceiling sits well above any real
// source (the crawler emits at most 500 nodes per inline source), so only a
// hostile body reaches it.
const maxSubscriptionNodes = 20_000

// ErrTooManyNodes reports a body above maxSubscriptionNodes. The body, not the
// service, is at fault, so callers answer 4xx rather than 502.
var ErrTooManyNodes = fmt.Errorf("subscription has more than %d nodes", maxSubscriptionNodes)

// processBody parses the subscription body and runs each node through the
// pipeline. Bodies above maxSubscriptionNodes are rejected before any lookup.
// It returns the context error when the request was cancelled so a truncated
// node list is never served as success.
//
// Lines the parser refused are booked as Stats.Unsupported. They are the only
// evidence that a source has started answering with something that is not a
// node list, and they are deliberately kept out of Stats.Total so a body of
// pure junk still trips Filter's "no supported URI nodes found".
func (p *Processor) processBody(ctx context.Context, body []byte, pctx *PipelineContext) error {
	// Checked up front: the ceiling has to bite before the first lookup, or a
	// hostile body has already cost maxSubscriptionNodes serial DNS resolutions
	// by the time a running counter trips.
	if tooManyNodes(body) {
		return ErrTooManyNodes
	}
	pctx.Stats.Unsupported += subscription.Parse(body, func(node subscription.Node) bool {
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

// tooManyNodes reports whether body holds more than maxSubscriptionNodes
// parseable nodes. Parse yields at most one node per line, so the newline count
// is a cheap upper bound (one vectorized pass): only a body that clears it pays
// for an exact count, which keeps the check off the hot path for real
// subscriptions.
func tooManyNodes(body []byte) bool {
	if bytes.Count(body, []byte{'\n'})+1 <= maxSubscriptionNodes {
		return false
	}
	return countNodes(body, maxSubscriptionNodes) > maxSubscriptionNodes
}

// countNodes counts the parseable nodes in body, stopping as soon as limit is
// exceeded so the pre-check costs one bounded parse pass and no DNS.
func countNodes(body []byte, limit int) int {
	count := 0
	subscription.Parse(body, func(subscription.Node) bool {
		count++
		return count <= limit
	})
	return count
}

// resolveNode returns the IPv4 addresses for a node's server and reports
// whether the server's address family is supported at all. Bare IPs are
// handled inline without touching the resolver cache: the address is written
// into pctx.addrScratch (no allocation) and returned directly, since a bare IP
// needs no DNS and the caller copies the result into pctx.Scratch before the
// next node. A literal that is not IPv4 is refused with supported=false — the
// rest of the pipeline (resolver, filters, annotator) is IPv4-only, and the
// caller must not report that refusal as a name-resolution failure. Hostnames
// go through the DNS resolver and are memoized in pctx.Resolved with an
// isolated copy so cached resolver slices are never aliased into request-local
// state.
func (p *Processor) resolveNode(ctx context.Context, server string, pctx *PipelineContext) (ips []netip.Addr, supported bool) {
	if cached, attempted := pctx.Resolved[server]; attempted {
		return cached, true
	}
	// Bare IPs skip DNS, the cache, and the request map: re-parsing on repeat
	// is allocation-free, so no memoization is needed.
	if addr, err := netip.ParseAddr(server); err == nil {
		if !addr.Is4() {
			return nil, false
		}
		pctx.addrScratch[0] = addr
		return pctx.addrScratch[:1], true
	}
	resolved, resolveErr := p.resolver.Resolve(ctx, server)
	if resolveErr != nil || len(resolved) == 0 {
		pctx.Resolved[server] = []netip.Addr{}
		return nil, true
	}
	// Isolate the per-request map from the resolver cache: the copy guarantees
	// request-local code never mutates or aliases a cached resolver slice.
	pctx.Resolved[server] = append([]netip.Addr(nil), resolved...)
	return pctx.Resolved[server], true
}

func (p *Processor) processNode(ctx context.Context, node subscription.Node, pctx *PipelineContext) {
	pctx.Stats.Total++
	select {
	case <-ctx.Done():
		return
	default:
	}
	if p.blocklist != nil && p.blocklist.Blocked(node.Server) {
		pctx.Stats.GeoBlockDrop++
		return
	}

	cached, supported := p.resolveNode(ctx, node.Server, pctx)
	if !supported {
		pctx.Stats.IPv6Drop++
		return
	}
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

// countryChain returns the lookup the country filter judges nodes with: the
// geofeed, then every downloadable database this process actually loaded, in
// the same geofeed -> dbip -> registry precedence the annotate chain uses.
//
// Filtering and annotation ask one question — "which country is this IP in?" —
// and used to answer it from different sources: the filter saw the geofeed
// alone, so a node DB-IP places in DE was geo-dropped as unknown while the tag
// it would have been published with said [GEO:DE]. The databases are already
// in memory (the lazy-build rule in NewProcessor downloads them only when an
// annotate entry names them), so consulting them costs a binary search on the
// IPs the geofeed misses and nothing else.
//
// The asn provider stays out: it is a per-IP Cymru round trip, not a local
// table, and the config exposes it as an explicit `{type: country, provider:
// asn}` filter for operators who want it. A GEO tag chain ending in asn can
// therefore still name a country the filter treated as unknown.
//
//nolint:ireturn // returns the CountryLookup interface, like currentEntries
func (p *Processor) countryChain(ctx context.Context) geofeed.CountryLookup {
	lookup := p.currentEntries(ctx)
	if p.dbip == nil && p.registry == nil {
		return lookup
	}
	chain := chainLookup{lookup}
	if p.dbip != nil {
		chain = append(chain, p.dbip.snapshot())
	}
	if p.registry != nil {
		chain = append(chain, p.registry.snapshot())
	}
	return chain
}

// chainLookup resolves an IP against several country databases in order and
// returns the first non-zero answer.
type chainLookup []geofeed.CountryLookup

func (c chainLookup) LookupCountry(ip netip.Addr) geofeed.CountryCode {
	for _, l := range c {
		if country := geofeed.LookupCountry(l, ip); country != (geofeed.CountryCode{}) {
			return country
		}
	}
	return geofeed.CountryCode{}
}

// swapRefusal reports why a freshly loaded geo database must not replace the
// live one, or "" when the swap is safe. It protects only a database worth
// protecting: an empty lookup, or one whose size is unknown (a carried-over
// stub that does not implement geofeed.SizedLookup), is always replaced.
func swapRefusal(current, loaded geofeed.CountryLookup, failed int) string {
	currentLen := lookupLen(current)
	if currentLen <= 0 {
		return ""
	}
	if failed > 0 {
		return "partial load"
	}
	if float64(lookupLen(loaded)) < float64(currentLen)*minSwapRatio {
		return "catastrophic shrink"
	}
	return ""
}

// lookupLen reports how many ranges a lookup indexes, or -1 when it does not
// report a size. An unknown size disables the swap guards: there is no
// baseline to compare against.
func lookupLen(lookup geofeed.CountryLookup) int {
	if sized, ok := lookup.(geofeed.SizedLookup); ok {
		return sized.Len()
	}
	return -1
}

// retryDelay is the wait before the next attempt after failures consecutive
// failed or refused reloads: reloadRetryInterval doubling each time, clamped
// to the refresh interval. A transient outage is therefore retried within
// minutes, while a permanently dead source degrades to the normal refresh
// cadence instead of re-downloading every five minutes forever.
func retryDelay(failures int, interval time.Duration) time.Duration {
	delay := reloadRetryInterval << min(failures, maxRetryBackoffShift)
	if interval > 0 && delay > interval {
		return interval
	}
	return delay
}

// scheduleRetryLocked arms the next reload attempt after a failed or refused
// swap and returns the delay it picked. Callers must hold p.mu.
func (p *Processor) scheduleRetryLocked() time.Duration {
	delay := retryDelay(p.reloadFailures, p.refreshInterval)
	p.reloadFailures++
	p.retryAt = time.Now().Add(delay)
	return delay
}

func (p *Processor) doReload(ctx context.Context) {
	entries, failed, err := p.loadEntries(ctx)
	// Index outside the lock: sorting half a million ranges under p.mu would
	// stall every in-flight request behind the reload.
	var next geofeed.CountryLookup
	if err == nil {
		next = geofeed.NewLookup(entries)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err != nil {
		p.logger.Error().Err(err).Dur("retry_in", p.scheduleRetryLocked()).
			Msg("background geofeed reload failed, keeping stale data")
		return
	}

	if reason := swapRefusal(p.countryLookup, next, failed); reason != "" {
		p.logger.Error().Str("reason", reason).Int("sources_failed", failed).
			Int("loaded", lookupLen(next)).Int("current", lookupLen(p.countryLookup)).
			Dur("retry_in", p.scheduleRetryLocked()).
			Msg("background geofeed reload refused, keeping existing data")
		return
	}

	p.countryLookup = next
	p.loadedAt = time.Now()
	p.retryAt = time.Time{}
	p.reloadFailures = 0
	p.logger.Info().Int("entries", len(entries)).Msg("geofeed reloaded in background")
}

// shouldReloadGeofeedLocked reports whether the geofeed is stale. Callers must
// hold p.mu (read or write). A pending retryAt overrides the interval in both
// directions: loadedAt still marks the last GOOD data, which after a failure
// reads as permanently stale and would re-download the feeds on every request.
func (p *Processor) shouldReloadGeofeedLocked(now time.Time) bool {
	if p.refreshInterval <= 0 {
		return false
	}
	if !p.retryAt.IsZero() {
		return !now.Before(p.retryAt)
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

// ResolverState and ASNState expose the live DNS and Cymru resolvers so a
// reload can hand their warm caches to the replacement processor. Both caches
// are internally locked, so the returned resolver stays safe to share with the
// outgoing processor while requests it already accepted drain.
func (p *Processor) ResolverState() *resolver.Resolver { return p.resolver }

// ASNState returns nil when no filter or annotate chain needed ASN data.
func (p *Processor) ASNState() *asn.Resolver { return p.asnResolver }

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
// an empty lookup: startup must never depend on a third-party database
// mirror. A failed or partial initial load schedules a short retry instead of
// waiting out the full refresh interval.
func newGeoDB(
	ctx context.Context,
	logger zerolog.Logger,
	name string,
	interval time.Duration,
	preloaded geofeed.CountryLookup,
	preloadedAt time.Time,
	load func(ctx context.Context) (ranges []geofeed.Range, failed int, err error),
) *geoDB {
	db := &geoDB{name: name, interval: interval, load: load}
	if preloaded != nil {
		logger.Info().Str("db", name).Msg("using preloaded geo database")
		db.lookup = preloaded
		db.loadedAt = preloadedAt
		return db
	}
	logger.Info().Str("db", name).Msg("loading geo database")
	ranges, failed, err := load(ctx)
	if err != nil {
		db.lookup = geofeed.NewRangeLookup(nil)
		logger.Warn().Err(err).Str("db", name).Dur("retry_in", db.scheduleRetryLocked()).
			Msg("initial geo database load failed; starting empty, retrying shortly")
		return db
	}
	if failed > 0 {
		// Same rule as the geofeed startup load: take what there is, but do
		// not call it fresh for a whole interval.
		logger.Warn().Str("db", name).Int("sources_failed", failed).Int("ranges", len(ranges)).
			Dur("retry_in", db.scheduleRetryLocked()).
			Msg("initial geo database load is partial; retrying shortly")
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
// db.mu (read or write). A pending retryAt (failed or refused load) gates the
// next attempt; otherwise a zero loadedAt (never loaded) is always stale.
func (db *geoDB) staleLocked(now time.Time) bool {
	if db.interval <= 0 {
		return false
	}
	if !db.retryAt.IsZero() {
		return !now.Before(db.retryAt)
	}
	if db.loadedAt.IsZero() {
		return true
	}
	return now.Sub(db.loadedAt) >= db.interval
}

// scheduleRetryLocked arms the next reload attempt after a failed or refused
// swap and returns the delay it picked. Callers must hold db.mu (newGeoDB runs
// before the database is reachable, so it needs no lock).
func (db *geoDB) scheduleRetryLocked() time.Duration {
	delay := retryDelay(db.reloadFailures, db.interval)
	db.reloadFailures++
	db.retryAt = time.Now().Add(delay)
	return delay
}

func (db *geoDB) doReload(ctx context.Context, logger zerolog.Logger) {
	ranges, failed, err := db.load(ctx)
	// Index outside the lock so the sort does not block snapshot readers.
	var next geofeed.CountryLookup
	if err == nil {
		next = geofeed.NewRangeLookup(ranges)
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if err != nil {
		logger.Error().Err(err).Str("db", db.name).Dur("retry_in", db.scheduleRetryLocked()).
			Msg("background geo database reload failed, keeping stale data")
		return
	}

	if reason := swapRefusal(db.lookup, next, failed); reason != "" {
		logger.Error().Str("db", db.name).Str("reason", reason).Int("sources_failed", failed).
			Int("loaded", lookupLen(next)).Int("current", lookupLen(db.lookup)).
			Dur("retry_in", db.scheduleRetryLocked()).
			Msg("background geo database reload refused, keeping existing data")
		return
	}

	db.lookup = next
	db.loadedAt = time.Now()
	db.retryAt = time.Time{}
	db.reloadFailures = 0
	logger.Info().Str("db", db.name).Int("ranges", len(ranges)).Msg("geo database reloaded in background")
}

func FormatStats(stats Stats) string {
	return fmt.Sprintf("done: total=%d kept=%d dns_drop=%d geo_drop=%d asn_drop=%d geoblock_drop=%d ipv6_drop=%d unsupported=%d",
		stats.Total, stats.Kept, stats.DNSDrop, stats.GeoDrop, stats.ASNDrop, stats.GeoBlockDrop, stats.IPv6Drop, stats.Unsupported)
}
