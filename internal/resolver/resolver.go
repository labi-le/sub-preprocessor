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
	resolveCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.ResolveIPv4(resolveCtx, host)
}

func (r *Resolver) ResolveIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is4() {
			out := make([]netip.Addr, 1)
			out[0] = addr
			return out, nil
		}
		return nil, errors.New("not an IPv4 address")
	}

	resolver := net.DefaultResolver
	if r.dialer != nil {
		resolver = &net.Resolver{Dial: r.dialer}
	}

	ips, err := resolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, fmt.Errorf("dns lookup: %w", err)
	}

	return dedupIPv4(ips), nil
}

func dedupIPv4(ips []netip.Addr) []netip.Addr {
	if len(ips) <= maxSmallIPs {
		var out []netip.Addr
		var seen [maxSmallIPs]netip.Addr
		var n int
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
				n++
				out = append(out, ip)
			}
		}
		return out
	}

	out := make([]netip.Addr, 0, len(ips))
	seen := make(map[netip.Addr]bool, len(ips))
	for _, ip := range ips {
		if ip.Is4() && !seen[ip] {
			out = append(out, ip)
			seen[ip] = true
		}
	}
	return out
}
