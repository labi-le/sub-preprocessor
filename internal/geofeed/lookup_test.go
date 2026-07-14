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
