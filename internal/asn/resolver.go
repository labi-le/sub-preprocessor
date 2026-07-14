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
	// negativeCacheTTL is how long a failed lookup is remembered so an
	// unreachable Cymru does not serialize one timeout per node per request.
	negativeCacheTTL = 5 * time.Minute
	// maxCacheEntries caps the cache; expired entries are evicted on insert.
	maxCacheEntries = 16384
)

type Result struct {
	Country geofeed.CountryCode
	Name    string
}

type cachedResult struct {
	result    Result
	err       error
	expiresAt time.Time
}

type Resolver struct {
	timeout time.Duration
	cache   map[netip.Addr]cachedResult
	mu      sync.Mutex
	// lookupFn overrides the Cymru DNS lookup in tests; nil means r.lookup.
	lookupFn func(ctx context.Context, ip netip.Addr) (Result, error)
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
		return cached.result, cached.err
	}
	r.mu.Unlock()

	lookup := r.lookupFn
	if lookup == nil {
		lookup = r.lookup
	}
	result, err := lookup(ctx, ip)
	if err != nil {
		// Negative-cache the failure unless the caller's context is done —
		// a cancellation says nothing about Cymru's reachability.
		if ctx.Err() == nil {
			r.storeCache(ip, cachedResult{err: err, expiresAt: time.Now().Add(negativeCacheTTL)})
		}
		return Result{}, err
	}

	r.storeCache(ip, cachedResult{result: result, expiresAt: time.Now().Add(cacheTTL)})

	return result, nil
}

func (r *Resolver) storeCache(ip netip.Addr, entry cachedResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= maxCacheEntries {
		r.evictExpiredLocked(time.Now())
	}
	r.cache[ip] = entry
}

// evictExpiredLocked drops expired entries; when everything is still fresh it
// resets the whole map so the cache never grows past maxCacheEntries.
func (r *Resolver) evictExpiredLocked(now time.Time) {
	for ip, entry := range r.cache {
		if now.After(entry.expiresAt) {
			delete(r.cache, ip)
		}
	}
	if len(r.cache) >= maxCacheEntries {
		clear(r.cache)
	}
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
		return Result{}, fmt.Errorf("cymru as lookup: %w", err)
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
