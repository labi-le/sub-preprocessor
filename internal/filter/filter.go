package filter

import (
	"net/netip"
	"strings"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

const (
	alphabetSize  = 26
	bitsPerUint64 = 64
	toUpperOffset = 32
)

// CountrySet is a bitset for 2-letter country codes (AA-ZZ).
// 26 * 26 = 676 bits required. 11 * 64 = 704 bits.
type CountrySet [11]uint64

func (s *CountrySet) Has(country geofeed.CountryCode) bool {
	c1, c2 := country[0], country[1]
	if c1 < 'A' || c1 > 'Z' || c2 < 'A' || c2 > 'Z' {
		return false
	}
	idx := int(c1-'A')*alphabetSize + int(c2-'A')
	return (s[idx/bitsPerUint64] & (1 << (idx % bitsPerUint64))) != 0
}

// AllAllowed compacts ips in-place and returns the allowed prefix sub-slice.
// Callers must not rely on the input slice contents remaining unchanged.
func AllAllowed(lookup geofeed.CountryLookup, ips []netip.Addr, allowed CountrySet) []netip.Addr {
	n := 0
	for _, ip := range ips {
		country := geofeed.LookupCountry(lookup, ip)
		if allowed.Has(country) {
			ips[n] = ip
			n++
		}
	}
	return ips[:n]
}

// Add parses a single country code string and adds it to the set.
// Whitespace is trimmed, case is normalized to uppercase, and
// non-2-letter or non-ASCII strings are silently ignored.
func (s *CountrySet) Add(part string) {
	parseCountryPart(s, part)
}

// ParseAllowed parses one or more comma-separated lists of 2-letter country codes
// into a CountrySet. Each part may itself contain commas for sub-splitting.
func ParseAllowed(parts ...string) CountrySet {
	var set CountrySet
	for _, part := range parts {
		for sub := range strings.SplitSeq(part, ",") {
			set.Add(sub)
		}
	}
	return set
}

// All returns a CountrySet with every valid 2-letter country code set.
func All() CountrySet {
	var set CountrySet
	for i := range set {
		set[i] = ^uint64(0)
	}
	return set
}

// fullSet is the All() set precomputed once for IsFull comparisons.
var fullSet = All()

// IsFull reports whether s allows every country code (equivalently, no
// exclusions are in effect). A country filter is a no-op in this case, so it
// keeps every IP including those with an unknown country.
func IsFull(s CountrySet) bool {
	return s == fullSet
}

// Exclude unsets every country code that is present in other.
func (s *CountrySet) Exclude(other CountrySet) {
	for i := range s {
		s[i] &^= other[i]
	}
}

func parseCountryPart(set *CountrySet, part string) {
	start := 0
	for start < len(part) && (part[start] == ' ' || part[start] == '\t' || part[start] == '\n' || part[start] == '\r') {
		start++
	}
	end := len(part)
	for end > start && (part[end-1] == ' ' || part[end-1] == '\t' || part[end-1] == '\n' || part[end-1] == '\r') {
		end--
	}

	if end-start == 2 { //nolint:mnd // ISO 3166-1 alpha-2 length
		c1 := part[start]
		c2 := part[start+1]
		if c1 >= 'a' && c1 <= 'z' {
			c1 -= toUpperOffset
		}
		if c2 >= 'a' && c2 <= 'z' {
			c2 -= toUpperOffset
		}
		if c1 >= 'A' && c1 <= 'Z' && c2 >= 'A' && c2 <= 'Z' {
			i := int(c1-'A')*alphabetSize + int(c2-'A')
			set[i/bitsPerUint64] |= 1 << (i % bitsPerUint64)
		}
	}
}
