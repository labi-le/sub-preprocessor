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

// asnResolver resolves an IP to ASN metadata.
type asnResolver interface {
	Resolve(ctx context.Context, ip netip.Addr) (asn.Result, error)
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

// ASNFilter drops nodes whose AS name matches configured deny patterns
// and whose ASN-resolved country is not in the allowed set.
type ASNFilter struct {
	resolver asnResolver
	patterns []*regexp.Regexp
}

func NewASNFilter(resolver *asn.Resolver, patterns []*regexp.Regexp) *ASNFilter {
	return &ASNFilter{resolver: resolver, patterns: patterns}
}

func (f *ASNFilter) isNameDenied(name string) bool {
	for _, pattern := range f.patterns {
		if pattern.MatchString(name) {
			return true
		}
	}
	return false
}

func (f *ASNFilter) Process(ctx context.Context, ips []netip.Addr, pctx *PipelineContext) []netip.Addr {
	if f.resolver == nil {
		return ips
	}
	n := 0
	asnDrop := false
	geoDrop := false
	for _, ip := range ips {
		result, err := f.resolver.Resolve(ctx, ip)
		if err != nil {
			ips[n] = ip
			n++
			continue
		}
		if result.Name != "" && f.isNameDenied(result.Name) {
			asnDrop = true
			continue
		}
		if !pctx.Allowed.Has(result.Country) {
			geoDrop = true
			continue
		}
		ips[n] = ip
		n++
	}
	if n == 0 {
		if asnDrop {
			pctx.Stats.ASNDrop++
		} else if geoDrop {
			pctx.Stats.GeoDrop++
		}
	}
	return ips[:n]
}

func buildFilters(stages []string, asnR *asn.Resolver, patterns []*regexp.Regexp) []Filter {
	filters := make([]Filter, 0, len(stages)+1)
	hasGeofeed := false
	for _, name := range stages {
		switch name {
		case "geofeed":
			filters = append(filters, NewGeofeedFilter())
			hasGeofeed = true
		case "asn":
			filters = append(filters, NewASNFilter(asnR, patterns))
		}
	}
	if !hasGeofeed {
		filters = append(filters, NewGeofeedFilter())
	}
	return filters
}
