package preprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/cidrset"
	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geo"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/ioutil"
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
	PreloadedGeofeed    GeoState
	// PreloadedDBIP / PreloadedRegistry carry an already-loaded database across
	// config reloads, mirroring PreloadedGeofeed. They are used only when the
	// matching provider is referenced by Annotate.
	PreloadedDBIP     GeoState
	PreloadedRegistry GeoState
	Blocklist         Blocklist
	FetchTimeout      time.Duration
	// PreloadedResolver / PreloadedASN carry the live DNS and Cymru caches
	// across a config reload. Both bake their timeouts and TTLs in at
	// construction, so a caller must only pass them on when the whole
	// resolver.* / geo.asn.* block is unchanged (config.ResolverChanged /
	// config.ASNChanged); otherwise the new knobs would be silently ignored.
	PreloadedResolver *resolver.Resolver
	PreloadedASN      *asn.Resolver
	// PreloadedCIDR carries the loaded allow-list across a reload, like
	// PreloadedDBIP; used only when a cidr filter is configured.
	PreloadedCIDR CIDRState
	// cidrLoad replaces the download in tests: the initial allow-list load
	// runs inline in NewProcessor, which no test may take to the network.
	cidrLoad cidrLoader
}

// GeoState is everything one processor hands its replacement about a single
// country database across a config reload: the loaded data and the retry
// schedule that belongs to it.
//
// The four values are one type because they are only correct together. A carry
// that kept Lookup/LoadedAt but dropped RetryAt/Failures would make a source
// whose last reload failed look permanently stale — LoadedAt marks the last
// GOOD data, so it sits behind the refresh interval until a load succeeds —
// and the first request after every reload would re-download it; the crawler
// rewrites private.yaml hourly, so that is a fresh download attempt per hour
// against a source that is already failing. The reverse split, a retry
// deadline carried without the data it throttles, would gate the initial load
// of a database this processor never had. Grouping them makes both
// unrepresentable rather than merely fixed.
type GeoState struct {
	Lookup   geofeed.CountryLookup
	LoadedAt time.Time
	// RetryAt, when non-zero, is the next-attempt deadline armed by a reload
	// that failed or was refused; Failures counts the consecutive ones that
	// drove its backoff. See retryDelay.
	RetryAt  time.Time
	Failures int
}

// CIDRState is the cidr allow-list's carry-over across a config reload. Its
// four fields travel together for the reason GeoState's doc gives.
type CIDRState struct {
	Set      cidrset.Set
	LoadedAt time.Time
	RetryAt  time.Time
	Failures int
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
	// cidr is nil unless a cidr filter is configured.
	cidr *cidrStore
	// countryOrder is the provider order countryChain walks, derived from the
	// configured GEO annotate chain; empty means "the geofeed alone". Written
	// once in NewProcessor before the processor is published, read-only after.
	countryOrder []string
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

// cidrLoader reports how many sources failed alongside the merged set, so a
// partial download can arm a retry instead of passing as fresh.
type cidrLoader func(ctx context.Context) (set cidrset.Set, failed int, err error)

// cidrStore holds the downloaded IP allow-list under geoDB's mutex discipline:
// mu guards set/loadedAt/retryAt, reloadMu serializes background refreshes.
type cidrStore struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex
	set      cidrset.Set
	loadedAt time.Time
	// retryAt mirrors geoDB.retryAt: the next-attempt gate after a failed or
	// refused reload, with reloadFailures driving its backoff.
	retryAt        time.Time
	reloadFailures int
	interval       time.Duration
	load           cidrLoader
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
	CIDRDrop     int
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
	// sink receives every node that survived the IP stage. Emission is behind
	// an interface because the two callers want different things out of the
	// same pipeline: `GET /` wants the published text, the stable worker wants
	// the nodes themselves so it can annotate after probing them.
	sink nodeSink
	// Lookup is the country-resolution chain the geofeed country filter judges
	// nodes with: the LOCAL country databases every GEO annotate entry names,
	// concatenated in written order. Not "every database this process loaded"
	// — a chain naming only dbip leaves the geofeed out — and never asn or
	// cloudflare, which are not local tables, so the verdict and the published
	// [GEO:xx] tag agree on everything but those two; see countryChain.
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
}

// NodeResult is one node that survived the IP stage, unannotated. IP is the
// address the pipeline judged it by; whatever tags are rendered later must
// describe that same address, or the published node contradicts the filter
// that kept it.
type NodeResult struct {
	Raw string
	IP  netip.Addr
}

type nodeSink interface {
	emit(ctx context.Context, node subscription.Node, ip netip.Addr)
}

// bufferSink renders nodes into the response buffer as they survive. The
// separator and the tag prefix are both part of the published text, so they
// belong here rather than in the pipeline.
type bufferSink struct {
	buf       *bytes.Buffer
	annotator *annotator
	tagBuf    bytes.Buffer
	wrote     bool
}

func (s *bufferSink) emit(ctx context.Context, node subscription.Node, ip netip.Addr) {
	if s.wrote {
		s.buf.WriteByte('\n')
	}
	s.wrote = true
	if s.annotator == nil {
		s.buf.WriteString(node.Raw)
		return
	}
	s.annotator.Annotate(ctx, s.buf, &s.tagBuf, AnnotateRequest{Node: node, IP: ip})
}

// sliceSink collects survivors for a caller that annotates later. arena packs
// their line bytes back to back, so a body costs a handful of allocations
// instead of one per node; byteBound is what is left of the upper bound on
// those bytes.
type sliceSink struct {
	nodes     []NodeResult
	arena     []byte
	byteBound int
}

func (s *sliceSink) emit(_ context.Context, node subscription.Node, ip netip.Addr) {
	s.nodes = append(s.nodes, NodeResult{Raw: s.intern(node.Raw), IP: ip})
}

// intern copies line into the arena and returns a view over the copy:
// subscription.Parse hands out views into the source body, and that body dies
// with the call. The copy needs no second one — the chunk is only ever appended
// to PAST the bytes a returned string covers, and that string's own pointer
// keeps the chunk reachable. Overflow starts a FRESH chunk rather than growing
// this one, which would move the live bytes and strand each string alone.
func (s *sliceSink) intern(line string) string {
	if len(line) > cap(s.arena)-len(s.arena) {
		s.arena = make([]byte, 0, max(min(arenaChunk, s.byteBound), len(line)))
	}
	start := len(s.arena)
	s.arena = append(s.arena, line...)
	s.byteBound -= len(line)

	return ioutil.UnsafeString(s.arena[start:])
}

// arenaChunk trades allocation count against two wastes pulling opposite ways:
// bytes abandoned at a chunk boundary, and the final chunk's tail, which the
// worker holds until the merge. Measured 2026-08-18 on the two shipped shapes:
// halving it costs ~65% more allocs/op, and doubling it takes the filtering
// shape's B/op from 1.7% over its pre-arena baseline to 6.4%.
const arenaChunk = 8192

// sizer is a sink that can be told how many nodes may follow. Only the
// collecting sink implements it — bufferSink renders into a buffer its caller
// owns and sizes.
type sizer interface {
	reserve(nodes, byteBound int)
}

// reserve sizes the survivor slice once, from a bound the parse loop already
// knows. Growing into it instead cost a measured 7.6 MB per cycle over the
// 157-source corpus, every byte of it a copy of the slice's own tail. The bound
// counts LINES, so it overshoots by whatever share of the body the IP stage
// drops — see fit, which is what keeps that overshoot from being retained.
//
// byteBound sizes the arena's last chunk the same way, and a one-node source
// gets an exactly sized chunk rather than a whole arenaChunk.
func (s *sliceSink) reserve(nodes, byteBound int) {
	if s.nodes == nil {
		s.nodes = make([]NodeResult, 0, nodes)
	}
	s.byteBound = byteBound
}

// fit hands the survivors back without the reservation's unwritten tail, which
// the caller would otherwise hold for as long as the nodes themselves. How far
// the line bound overshoots is a property of the CONFIG, not of this code:
// measured 2026-08-14 over every configured source, config/ keeps 66495
// survivors out of 71512 lines (1.08x), while the cidr allow-list on the second
// instance (retired 2026-08-26) kept 15904 of 108742 (6.84x) and left 3.69 MB
// of never-written capacity live across the whole fetch phase.
//
// That contrast is why copying is GATED on the copy being no larger than the
// capacity it releases: no shape can lose. A permissive config keeps its slice
// untouched (measured byte-identical B/op, its 9519-line largest source
// included) and a filtering one trades 0.71 MB of transient copy for those
// 3.69 MB. Zero survivors release the whole reservation and allocate nothing.
func (s *sliceSink) fit() []NodeResult {
	if len(s.nodes)*2 > cap(s.nodes) {
		return s.nodes
	}
	if len(s.nodes) == 0 {
		return nil
	}
	return append(make([]NodeResult, 0, len(s.nodes)), s.nodes...)
}

// providerNeeds reports which lazily-built geo backends the configured IP
// filters and annotate chains reference. It reads filter types and PROVIDER
// names only, never a tag name: the two sources of needsASN are independent
// (an asn filter with nothing annotated, and a GEO chain naming asn with no
// asn filter), which is why retiring the ASN annotate TAG left the Cymru
// resolver exactly as reachable as before.
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

func initialGeofeedState(ctx context.Context, initLog zerolog.Logger, opts Options) (GeoState, error) {
	if opts.PreloadedGeofeed.Lookup != nil {
		initLog.Info().Msg("using preloaded geofeed lookup")
		// Adopted whole, retry schedule included: see GeoState.
		return opts.PreloadedGeofeed, nil
	}
	initLog.Info().Int("sources", len(opts.GeofeedSources)).Msg("loading geofeed")
	entries, failed, err := geofeed.LoadAll(ctx, opts.GeofeedSources, initLog)
	if err != nil {
		return GeoState{}, fmt.Errorf("load geofeed: %w", err)
	}
	var state GeoState
	if failed > 0 {
		// Startup takes a partial feed because there is nothing better to
		// keep, but must not wait a whole refresh interval to complete it.
		delay := retryDelay(0, opts.RefreshInterval)
		state.RetryAt = time.Now().Add(delay)
		initLog.Warn().Int("sources_failed", failed).Int("entries", len(entries)).
			Dur("retry_in", delay).Msg("initial geofeed load is partial; retrying shortly")
	}
	initLog.Info().Int("entries", len(entries)).Msg("geofeed loaded")
	state.Lookup = geofeed.NewLookup(entries)
	state.LoadedAt = time.Now()
	return state, nil
}

func NewProcessor(ctx context.Context, logger zerolog.Logger, opts Options) (*Processor, error) {
	initLog := log.Op(logger, "processor.New")

	geoState, errGeo := initialGeofeedState(ctx, initLog, opts)
	if errGeo != nil {
		return nil, errGeo
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

	var cidrs *cidrStore
	var cidrSet func() cidrset.Set
	if spec, ok := cidrSpec(opts.IPFilters); ok {
		store, errCIDR := newCIDRStore(ctx, initLog, spec.RefreshInterval,
			opts.PreloadedCIDR, cidrLoadFor(spec, logger, opts.cidrLoad))
		if errCIDR != nil {
			return nil, errCIDR
		}
		cidrs, cidrSet = store, store.snapshot
	}

	filters, errBuild := buildFilters(opts.IPFilters, asnR, cidrSet)
	if errBuild != nil {
		return nil, errBuild
	}

	sources := append([]geofeed.Source(nil), opts.GeofeedSources...)
	p := &Processor{
		logger:          logger,
		countryLookup:   geoState.Lookup,
		loadedAt:        geoState.LoadedAt,
		retryAt:         geoState.RetryAt,
		reloadFailures:  geoState.Failures,
		refreshInterval: opts.RefreshInterval,
		resolver:        dnsR,
		blocklist:       opts.Blocklist,
		fetchTimeout:    opts.FetchTimeout,
		filters:         filters,
		asnResolver:     asnR,
		cidr:            cidrs,
		loadEntries: func(ctx context.Context) ([]geofeed.Entry, int, error) {
			return geofeed.LoadAll(ctx, sources, logger)
		},
	}
	if wantDBIP {
		url := opts.DBIP.URL
		p.dbip = newGeoDB(ctx, initLog, config.ProviderDBIP, *opts.DBIP.RefreshInterval,
			opts.PreloadedDBIP,
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
			opts.PreloadedRegistry,
			func(ctx context.Context) ([]geofeed.Range, int, error) {
				return geofeed.LoadRegistry(ctx, urls, logger)
			})
	}

	p.countryOrder = countryChainOrder(opts.Annotate, p.dbip != nil, p.registry != nil)

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

// Filter renders the surviving nodes as the published subscription text,
// annotated inline: `GET /` has no post-probe stage to annotate in.
func (p *Processor) Filter(ctx context.Context, b *bytes.Buffer, req FilterRequest) (Stats, error) {
	return p.filterInto(ctx, &bufferSink{buf: b, annotator: p.annotator}, req)
}

// FilterNodes runs the same pipeline but hands the survivors back unannotated,
// each paired with the address the filters judged it by. The stable worker
// annotates only after probing, so the tags can carry what the probes learned
// — and must carry them for THIS address, never a second resolution of the
// same hostname.
func (p *Processor) FilterNodes(ctx context.Context, req FilterRequest) ([]NodeResult, Stats, error) {
	sink := &sliceSink{}
	stats, err := p.filterInto(ctx, sink, req)
	return sink.fit(), stats, err
}

func (p *Processor) filterInto(ctx context.Context, sink nodeSink, req FilterRequest) (Stats, error) {
	label := string(req.SubscriptionURL)
	if len(req.Body) > 0 {
		label = "inline"
	}
	requestLog := p.logger.With().Str("url", label).Logger()
	start := time.Now()

	p.maybeRefreshDatabases(ctx)
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
		sink:     sink,
		Lookup:   lookup,
		Allowed:  allowed,
		Denied:   req.DeniedCountries,
		Resolved: resolved,
		Stats:    &stats,
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
		Int("cidr_drop", stats.CIDRDrop).
		Int("geoblock_drop", stats.GeoBlockDrop).
		Int("ipv6_drop", stats.IPv6Drop).
		Int("unsupported", stats.Unsupported).
		Dur("latency", time.Since(start)).
		Msg("subscription processed")

	return stats, nil
}

// maxSubscriptionNodes caps how many parseable nodes one Filter call accepts.
// Nodes are resolved serially with a per-hostname DNS timeout, so an unbounded
// node list turns a single request into hours of lookups.
//
// This is a denial-of-service bound, NOT a quality filter: real aggregator
// sources do reach five digits. The first 20 000 ceiling dropped a configured
// 36 421-node source outright, so the number has to clear the largest source an
// operator would legitimately configure, not the largest one seen so far.
//
// It is not the binding constraint, though: subscription.maxSubscriptionSize
// (10 MiB) rejects the body first for any realistic URI length. That same source
// measured 8.92 MB over 36 421 nodes — 245 B/node, so the byte cap bites at
// ~43k. Raising this ceiling alone buys a source of that shape ~6k nodes of
// growth, not ~14k; both numbers have to move to buy more.
const maxSubscriptionNodes = 50_000

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
	// Parse yields at most one node per line, so the newline count bounds the
	// node count above in one vectorized pass — enough for both the DoS ceiling
	// and the survivor slice's size.
	lines := bytes.Count(body, []byte{'\n'}) + 1
	// Checked up front: the ceiling has to bite before the first lookup, or a
	// hostile body has already cost maxSubscriptionNodes serial DNS resolutions
	// by the time a running counter trips.
	if lines > maxSubscriptionNodes && countNodes(body, maxSubscriptionNodes) > maxSubscriptionNodes {
		return ErrTooManyNodes
	}
	if s, ok := pctx.sink.(sizer); ok {
		// The same count bounds the survivor BYTES: node lines are disjoint
		// slices of the body, and none of them holds one of its separators.
		s.reserve(min(lines, maxNodeHint), len(body)-lines+1)
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

// maxNodeHint caps the survivor-slice reservation a body's line count implies.
// The count is an upper bound, not an estimate: a body of junk lines carries no
// nodes at all, so the cap is what keeps reserving for one cheaper than growing
// into the real thing. 16384 clears the largest configured source (11k nodes in
// one 3.06 MB body) and bounds a junk body at a single 640 KB buffer.
const maxNodeHint = 16384

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

	pctx.sink.emit(ctx, node, ips[0])
	pctx.Stats.Kept++
}

// Annotator returns the renderer for the configured tag list, or nil when
// annotation is disabled. The nil is explicit: a *annotator in an interface
// would be non-nil to a caller branching on it.
//
//nolint:ireturn // the point of the getter is to hand out the interface
func (p *Processor) Annotator() Annotator {
	if p.annotator == nil {
		return nil
	}
	return p.annotator
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
// local country databases the configured GEO annotate chains name, in the
// order they name them.
//
// Filtering and annotation ask one question — "which country is this IP in?" —
// and used to answer it from different sources: the filter saw the geofeed
// alone, so a node DB-IP places in DE was geo-dropped as unknown while the tag
// it would have been published with said [GEO:DE]. Reading the order off the
// annotate config keeps the two answers identical over the LOCAL databases for
// any ordering an operator writes, not just the one config.yaml ships. EVERY
// GEO entry contributes, because Annotate resolves across entries too — it
// returns the leftmost country any of them placed — so a filter reading one
// entry judged nodes without a database their own published tag had already
// consulted. The databases are already in memory (the lazy-build rule in
// NewProcessor downloads them only when an annotate entry names them), so
// consulting them costs a binary search on the IPs the earlier providers miss
// and nothing else.
//
// LOCAL is the whole of the promise, and it has two exceptions -- ONE of which
// the shipped chain still names. The asn provider stays out: it is a per-IP
// Cymru round trip, not a local table, and the config exposes it as an explicit
// `{type: country, provider: asn}` filter for operators who want it. Since it
// left the shipped annotate chain it costs the default config nothing, and only
// a config that puts it back reopens that half of the gap. cloudflare stays out
// for a harder reason (countryChainOrder's doc carries it): it is not a lookup
// this stage can make, so there is no filter form to expose -- and it IS in the
// shipped chain, which is what keeps the gap a DEFAULT rather than a corner
// config. A GEO chain resolving through either can therefore still name a
// country the filter treated as unknown. Measured on config.yaml's own
// `[cloudflare, geofeed, dbip, registry]`, which merges to
// [geofeed, dbip, registry]: with all three loaded and none of them
// able to place the IP, `exclude_countries=DE` keeps the node while the stable
// worker's traced egress publishes [GEO:DE].
//
//nolint:ireturn // returns the CountryLookup interface, like currentEntries
func (p *Processor) countryChain(ctx context.Context) geofeed.CountryLookup {
	// currentEntries doubles as the opportunistic background-reload trigger, so
	// it runs on every request whether or not the geofeed is in the chain.
	lookup := p.currentEntries(ctx)
	if len(p.countryOrder) == 0 {
		return lookup
	}
	chain := make(chainLookup, len(p.countryOrder))
	for i, name := range p.countryOrder {
		switch name {
		case config.ProviderDBIP:
			chain[i] = p.dbip.snapshot()
		case config.ProviderRegistry:
			chain[i] = p.registry.snapshot()
		default:
			// countryChainOrder emits nothing but the three local providers,
			// and the two above are taken: the remainder is the geofeed.
			chain[i] = lookup
		}
	}
	return chain
}

// countryChainOrder is the provider order countryChain walks: EVERY GEO
// annotate entry's chain, concatenated in written order and de-duplicated by
// first occurrence, so the filter's verdict and the [GEO:xx] tags resolve an IP
// through the same databases in the same precedence. Reading the FIRST entry
// alone inverted both verdicts on a config that split one chain across two
// entries: with the geofeed unable to place an IP DB-IP puts in DE and
// `[{GEO,[geofeed]},{GEO,[dbip]}]` written, `countries=DE` dropped the node as
// unplaceable while `exclude_countries=DE` KEPT it and published
// `[GEO:??][GEO:DE]` — a deny-list quietly ceasing to work. Concatenation is a
// no-op for every single-entry config, the shipped one included.
//
// asn is dropped (see countryChain), as is any provider this process did not
// build — the lazy-build rule in NewProcessor makes the latter unreachable for
// a GEO chain, but a nil geoDB in the chain would panic and that is too sharp
// an edge to leave resting on an argument made elsewhere.
//
// cloudflare is dropped for a harder reason than asn: the address it is asked
// about does not exist yet. It answers with what a node reported through ITS
// OWN proxy, and the IP stage runs before any proxy exists — there is nothing
// to ask. A `{type: country}` filter therefore stays offline, and a GEO chain
// led by cloudflare still filters on the local databases behind it.
//
// A merged order of the geofeed alone collapses to nil, as does one naming
// nothing local (GEO through asn alone) and a config with no GEO entry at all:
// countryChain then hands the filter the geofeed lookup directly instead of
// wrapping a single element. The geofeed is the one database every processor
// loads, so it is the only sound fallback when the config expresses no
// preference.
//
// Dedup bounds the result at the three local providers however many entries are
// written, so countryChain's per-request chainLookup cannot grow past what one
// entry could already ask for, and the walk itself fits a stack array: only the
// surviving order reaches the heap, exactly sized. This walk runs once per
// processor build, not per request — NewProcessor stores the result in
// p.countryOrder. Measured against a392316, the pre-merge parent, at
// -benchtime 200000x -count=2 on a 9800X3D: the shipped chain 80 -> 48 B/op,
// geofeed-only 16 -> 0, a two-entry split 16 -> 32, a three-entry split
// 16 -> 48; 1 alloc/op throughout except the nil collapse, which reaches 0.
//
// The PER-REQUEST cost is where this change is not free, and it is accepted,
// not absent. countryChain runs once per request from filterInto, and for a
// split-chain config p.countryOrder goes nil -> 2 or 3 entries, so that call
// stops handing back the geofeed lookup untouched and starts building a
// chainLookup and boxing it: `[{GEO,[geofeed]},{GEO,[dbip]}]` measures
// 0 B/op 0 allocs -> 56 B/op 2 allocs across those same two trees. That is
// exactly what the equivalent merged `[{GEO,[geofeed,dbip]}]` already cost —
// 56/2 on BOTH trees — so the split config now pays what its own verdict is
// worth and nothing more; the shipped single-entry config is unmoved at 72/2
// and geofeed-only stays at 0/0. AGENTS.md makes an allocs/op increase a
// finding the reviewer must accept before the round closes: this one was
// accepted as the price of the restored PP-02/VP-03 verdict.
func countryChainOrder(annotate []config.AnnotateSpec, haveDBIP, haveRegistry bool) []string {
	// localCountryProvider admits exactly three names and the dedup below
	// rejects repeats, so n can never run past the array.
	var buf [localCountryProviders]string
	n := 0
	for _, a := range annotate {
		if a.Tag != config.TagGEO {
			continue
		}
		for _, prov := range a.Providers {
			if !localCountryProvider(prov, haveDBIP, haveRegistry) || slices.Contains(buf[:n], prov) {
				continue
			}
			buf[n] = prov
			n++
		}
	}
	if n == 0 || (n == 1 && buf[0] == config.ProviderGeofeed) {
		return nil
	}
	return slices.Clone(buf[:n])
}

// localCountryProviders is how many providers countryChainOrder can emit: the
// geofeed plus the two downloadable databases.
const localCountryProviders = 3

// localCountryProvider reports whether prov is a local country database this
// process built, and so one the IP-stage filter can consult. asn and cloudflare
// are false by the arguments in countryChainOrder's doc comment; a provider
// this process did not build is false because its geoDB is nil.
func localCountryProvider(prov string, haveDBIP, haveRegistry bool) bool {
	switch prov {
	case config.ProviderGeofeed:
		return true
	case config.ProviderDBIP:
		return haveDBIP
	case config.ProviderRegistry:
		return haveRegistry
	default:
		return false
	}
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
// live one, or "" when the swap is safe: an empty current lookup, or one whose
// size is unknown, is always replaced. Only the type system permits an unsized
// lookup — no production path holds one — but lookupLen's -1 wraps to 1.8e19
// unsigned, so both conversions below are pinned by test, not by inspection.
func swapRefusal(current, loaded geofeed.CountryLookup, failed int) string {
	currentLen := lookupLen(current)
	if currentLen < 0 {
		return ""
	}
	// A replacement that cannot report a size cannot be shown to be healthy,
	// so the shrink guard refuses it exactly as it refuses an empty one.
	return swapRefusalSize(uint64(currentLen), uint64(max(lookupLen(loaded), 0)), failed)
}

// swapRefusalSize is the size-only half, shared with the cidr allow-list, which
// has no CountryLookup to measure. The unit is the caller's: ranges for the geo
// databases, COVERED ADDRESSES for the allow-list, whose merged range count
// moves independently of how much address space it admits. That second unit is
// why the sizes are uint64: 0.0.0.0/0 covers 2^32 addresses, which an int does
// not hold on a 32-bit build, and narrowing it to 0 there would silently
// disable this guard for the widest possible list. The geo callers widen their
// int range counts instead. The two zeroes are not symmetric: a zero
// currentSize is "nothing to protect" and allows, a zero loadedSize is a
// replacement that cannot be shown healthy and is refused as the collapse it is.
func swapRefusalSize(currentSize, loadedSize uint64, failed int) string {
	if currentSize == 0 {
		return ""
	}
	if failed > 0 {
		return "partial load"
	}
	if float64(loadedSize) < float64(currentSize)*minSwapRatio {
		return "catastrophic shrink"
	}
	return ""
}

// lookupLen reports how many ranges a lookup indexes, or -1 when it does not
// report a size. An unknown CURRENT size disables the swap guards: there is no
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

// GeofeedState hands this processor's geofeed database to its replacement
// across a config reload; DBIPState, RegistryState and CIDRState do the same.
// Each snapshot is taken under the read lock and returned whole, so a retry
// schedule can never travel without the data it throttles. A zero GeoState
// means the provider was not built in this processor.
func (p *Processor) GeofeedState() GeoState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return GeoState{
		Lookup:   p.countryLookup,
		LoadedAt: p.loadedAt,
		RetryAt:  p.retryAt,
		Failures: p.reloadFailures,
	}
}

func (p *Processor) DBIPState() GeoState {
	if p.dbip == nil {
		return GeoState{}
	}
	return p.dbip.state()
}

func (p *Processor) RegistryState() GeoState {
	if p.registry == nil {
		return GeoState{}
	}
	return p.registry.state()
}

func (p *Processor) CIDRState() CIDRState {
	if p.cidr == nil {
		return CIDRState{}
	}
	return p.cidr.state()
}

// ResolverState and ASNState expose the live DNS and Cymru resolvers so a
// reload can hand their warm caches to the replacement processor. Both caches
// are internally locked, so the returned resolver stays safe to share with the
// outgoing processor while requests it already accepted drain.
func (p *Processor) ResolverState() *resolver.Resolver { return p.resolver }

// ASNState returns nil when no filter or annotate chain needed ASN data.
func (p *Processor) ASNState() *asn.Resolver { return p.asnResolver }

// maybeRefreshDatabases opportunistically refreshes every downloaded table on
// the request path — the geo databases and the cidr allow-list — at the same
// trigger point as the geofeed refresh in currentEntries.
func (p *Processor) maybeRefreshDatabases(ctx context.Context) {
	if p.dbip != nil {
		p.dbip.maybeRefresh(ctx, p.logger)
	}
	if p.registry != nil {
		p.registry.maybeRefresh(ctx, p.logger)
	}
	if p.cidr != nil {
		p.cidr.maybeRefresh(ctx, p.logger)
	}
}

// newGeoDB builds the state for one lazily-referenced geo database. A
// preloaded state (reload carry-over) is adopted whole — data, load time and
// the retry schedule in flight — so a mirror that is currently failing keeps
// its backoff instead of being re-downloaded on the first request after every
// config reload. Otherwise the initial load runs inline but, unlike geofeed, a
// failure only WARNs and starts with an empty lookup: startup must never depend
// on a third-party database mirror. A failed or partial initial load schedules
// a short retry instead of waiting out the full refresh interval.
func newGeoDB(
	ctx context.Context,
	logger zerolog.Logger,
	name string,
	interval time.Duration,
	preloaded GeoState,
	load func(ctx context.Context) (ranges []geofeed.Range, failed int, err error),
) *geoDB {
	db := &geoDB{name: name, interval: interval, load: load}
	if preloaded.Lookup != nil {
		logger.Info().Str("db", name).Msg("using preloaded geo database")
		db.lookup = preloaded.Lookup
		db.loadedAt = preloaded.LoadedAt
		db.retryAt = preloaded.RetryAt
		db.reloadFailures = preloaded.Failures
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

// state returns the carry-over snapshot: the data and the retry schedule that
// belongs to it, read together under the lock.
func (db *geoDB) state() GeoState {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return GeoState{
		Lookup:   db.lookup,
		LoadedAt: db.loadedAt,
		RetryAt:  db.retryAt,
		Failures: db.reloadFailures,
	}
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

// cidrSpec returns the configured cidr filter's spec; config rejects a second
// one, so the first match is the only one.
func cidrSpec(specs []config.IPFilterSpec) (config.IPFilterSpec, bool) {
	for _, spec := range specs {
		if spec.Type == config.FilterCIDR {
			return spec, true
		}
	}
	return config.IPFilterSpec{}, false
}

// cidrLoadFor returns the downloader for spec, or override when a test set one.
func cidrLoadFor(spec config.IPFilterSpec, logger zerolog.Logger, override cidrLoader) cidrLoader {
	if override != nil {
		return override
	}
	urls, fileType := slices.Clone(spec.URLs), spec.FileType
	return func(ctx context.Context) (cidrset.Set, int, error) {
		return cidrset.Load(ctx, urls, fileType, logger)
	}
}

// errEmptyCIDRSet fails the build where an empty geo database only warns: an
// allow-list nobody is inside is not a milder filter but the opposite one, and
// the instance would publish nothing at all.
var errEmptyCIDRSet = errors.New("cidr allow-list loaded empty: every node would be dropped")

// newCIDRStore builds the allow-list behind a configured cidr filter. A
// preloaded state is adopted whole, exactly as newGeoDB adopts one; otherwise
// the initial load runs inline and a failed or empty one is fatal, per
// errEmptyCIDRSet.
func newCIDRStore(
	ctx context.Context,
	initLog zerolog.Logger,
	interval time.Duration,
	preloaded CIDRState,
	load cidrLoader,
) (*cidrStore, error) {
	store := &cidrStore{interval: interval, load: load}
	// No live processor can hold an empty set, so a non-empty one is exactly
	// "a reload carried its state over".
	if preloaded.Set.Len() > 0 {
		initLog.Info().Int("ranges", preloaded.Set.Len()).Msg("using preloaded cidr allow-list")
		store.set = preloaded.Set
		store.loadedAt = preloaded.LoadedAt
		store.retryAt = preloaded.RetryAt
		store.reloadFailures = preloaded.Failures
		return store, nil
	}
	initLog.Info().Msg("loading cidr allow-list")
	set, failed, err := load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load cidr allow-list: %w", err)
	}
	if set.Len() == 0 {
		return nil, errEmptyCIDRSet
	}
	if failed > 0 {
		// Same rule as the geofeed startup load: take what there is, but do
		// not call it fresh for a whole interval.
		initLog.Warn().Int("sources_failed", failed).Int("ranges", set.Len()).
			Dur("retry_in", store.scheduleRetryLocked()).
			Msg("initial cidr allow-list load is partial; retrying shortly")
	}
	store.set = set
	store.loadedAt = time.Now()
	initLog.Info().Int("ranges", set.Len()).Msg("cidr allow-list loaded")
	return store, nil
}

// snapshot backs the filter's getter, so a node is judged against the list the
// last background refresh left behind. Taken per NODE, unlike the sibling
// tables that snapshot once per request into pctx.Lookup: the extra RLock pair
// is free on the single-goroutine / path and, measured against that
// alternative, costs a real but immaterial fraction of a concurrent worker
// cycle — and a list that swaps mid-request is still one the operator
// published, so either verdict is valid.
func (s *cidrStore) snapshot() cidrset.Set {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.set
}

// state returns the carry-over snapshot: the data and the retry schedule that
// belongs to it, read together under the lock.
func (s *cidrStore) state() CIDRState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return CIDRState{
		Set:      s.set,
		LoadedAt: s.loadedAt,
		RetryAt:  s.retryAt,
		Failures: s.reloadFailures,
	}
}

func (s *cidrStore) maybeRefresh(ctx context.Context, logger zerolog.Logger) {
	s.mu.RLock()
	stale := s.staleLocked(time.Now())
	s.mu.RUnlock()
	if !stale {
		return
	}
	if s.reloadMu.TryLock() {
		bgCtx := context.WithoutCancel(ctx)
		go func() {
			defer s.reloadMu.Unlock()
			s.doReload(bgCtx, logger)
		}()
	}
}

// staleLocked reports whether the allow-list needs a reload. Callers must hold
// s.mu (read or write); a pending retryAt gates the next attempt.
func (s *cidrStore) staleLocked(now time.Time) bool {
	if s.interval <= 0 {
		return false
	}
	if !s.retryAt.IsZero() {
		return !now.Before(s.retryAt)
	}
	if s.loadedAt.IsZero() {
		return true
	}
	return now.Sub(s.loadedAt) >= s.interval
}

// scheduleRetryLocked arms the next attempt and returns the delay it picked.
// Callers must hold s.mu (newCIDRStore runs before the store is reachable).
func (s *cidrStore) scheduleRetryLocked() time.Duration {
	delay := retryDelay(s.reloadFailures, s.interval)
	s.reloadFailures++
	s.retryAt = time.Now().Add(delay)
	return delay
}

func (s *cidrStore) doReload(ctx context.Context, logger zerolog.Logger) {
	next, failed, err := s.load(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		logger.Error().Err(err).Dur("retry_in", s.scheduleRetryLocked()).
			Msg("background cidr allow-list reload failed, keeping stale data")
		return
	}

	if reason := swapRefusalSize(s.set.Covered(), next.Covered(), failed); reason != "" {
		logger.Error().Str("reason", reason).Int("sources_failed", failed).
			Int("loaded", next.Len()).Int("current", s.set.Len()).
			Uint64("loaded_covered", next.Covered()).Uint64("current_covered", s.set.Covered()).
			Dur("retry_in", s.scheduleRetryLocked()).
			Msg("background cidr allow-list reload refused, keeping existing data")
		return
	}

	s.set = next
	s.loadedAt = time.Now()
	s.retryAt = time.Time{}
	s.reloadFailures = 0
	logger.Info().Int("ranges", next.Len()).Msg("cidr allow-list reloaded in background")
}

func FormatStats(stats Stats) string {
	return fmt.Sprintf("done: total=%d kept=%d dns_drop=%d geo_drop=%d asn_drop=%d cidr_drop=%d geoblock_drop=%d ipv6_drop=%d unsupported=%d",
		stats.Total, stats.Kept, stats.DNSDrop, stats.GeoDrop, stats.ASNDrop, stats.CIDRDrop, stats.GeoBlockDrop, stats.IPv6Drop, stats.Unsupported)
}
