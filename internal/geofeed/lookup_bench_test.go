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

// BenchmarkV6Lookup_100k probes a hit in ~100k v6 ranges (DB-IP scale). Named
// V6 so before/after the indexed-v6 rewrite compares with the same bench name.
func BenchmarkV6Lookup_100k(b *testing.B) {
	lookup := geofeed.NewLookup(makeBenchmarkV6Entries(100_000))
	ip := netip.MustParseAddr("2001:db8:0:1234::1")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := lookup.LookupCountry(ip); got != (geofeed.CountryCode{'D', 'E'}) {
			b.Fatalf("unexpected %q", got)
		}
	}
}

// BenchmarkV6Lookup_100kMiss probes an IP outside every range: the worst case
// for a linear scan (touches all entries on every lookup).
func BenchmarkV6Lookup_100kMiss(b *testing.B) {
	lookup := geofeed.NewLookup(makeBenchmarkV6Entries(100_000))
	ip := netip.MustParseAddr("3000::1")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := lookup.LookupCountry(ip); got != (geofeed.CountryCode{}) {
			b.Fatalf("unexpected %q", got)
		}
	}
}

func makeBenchmarkV6Entries(n int) []geofeed.Entry {
	entries := make([]geofeed.Entry, n)
	for i := range n {
		entries[i] = geofeed.Entry{
			Prefix:  netip.MustParsePrefix(fmt.Sprintf("2001:db8:%x:%x::/64", i/65536, i%65536)),
			Country: geofeed.CountryCode{'D', 'E'},
		}
	}
	return entries
}
