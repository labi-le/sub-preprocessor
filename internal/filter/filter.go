package filter

import (
	"net/netip"
	"strings"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

const (
	countryCodeLen = 2
	alphabetSize   = 26
	bitsPerUint64  = 64
	toUpperOffset  = 32
)

// CountrySet is a bitset for 2-letter country codes (AA-ZZ).
// 26 * 26 = 676 bits required. 11 * 64 = 704 bits.
type CountrySet [11]uint64

func (s *CountrySet) Has(country string) bool {
	if len(country) != countryCodeLen {
		return false
	}
	c1, c2 := country[0], country[1]
	if c1 < 'A' || c1 > 'Z' || c2 < 'A' || c2 > 'Z' {
		return false
	}
	idx := int(c1-'A')*alphabetSize + int(c2-'A')
	return (s[idx/bitsPerUint64] & (1 << (idx % bitsPerUint64))) != 0
}

func FirstAllowed(lookup geofeed.CountryLookup, ips []netip.Addr, allowed CountrySet, strict bool) (netip.Addr, string, bool) {
	for _, ip := range ips {
		country := geofeed.LookupCountry(lookup, ip)
		if allowed.Has(country) {
			if !strict {
				return ip, country, true
			}
		} else if strict {
			return netip.Addr{}, "", false
		}
	}
	if strict {
		if len(ips) > 0 {
			country := geofeed.LookupCountry(lookup, ips[0])
			return ips[0], country, true
		}
	}
	return netip.Addr{}, "", false
}

// AllAllowed returns all IPs from ips whose country is in the allowed set.
func AllAllowed(lookup geofeed.CountryLookup, ips []netip.Addr, allowed CountrySet) []netip.Addr {
	result := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		country := geofeed.LookupCountry(lookup, ip)
		if allowed.Has(country) {
			result = append(result, ip)
		}
	}
	return result
}

func ParseAllowCountries(raw string) CountrySet {
	var set CountrySet
	for part := range strings.SplitSeq(raw, ",") {
		parseCountryPart(&set, part)
	}
	return set
}

// ParseAllowed parses rawCountries (comma-separated) and rawGroups (comma-separated),
// looking up each group in groupsMap to expand into individual country codes.
func ParseAllowed(rawCountries string, rawGroups string, groupsMap map[string][]string) CountrySet {
	var set CountrySet
	for part := range strings.SplitSeq(rawCountries, ",") {
		parseCountryPart(&set, part)
	}
	for part := range strings.SplitSeq(rawGroups, ",") {
		part = trimSpace(part)
		if part == "" {
			continue
		}
		countries, ok := groupsMap[part]
		if !ok {
			continue
		}
		for _, c := range countries {
			parseCountryPart(&set, c)
		}
	}
	return set
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
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

	if end-start == countryCodeLen {
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
