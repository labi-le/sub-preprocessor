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

type Resolver struct {
	timeout      time.Duration
	resolvedPool sync.Pool
	dialer       func(ctx context.Context, network, address string) (net.Conn, error)
}

func New(timeout time.Duration, address string) *Resolver {
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if address != "" {
		d := net.Dialer{Timeout: timeout}
		dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, address)
		}
	}
	return &Resolver{
		timeout: timeout,
		dialer:  dial,
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

	return dedupIPv4(ips), nil
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
