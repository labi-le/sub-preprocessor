package preprocess

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/subscription"
)

type Service struct {
	mu                sync.RWMutex
	entries           []geofeed.Entry
	sources           []geofeed.Source
	LoadedAt          time.Time
	RefreshInterval   time.Duration
	dnsTimeout        time.Duration
	strictDNS         bool
	countriesCacheKey string
	countriesCacheVal map[string]bool
	countriesCacheMu  sync.Mutex
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

	allowed := s.getAllowedCountries(countries)
	if len(allowed) == 0 {
		return nil, Stats{}, errors.New("no allowed countries provided")
	}

	nodes, errLoad := subscription.Load(ctx, subscriptionURL)
	if errLoad != nil {
		return nil, Stats{}, fmt.Errorf("load subscription: %w", errLoad)
	}

	stats := Stats{}
	output := make([]string, 0, len(nodes))

	// Step 1: collect unique servers for batched DNS resolution
	uniqueServers := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node.Server == "" || node.Port == "" {
			continue
		}
		uniqueServers[node.Server] = struct{}{}
	}

	// Step 2: resolve all unique servers once and cache results
	resolved := make(map[string][]netip.Addr, len(uniqueServers))
	for server := range uniqueServers {
		resolveCtx, cancel := context.WithTimeout(ctx, s.dnsTimeout)
		ips, resolveErr := resolveIPv4(resolveCtx, server)
		cancel()
		if resolveErr == nil && len(ips) > 0 {
			resolved[server] = ips
		}
	}

	// Step 3: filter loop — look up from resolved map
	for _, node := range nodes {
		stats.Total++
		if node.Server == "" || node.Port == "" {
			stats.Unsupported++
			continue
		}

		ips, ok := resolved[node.Server]
		if !ok {
			stats.DNSDrop++
			continue
		}

		chosenIP, chosenCountry, ok := filterAndFirstAllowed(entries, ips, allowed, s.strictDNS)
		if !ok {
			if s.strictDNS {
				stats.StrictReject++
			} else {
				stats.GeoDrop++
			}
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

// resolveIPv4 resolves a hostname to IPv4 addresses only.
// IPv6 addresses are silently dropped (full IPv6 support is not yet implemented).
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
	seen := make(map[netip.Addr]bool, len(ips))
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

// filterAndFirstAllowed scans ips once, checking if all are allowed (strict)
// and finding the first allowed IP. strict=true requires ALL IPs to be allowed.
func filterAndFirstAllowed(entries []geofeed.Entry, ips []netip.Addr, allowed map[string]bool, strict bool) (netip.Addr, string, bool) {
	for _, ip := range ips {
		country := geofeed.LookupCountry(entries, ip)
		if allowed[country] {
			if !strict {
				return ip, country, true
			}
			// strict mode: found an allowed one, keep checking others
		} else if strict {
			return netip.Addr{}, "", false
		}
	}
	if strict {
		// All IPs passed strict check, return first IP
		if len(ips) > 0 {
			country := geofeed.LookupCountry(entries, ips[0])
			return ips[0], country, true
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

// getAllowedCountries returns the parsed countries map, caching the result
// keyed by the joined countries string to avoid repeated allocations.
func (s *Service) getAllowedCountries(countries []string) map[string]bool {
	key := strings.Join(countries, ",")
	s.countriesCacheMu.Lock()
	defer s.countriesCacheMu.Unlock()
	if key == s.countriesCacheKey {
		return s.countriesCacheVal
	}
	out := ParseAllowCountries(countries)
	s.countriesCacheKey = key
	s.countriesCacheVal = out
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

	// Use node.FragmentIdx to avoid scanning Raw for '#'.
	if node.FragmentIdx >= 0 {
		b.WriteString(node.Raw[:node.FragmentIdx])
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

const (
	decimalBase = 10
	hundred     = 100
)

func writeOctet(b *strings.Builder, n byte) {
	switch {
	case n >= hundred:
		b.WriteByte('0' + n/hundred)
		b.WriteByte('0' + (n/decimalBase)%decimalBase)
		b.WriteByte('0' + n%decimalBase)
	case n >= decimalBase:
		b.WriteByte('0' + n/decimalBase)
		b.WriteByte('0' + n%decimalBase)
	default:
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
	var b strings.Builder
	b.Grow(160) // rough upper bound for all stats
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
