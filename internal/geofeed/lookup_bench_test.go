package geofeed_test

import (
	"fmt"
	"net/netip"
	"testing"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

func BenchmarkLinearLookup_ManyEntries(b *testing.B) {
	entries := makeBenchmarkEntries(500)
	lookup := geofeed.NewLinearLookup(entries)
	ip := netip.MustParseAddr("198.51.200.1")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := lookup.LookupCountry(ip)
		if got != "JP" {
			b.Fatalf("unexpected %q", got)
		}
	}
}

func BenchmarkIndexedLookup_ManyEntries(b *testing.B) {
	entries := makeBenchmarkEntries(500)
	lookup := geofeed.NewIndexedLookup(entries)
	ip := netip.MustParseAddr("198.51.200.1")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := lookup.LookupCountry(ip)
		if got != "JP" {
			b.Fatalf("unexpected %q", got)
		}
	}
}

func makeBenchmarkEntries(n int) []geofeed.Entry {
	entries := make([]geofeed.Entry, n)
	for i := range n {
		entries[i] = geofeed.Entry{
			Prefix:  netip.MustParsePrefix(fmt.Sprintf("198.51.%d.0/24", i%200)),
			Country: "DE",
		}
	}
	entries[n-1] = geofeed.Entry{
		Prefix:  netip.MustParsePrefix("198.51.200.0/24"),
		Country: "JP",
	}
	return entries
}
