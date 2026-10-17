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

// cachedName is the AS<n>.asn.cymru.com half of a lookup, keyed by ASN rather
// than IP. The country stays in cachedResult: it comes from the origin record
// and differs per prefix even under one AS.
type cachedName struct {
	name      string
	err       error
	expiresAt time.Time
}

// ipFlight and nameFlight let concurrent resolves share one lookup. A flight
// is resolver-owned: its context carries the total budget (r.timeout for an
// IP, r.timeout for a name query), not any caller's cancellation, and it is
// cancelled once its last waiter leaves. The result fields are published under
// flightMu and done is closed after them, so a receiver of done has a
// happens-before edge.
type ipFlight struct {
	done      chan struct{}
	result    Result
	err       error
	cancel    context.CancelFunc
	waiters   int
	finished  bool
	abandoned bool
}

type nameFlight struct {
	done      chan struct{}
	name      string
	expiresAt time.Time
	err       error
	cancel    context.CancelFunc
	waiters   int
	finished  bool
	abandoned bool
}

type Resolver struct {
	timeout     time.Duration
	cacheTTL    time.Duration
	cache       map[netip.Addr]cachedResult
	nameCache   map[uint32]cachedName
	mu          sync.RWMutex
	flights     map[netip.Addr]*ipFlight
	nameFlights map[uint32]*nameFlight
	flightMu    sync.Mutex
	// lookupResolver is the one net.Resolver every Cymru query goes through,
	// shared like internal/resolver shares its instance (and set once here so
	// a cache miss does not allocate a fresh one per IP). Tests replace it
	// with a resolver dialing a local fake DNS server.
	lookupResolver *net.Resolver
	// lookupTXT overrides the TXT query in tests; nil means
	// lookupResolver.LookupTXT.
	lookupTXT func(ctx context.Context, name string) ([]string, error)
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
		nameCache:      make(map[uint32]cachedName),
		flights:        make(map[netip.Addr]*ipFlight),
		nameFlights:    make(map[uint32]*nameFlight),
		lookupResolver: &net.Resolver{PreferGo: true},
	}
}

func (r *Resolver) Resolve(ctx context.Context, ip netip.Addr) (Result, error) {
	if !ip.Is4() {
		return Result{}, fmt.Errorf("ASN lookup is not supported for IPv6: %s", ip)
	}

	// A warm entry is read-only, and every per-node ASN filter call pays this
	// lock once per IP, so the hit path takes the read lock; only the stores
	// (and their eviction) take the write one. The warm success and negative
	// paths do no allocation and never inspect the caller's context.
	if cached, ok := r.cachedResult(ip); ok {
		return cached.result, cached.err
	}
	return r.resolveCold(ctx, ip)
}

func (r *Resolver) cachedResult(ip netip.Addr) (cachedResult, bool) {
	r.mu.RLock()
	entry, ok := r.cache[ip]
	r.mu.RUnlock()
	if !ok || !time.Now().Before(entry.expiresAt) {
		return cachedResult{}, false
	}
	return entry, true
}

// resolveCold joins the in-flight lookup for ip or starts one. A flight is
// resolver-owned: it runs on its own r.timeout budget, and a caller only waits
// on it. A caller's cancellation removes that waiter and, when it was the last
// one, cancels and unregisters the flight; it never fails the remaining
// waiters.
func (r *Resolver) resolveCold(ctx context.Context, ip netip.Addr) (Result, error) {
	// An already-canceled caller has nothing to wait for, and the cancellation
	// is not a Cymru answer: do not start or join a flight for it.
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("resolve canceled: %w", err)
	}

	r.flightMu.Lock()
	if f, ok := r.flights[ip]; ok {
		f.waiters++
		r.flightMu.Unlock()
		return r.waitIPFlight(ctx, ip, f)
	}
	f := &ipFlight{done: make(chan struct{}), waiters: 1}
	flightCtx, cancel := context.WithTimeout(context.Background(), r.timeout)
	f.cancel = cancel
	r.flights[ip] = f
	r.flightMu.Unlock()

	// A lookup can finish between the cache probe and the registration above;
	// re-check so a just-warmed IP does not issue a second query.
	if cached, ok := r.cachedResult(ip); ok {
		r.finishIPFlight(ip, f, cached.result, cached.err, nil)
		return cached.result, cached.err
	}

	go r.runIPFlight(flightCtx, ip, f)
	return r.waitIPFlight(ctx, ip, f)
}

// runIPFlight owns the one budget that covers the origin and AS-name halves of
// an IP lookup, matching the pre-coalescing resolveCtx.
func (r *Resolver) runIPFlight(ctx context.Context, ip netip.Addr, f *ipFlight) {
	var result Result
	var capExpiry time.Time
	var err error
	if r.lookupFn != nil {
		result, err = r.lookupFn(ctx, ip)
	} else {
		result, capExpiry, err = r.lookup(ctx, ip)
	}

	var entry *cachedResult
	if err == nil {
		expiresAt := time.Now().Add(r.cacheTTL)
		if !capExpiry.IsZero() && capExpiry.Before(expiresAt) {
			expiresAt = capExpiry
		}
		entry = &cachedResult{result: result, expiresAt: expiresAt}
	} else if !errors.Is(err, context.Canceled) {
		// A cancellation says nothing about Cymru; a resolver-owned timeout
		// does, and stays cacheable as before. A reused AS-name error caps the
		// IP entry so a fresh IP cannot extend the failure past the name entry
		// it was built from.
		expiresAt := time.Now().Add(negativeCacheTTL)
		if !capExpiry.IsZero() && capExpiry.Before(expiresAt) {
			expiresAt = capExpiry
		}
		entry = &cachedResult{err: err, expiresAt: expiresAt}
	}

	r.finishIPFlight(ip, f, result, err, entry)
}

func (r *Resolver) waitIPFlight(ctx context.Context, ip netip.Addr, f *ipFlight) (Result, error) {
	select {
	case <-f.done:
		return f.result, f.err
	case <-ctx.Done():
		r.dropIPWaiter(ip, f)
		return Result{}, fmt.Errorf("resolve canceled: %w", ctx.Err())
	}
}

// dropIPWaiter unregisters one waiter. The flight is cancelled only when no
// waiter remains and it has not finished, so abandoned work does not run out
// its full timeout.
func (r *Resolver) dropIPWaiter(ip netip.Addr, f *ipFlight) {
	r.flightMu.Lock()
	f.waiters--
	if f.waiters > 0 || f.finished {
		r.flightMu.Unlock()
		return
	}
	f.abandoned = true
	if r.flights[ip] == f {
		delete(r.flights, ip)
	}
	r.flightMu.Unlock()
	f.cancel()
}

// finishIPFlight publishes the result and commits its cache entry only while
// this flight still owns the IP. A flight whose last waiter left is already
// unregistered, and a replacement can own the IP by the time the old lookup
// returns; neither case may publish stale work. The cache store takes r.mu
// under flightMu; no path holds r.mu and then takes flightMu.
func (r *Resolver) finishIPFlight(ip netip.Addr, f *ipFlight, result Result, err error, entry *cachedResult) {
	r.flightMu.Lock()
	f.result, f.err = result, err
	f.finished = true
	if r.flights[ip] == f {
		delete(r.flights, ip)
		if entry != nil && !f.abandoned {
			r.storeCache(ip, *entry)
		}
	}
	r.flightMu.Unlock()
	f.cancel()
	close(f.done)
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

// lookup runs under the IP flight's total budget and returns the expiry of
// the AS-name entry it used, so the caller can cap the IP entry by it. A zero
// expiry means no AS-name entry was involved (origin failure).
func (r *Resolver) lookup(ctx context.Context, ip netip.Addr) (Result, time.Time, error) {
	rev := reverseIP(ip)

	originTXT, err := r.queryTXT(ctx, rev+"."+cymruOriginDomain)
	if err != nil {
		return Result{}, time.Time{}, fmt.Errorf("cymru origin lookup: %w", err)
	}

	asn, country, err := parseOriginRecords(originTXT)
	if err != nil {
		return Result{}, time.Time{}, fmt.Errorf("parse origin record: %w", err)
	}

	name, nameExpiresAt, err := r.lookupName(ctx, asn)
	if err != nil {
		return Result{}, nameExpiresAt, fmt.Errorf("cymru as lookup: %w", err)
	}

	return Result{Country: country, Name: name}, nameExpiresAt, nil
}

func (r *Resolver) queryTXT(ctx context.Context, name string) ([]string, error) {
	if r.lookupTXT != nil {
		return r.lookupTXT(ctx, name)
	}
	return r.lookupResolver.LookupTXT(ctx, name) //nolint:wrapcheck // lookup wraps it with the stage-specific prefix
}

// lookupName resolves AS<n>.asn.cymru.com once per ASN and shares an
// in-flight query between the IP lookups that need the same name. The cached
// entry carries only the name (or its error) and its expiry; the country is
// never part of it. Waiters stop at their own IP budget, while the query
// itself is bounded by a resolver-owned r.timeout.
func (r *Resolver) lookupName(ctx context.Context, asn uint32) (string, time.Time, error) {
	if entry, ok := r.cachedName(asn); ok {
		return entry.name, entry.expiresAt, entry.err
	}
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, fmt.Errorf("AS name lookup canceled: %w", err)
	}

	r.flightMu.Lock()
	if f, ok := r.nameFlights[asn]; ok {
		f.waiters++
		r.flightMu.Unlock()
		return r.waitNameFlight(ctx, asn, f)
	}
	f := &nameFlight{done: make(chan struct{}), waiters: 1}
	nameCtx, cancel := context.WithTimeout(context.Background(), r.timeout)
	f.cancel = cancel
	r.nameFlights[asn] = f
	r.flightMu.Unlock()

	// Same race as resolveCold: a name lookup may finish between the cache
	// probe and the registration.
	if entry, ok := r.cachedName(asn); ok {
		r.finishNameFlight(asn, f, entry.name, entry.expiresAt, entry.err, nil)
		return entry.name, entry.expiresAt, entry.err
	}

	go r.runNameFlight(nameCtx, asn, f)
	return r.waitNameFlight(ctx, asn, f)
}

func (r *Resolver) runNameFlight(ctx context.Context, asn uint32, f *nameFlight) {
	name, err := r.queryName(ctx, asn)
	var expiresAt time.Time
	var entry *cachedName
	if err == nil {
		// An empty name is a successful answer, not a missing one: Cymru
		// answered and no parseable record was in it.
		expiresAt = time.Now().Add(r.cacheTTL)
		entry = &cachedName{name: name, expiresAt: expiresAt}
	} else if !errors.Is(err, context.Canceled) {
		// ASN-wide negative caching is deliberate: one failed AS-name query is
		// shared by every cold IP in that AS for negativeCacheTTL.
		expiresAt = time.Now().Add(negativeCacheTTL)
		entry = &cachedName{err: err, expiresAt: expiresAt}
	}
	r.finishNameFlight(asn, f, name, expiresAt, err, entry)
}

func (r *Resolver) queryName(ctx context.Context, asn uint32) (string, error) {
	asTXT, err := r.queryTXT(ctx, fmt.Sprintf("AS%d.%s", asn, cymruASDomain))
	if err != nil {
		return "", err
	}
	for _, txt := range asTXT {
		if n := parseASRecord(txt); n != "" {
			return n, nil
		}
	}
	return "", nil
}

func (r *Resolver) waitNameFlight(ctx context.Context, asn uint32, f *nameFlight) (string, time.Time, error) {
	select {
	case <-f.done:
		return f.name, f.expiresAt, f.err
	case <-ctx.Done():
		r.dropNameWaiter(asn, f)
		return "", time.Time{}, fmt.Errorf("AS name lookup canceled: %w", ctx.Err())
	}
}

func (r *Resolver) dropNameWaiter(asn uint32, f *nameFlight) {
	r.flightMu.Lock()
	f.waiters--
	if f.waiters > 0 || f.finished {
		r.flightMu.Unlock()
		return
	}
	f.abandoned = true
	if r.nameFlights[asn] == f {
		delete(r.nameFlights, asn)
	}
	r.flightMu.Unlock()
	f.cancel()
}

// finishNameFlight mirrors finishIPFlight: the ASN-keyed entry is committed
// only while this flight still owns the ASN, so an abandoned or replaced name
// query cannot overwrite newer work. storeName takes r.mu under flightMu, the
// same one-way nesting as the IP cache.
func (r *Resolver) finishNameFlight(asn uint32, f *nameFlight, name string, expiresAt time.Time, err error, entry *cachedName) {
	r.flightMu.Lock()
	f.name, f.expiresAt, f.err = name, expiresAt, err
	f.finished = true
	if r.nameFlights[asn] == f {
		delete(r.nameFlights, asn)
		if entry != nil && !f.abandoned {
			r.storeName(asn, *entry)
		}
	}
	r.flightMu.Unlock()
	f.cancel()
	close(f.done)
}

func (r *Resolver) cachedName(asn uint32) (cachedName, bool) {
	r.mu.RLock()
	entry, ok := r.nameCache[asn]
	r.mu.RUnlock()
	if !ok || !time.Now().Before(entry.expiresAt) {
		return cachedName{}, false
	}
	return entry, true
}

func (r *Resolver) storeName(asn uint32, entry cachedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.nameCache) >= maxCacheEntries {
		r.evictExpiredNamesLocked(time.Now())
	}
	r.nameCache[asn] = entry
}

// evictExpiredNamesLocked applies the same policy as evictExpiredLocked to the
// ASN-keyed names, so the optimization cannot grow an unbounded second map.
func (r *Resolver) evictExpiredNamesLocked(now time.Time) {
	for asn, entry := range r.nameCache {
		if now.After(entry.expiresAt) {
			delete(r.nameCache, asn)
		}
	}
	if len(r.nameCache) < maxCacheEntries {
		return
	}
	drop := maxCacheEntries / evictFraction
	for asn := range r.nameCache {
		delete(r.nameCache, asn)
		drop--
		if drop == 0 {
			return
		}
	}
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
