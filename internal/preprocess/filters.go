package preprocess

import (
	"context"
	"net/netip"
	"regexp"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/filter"
)

// Filter processes a node's IPs through one workflow stage.
type Filter interface {
	Process(ctx context.Context, ips []netip.Addr, pctx *PipelineContext) []netip.Addr
}

// GeofeedFilter returns IPs whose geofeed country is in the allowed set.
type GeofeedFilter struct{}

func NewGeofeedFilter() *GeofeedFilter {
	return &GeofeedFilter{}
}

func (f *GeofeedFilter) Process(_ context.Context, ips []netip.Addr, pctx *PipelineContext) []netip.Addr {
	result := filter.AllAllowed(pctx.Lookup, ips, pctx.Allowed)
	if len(result) == 0 {
		pctx.Stats.GeoDrop++
	}
	return result
}

// ASNFilter drops nodes whose AS name matches configured deny patterns.
type ASNFilter struct {
	resolver *asn.Resolver
	patterns []*regexp.Regexp
}

func NewASNFilter(resolver *asn.Resolver, patterns []*regexp.Regexp) *ASNFilter {
	return &ASNFilter{resolver: resolver, patterns: patterns}
}

func (f *ASNFilter) isAllowed(ctx context.Context, ip netip.Addr) bool {
	if f.resolver == nil {
		return true
	}
	result, err := f.resolver.Resolve(ctx, ip)
	if err != nil {
		return true
	}
	if result.Name != "" {
		for _, pattern := range f.patterns {
			if pattern.MatchString(result.Name) {
				return false
			}
		}
	}
	return true
}

func (f *ASNFilter) Process(ctx context.Context, ips []netip.Addr, pctx *PipelineContext) []netip.Addr {
	if f.resolver == nil {
		return ips
	}
	n := 0
	for _, ip := range ips {
		if f.isAllowed(ctx, ip) {
			ips[n] = ip
			n++
		}
	}
	if n == 0 {
		pctx.Stats.ASNDrop++
	}
	return ips[:n]
}

func buildFilters(stages []string, asnR *asn.Resolver, patterns []*regexp.Regexp) []Filter {
	filters := make([]Filter, 0, len(stages))
	for _, name := range stages {
		switch name {
		case "geofeed":
			filters = append(filters, NewGeofeedFilter())
		case "asn":
			filters = append(filters, NewASNFilter(asnR, patterns))
		}
	}
	return filters
}
