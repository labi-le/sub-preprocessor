package preprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"sync"
	"time"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/log"
	"domains.lst/sub-preprocessor/internal/resolver"
	"domains.lst/sub-preprocessor/internal/rewrite"
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
	ASNDenyPatterns     []string
	WorkflowStages      []string
	PreloadedGeofeed    geofeed.CountryLookup
	PreloadedLoadedAt   time.Time
	Blocklist           Blocklist
}

type FilterRequest struct {
	SubscriptionURL  fetch.SubscriptionURL
	AllowedCountries filter.CountrySet
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
	LoadedAt        time.Time
	RefreshInterval time.Duration
	resolver        *resolver.Resolver
	filters         []Filter
	blocklist       Blocklist
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
	Buffer      *bytes.Buffer
	Lookup      geofeed.CountryLookup
	Allowed     filter.CountrySet
	Resolved    map[string][]netip.Addr
	Stats       *Stats
	IsFirstNode bool
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
		entries, err := geofeed.LoadAll(ctx, opts.GeofeedSources)
		if err != nil {
			return nil, fmt.Errorf("load geofeed: %w", err)
		}
		initLog.Info().Int("entries", len(entries)).Msg("geofeed loaded")
		lookup = geofeed.NewLookup(entries)
		loadedAt = time.Now()
	}

	patterns := make([]*regexp.Regexp, 0, len(opts.ASNDenyPatterns))
	for _, p := range opts.ASNDenyPatterns {
		re, errCompile := regexp.Compile(p)
		if errCompile != nil {
			return nil, fmt.Errorf("compile asn deny pattern %q: %w", p, errCompile)
		}
		patterns = append(patterns, re)
	}

	var asnR *asn.Resolver
	if len(patterns) > 0 || slices.Contains(opts.WorkflowStages, "asn") {
		asnR = asn.New(opts.ASNTimeout)
	}

	filters := buildFilters(opts.WorkflowStages, asnR, patterns)

	return &Processor{
		logger:          logger,
		countryLookup:   lookup,
		sources:         append([]geofeed.Source(nil), opts.GeofeedSources...),
		LoadedAt:        loadedAt,
		RefreshInterval: opts.RefreshInterval,
		resolver:        resolver.New(opts.DNSTimeout, opts.DNSAddress, opts.DNSCacheTTL, opts.DNSCacheNegativeTTL),
		blocklist:       opts.Blocklist,
		filters:         filters,
	}, nil
}

func (p *Processor) Filter(ctx context.Context, b *bytes.Buffer, req FilterRequest) (Stats, error) {
	requestLog := p.logger.With().Str("url", string(req.SubscriptionURL)).Logger()
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

	body, errLoad := subscription.Load(ctx, req.SubscriptionURL)
	if errLoad != nil {
		return Stats{}, fmt.Errorf("load subscription: %w", errLoad)
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

	subscription.Parse(body, func(node subscription.Node) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		p.processNode(ctx, node, pctx)
		return true
	})

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

func (p *Processor) resolveNode(ctx context.Context, server string, resolved map[string][]netip.Addr) []netip.Addr {
	ips, attempted := resolved[server]
	if !attempted {
		var resolveErr error
		ips, resolveErr = p.resolver.Resolve(ctx, server)
		if resolveErr != nil || len(ips) == 0 {
			resolved[server] = []netip.Addr{}
			return []netip.Addr{}
		}
		resolved[server] = append([]netip.Addr(nil), ips...)
		ips = resolved[server]
	}
	return ips
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

	ips := p.resolveNode(ctx, node.Server, pctx.Resolved)
	if len(ips) == 0 {
		pctx.Stats.DNSDrop++
		return
	}

	for _, f := range p.filters {
		ips = f.Process(ctx, ips, pctx)
		if len(ips) == 0 {
			return
		}
	}

	chosenIP := ips[0]
	chosenCountry := geofeed.LookupCountry(pctx.Lookup, chosenIP)

	if !pctx.IsFirstNode {
		pctx.Buffer.WriteByte('\n')
	}
	pctx.IsFirstNode = false
	rewrite.NodeName(pctx.Buffer, node, chosenCountry, chosenIP)
	pctx.Stats.Kept++
}

//nolint:ireturn // pre-existing: returns interface for flexibility
func (p *Processor) currentEntries(ctx context.Context) geofeed.CountryLookup {
	p.mu.RLock()
	lookup := p.countryLookup
	needsReload := p.ShouldReloadGeofeed(time.Now())
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
	entries, err := geofeed.LoadAll(ctx, p.sources)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.LoadedAt = time.Now()

	if err != nil {
		p.logger.Error().Err(err).Msg("background geofeed reload failed, keeping stale data")
		return
	}

	p.countryLookup = geofeed.NewLookup(entries)
	p.logger.Info().Int("entries", len(entries)).Msg("geofeed reloaded in background")
}

func (p *Processor) ShouldReloadGeofeed(now time.Time) bool {
	if p.RefreshInterval <= 0 {
		return false
	}
	if p.LoadedAt.IsZero() {
		return true
	}
	return now.Sub(p.LoadedAt) >= p.RefreshInterval
}

//nolint:ireturn // returns the countryLookup interface so callers can carry geofeed state across reloads
func (p *Processor) GeofeedState() (geofeed.CountryLookup, time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.countryLookup, p.LoadedAt
}

func FormatStats(stats Stats) string {
	return fmt.Sprintf("done: total=%d kept=%d dns_drop=%d geo_drop=%d asn_drop=%d geoblock_drop=%d unsupported=%d",
		stats.Total, stats.Kept, stats.DNSDrop, stats.GeoDrop, stats.ASNDrop, stats.GeoBlockDrop, stats.Unsupported)
}
