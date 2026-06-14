package preprocess

import (
	"context"
	"errors"
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
	LoadedAt        time.Time
	RefreshInterval time.Duration
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
		return nil, fmt.Errorf("load geofeed: %w", err)
	}

	return &Service{
		entries:         entries,
		sources:         append([]geofeed.Source(nil), geofeedSources...),
		LoadedAt:        time.Now(),
		RefreshInterval: refreshInterval,
		dnsTimeout:      dnsTimeout,
		strictDNS:       strictDNS,
	}, nil
}

func (s *Service) Filter(ctx context.Context, subscriptionURL string, countries []string) ([]string, Stats, error) {
	entries, err := s.currentEntries(ctx)
	if err != nil {
		return nil, Stats{}, err
	}

	allowed := ParseAllowCountries(countries)
	if len(allowed) == 0 {
		return nil, Stats{}, errors.New("no allowed countries provided")
	}

	nodes, errLoad := subscription.Load(ctx, subscriptionURL)
	if errLoad != nil {
		return nil, Stats{}, fmt.Errorf("load subscription: %w", errLoad)
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
		ips, errResolve := resolveIPv4(resolveCtx, node.Server)
		cancel()
		if errResolve != nil || len(ips) == 0 {
			stats.DNSDrop++
			continue
		}

		if s.strictDNS && !AllIPsAllowed(entries, ips, allowed) {
			stats.StrictReject++
			continue
		}

		chosenIP, chosenCountry, ok := FirstAllowedIP(entries, ips, allowed)
		if !ok {
			stats.GeoDrop++
			continue
		}

		output = append(output, RewriteNodeName(node, chosenCountry, chosenIP))
		stats.Kept++
	}

	return output, stats, nil
}

func (s *Service) currentEntries(ctx context.Context) ([]geofeed.Entry, error) {
	s.mu.RLock()
	if !s.ShouldReloadGeofeed(time.Now()) {
		entries := s.entries
		s.mu.RUnlock()
		return entries, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ShouldReloadGeofeed(time.Now()) {
		return s.entries, nil
	}

	entries, err := geofeed.LoadAll(ctx, s.sources)
	if err != nil {
		return nil, fmt.Errorf("load geofeed: %w", err)
	}
	s.entries = entries
	s.LoadedAt = time.Now()
	return s.entries, nil
}

func (s *Service) ShouldReloadGeofeed(now time.Time) bool {
	if s.RefreshInterval <= 0 {
		return false
	}
	if s.LoadedAt.IsZero() {
		return true
	}
	return now.Sub(s.LoadedAt) >= s.RefreshInterval
}

func resolveIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is4() {
			return []netip.Addr{addr}, nil
		}
		return nil, errors.New("not an IPv4 address")
	}

	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, fmt.Errorf("dns lookup: %w", err)
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

func AllIPsAllowed(entries []geofeed.Entry, ips []netip.Addr, allowed map[string]bool) bool {
	for _, ip := range ips {
		if !allowed[geofeed.LookupCountry(entries, ip)] {
			return false
		}
	}
	return true
}

func FirstAllowedIP(entries []geofeed.Entry, ips []netip.Addr, allowed map[string]bool) (netip.Addr, string, bool) {
	for i := range ips {
		ip := ips[i]
		country := geofeed.LookupCountry(entries, ip)
		if allowed[country] {
			return ip, country, true
		}
	}
	return netip.Addr{}, "", false
}

func ParseAllowCountries(countries []string) map[string]bool {
	out := make(map[string]bool, len(countries))
	for _, country := range countries {
		country = strings.ToUpper(strings.TrimSpace(country))
		if country != "" {
			out[country] = true
		}
	}
	return out
}

func RewriteNodeName(node subscription.Node, country string, ip netip.Addr) string {
	if !supportsFragmentRewrite(node) {
		return node.Raw
	}

	cleanName := StripKnownTags(node.Name)
	if cleanName == "" {
		cleanName = node.Server
	}

	// Use strings.Builder to avoid fmt.Sprintf allocations.
	var b strings.Builder
	const growExtra = 32 // rough upper bound: raw + tags overhead
	b.Grow(len(node.Raw) + growExtra)

	// Find the '#' separator instead of strings.Cut to avoid extra string copy.
	hashIdx := strings.IndexByte(node.Raw, '#')
	if hashIdx >= 0 {
		b.WriteString(node.Raw[:hashIdx])
	} else {
		b.WriteString(node.Raw)
	}
	b.WriteString("#[GEO:")
	b.WriteString(country)
	b.WriteString("][IP:")
	// Write IPv4 octets directly — avoids ip.String() allocation.
	ip4 := ip.As4()
	writeOctet(&b, ip4[0])
	b.WriteByte('.')
	writeOctet(&b, ip4[1])
	b.WriteByte('.')
	writeOctet(&b, ip4[2])
	b.WriteByte('.')
	writeOctet(&b, ip4[3])
	b.WriteString("] ")
	b.WriteString(cleanName)
	return b.String()
}

// writeOctet writes a uint8 (0-255) to a strings.Builder without allocating.
func writeOctet(b *strings.Builder, n byte) {
	if n >= 100 {
		b.WriteByte('0' + n/100)
		b.WriteByte('0' + (n/10)%10)
		b.WriteByte('0' + n%10)
	} else if n >= 10 {
		b.WriteByte('0' + n/10)
		b.WriteByte('0' + n%10)
	} else {
		b.WriteByte('0' + n)
	}
}

func supportsFragmentRewrite(node subscription.Node) bool {
	return node.Scheme != ""
}

func StripKnownTags(s string) string {
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
