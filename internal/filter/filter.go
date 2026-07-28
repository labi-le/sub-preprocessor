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
	// codeCount is the number of addressable codes: index 675 is ZZ, and every
	// index at or above codeCount maps to no country code at all.
	codeCount = alphabetSize * alphabetSize
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

// Permits reports whether country passes an allow-list / deny-list pair.
//
// The two sets are deliberately asymmetric about the unknown country
// geofeed.LookupCountry returns for an IP no source covers. A non-full allowed
// set is an allow-list and an unknown country is not in it, so it is dropped.
// denied is matched positively: "exclude RU" says nothing about an IP that
// cannot be placed, so an unknown country survives it. A full allowed set means
// no allow-list was requested and constrains nothing.
func Permits(allowed, denied CountrySet, country geofeed.CountryCode) bool {
	if !IsFull(allowed) && !allowed.Has(country) {
		return false
	}
	return !denied.Has(country)
}

// Permitted compacts ips in-place and returns the prefix sub-slice whose
// countries Permits accepts. Callers must not rely on the input slice contents
// remaining unchanged.
func Permitted(lookup geofeed.CountryLookup, ips []netip.Addr, allowed, denied CountrySet) []netip.Addr {
	allowAll := IsFull(allowed) // whole-bitset compare, hoisted out of the loop
	n := 0
	for _, ip := range ips {
		country := geofeed.LookupCountry(lookup, ip)
		if (allowAll || allowed.Has(country)) && !denied.Has(country) {
			ips[n] = ip
			n++
		}
	}
	return ips[:n]
}

// Add parses a single country code string and adds it to the set. Whitespace is
// trimmed and case is normalized to uppercase. It reports whether part was a
// 2-letter ASCII code; a false return means nothing was added, which callers
// validating user-supplied input must not ignore.
func (s *CountrySet) Add(part string) bool {
	return parseCountryPart(s, part)
}

// ParseAllowed parses one or more comma-separated lists of 2-letter country codes
// into a CountrySet. Each part may itself contain commas for sub-splitting.
// Tokens that are not country codes are ignored; use Add to detect them.
func ParseAllowed(parts ...string) CountrySet {
	var set CountrySet
	for _, part := range parts {
		for sub := range strings.SplitSeq(part, ",") {
			set.Add(sub)
		}
	}
	return set
}

// All returns a CountrySet with every valid 2-letter country code set. The bits
// above ZZ are left clear: they address no code, and a set that keeps them can
// never compare empty however much is excluded from it.
func All() CountrySet {
	var set CountrySet
	whole := codeCount / bitsPerUint64
	for i := range whole {
		set[i] = ^uint64(0)
	}
	set[whole] = 1<<(codeCount%bitsPerUint64) - 1
	return set
}

// fullSet is the All() set precomputed once for IsFull comparisons.
var fullSet = All()

// IsFull reports whether s contains every country code. As an allow-list a full
// set imposes no constraint, so the country filter keeps every IP including
// those whose country is unknown; see Permits.
func IsFull(s CountrySet) bool {
	return s == fullSet
}

// IsEmpty reports whether s contains no country code at all.
func IsEmpty(s CountrySet) bool {
	for _, v := range s {
		if v != 0 {
			return false
		}
	}
	return true
}

// Exclude unsets every country code that is present in other.
func (s *CountrySet) Exclude(other CountrySet) {
	for i := range s {
		s[i] &^= other[i]
	}
}

func parseCountryPart(set *CountrySet, part string) bool {
	start := 0
	for start < len(part) && (part[start] == ' ' || part[start] == '\t' || part[start] == '\n' || part[start] == '\r') {
		start++
	}
	end := len(part)
	for end > start && (part[end-1] == ' ' || part[end-1] == '\t' || part[end-1] == '\n' || part[end-1] == '\r') {
		end--
	}

	if end-start != 2 { //nolint:mnd // ISO 3166-1 alpha-2 length
		return false
	}
	c1 := part[start]
	c2 := part[start+1]
	if c1 >= 'a' && c1 <= 'z' {
		c1 -= toUpperOffset
	}
	if c2 >= 'a' && c2 <= 'z' {
		c2 -= toUpperOffset
	}
	if c1 < 'A' || c1 > 'Z' || c2 < 'A' || c2 > 'Z' {
		return false
	}
	i := int(c1-'A')*alphabetSize + int(c2-'A')
	set[i/bitsPerUint64] |= 1 << (i % bitsPerUint64)
	return true
}
