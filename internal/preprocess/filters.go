package preprocess

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/cidrset"
	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/filter"
)

// Filter processes a node's IPs through one IP-stage filter.
type Filter interface {
	Process(ctx context.Context, ips []netip.Addr, pctx *PipelineContext) []netip.Addr
}

type asnResolver interface {
	Resolve(ctx context.Context, ip netip.Addr) (asn.Result, error)
}

// GeofeedFilter applies the request's country policy to pctx.Lookup: an IP
// must be in the allow-list, unless the caller asked for none, and must not be
// in the deny-list. An IP no database can place has an unknown country, which
// is in no excluded country, so only the allow-list can drop it.
//
// The name is historical: pctx.Lookup is the LOCAL country databases every GEO
// annotate entry names, in the order the OPERATOR wrote them — not a hardcoded
// geofeed-first order, and a chain naming only dbip leaves the geofeed out of
// the filter entirely. So a node is never dropped as unplaceable while carrying
// a tag a LOCAL database placed it with. asn and cloudflare are not local
// tables and stay out of the chain, so a node only Cymru or the egress probe
// can place is still geo-dropped while its tag renders the country. See
// Processor.countryChain.
type GeofeedFilter struct{}

func NewGeofeedFilter() *GeofeedFilter {
	return &GeofeedFilter{}
}

func (f *GeofeedFilter) Process(_ context.Context, ips []netip.Addr, pctx *PipelineContext) []netip.Addr {
	if filter.IsFull(pctx.Allowed) && filter.IsEmpty(pctx.Denied) {
		return ips
	}
	result := filter.Permitted(pctx.Lookup, ips, pctx.Allowed, pctx.Denied)
	if len(result) == 0 {
		pctx.Stats.GeoDrop++
	}
	return result
}

// ASNFilter drops nodes whose AS name matches configured deny patterns and
// nodes whose ASN-resolved country the request's country policy rejects.
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
		if !filter.Permits(pctx.Allowed, pctx.Denied, result.Country) {
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

// CIDRFilter keeps the IPs inside a downloaded allow-list and drops a node
// that has none left. It reads nothing from the request: membership of the
// node's own address is the whole policy, so every caller gets one verdict.
type CIDRFilter struct {
	// set is a getter so the filter follows background refreshes of the list,
	// the same reason geo.NewLookupProvider takes one.
	set func() cidrset.Set
}

func NewCIDRFilter(set func() cidrset.Set) *CIDRFilter {
	return &CIDRFilter{set: set}
}

func (f *CIDRFilter) Process(_ context.Context, ips []netip.Addr, pctx *PipelineContext) []netip.Addr {
	set := f.set()
	n := 0
	for _, ip := range ips {
		if set.Contains(ip) {
			ips[n] = ip
			n++
		}
	}
	if n == 0 {
		pctx.Stats.CIDRDrop++
	}
	return ips[:n]
}

// errNoCIDRSet is structural insurance, unreachable from buildFilters' single
// caller: that one passes a non-nil set exactly when a cidr spec exists, which
// is exactly when this case fires. It exists so a SECOND caller getting the
// wiring wrong fails NewProcessor instead of panicking on a nil getter once
// per request.
var errNoCIDRSet = errors.New("cidr filter configured without a loaded allow-list")

// errNoASNResolver is the same insurance for the ASN arms: NewASNFilter
// stores its *asn.Resolver in an interface field, so a typed nil would pass
// the `f.resolver == nil` guard in Process and panic on the first IPv4
// lookup. Unreachable from buildFilters' single caller — NewProcessor builds a
// resolver whenever any spec needs ASN data (providerNeeds) — it exists so a
// SECOND caller getting the wiring wrong fails NewProcessor instead of the
// request path.
var errNoASNResolver = errors.New("asn filter configured without a resolver")

// buildFilters constructs the IP-stage filter chain from the parsed spec list,
// in config order. A country filter uses the geofeed provider by default or the
// ASN resolver when provider is "asn"; an asn filter applies its compiled
// name-deny patterns plus ASN-country filtering; a cidr filter reads the
// allow-list through cidrSet.
func buildFilters(specs []config.IPFilterSpec, asnR *asn.Resolver, cidrSet func() cidrset.Set) ([]Filter, error) {
	filters := make([]Filter, 0, len(specs))
	for _, spec := range specs {
		switch spec.Type {
		case config.FilterCountry:
			if spec.Provider == config.ProviderASN {
				if asnR == nil {
					return nil, errNoASNResolver
				}
				filters = append(filters, NewASNFilter(asnR, nil))
			} else {
				filters = append(filters, NewGeofeedFilter())
			}
		case config.FilterASN:
			if asnR == nil {
				return nil, errNoASNResolver
			}
			patterns, err := compilePatterns(spec.DenyPatterns)
			if err != nil {
				return nil, err
			}
			filters = append(filters, NewASNFilter(asnR, patterns))
		case config.FilterCIDR:
			if cidrSet == nil {
				return nil, errNoCIDRSet
			}
			filters = append(filters, NewCIDRFilter(cidrSet))
		}
	}
	return filters, nil
}

func compilePatterns(pats []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(pats))
	for _, p := range pats {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("compile asn deny pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}
