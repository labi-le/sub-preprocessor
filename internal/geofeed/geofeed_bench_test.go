package geofeed_test

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

func benchmarkParseInput(n int) []byte {
	var sb strings.Builder
	for i := range n {
		_, _ = fmt.Fprintf(&sb, "198.51.%d.%d/24,DE,Bavaria,Munich\n", i/256, i%256)
	}
	return []byte(sb.String())
}

func BenchmarkParse_Small(b *testing.B) {
	body := []byte(strings.Join([]string{
		"# comment line\n",
		"198.51.100.0/24,DE",
		"198.51.100.10/32,NL,ZH,Amsterdam",
		"203.0.113.0/24,US",
	}, "\n"))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := geofeed.Parse(body)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParse_1000Entries(b *testing.B) {
	body := benchmarkParseInput(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := geofeed.Parse(body)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLookupCountry_Hit(b *testing.B) {
	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: geofeed.CountryCode{'D', 'E'}},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: geofeed.CountryCode{'U', 'S'}},
		{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Country: geofeed.CountryCode{'G', 'B'}},
	}
	lookup := geofeed.NewLookup(entries)
	ip := netip.MustParseAddr("198.51.100.42")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := geofeed.LookupCountry(lookup, ip)
		if got != (geofeed.CountryCode{'D', 'E'}) {
			b.Fatalf("unexpected %q", got)
		}
	}
}

func BenchmarkLookupCountry_Miss(b *testing.B) {
	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: geofeed.CountryCode{'D', 'E'}},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: geofeed.CountryCode{'U', 'S'}},
	}
	lookup := geofeed.NewLookup(entries)
	ip := netip.MustParseAddr("10.0.0.1")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = geofeed.LookupCountry(lookup, ip)
	}
}

func BenchmarkLookupCountry_ManyEntries(b *testing.B) {
	n := 500
	entries := make([]geofeed.Entry, n)
	for i := range n {
		entries[i] = geofeed.Entry{
			Prefix:  netip.MustParsePrefix(fmt.Sprintf("198.51.%d.0/24", i%200)),
			Country: geofeed.CountryCode{'D', 'E'},
		}
	}
	// last entry contains the target IP
	entries[n-1] = geofeed.Entry{
		Prefix:  netip.MustParsePrefix("198.51.200.0/24"),
		Country: geofeed.CountryCode{'J', 'P'},
	}
	ip := netip.MustParseAddr("198.51.200.1")
	lookup := geofeed.NewLookup(entries)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := geofeed.LookupCountry(lookup, ip)
		if got != (geofeed.CountryCode{'J', 'P'}) {
			b.Fatalf("unexpected %q", got)
		}
	}
}
