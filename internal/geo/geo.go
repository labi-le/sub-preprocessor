// Package geo provides a shared Provider abstraction over the in-memory
// IP->country lookups (geofeed/dbip/registry) and the Team-Cymru ASN resolver,
// so that filtering and annotation can reuse the same provider instances.
package geo

import (
	"context"
	"net/netip"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

// Info is the resolved geo metadata for an IP. A zero Country and/or an empty
// ASN mean the corresponding datum is unknown.
type Info struct {
	Country geofeed.CountryCode
	ASN     string
}

// Provider resolves geo metadata for an IP address.
type Provider interface {
	Name() string
	Lookup(ctx context.Context, ip netip.Addr) Info
}

// lookupProvider resolves the country via an in-memory CountryLookup. It reads
// the lookup through a getter so it reflects background reloads instead of
// capturing a stale snapshot. Backs the geofeed, dbip, and registry providers.
type lookupProvider struct {
	name    string
	current func() geofeed.CountryLookup
}

// NewLookupProvider returns a Provider named name, backed by the lookup
// obtained from current on each call.
//
//nolint:ireturn // constructor intentionally returns the Provider interface
func NewLookupProvider(name string, current func() geofeed.CountryLookup) Provider {
	return &lookupProvider{name: name, current: current}
}

func (p *lookupProvider) Name() string { return p.name }

func (p *lookupProvider) Lookup(_ context.Context, ip netip.Addr) Info {
	return Info{Country: geofeed.LookupCountry(p.current(), ip)}
}

// asnResolver is the subset of *asn.Resolver used by asnProvider. Keeping it a
// local interface lets tests stub the resolver so they stay network-free.
type asnResolver interface {
	Resolve(ctx context.Context, ip netip.Addr) (asn.Result, error)
}

// asnProvider resolves country and AS identity via the Team-Cymru resolver.
type asnProvider struct {
	resolver asnResolver
}

// NewASN returns a Provider backed by the given ASN resolver. The real
// *asn.Resolver satisfies asnResolver.
//
//nolint:ireturn // constructor intentionally returns the Provider interface
func NewASN(r asnResolver) Provider {
	return &asnProvider{resolver: r}
}

func (p *asnProvider) Name() string { return "asn" }

func (p *asnProvider) Lookup(ctx context.Context, ip netip.Addr) Info {
	result, err := p.resolver.Resolve(ctx, ip)
	if err != nil {
		return Info{}
	}
	return Info{Country: result.Country, ASN: result.Name}
}
