package asn

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
)

const (
	cymruOriginDomain = "origin.asn.cymru.com"
	cymruASDomain     = "asn.cymru.com"
	minASRecordFields = 5
	minOriginFields   = 3

	// defaultCacheTTL is the fallback TTL for cached Cymru results when the
	// configured asn.cache_ttl is unset. Cymru data is static (RIR
	// allocations), so a day is safe; callers override via asn.cache_ttl.
	defaultCacheTTL = 24 * time.Hour
	// negativeCacheTTL is how long a failed lookup is remembered so an
	// unreachable Cymru does not serialize one timeout per node per request.
	negativeCacheTTL = 5 * time.Minute
	// maxCacheEntries caps the cache; expired entries are evicted on insert.
	maxCacheEntries = 16384
	// evictFraction sets how much of a full cache is dropped when nothing has
	// expired; see evictExpiredLocked.
	evictFraction = 8
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
	timeout  time.Duration
	cacheTTL time.Duration
	cache    map[netip.Addr]cachedResult
	mu       sync.RWMutex
	// lookupResolver is the one net.Resolver every Cymru query goes through,
	// shared like internal/resolver shares its instance (and set once here so
	// a cache miss does not allocate a fresh one per IP). Tests replace it
	// with a resolver dialing a local fake DNS server.
	lookupResolver *net.Resolver
	// lookupFn overrides the Cymru DNS lookup in tests; nil means r.lookup.
	lookupFn func(ctx context.Context, ip netip.Addr) (Result, error)
}

func New(timeout, cacheTTL time.Duration) *Resolver {
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	return &Resolver{
		timeout:        timeout,
		cacheTTL:       cacheTTL,
		cache:          make(map[netip.Addr]cachedResult),
		lookupResolver: &net.Resolver{PreferGo: true},
	}
}

func (r *Resolver) Resolve(ctx context.Context, ip netip.Addr) (Result, error) {
	if !ip.Is4() {
		return Result{}, fmt.Errorf("ASN lookup is not supported for IPv6: %s", ip)
	}

	// A warm entry is read-only, and every per-node ASN filter call pays this
	// lock once per IP, so the hit path takes the read lock; only storeCache
	// (and its eviction) takes the write one.
	r.mu.RLock()
	if cached, ok := r.cache[ip]; ok && time.Now().Before(cached.expiresAt) {
		r.mu.RUnlock()
		return cached.result, cached.err
	}
	r.mu.RUnlock()

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

	r.storeCache(ip, cachedResult{result: result, expiresAt: time.Now().Add(r.cacheTTL)})

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

// evictExpiredLocked drops expired entries and, when that frees nothing, a
// bounded sample of live ones so the cache never grows past maxCacheEntries.
// See internal/resolver for why a wipe (and a real LRU) are both wrong here;
// the cliff is worse for ASN data, whose 24h TTL means nothing expires within
// a day, so a full map takes the wipe branch on every single insert.
func (r *Resolver) evictExpiredLocked(now time.Time) {
	for ip, entry := range r.cache {
		if now.After(entry.expiresAt) {
			delete(r.cache, ip)
		}
	}
	if len(r.cache) < maxCacheEntries {
		return
	}
	drop := maxCacheEntries / evictFraction
	for ip := range r.cache {
		delete(r.cache, ip)
		drop--
		if drop == 0 {
			return
		}
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

	rev := reverseIP(ip)

	originTXT, err := r.lookupResolver.LookupTXT(resolveCtx, rev+"."+cymruOriginDomain)
	if err != nil {
		return Result{}, fmt.Errorf("cymru origin lookup: %w", err)
	}

	asn, country, err := parseOriginRecords(originTXT)
	if err != nil {
		return Result{}, fmt.Errorf("parse origin record: %w", err)
	}

	asTXT, err := r.lookupResolver.LookupTXT(resolveCtx, fmt.Sprintf("AS%d.%s", asn, cymruASDomain))
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

// errEmptyOriginAnswer marks a Cymru origin answer with no records at all,
// which parses nothing and therefore names no AS.
var errEmptyOriginAnswer = errors.New("empty origin TXT answer")

// parseOriginRecords picks the first parseable record from a Cymru origin TXT
// answer. An answer with no parseable record — an empty one included — must
// error rather than hand back asn 0: the caller builds AS<n>.asn.cymru.com
// from it, and AS0 is a name Cymru does not serve.
func parseOriginRecords(originTXT []string) (uint32, geofeed.CountryCode, error) {
	var err error
	for _, txt := range originTXT {
		var asn uint32
		var country geofeed.CountryCode
		asn, country, err = parseOriginRecord(txt)
		if err == nil {
			return asn, country, nil
		}
	}
	if err == nil {
		err = errEmptyOriginAnswer
	}
	return 0, geofeed.CountryCode{}, err
}

// parseOriginRecord parses one Cymru origin TXT record, e.g.
// "216071 | 146.103.121.0/24 | AE | ripencc | 1992-10-23".
//
// Field 0 is a space-separated AS LIST, not a single number: a prefix announced
// by more than one AS reads "15169 43515 | 35.212.128.0/17 | US | arin |
// 2017-09-29". Only that field is ambiguous -- the prefix, country and registry
// describe the one registration behind it -- so a list of any length must still
// yield the country. Parsing field 0 whole used to fail the record and discard
// it, which cost the annotate chain a hit and left the ASN filter fail-open
// (Resolve returning an error keeps the node).
//
// The caller needs ONE number for the AS<n>.asn.cymru.com name lookup and the
// record ranks nothing, so this takes the first listed. That name is read only
// by the {type: asn} deny patterns, and they match the NAME STRING -- so what
// decides whether dropping the rest costs a match is whether the names share a
// token a pattern could have been written around. Corporate affiliation is a
// different question and is deliberately not the one measured here. Measured,
// not hypothetical: of 68 live multi-origin records found by sampling random
// RIR delegations on 2026-08-04, 28 pair the first-listed AS with one whose
// name shares no token with it -- and one of this file's own test fixtures is
// among them. TestParseOriginRecord_MultiOrigin's 87.250.224.0/19 is AS13238
// "YANDEX - YANDEX LLC, RU" plus AS208398 "TELETECH - Edge Technology Plus
// d.o.o. Beograd, RS", two RIPE orgs (ORG-YA1-RIPE, ORG-TDB4-RIPE) with no
// token in common, so a pattern for either misses the other. The change is
// still strictly better than what it replaced, which missed every AS on such a
// prefix AND the country; taking one AS is a deliberate residual gap, not a
// solved problem.
func parseOriginRecord(txt string) (uint32, geofeed.CountryCode, error) {
	parts := strings.Split(txt, "|")
	asnField := strings.TrimSpace(parts[0])
	asnStr := asnField
	if i := strings.IndexAny(asnStr, " \t"); i >= 0 {
		asnStr = asnStr[:i]
	}
	asn, err := strconv.ParseUint(asnStr, 10, 32)
	if err != nil {
		return 0, geofeed.CountryCode{}, fmt.Errorf("parse asn %q: %w", asnField, err)
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}
