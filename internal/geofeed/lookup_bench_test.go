package geofeed_test

import (
	"fmt"
	"net/netip"
	"testing"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

func BenchmarkIndexedLookup_ManyEntries(b *testing.B) {
	entries := makeBenchmarkEntries(500)
	lookup := geofeed.NewLookup(entries)
	ip := netip.MustParseAddr("198.51.200.1")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := lookup.LookupCountry(ip)
		if got != (geofeed.CountryCode{'J', 'P'}) {
			b.Fatalf("unexpected %q", got)
		}
	}
}

// BenchmarkIndexedLookup_ManyEntriesMiss exercises the common miss path: the
// probe IP sits above every range, which without the prefix-max early exit
// would walk all entries backwards on every lookup.
func BenchmarkIndexedLookup_ManyEntriesMiss(b *testing.B) {
	entries := makeBenchmarkEntries(500)
	lookup := geofeed.NewLookup(entries)
	ip := netip.MustParseAddr("198.52.0.1")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := lookup.LookupCountry(ip); got != (geofeed.CountryCode{}) {
			b.Fatalf("unexpected %q", got)
		}
	}
}

func makeBenchmarkEntries(n int) []geofeed.Entry {
	entries := make([]geofeed.Entry, n)
	for i := range n {
		entries[i] = geofeed.Entry{
			Prefix:  netip.MustParsePrefix(fmt.Sprintf("198.51.%d.0/24", i%200)),
			Country: geofeed.CountryCode{'D', 'E'},
		}
	}
	entries[n-1] = geofeed.Entry{
		Prefix:  netip.MustParsePrefix("198.51.200.0/24"),
		Country: geofeed.CountryCode{'J', 'P'},
	}
	return entries
}
