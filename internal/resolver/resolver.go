package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	maxSmallIPs     = 4
	mapInitSize     = 32
	cacheInitSize   = 1024
	maxCacheEntries = 16384
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
	dialer       func(ctx context.Context, network, address string) (net.Conn, error)
	cacheMu      sync.RWMutex
	cache        map[string]cacheEntry
}

// New builds a Resolver. cacheTTL / negativeTTL control process-wide caching
// of successful / failed lookups; zero disables the respective cache.
func New(timeout time.Duration, address string, cacheTTL, negativeTTL time.Duration) *Resolver {
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if address != "" {
		d := net.Dialer{Timeout: timeout}
		dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, address)
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
		dialer:      dial,
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
	// Bare IPs — skip DNS lookup.
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is4() {
			out := make([]netip.Addr, 1)
			out[0] = addr
			return out, nil
		}
		return nil, errors.New("not an IPv4 address")
	}

	if ips, ok := r.cachedIPs(host); ok {
		return ips, nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resolver := net.DefaultResolver
	if r.dialer != nil {
		resolver = &net.Resolver{PreferGo: true, Dial: r.dialer}
	}

	ips, err := resolver.LookupNetIP(resolveCtx, "ip4", host)
	if err != nil {
		r.storeCache(host, nil, r.negativeTTL)
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
	r.cache[host] = cacheEntry{ips: ips, expires: now.Add(ttl)}
}

// evictExpiredLocked drops expired entries; when everything is still fresh it
// resets the whole map so the cache never grows past maxCacheEntries.
func (r *Resolver) evictExpiredLocked(now time.Time) {
	for host, entry := range r.cache {
		if now.After(entry.expires) {
			delete(r.cache, host)
		}
	}
	if len(r.cache) >= maxCacheEntries {
		clear(r.cache)
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
				ips[n] = ip // write in-place
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
			ips[n] = ip // write in-place
			n++
		}
	}
	return ips[:n]
}
