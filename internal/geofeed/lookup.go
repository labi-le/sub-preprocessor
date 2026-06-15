package geofeed

import (
	"net/netip"
	"sort"
)

type CountryLookup interface {
	LookupCountry(ip netip.Addr) CountryCode
}

// ipRange represents an IPv4 CIDR range with its associated country.
type ipRange struct {
	start   uint32
	end     uint32
	country CountryCode
}

type indexedLookup struct {
	v4 []ipRange
	v6 []Entry
}

func newIndexedLookup(entries []Entry) *indexedLookup {
	lookup := &indexedLookup{}

	for _, entry := range entries {
		prefix := entry.Prefix.Masked()
		addr := prefix.Addr()
		if !addr.Is4() {
			lookup.v6 = append(lookup.v6, entry)
			continue
		}

		r := ipv4PrefixRange(prefix, entry.Country)
		lookup.v4 = append(lookup.v4, r)
	}

	// Sort IPv4 ranges by start IP ascending.
	// For ties on start, sort by bits descending (more specific = later).
	// This ensures binary search picks the most specific entry for an IP.
	sort.Slice(lookup.v4, func(i, j int) bool {
		if lookup.v4[i].start != lookup.v4[j].start {
			return lookup.v4[i].start < lookup.v4[j].start
		}
		// Longer prefix = smaller range = higher priority = come last
		return (lookup.v4[i].end - lookup.v4[i].start) > (lookup.v4[j].end - lookup.v4[j].start)
	})

	// Sort IPv6 entries by prefix length descending (most specific first).
	// The linear scan depends on this ordering for correct longest-prefix-match.
	sort.Slice(lookup.v6, func(i, j int) bool {
		return lookup.v6[i].Prefix.Bits() > lookup.v6[j].Prefix.Bits()
	})

	return lookup
}

// ipv4PrefixRange computes the start/end IP addresses for an IPv4 CIDR prefix.
func ipv4PrefixRange(prefix netip.Prefix, country CountryCode) ipRange {
	addr := prefix.Addr()
	ip := addrToUint32(addr)
	bits := prefix.Bits()
	if bits == 0 {
		return ipRange{start: 0, end: ^uint32(0), country: country}
	}
	mask := ^uint32(0) << (32 - bits) //nolint:mnd // IPv4 = 32 bits
	start := ip & mask
	end := start | ^mask
	return ipRange{start: start, end: end, country: country}
}

//nolint:ireturn // constructor intentionally returns the lookup interface
func NewLookup(entries []Entry) CountryLookup {
	return newIndexedLookup(entries)
}

func (l *indexedLookup) LookupCountry(ip netip.Addr) CountryCode {
	if ip.Is4() {
		ip32 := addrToUint32(ip)
		// Binary search: find first entry where start > ip32,
		// then check the entry just before that (largest start <= ip32).
		idx := sort.Search(len(l.v4), func(i int) bool {
			return l.v4[i].start > ip32
		})
		// Walk backwards through entries with start <= ip32.
		// The first one whose range covers ip32 wins (most specific due to sort order).
		for idx > 0 {
			idx--
			if ip32 <= l.v4[idx].end {
				return l.v4[idx].country
			}
			// If this entry's start < last checked entry's start, and it didn't cover,
			// no earlier entry can cover either (all earlier entries have start <= this start).
			// But to be safe, we just continue.
		}
		return CountryCode{}
	}

	for _, entry := range l.v6 {
		if entry.Prefix.Contains(ip) {
			return entry.Country
		}
	}
	return CountryCode{}
}

func LookupCountry(lookup CountryLookup, ip netip.Addr) CountryCode {
	if lookup == nil {
		return CountryCode{}
	}
	return lookup.LookupCountry(ip)
}

func addrToUint32(addr netip.Addr) uint32 {
	a4 := addr.As4()
	return uint32(a4[0])<<24 | uint32(a4[1])<<16 | uint32(a4[2])<<8 | uint32(a4[3])
}
