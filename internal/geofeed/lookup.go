package geofeed

import "net/netip"

const ipv4Bits = 32

type CountryLookup interface {
	LookupCountry(ip netip.Addr) string
}

type LinearLookup struct {
	entries []Entry
}

func NewLinearLookup(entries []Entry) *LinearLookup {
	return &LinearLookup{entries: entries}
}

func (l *LinearLookup) LookupCountry(ip netip.Addr) string {
	for _, e := range l.entries {
		if e.Prefix.Contains(ip) {
			return e.Country
		}
	}
	return ""
}

type IndexedLookup struct {
	v4ByBits [ipv4Bits + 1]map[uint32]string
	v4Bits   []uint8
	v6       []Entry
}

func NewIndexedLookup(entries []Entry) *IndexedLookup {
	lookup := &IndexedLookup{}
	var present [ipv4Bits + 1]bool

	for _, entry := range entries {
		prefix := entry.Prefix.Masked()
		addr := prefix.Addr()
		if !addr.Is4() {
			lookup.v6 = append(lookup.v6, entry)
			continue
		}

		bits := prefix.Bits()
		if lookup.v4ByBits[bits] == nil {
			lookup.v4ByBits[bits] = make(map[uint32]string)
			present[bits] = true
		}

		key := maskIPv4(addrToUint32(addr), uint8(bits))
		if _, exists := lookup.v4ByBits[bits][key]; !exists {
			lookup.v4ByBits[bits][key] = entry.Country
		}
	}

	for bits := ipv4Bits; bits >= 0; bits-- {
		if present[bits] {
			lookup.v4Bits = append(lookup.v4Bits, uint8(bits))
		}
	}

	return lookup
}

//nolint:ireturn // constructor intentionally returns the lookup interface
func NewLookup(entries []Entry) CountryLookup {
	return NewIndexedLookup(entries)
}

func (l *IndexedLookup) LookupCountry(ip netip.Addr) string {
	if ip.Is4() {
		ip32 := addrToUint32(ip)
		for _, bits := range l.v4Bits {
			if country, ok := l.v4ByBits[bits][maskIPv4(ip32, bits)]; ok {
				return country
			}
		}
		return ""
	}

	for _, entry := range l.v6 {
		if entry.Prefix.Contains(ip) {
			return entry.Country
		}
	}
	return ""
}

func LookupCountry(lookup CountryLookup, ip netip.Addr) string {
	if lookup == nil {
		return ""
	}
	return lookup.LookupCountry(ip)
}

func addrToUint32(addr netip.Addr) uint32 {
	a4 := addr.As4()
	return uint32(a4[0])<<24 | uint32(a4[1])<<16 | uint32(a4[2])<<8 | uint32(a4[3])
}

func maskIPv4(value uint32, bits uint8) uint32 {
	if bits == 0 {
		return 0
	}
	return value & (^uint32(0) << (ipv4Bits - bits))
}
