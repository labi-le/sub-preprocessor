package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	maxSmallIPs     = 4
	mapInitSize     = 32
	cacheInitSize   = 1024
	maxCacheEntries = 16384
	// evictFraction sets how much of a full cache is dropped when nothing has
	// expired: 1/8 leaves most of the working set warm while still making room
	// in amortised O(1) inserts.
	evictFraction = 8
)

type cacheEntry struct {
	ips     []netip.Addr
	expires time.Time
}

type Resolver struct {
	timeout      time.Duration
	cacheTTL     time.Duration
	negativeTTL  time.Duration
	resolvedPool sync.Pool
	lookup       *net.Resolver
	cacheMu      sync.RWMutex
	cache        map[string]cacheEntry
}

// New builds a Resolver. cacheTTL / negativeTTL control process-wide caching
// of successful / failed lookups; zero disables the respective cache.
func New(timeout time.Duration, address string, cacheTTL, negativeTTL time.Duration) *Resolver {
	// One instance for the process, not one per call: net.Resolver's internal
	// singleflight group is what collapses N concurrent lookups of the same
	// host into one wire query, and a fresh instance has an empty group.
	lookup := net.DefaultResolver
	if address != "" {
		d := net.Dialer{Timeout: timeout}
		lookup = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, address)
			},
		}
	}
	var cache map[string]cacheEntry
	if cacheTTL > 0 || negativeTTL > 0 {
		cache = make(map[string]cacheEntry, cacheInitSize)
	}
	return &Resolver{
		timeout:     timeout,
		cacheTTL:    cacheTTL,
		negativeTTL: negativeTTL,
		lookup:      lookup,
		cache:       cache,
		resolvedPool: sync.Pool{
			New: func() any { return make(map[string][]netip.Addr, mapInitSize) },
		},
	}
}

func (r *Resolver) GetResolvedMap() map[string][]netip.Addr {
	m, _ := r.resolvedPool.Get().(map[string][]netip.Addr)
	if m == nil {
		m = make(map[string][]netip.Addr, mapInitSize)
	}
	return m
}

func (r *Resolver) PutResolvedMap(m map[string][]netip.Addr) {
	clear(m)
	r.resolvedPool.Put(m)
}

func (r *Resolver) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	// Probe the cache before the IP parse: bare IPs are never stored (they
	// return below), so a hit is by construction a domain host, and ParseAddr
	// heap-allocates its ~48 B parse error on every domain (fetch.parseIPHost
	// measures the same cost) — a warm hit must not pay it.
	if ips, ok := r.cachedIPs(host); ok {
		return ips, nil
	}

	// Bare IPs — skip DNS lookup.
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is4() {
			out := make([]netip.Addr, 1)
			out[0] = addr
			return out, nil
		}
		return nil, errors.New("not an IPv4 address")
	}

	resolveCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	ips, err := r.lookup.LookupNetIP(resolveCtx, "ip4", host)
	if err != nil {
		// A caller cancellation or deadline says nothing about DNS reachability,
		// so it must not poison the negative cache. resolveCtx is done in that
		// case too, so test the parent: the resolver's own r.timeout expiry is
		// a genuine DNS failure and stays cacheable.
		if ctx.Err() == nil {
			r.storeCache(host, nil, r.negativeTTL)
		}
		return nil, fmt.Errorf("dns lookup: %w", err)
	}

	deduped := dedupIPv4(ips)
	if len(deduped) == 0 {
		r.storeCache(host, nil, r.negativeTTL)
		return deduped, nil
	}
	r.storeCache(host, deduped, r.cacheTTL)
	return deduped, nil
}

// cachedIPs returns the cached lookup result for host. The second return
// reports a hit; a hit with empty ips is a cached negative answer. Callers
// must not mutate the returned slice.
func (r *Resolver) cachedIPs(host string) ([]netip.Addr, bool) {
	if r.cache == nil {
		return nil, false
	}
	r.cacheMu.RLock()
	entry, ok := r.cache[host]
	r.cacheMu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.ips, true
}

func (r *Resolver) storeCache(host string, ips []netip.Addr, ttl time.Duration) {
	if r.cache == nil || ttl <= 0 {
		return
	}
	now := time.Now()
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if len(r.cache) >= maxCacheEntries {
		r.evictExpiredLocked(now)
	}
	// Clone: host is often a substring of a fetched subscription body; using it
	// directly as a key would pin the whole body in memory for the cache TTL.
	r.cache[strings.Clone(host)] = cacheEntry{ips: ips, expires: now.Add(ttl)}
}

// evictExpiredLocked drops expired entries and, when that frees nothing, a
// bounded sample of live ones so the cache never grows past maxCacheEntries.
// Wiping the whole map instead — the obvious way to make room — would take the
// hit rate to zero at exactly the moment the working set is largest, and would
// do so for every insert once the map is full of unexpired entries.
//
// Go randomises map iteration, so the sample carries no recency signal and can
// drop a hot host. A real LRU would need an intrusive list plus a write lock
// on every read, which costs more on the request path than it saves.
func (r *Resolver) evictExpiredLocked(now time.Time) {
	for host, entry := range r.cache {
		if now.After(entry.expires) {
			delete(r.cache, host)
		}
	}
	if len(r.cache) < maxCacheEntries {
		return
	}
	drop := maxCacheEntries / evictFraction
	for host := range r.cache {
		delete(r.cache, host)
		drop--
		if drop == 0 {
			return
		}
	}
}

func dedupIPv4(ips []netip.Addr) []netip.Addr {
	if len(ips) <= maxSmallIPs {
		var seen [maxSmallIPs]netip.Addr
		n := 0
		for _, ip := range ips {
			if !ip.Is4() {
				continue
			}
			dup := false
			for j := range n {
				if seen[j] == ip {
					dup = true
					break
				}
			}
			if !dup {
				seen[n] = ip
				ips[n] = ip
				n++
			}
		}
		return ips[:n]
	}

	seen := make(map[netip.Addr]bool, len(ips))
	n := 0
	for _, ip := range ips {
		if ip.Is4() && !seen[ip] {
			seen[ip] = true
			ips[n] = ip
			n++
		}
	}
	return ips[:n]
}
