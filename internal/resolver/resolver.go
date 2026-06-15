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
	maxSmallIPs = 4
	mapInitSize = 32
)

type cacheEntry struct {
	addrs   []netip.Addr
	expires time.Time
}

type Resolver struct {
	timeout      time.Duration
	cache        sync.Map
	cacheTTL     time.Duration
	resolvedPool sync.Pool
	dialer       func(ctx context.Context, network, address string) (net.Conn, error)
}

func New(timeout time.Duration, address string, ttl time.Duration) *Resolver {
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if address != "" {
		d := net.Dialer{Timeout: timeout}
		dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, address)
		}
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Resolver{
		timeout:  timeout,
		cacheTTL: ttl,
		dialer:   dial,
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
	// Check cache.
	if entry, ok := r.cache.Load(host); ok {
		ce := entry.(cacheEntry)
		if ce.expires.After(time.Now()) {
			return ce.addrs, nil
		}
	}

	// Bare IPs — skip DNS lookup after cache miss.
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

	resolver := net.DefaultResolver
	if r.dialer != nil {
		resolver = &net.Resolver{Dial: r.dialer}
	}

	ips, err := resolver.LookupNetIP(resolveCtx, "ip4", host)
	if err != nil {
		return nil, fmt.Errorf("dns lookup: %w", err)
	}

	deduped := dedupIPv4(ips)

	r.cache.Store(host, cacheEntry{addrs: deduped, expires: time.Now().Add(r.cacheTTL)})
	return deduped, nil
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
