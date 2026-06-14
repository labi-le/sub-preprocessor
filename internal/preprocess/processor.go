package preprocess

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

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
	strictDNS         bool
	countriesCacheKey string
	countriesCacheVal filter.CountrySet
	countriesCacheMu  sync.Mutex
	resolver          *resolver.Resolver
}

type Stats struct {
	Total        int
	Kept         int
	DNSDrop      int
	GeoDrop      int
	StrictReject int
	Unsupported  int
}

func NewProcessor(ctx context.Context, geofeedSources []geofeed.Source, refreshInterval, dnsTimeout time.Duration, strictDNS bool) (*Processor, error) {
	entries, err := geofeed.LoadAll(ctx, geofeedSources)
	if err != nil {
		return nil, fmt.Errorf("load geofeed: %w", err)
	}

	return &Processor{
		entries:         entries,
		sources:         append([]geofeed.Source(nil), geofeedSources...),
		LoadedAt:        time.Now(),
		RefreshInterval: refreshInterval,
		strictDNS:       strictDNS,
		resolver:        resolver.New(dnsTimeout),
	}, nil
}

func (p *Processor) Filter(ctx context.Context, b *strings.Builder, subscriptionURL string, rawCountries string) (Stats, error) {
	entries, err := p.currentEntries(ctx)
	if err != nil {
		return Stats{}, err
	}

	allowed := p.getAllowedCountries(rawCountries)
	// Check if the bitset is empty (all zeros)
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

func (p *Processor) processNode(ctx context.Context, node subscription.Node, b *strings.Builder, entries []geofeed.Entry, allowed filter.CountrySet, resolved map[string][]netip.Addr, stats *Stats, first *bool) {
	stats.Total++
	if node.Server == "" || node.Port == "" {
		stats.Unsupported++
		return
	}

	ips, attempted := resolved[node.Server]
	if !attempted {
		var resolveErr error
		ips, resolveErr = p.resolver.Resolve(ctx, node.Server)
		if resolveErr != nil || len(ips) == 0 {
			ips = []netip.Addr{} // empty slice means attempted and failed
		}
		resolved[node.Server] = ips
	}

	if len(ips) == 0 {
		stats.DNSDrop++
		return
	}

	chosenIP, chosenCountry, ok := filter.FirstAllowed(entries, ips, allowed, p.strictDNS)
	if !ok {
		if p.strictDNS {
			stats.StrictReject++
		} else {
			stats.GeoDrop++
		}
		return
	}

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
	const growSize = 160
	b.Grow(growSize)
	b.WriteString("done: total=")
	b.WriteString(strconv.Itoa(stats.Total))
	b.WriteString(" kept=")
	b.WriteString(strconv.Itoa(stats.Kept))
	b.WriteString(" dns_drop=")
	b.WriteString(strconv.Itoa(stats.DNSDrop))
	b.WriteString(" geo_drop=")
	b.WriteString(strconv.Itoa(stats.GeoDrop))
	b.WriteString(" strict_reject=")
	b.WriteString(strconv.Itoa(stats.StrictReject))
	b.WriteString(" unsupported=")
	b.WriteString(strconv.Itoa(stats.Unsupported))
	return b.String()
}
