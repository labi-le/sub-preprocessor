package asn

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

const (
	cymruOriginDomain = "origin.asn.cymru.com"
	cymruASDomain     = "asn.cymru.com"
	minASRecordFields = 5
	minOriginFields   = 3

	// cacheTTL is how long Cymru results are cached in memory.
	// Cymru data is static (RIR allocations), so 6h is conservative.
	cacheTTL = 6 * time.Hour
)

type Result struct {
	Country geofeed.CountryCode
	Name    string
}

type cachedResult struct {
	result    Result
	expiresAt time.Time
}

type Resolver struct {
	timeout time.Duration
	cache   map[netip.Addr]cachedResult
	mu      sync.Mutex
}

func New(timeout time.Duration) *Resolver {
	return &Resolver{
		timeout: timeout,
		cache:   make(map[netip.Addr]cachedResult),
	}
}

func (r *Resolver) Resolve(ctx context.Context, ip netip.Addr) (Result, error) {
	if !ip.Is4() {
		return Result{}, fmt.Errorf("ASN lookup is not supported for IPv6: %s", ip)
	}

	r.mu.Lock()
	if cached, ok := r.cache[ip]; ok && time.Now().Before(cached.expiresAt) {
		r.mu.Unlock()
		return cached.result, nil
	}
	r.mu.Unlock()

	result, err := r.lookup(ctx, ip)
	if err != nil {
		return Result{}, err
	}

	r.mu.Lock()
	r.cache[ip] = cachedResult{result: result, expiresAt: time.Now().Add(cacheTTL)}
	r.mu.Unlock()

	return result, nil
}

func reverseIP(ip netip.Addr) string {
	if !ip.Is4() {
		return ""
	}
	ip4 := ip.As4()
	return fmt.Sprintf("%d.%d.%d.%d", ip4[3], ip4[2], ip4[1], ip4[0])
}

func (r *Resolver) lookup(ctx context.Context, ip netip.Addr) (Result, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	netR := &net.Resolver{
		PreferGo: true,
	}

	rev := reverseIP(ip)

	originTXT, err := netR.LookupTXT(resolveCtx, rev+"."+cymruOriginDomain)
	if err != nil {
		return Result{}, fmt.Errorf("cymru origin lookup: %w", err)
	}

	var asn uint32
	var country geofeed.CountryCode
	for _, txt := range originTXT {
		asn, country, err = parseOriginRecord(txt)
		if err == nil {
			break
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("parse origin record: %w", err)
	}

	asTXT, err := netR.LookupTXT(resolveCtx, fmt.Sprintf("AS%d.%s", asn, cymruASDomain))
	if err != nil {
		return Result{Country: country}, fmt.Errorf("cymru as lookup: %w", err)
	}

	name := ""
	for _, txt := range asTXT {
		if n := parseASRecord(txt); n != "" {
			name = n
			break
		}
	}

	return Result{Country: country, Name: name}, nil
}

func parseOriginRecord(txt string) (uint32, geofeed.CountryCode, error) {
	// "216071 | 146.103.121.0/24 | AE | ripencc | 1992-10-23"
	parts := strings.Split(txt, "|")
	if len(parts) < 1 {
		return 0, geofeed.CountryCode{}, fmt.Errorf("unexpected origin format: %q", txt)
	}
	asnStr := strings.TrimSpace(parts[0])
	asn, err := strconv.ParseUint(asnStr, 10, 32)
	if err != nil {
		return 0, geofeed.CountryCode{}, fmt.Errorf("parse asn %q: %w", asnStr, err)
	}
	var country geofeed.CountryCode
	if len(parts) >= minOriginFields {
		s := strings.TrimSpace(parts[2])
		s = strings.ToUpper(s)
		if len(s) == 2 { //nolint:mnd // ISO 3166-1 alpha-2 length
			country = geofeed.CountryCode{s[0], s[1]}
		}
	}
	return uint32(asn), country, nil
}

func parseASRecord(txt string) string {
	// "216071 | AE | ripencc | 2023-10-30 | VDSINA - SERVERS TECH FZCO, AE"
	parts := strings.Split(txt, "|")
	if len(parts) < minASRecordFields {
		return ""
	}
	return strings.TrimSpace(parts[4])
}

// CacheLen returns the number of cached ASN entries (for observability).
func (r *Resolver) CacheLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cache)
}
