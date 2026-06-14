package preprocess

import (
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
	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/resolver"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

type Processor struct {
	mu                sync.RWMutex
	entries           []geofeed.Entry
	sources           []geofeed.Source
	LoadedAt          time.Time
	RefreshInterval   time.Duration
	countriesCacheKey string
	countriesCacheVal filter.CountrySet
	countriesCacheMu  sync.Mutex
	resolver          *resolver.Resolver
	filters           []Filter
	workflowAlgorithm string
}

type Stats struct {
	Total       int
	Kept        int
	DNSDrop     int
	GeoDrop     int
	ASNDrop     int
	Unsupported int
}

func NewProcessor(ctx context.Context, geofeedSources []geofeed.Source, refreshInterval time.Duration, dnsTimeout time.Duration, dnsAddress string, asnTimeout time.Duration, asnDenyPatterns []string, workflowStages []string, workflowAlgorithm string) (*Processor, error) {
	entries, err := geofeed.LoadAll(ctx, geofeedSources)
	if err != nil {
		return nil, fmt.Errorf("load geofeed: %w", err)
	}

	patterns := make([]*regexp.Regexp, 0, len(asnDenyPatterns))
	for _, p := range asnDenyPatterns {
		re, errCompile := regexp.Compile(p)
		if errCompile != nil {
			return nil, fmt.Errorf("compile asn deny pattern %q: %w", p, errCompile)
		}
		patterns = append(patterns, re)
	}

	var asnR *asn.Resolver
	if len(patterns) > 0 {
		asnR = asn.New(asnTimeout)
	}

	filters := buildFilters(workflowStages, asnR, patterns)

	return &Processor{
		entries:           entries,
		sources:           append([]geofeed.Source(nil), geofeedSources...),
		LoadedAt:          time.Now(),
		RefreshInterval:   refreshInterval,
		resolver:          resolver.New(dnsTimeout, dnsAddress),
		filters:           filters,
		workflowAlgorithm: workflowAlgorithm,
	}, nil
}

func (p *Processor) Filter(ctx context.Context, b *strings.Builder, subscriptionURL string, rawCountries string) (Stats, error) {
	entries, err := p.currentEntries(ctx)
	if err != nil {
		return Stats{}, err
	}

	allowed := p.getAllowedCountries(rawCountries)
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

	body, errLoad := subscription.Load(ctx, subscriptionURL)
	if errLoad != nil {
		return Stats{}, fmt.Errorf("load subscription: %w", errLoad)
	}

	stats := Stats{}

	resolved := p.resolver.GetResolvedMap()
	defer p.resolver.PutResolvedMap(resolved)

	first := true
	subscription.Parse(body, func(node subscription.Node) bool {
		p.processNode(ctx, node, b, entries, allowed, resolved, &stats, &first)
		return true
	})

	if stats.Total == 0 {
		return stats, errors.New("no supported URI nodes found")
	}

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

func (p *Processor) processNode(ctx context.Context, node subscription.Node, b *strings.Builder, entries []geofeed.Entry, allowed filter.CountrySet, resolved map[string][]netip.Addr, stats *Stats, first *bool) {
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
		ips = f.Process(ctx, ips, entries, allowed, stats)
		if len(ips) == 0 {
			return
		}
		if p.workflowAlgorithm == config.WorkflowFailFirst {
			ips = ips[:1]
		}
	}

	chosenIP := ips[0]
	chosenCountry := geofeed.LookupCountry(entries, chosenIP)

	if !*first {
		b.WriteByte('\n')
	}
	*first = false
	rewrite.NodeName(b, node, chosenCountry, chosenIP)
	stats.Kept++
}

func (p *Processor) currentEntries(ctx context.Context) ([]geofeed.Entry, error) {
	p.mu.RLock()
	if !p.ShouldReloadGeofeed(time.Now()) {
		entries := p.entries
		p.mu.RUnlock()
		return entries, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ShouldReloadGeofeed(time.Now()) {
		return p.entries, nil
	}

	entries, err := geofeed.LoadAll(ctx, p.sources)
	if err != nil {
		return nil, fmt.Errorf("load geofeed: %w", err)
	}
	p.entries = entries
	p.LoadedAt = time.Now()
	return p.entries, nil
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

func (p *Processor) getAllowedCountries(rawCountries string) filter.CountrySet {
	p.countriesCacheMu.Lock()
	defer p.countriesCacheMu.Unlock()
	if rawCountries == p.countriesCacheKey {
		return p.countriesCacheVal
	}
	out := filter.ParseAllowCountries(rawCountries)
	p.countriesCacheKey = rawCountries
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
