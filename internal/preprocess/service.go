package preprocess

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/subscription"
)

type Service struct {
	mu              sync.RWMutex
	entries         []geofeed.Entry
	sources         []geofeed.Source
	loadedAt        time.Time
	refreshInterval time.Duration
	dnsTimeout      time.Duration
	strictDNS       bool
}

type Stats struct {
	Total        int
	Kept         int
	DNSDrop      int
	GeoDrop      int
	StrictReject int
	Unsupported  int
}

func NewService(ctx context.Context, geofeedSources []geofeed.Source, refreshInterval, dnsTimeout time.Duration, strictDNS bool) (*Service, error) {
	entries, err := geofeed.LoadAll(ctx, geofeedSources)
	if err != nil {
		return nil, err
	}

	return &Service{
		entries:         entries,
		sources:         append([]geofeed.Source(nil), geofeedSources...),
		loadedAt:        time.Now(),
		refreshInterval: refreshInterval,
		dnsTimeout:      dnsTimeout,
		strictDNS:       strictDNS,
	}, nil
}

func (s *Service) Filter(ctx context.Context, subscriptionURL string, countries []string) ([]string, Stats, error) {
	entries, err := s.currentEntries(ctx)
	if err != nil {
		return nil, Stats{}, err
	}

	allowed := parseAllowCountries(countries)
	if len(allowed) == 0 {
		return nil, Stats{}, fmt.Errorf("no allowed countries provided")
	}

	nodes, err := subscription.Load(ctx, subscriptionURL)
	if err != nil {
		return nil, Stats{}, err
	}

	stats := Stats{}
	var output []string

	for _, node := range nodes {
		stats.Total++
		if node.Server == "" || node.Port == "" {
			stats.Unsupported++
			continue
		}

		resolveCtx, cancel := context.WithTimeout(ctx, s.dnsTimeout)
		ips, err := resolveIPv4(resolveCtx, node.Server)
		cancel()
		if err != nil || len(ips) == 0 {
			stats.DNSDrop++
			continue
		}

		if s.strictDNS && !allIPsAllowed(entries, ips, allowed) {
			stats.StrictReject++
			continue
		}

		chosenIP, chosenCountry, ok := firstAllowedIP(entries, ips, allowed)
		if !ok {
			stats.GeoDrop++
			continue
		}

		output = append(output, rewriteNodeName(node, chosenCountry, chosenIP))
		stats.Kept++
	}

	return output, stats, nil
}

func (s *Service) currentEntries(ctx context.Context) ([]geofeed.Entry, error) {
	s.mu.RLock()
	if !s.shouldReloadGeofeed(time.Now()) {
		entries := s.entries
		s.mu.RUnlock()
		return entries, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.shouldReloadGeofeed(time.Now()) {
		return s.entries, nil
	}

	entries, err := geofeed.LoadAll(ctx, s.sources)
	if err != nil {
		return nil, err
	}
	s.entries = entries
	s.loadedAt = time.Now()
	return s.entries, nil
}

func (s *Service) shouldReloadGeofeed(now time.Time) bool {
	if s.refreshInterval <= 0 {
		return false
	}
	if s.loadedAt.IsZero() {
		return true
	}
	return now.Sub(s.loadedAt) >= s.refreshInterval
}

func resolveIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is4() {
			return []netip.Addr{addr}, nil
		}
		return nil, fmt.Errorf("not an IPv4 address")
	}

	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}

	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, ip := range ips {
		if ip.Is4() && !seen[ip] {
			out = append(out, ip)
			seen[ip] = true
		}
	}
	return out, nil
}

func allIPsAllowed(entries []geofeed.Entry, ips []netip.Addr, allowed map[string]bool) bool {
	for _, ip := range ips {
		if !allowed[geofeed.LookupCountry(entries, ip)] {
			return false
		}
	}
	return true
}

func firstAllowedIP(entries []geofeed.Entry, ips []netip.Addr, allowed map[string]bool) (netip.Addr, string, bool) {
	for _, ip := range ips {
		country := geofeed.LookupCountry(entries, ip)
		if allowed[country] {
			return ip, country, true
		}
	}
	return netip.Addr{}, "", false
}

func parseAllowCountries(countries []string) map[string]bool {
	out := map[string]bool{}
	for _, country := range countries {
		country = strings.ToUpper(strings.TrimSpace(country))
		if country != "" {
			out[country] = true
		}
	}
	return out
}

func rewriteNodeName(node subscription.Node, country string, ip netip.Addr) string {
	cleanName := stripKnownTags(node.Name)
	if cleanName == "" {
		cleanName = node.Server
	}

	fragment := fmt.Sprintf("[GEO:%s][IP:%s] %s", country, ip.String(), cleanName)
	base, _, _ := strings.Cut(node.Raw, "#")
	return base + "#" + fragment
}

func stripKnownTags(s string) string {
	s = strings.TrimSpace(s)
	for {
		if !strings.HasPrefix(s, "[") {
			return strings.TrimSpace(s)
		}
		end := strings.Index(s, "]")
		if end < 0 {
			return strings.TrimSpace(s)
		}
		tag := s[1:end]
		if strings.HasPrefix(tag, "GEO:") || strings.HasPrefix(tag, "IP:") || strings.HasPrefix(tag, "JUR:") || tag == "OK" || tag == "BAD" {
			s = strings.TrimSpace(s[end+1:])
			continue
		}
		return strings.TrimSpace(s)
	}
}

func FormatStats(stats Stats) string {
	parts := []string{
		fmt.Sprintf("done: total=%d", stats.Total),
		fmt.Sprintf("kept=%d", stats.Kept),
		fmt.Sprintf("dns_drop=%d", stats.DNSDrop),
		fmt.Sprintf("geo_drop=%d", stats.GeoDrop),
		fmt.Sprintf("strict_reject=%d", stats.StrictReject),
		fmt.Sprintf("unsupported=%d", stats.Unsupported),
	}
	return strings.Join(parts, " ")
}
