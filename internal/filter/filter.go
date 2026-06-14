package filter

import (
	"net/netip"
	"strings"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

func FirstAllowed(entries []geofeed.Entry, ips []netip.Addr, allowed map[string]bool, strict bool) (netip.Addr, string, bool) {
	for _, ip := range ips {
		country := geofeed.LookupCountry(entries, ip)
		if allowed[country] {
			if !strict {
				return ip, country, true
			}
		} else if strict {
			return netip.Addr{}, "", false
		}
	}
	if strict {
		if len(ips) > 0 {
			country := geofeed.LookupCountry(entries, ips[0])
			return ips[0], country, true
		}
	}
	return netip.Addr{}, "", false
}

func ParseAllowCountries(raw string) map[string]bool {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	// Count commas to pre-size the map
	commas := strings.Count(raw, ",")
	out := make(map[string]bool, commas+1)

	for len(raw) > 0 {
		idx := strings.IndexByte(raw, ',')
		var part string
		if idx >= 0 {
			part = raw[:idx]
			raw = raw[idx+1:]
		} else {
			part = raw
			raw = ""
		}

		if len(part) > 0 && isUpperASCII(part) && !hasSpace(part) {
			out[part] = true
			continue
		}

		part = strings.ToUpper(strings.TrimSpace(part))
		if part != "" {
			out[part] = true
		}
	}
	return out
}

func isUpperASCII(s string) bool {
	for i := range s {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

func hasSpace(s string) bool {
	for i := range s {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' {
			return true
		}
	}
	return false
}
