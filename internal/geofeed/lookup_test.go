package geofeed_test

import (
	"net/netip"
	"testing"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

// TestLookupCountry_MissPaths guards the early-exit miss path (prefix-max
// array): an IP not covered by any range must return the zero country without
// scanning every preceding entry, for misses below, between, and above ranges.
func TestLookupCountry_MissPaths(t *testing.T) {
	t.Parallel()

	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("20.0.0.0/8"), Country: geofeed.CountryCode{'D', 'E'}},
		{Prefix: netip.MustParsePrefix("40.0.0.0/8"), Country: geofeed.CountryCode{'U', 'S'}},
		{Prefix: netip.MustParsePrefix("40.1.0.0/16"), Country: geofeed.CountryCode{'N', 'L'}},
		{Prefix: netip.MustParsePrefix("60.0.0.0/8"), Country: geofeed.CountryCode{'J', 'P'}},
	}
	lookup := geofeed.NewLookup(entries)

	tests := []struct {
		name string
		ip   string
		want geofeed.CountryCode
	}{
		{name: "miss below all ranges", ip: "5.5.5.5", want: geofeed.CountryCode{}},
		{name: "miss between ranges", ip: "30.0.0.1", want: geofeed.CountryCode{}},
		{name: "miss between later ranges", ip: "50.255.255.255", want: geofeed.CountryCode{}},
		{name: "miss above all ranges", ip: "200.0.0.1", want: geofeed.CountryCode{}},
		{name: "hit outer range", ip: "40.2.0.1", want: geofeed.CountryCode{'U', 'S'}},
		{name: "hit most specific nested range", ip: "40.1.2.3", want: geofeed.CountryCode{'N', 'L'}},
		{name: "hit first range start boundary", ip: "20.0.0.0", want: geofeed.CountryCode{'D', 'E'}},
		{name: "hit last range end boundary", ip: "60.255.255.255", want: geofeed.CountryCode{'J', 'P'}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := geofeed.LookupCountry(lookup, netip.MustParseAddr(tc.ip)); got != tc.want {
				t.Fatalf("LookupCountry(%s) = %q, want %q", tc.ip, got, tc.want)
			}
		})
	}
}

func mustRange(start, end, cc string) geofeed.Range {
	return geofeed.Range{
		Start:   netip.MustParseAddr(start),
		End:     netip.MustParseAddr(end),
		Country: geofeed.CountryCode{cc[0], cc[1]},
	}
}

// TestNewRangeLookup_V4 covers arbitrary (non-CIDR) v4 ranges: nesting picks
// the smallest covering span even when a larger range starts later, identical
// ranges resolve to the earliest input, and family-mismatched probes miss.
func TestNewRangeLookup_V4(t *testing.T) {
	t.Parallel()

	ranges := []geofeed.Range{
		mustRange("10.0.0.0", "10.0.255.255", "DE"),
		mustRange("10.0.1.0", "10.0.1.255", "NL"),
		// Partial overlap: FR is smaller but starts earlier than PL, so a
		// first-cover backward walk would wrongly return PL for 20.0.0.11.
		mustRange("20.0.0.10", "20.0.0.12", "FR"),
		mustRange("20.0.0.11", "20.0.0.200", "PL"),
		// Identical ranges: earliest input order wins.
		mustRange("30.0.0.0", "30.0.0.255", "US"),
		mustRange("30.0.0.0", "30.0.0.255", "CA"),
	}
	lookup := geofeed.NewRangeLookup(ranges)

	tests := []struct {
		name string
		ip   string
		want geofeed.CountryCode
	}{
		{name: "outer range", ip: "10.0.2.1", want: geofeed.CountryCode{'D', 'E'}},
		{name: "nested smallest span", ip: "10.0.1.7", want: geofeed.CountryCode{'N', 'L'}},
		{name: "smallest span beats later start", ip: "20.0.0.11", want: geofeed.CountryCode{'F', 'R'}},
		{name: "overlap tail", ip: "20.0.0.100", want: geofeed.CountryCode{'P', 'L'}},
		{name: "identical ranges first wins", ip: "30.0.0.9", want: geofeed.CountryCode{'U', 'S'}},
		{name: "miss below", ip: "9.255.255.255", want: geofeed.CountryCode{}},
		{name: "miss between", ip: "15.0.0.1", want: geofeed.CountryCode{}},
		{name: "miss above", ip: "200.0.0.1", want: geofeed.CountryCode{}},
		{name: "v6 probe misses v4 db", ip: "2001:db8::1", want: geofeed.CountryCode{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lookup.LookupCountry(netip.MustParseAddr(tc.ip)); got != tc.want {
				t.Fatalf("LookupCountry(%s) = %q, want %q", tc.ip, got, tc.want)
			}
		})
	}
}

// TestNewRangeLookup_V6 mirrors the v4 semantics on the indexed v6 structure
// that replaces the linear scan: nesting, equal-start span ties, duplicates,
// misses, and family separation.
func TestNewRangeLookup_V6(t *testing.T) {
	t.Parallel()

	ranges := []geofeed.Range{
		mustRange("2001:db8::", "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", "DE"),
		mustRange("2001:db8:1::", "2001:db8:1::ffff", "NL"),
		// Equal start, different span: smallest span must win in either order.
		mustRange("2001:db8:2::", "2001:db8:2::ffff", "FI"),
		mustRange("2001:db8:2::", "2001:db8:2::ff", "SE"),
		// Smaller span starting earlier than a larger overlapping range.
		mustRange("2001:db8:3::10", "2001:db8:3::12", "FR"),
		mustRange("2001:db8:3::11", "2001:db8:3::200", "PL"),
		// Identical ranges: earliest input order wins.
		mustRange("2001:db8:4::", "2001:db8:4::ff", "US"),
		mustRange("2001:db8:4::", "2001:db8:4::ff", "CA"),
	}
	lookup := geofeed.NewRangeLookup(ranges)

	tests := []struct {
		name string
		ip   string
		want geofeed.CountryCode
	}{
		{name: "outer range", ip: "2001:db8:9::1", want: geofeed.CountryCode{'D', 'E'}},
		{name: "nested smallest span", ip: "2001:db8:1::7", want: geofeed.CountryCode{'N', 'L'}},
		{name: "equal start smallest span", ip: "2001:db8:2::1", want: geofeed.CountryCode{'S', 'E'}},
		{name: "equal start outside small range", ip: "2001:db8:2::1ff", want: geofeed.CountryCode{'F', 'I'}},
		{name: "smallest span beats later start", ip: "2001:db8:3::11", want: geofeed.CountryCode{'F', 'R'}},
		{name: "identical ranges first wins", ip: "2001:db8:4::9", want: geofeed.CountryCode{'U', 'S'}},
		{name: "miss below", ip: "2001:db7::1", want: geofeed.CountryCode{}},
		{name: "miss above", ip: "3000::1", want: geofeed.CountryCode{}},
		{name: "v4 probe misses v6 db", ip: "10.0.0.1", want: geofeed.CountryCode{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lookup.LookupCountry(netip.MustParseAddr(tc.ip)); got != tc.want {
				t.Fatalf("LookupCountry(%s) = %q, want %q", tc.ip, got, tc.want)
			}
		})
	}
}

// TestNewLookup_V6MostSpecific is the regression net for routing NewLookup
// through the range machinery: prefix-based v6 entries must keep
// longest-prefix-match semantics (equivalent to smallest span for CIDR).
func TestNewLookup_V6MostSpecific(t *testing.T) {
	t.Parallel()

	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("2001:db8::/32"), Country: geofeed.CountryCode{'D', 'E'}},
		{Prefix: netip.MustParsePrefix("2001:db8:1::/48"), Country: geofeed.CountryCode{'N', 'L'}},
	}
	lookup := geofeed.NewLookup(entries)

	if got := lookup.LookupCountry(netip.MustParseAddr("2001:db8:1::1")); got != (geofeed.CountryCode{'N', 'L'}) {
		t.Fatalf("nested prefix = %q, want NL", got)
	}
	if got := lookup.LookupCountry(netip.MustParseAddr("2001:db8:2::1")); got != (geofeed.CountryCode{'D', 'E'}) {
		t.Fatalf("outer prefix = %q, want DE", got)
	}
	if got := lookup.LookupCountry(netip.MustParseAddr("2001:db9::1")); got != (geofeed.CountryCode{}) {
		t.Fatalf("miss = %q, want zero", got)
	}
}
