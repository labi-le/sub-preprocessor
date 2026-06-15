package filter_test

import (
	"net/netip"
	"testing"

	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

func BenchmarkAllAllowed(b *testing.B) {
	lookup := geofeed.NewLookup([]geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: geofeed.CountryCode{'N', 'L'}},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: geofeed.CountryCode{'D', 'E'}},
		{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Country: geofeed.CountryCode{'U', 'S'}},
	})
	allowed := filter.ParseAllowed("NL,DE")
	base := []netip.Addr{
		netip.MustParseAddr("198.51.100.10"),
		netip.MustParseAddr("203.0.113.5"),
		netip.MustParseAddr("192.0.2.44"),
		netip.MustParseAddr("198.51.100.20"),
	}
	ips := make([]netip.Addr, len(base))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		copy(ips, base)
		got := filter.AllAllowed(lookup, ips, allowed)
		if len(got) != 3 {
			b.Fatalf("unexpected allowed count: %d", len(got))
		}
	}
}
