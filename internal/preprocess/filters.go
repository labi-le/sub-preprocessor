package preprocess

import (
	"context"
	"net/netip"
	"regexp"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

// Filter processes a node's IPs through one workflow stage.
type Filter interface {
	Process(ctx context.Context, ips []netip.Addr, lookup geofeed.CountryLookup, allowed filter.CountrySet, stats *Stats) []netip.Addr
}

// GeofeedFilter returns IPs whose geofeed country is in the allowed set.
type GeofeedFilter struct{}

func NewGeofeedFilter() *GeofeedFilter {
	return &GeofeedFilter{}
}

func (f *GeofeedFilter) Process(_ context.Context, ips []netip.Addr, lookup geofeed.CountryLookup, allowed filter.CountrySet, stats *Stats) []netip.Addr {
	result := filter.AllAllowed(lookup, ips, allowed)
	if len(result) == 0 {
		stats.GeoDrop++
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

func (f *ASNFilter) isAllowed(ctx context.Context, ip netip.Addr, allowed filter.CountrySet) bool {
	if f.resolver == nil {
		return true
	}
	result, err := f.resolver.Resolve(ctx, ip)
	if err != nil || result.Name == "" {
		return true
	}
	for _, pattern := range f.patterns {
		if pattern.MatchString(result.Name) {
			return false
		}
	}
	if result.Country != "" && !allowed.Has(result.Country) {
		return false
	}
	return true
}

func (f *ASNFilter) Process(ctx context.Context, ips []netip.Addr, _ geofeed.CountryLookup, allowed filter.CountrySet, stats *Stats) []netip.Addr {
	if f.resolver == nil {
		return ips
	}
	result := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		if f.isAllowed(ctx, ip, allowed) {
			result = append(result, ip)
		}
	}
	if len(result) == 0 {
		stats.ASNDrop++
	}
	return result
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
