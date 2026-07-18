package geofeed

import (
	"encoding/binary"
	"math/bits"
	"net/netip"
	"sort"
)

type CountryLookup interface {
	LookupCountry(ip netip.Addr) CountryCode
}

// Range is an inclusive IP range with its associated country. Start and End
// must be the same address family; netip.Addr pairs (not Prefix) because DB-IP
// and RIR v4 ranges are not CIDR-aligned.
type Range struct {
	Start   netip.Addr
	End     netip.Addr
	Country CountryCode
}

// ipRange is an inclusive IPv4 range in native uint32 form.
type ipRange struct {
	start   uint32
	end     uint32
	country CountryCode
}

// uint128 is an IPv6 address as big-endian (hi, lo) words, so range
// comparisons and span arithmetic stay branch-cheap and allocation-free.
type uint128 struct {
	hi uint64
	lo uint64
}

func (a uint128) less(b uint128) bool {
	return a.hi < b.hi || (a.hi == b.hi && a.lo < b.lo)
}

// sub returns a-b; the caller guarantees a >= b (range ends never precede
// their starts after builder validation).
func (a uint128) sub(b uint128) uint128 {
	lo, borrow := bits.Sub64(a.lo, b.lo, 0)
	hi, _ := bits.Sub64(a.hi, b.hi, borrow)
	return uint128{hi: hi, lo: lo}
}

func (a uint128) or(b uint128) uint128 {
	return uint128{hi: a.hi | b.hi, lo: a.lo | b.lo}
}

func (a uint128) addr() netip.Addr {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], a.hi)
	binary.BigEndian.PutUint64(b[8:], a.lo)
	return netip.AddrFrom16(b)
}

func addrToUint128(addr netip.Addr) uint128 {
	b := addr.As16()
	return uint128{
		hi: binary.BigEndian.Uint64(b[:8]),
		lo: binary.BigEndian.Uint64(b[8:]),
	}
}

// ip6Range is an inclusive IPv6 range in native uint128 form.
type ip6Range struct {
	start   uint128
	end     uint128
	country CountryCode
}

type indexedLookup struct {
	v4 []ipRange
	// v4MaxEnd[i] is the maximum range end among v4[0..i]. Non-decreasing, so
	// the backward walk in LookupCountry can stop as soon as no earlier range
	// can still cover the IP, making misses O(log n) instead of O(n).
	v4MaxEnd []uint32
	v6       []ip6Range
	// v6MaxEnd mirrors v4MaxEnd for the v6 array.
	v6MaxEnd []uint128
}

func newIndexedLookup(ranges []Range) *indexedLookup {
	lookup := &indexedLookup{}

	for _, r := range ranges {
		// One family per entry, inclusive, ordered — parsers guarantee this;
		// garbage from other callers is dropped rather than corrupting the index.
		if !r.Start.IsValid() || r.Start.Is4() != r.End.Is4() || r.End.Less(r.Start) {
			continue
		}
		if r.Start.Is4() {
			lookup.v4 = append(lookup.v4, ipRange{
				start:   addrToUint32(r.Start),
				end:     addrToUint32(r.End),
				country: r.Country,
			})
			continue
		}
		lookup.v6 = append(lookup.v6, ip6Range{
			start:   addrToUint128(r.Start),
			end:     addrToUint128(r.End),
			country: r.Country,
		})
	}

	// Stable sort by start only: identical ranges stay in input order, and the
	// backward walk's <=-span rule then resolves them to the earliest input.
	sort.SliceStable(lookup.v4, func(i, j int) bool {
		return lookup.v4[i].start < lookup.v4[j].start
	})
	sort.SliceStable(lookup.v6, func(i, j int) bool {
		return lookup.v6[i].start.less(lookup.v6[j].start)
	})

	if len(lookup.v4) > 0 {
		lookup.v4MaxEnd = make([]uint32, len(lookup.v4))
		maxEnd := uint32(0)
		for i, r := range lookup.v4 {
			if r.end > maxEnd {
				maxEnd = r.end
			}
			lookup.v4MaxEnd[i] = maxEnd
		}
	}
	if len(lookup.v6) > 0 {
		lookup.v6MaxEnd = make([]uint128, len(lookup.v6))
		maxEnd := uint128{}
		for i, r := range lookup.v6 {
			if maxEnd.less(r.end) {
				maxEnd = r.end
			}
			lookup.v6MaxEnd[i] = maxEnd
		}
	}

	return lookup
}

// prefixRange converts a CIDR prefix to an inclusive Range.
func prefixRange(prefix netip.Prefix, country CountryCode) Range {
	prefix = prefix.Masked()
	addr := prefix.Addr()
	if addr.Is4() {
		r := ipv4PrefixRange(prefix, country)
		return Range{Start: uint32ToAddr(r.start), End: uint32ToAddr(r.end), Country: country}
	}
	start := addrToUint128(addr)
	end := start.or(hostMask128(prefix.Bits()))
	return Range{Start: addr, End: end.addr(), Country: country}
}

// hostMask128 returns the host-part mask (low ones) for a v6 prefix length.
func hostMask128(prefixBits int) uint128 {
	const v6Bits, wordBits = 128, 64
	host := v6Bits - prefixBits
	switch {
	case host >= v6Bits:
		return uint128{hi: ^uint64(0), lo: ^uint64(0)}
	case host > wordBits:
		return uint128{hi: ^uint64(0) >> (v6Bits - host), lo: ^uint64(0)}
	case host == wordBits:
		return uint128{lo: ^uint64(0)}
	default:
		return uint128{lo: (uint64(1) << host) - 1}
	}
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
	ranges := make([]Range, len(entries))
	for i, entry := range entries {
		ranges[i] = prefixRange(entry.Prefix, entry.Country)
	}
	return newIndexedLookup(ranges)
}

//nolint:ireturn // constructor intentionally returns the lookup interface
func NewRangeLookup(ranges []Range) CountryLookup {
	return newIndexedLookup(ranges)
}

func (l *indexedLookup) LookupCountry(ip netip.Addr) CountryCode {
	if ip.Is4() {
		return l.lookupV4(addrToUint32(ip))
	}
	return l.lookupV6(addrToUint128(ip))
}

// lookupV4 finds the smallest-span range covering ip32. Binary search locates
// the first range starting after ip32; the backward walk then scans candidates
// while the running max-end can still reach ip32, keeping the smallest span
// (<= so equal spans resolve to the earliest sorted — and thus input — order).
func (l *indexedLookup) lookupV4(ip32 uint32) CountryCode {
	idx := sort.Search(len(l.v4), func(i int) bool {
		return l.v4[i].start > ip32
	})
	var (
		found    bool
		bestSpan uint32
		country  CountryCode
	)
	for idx > 0 {
		idx--
		if l.v4MaxEnd[idx] < ip32 {
			break
		}
		r := l.v4[idx]
		if ip32 > r.end {
			continue
		}
		if span := r.end - r.start; !found || span <= bestSpan {
			found, bestSpan, country = true, span, r.country
		}
	}
	return country
}

// lookupV6 mirrors lookupV4 over uint128 words.
func (l *indexedLookup) lookupV6(ip128 uint128) CountryCode {
	idx := sort.Search(len(l.v6), func(i int) bool {
		return ip128.less(l.v6[i].start)
	})
	var (
		found    bool
		bestSpan uint128
		country  CountryCode
	)
	for idx > 0 {
		idx--
		if l.v6MaxEnd[idx].less(ip128) {
			break
		}
		r := l.v6[idx]
		if r.end.less(ip128) {
			continue
		}
		if span := r.end.sub(r.start); !found || !bestSpan.less(span) {
			found, bestSpan, country = true, span, r.country
		}
	}
	return country
}

func LookupCountry(lookup CountryLookup, ip netip.Addr) CountryCode {
	if lookup == nil {
		return CountryCode{}
	}
	return lookup.LookupCountry(ip)
}

func addrToUint32(addr netip.Addr) uint32 {
	a4 := addr.As4()
	return binary.BigEndian.Uint32(a4[:])
}

func uint32ToAddr(ip uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], ip)
	return netip.AddrFrom4(b)
}
