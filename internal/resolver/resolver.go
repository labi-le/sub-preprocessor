package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"domains.lst/sub-preprocessor/internal/subscription"
)

const (
	maxSmallIPs = 4
	mapInitSize = 32
)

type Resolver struct {
	timeout           time.Duration
	uniqueServersPool sync.Pool
	resolvedPool      sync.Pool
}

func New(timeout time.Duration) *Resolver {
	return &Resolver{
		timeout: timeout,
		uniqueServersPool: sync.Pool{
			New: func() any { return make(map[string]struct{}, mapInitSize) },
		},
		resolvedPool: sync.Pool{
			New: func() any { return make(map[string][]netip.Addr, mapInitSize) },
		},
	}
}

func (r *Resolver) ResolveServers(ctx context.Context, nodes []subscription.Node) map[string][]netip.Addr {
	uniqueServers, _ := r.uniqueServersPool.Get().(map[string]struct{})
	if uniqueServers == nil {
		uniqueServers = make(map[string]struct{}, mapInitSize)
	}
	defer func() {
		clear(uniqueServers)
		r.uniqueServersPool.Put(uniqueServers)
	}()

	resolved, _ := r.resolvedPool.Get().(map[string][]netip.Addr)
	if resolved == nil {
		resolved = make(map[string][]netip.Addr, mapInitSize)
	}

	for _, node := range nodes {
		if node.Server == "" || node.Port == "" {
			continue
		}
		uniqueServers[node.Server] = struct{}{}
	}

	for server := range uniqueServers {
		resolveCtx, cancel := context.WithTimeout(ctx, r.timeout)
		ips, resolveErr := r.ResolveIPv4(resolveCtx, server)
		cancel()
		if resolveErr == nil && len(ips) > 0 {
			resolved[server] = ips
		}
	}
	return resolved
}

func (r *Resolver) ReturnResolved(resolved map[string][]netip.Addr) {
	clear(resolved)
	r.resolvedPool.Put(resolved)
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

	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
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
