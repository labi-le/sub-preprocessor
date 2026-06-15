package preprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
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
	GeofeedSources  []geofeed.Source
	RefreshInterval time.Duration
	DNSTimeout      time.Duration
	DNSAddress      string
	ASNTimeout      time.Duration
	ASNDenyPatterns []string
	WorkflowStages  []string
	GroupsMap       map[string][]string
}

type FilterRequest struct {
	SubscriptionURL fetch.SubscriptionURL
	RawCountries    string
	RawGroups       string
}

type Processor struct {
	mu                sync.RWMutex
	logger            zerolog.Logger
	entries           []geofeed.Entry
	countryLookup     geofeed.CountryLookup
	sources           []geofeed.Source
	LoadedAt          time.Time
	RefreshInterval   time.Duration
	countriesCacheKey struct{ countries, groups string }
	countriesCacheVal filter.CountrySet
	countriesCacheMu  sync.Mutex
	resolver          *resolver.Resolver
	filters           []Filter
	groupsMap         map[string][]string
}

type Stats struct {
	Total       int
	Kept        int
	DNSDrop     int
	GeoDrop     int
	ASNDrop     int
	Unsupported int
}

func NewProcessor(ctx context.Context, logger zerolog.Logger, opts Options) (*Processor, error) {
	initLog := log.Op(logger, "processor.New")
	initLog.Info().Int("sources", len(opts.GeofeedSources)).Msg("loading geofeed")

	entries, err := geofeed.LoadAll(ctx, opts.GeofeedSources)
	if err != nil {
		return nil, fmt.Errorf("load geofeed: %w", err)
	}
	initLog.Info().Int("entries", len(entries)).Msg("geofeed loaded")

	patterns := make([]*regexp.Regexp, 0, len(opts.ASNDenyPatterns))
	for _, p := range opts.ASNDenyPatterns {
		re, errCompile := regexp.Compile(p)
		if errCompile != nil {
			return nil, fmt.Errorf("compile asn deny pattern %q: %w", p, errCompile)
		}
		patterns = append(patterns, re)
	}

	var asnR *asn.Resolver
	if len(patterns) > 0 {
		asnR = asn.New(opts.ASNTimeout)
	}

	filters := buildFilters(opts.WorkflowStages, asnR, patterns)

	return &Processor{
		logger:          logger,
		entries:         entries,
		countryLookup:   geofeed.NewLookup(entries),
		sources:         append([]geofeed.Source(nil), opts.GeofeedSources...),
		LoadedAt:        time.Now(),
		RefreshInterval: opts.RefreshInterval,
		resolver:        resolver.New(opts.DNSTimeout, opts.DNSAddress),
		filters:         filters,
		groupsMap:       opts.GroupsMap,
	}, nil
}

func (p *Processor) Filter(ctx context.Context, b *bytes.Buffer, req FilterRequest) (Stats, error) {
	requestLog := p.logger.With().Str("url", string(req.SubscriptionURL)).Str("countries", req.RawCountries).Logger()
	start := time.Now()

	_, lookup, err := p.currentEntries(ctx)
	if err != nil {
		return Stats{}, err
	}

	allowed := p.getAllowedCountries(req.RawCountries, req.RawGroups)
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

	first := true
	subscription.Parse(body, func(node subscription.Node) bool {
		p.processNode(ctx, node, b, lookup, allowed, resolved, &stats, &first)
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
		resolved[server] = ips
	}
	return ips
}

func (p *Processor) processNode(ctx context.Context, node subscription.Node, b *bytes.Buffer, lookup geofeed.CountryLookup, allowed filter.CountrySet, resolved map[string][]netip.Addr, stats *Stats, first *bool) {
	stats.Total++
	if node.Server == "" || node.Port == "" {
		stats.Unsupported++
		return
	}

	ips := p.resolveNode(ctx, node.Server, resolved)
	if len(ips) == 0 {
		stats.DNSDrop++
		return
	}

	for _, f := range p.filters {
		ips = f.Process(ctx, ips, lookup, allowed, stats)
		if len(ips) == 0 {
			return
		}
	}

	chosenIP := ips[0]
	chosenCountry := geofeed.LookupCountry(lookup, chosenIP)

	if !*first {
		b.WriteByte('\n')
	}
	*first = false
	rewrite.NodeName(b, node, chosenCountry, chosenIP)
	stats.Kept++
}

//nolint:ireturn // processor stores and returns the lookup behind an interface on purpose
func (p *Processor) currentEntries(ctx context.Context) ([]geofeed.Entry, geofeed.CountryLookup, error) {
	p.mu.RLock()
	if !p.ShouldReloadGeofeed(time.Now()) {
		entries := p.entries
		lookup := p.countryLookup
		p.mu.RUnlock()
		return entries, lookup, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ShouldReloadGeofeed(time.Now()) {
		return p.entries, p.countryLookup, nil
	}

	entries, err := geofeed.LoadAll(ctx, p.sources)
	if err != nil {
		return nil, nil, fmt.Errorf("load geofeed: %w", err)
	}
	p.entries = entries
	p.countryLookup = geofeed.NewLookup(entries)
	p.LoadedAt = time.Now()
	return p.entries, p.countryLookup, nil
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

func (p *Processor) getAllowedCountries(rawCountries string, rawGroups string) filter.CountrySet {
	p.countriesCacheMu.Lock()
	defer p.countriesCacheMu.Unlock()
	key := struct{ countries, groups string }{rawCountries, rawGroups}
	if key == p.countriesCacheKey {
		return p.countriesCacheVal
	}
	out := filter.ParseAllowed(rawCountries, rawGroups, p.groupsMap)
	p.countriesCacheKey = key
	p.countriesCacheVal = out
	return out
}

func FormatStats(stats Stats) string {
	var b strings.Builder
	const growSize = 200
	b.Grow(growSize)
	b.WriteString("done: total=")
	b.WriteString(strconv.Itoa(stats.Total))
	b.WriteString(" kept=")
	b.WriteString(strconv.Itoa(stats.Kept))
	b.WriteString(" dns_drop=")
	b.WriteString(strconv.Itoa(stats.DNSDrop))
	b.WriteString(" geo_drop=")
	b.WriteString(strconv.Itoa(stats.GeoDrop))
	b.WriteString(" asn_drop=")
	b.WriteString(strconv.Itoa(stats.ASNDrop))
	b.WriteString(" unsupported=")
	b.WriteString(strconv.Itoa(stats.Unsupported))
	return b.String()
}
