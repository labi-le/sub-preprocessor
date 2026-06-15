package preprocess_test

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

func BenchmarkParseAllowCountries(b *testing.B) {
	countries := "DE,US,JP,GB,FR,  nl  "
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		allowed := filter.ParseAllowCountries(countries)
		if !allowed.Has("DE") || !allowed.Has("NL") {
			b.Fatalf("unexpected result")
		}
	}
}

func BenchmarkParseAllowCountries_Single(b *testing.B) {
	countries := "DE"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filter.ParseAllowCountries(countries)
	}
}

func BenchmarkRewriteNodeName(b *testing.B) {
	var nodes []subscription.Node
	subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#Old Name"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})
	node := nodes[0]
	ip := netip.MustParseAddr("198.51.100.10")
	country := "NL"
	var sb bytes.Buffer
	sb.Grow(256)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sb.Reset()
		rewrite.NodeName(&sb, node, country, ip)
	}
}

func BenchmarkRewriteNodeName_EmptyName(b *testing.B) {
	var nodes []subscription.Node
	subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})
	node := nodes[0]
	ip := netip.MustParseAddr("203.0.113.5")
	country := "US"
	var sb bytes.Buffer
	sb.Grow(256)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sb.Reset()
		rewrite.NodeName(&sb, node, country, ip)
	}
}

func BenchmarkFirstAllowedIP_Hit(b *testing.B) {
	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: "DE"},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: "US"},
		{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Country: "GB"},
	}
	ips := []netip.Addr{
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("198.51.100.10"),
		netip.MustParseAddr("203.0.113.10"),
	}
	allowed := filter.ParseAllowCountries("GB")
	lookup := geofeed.NewLookup(entries)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ip, country, ok := filter.FirstAllowed(lookup, ips, allowed, false)
		if !ok || country != "GB" || ip != ips[0] {
			b.Fatalf("unexpected result: %v %q %v", ip, country, ok)
		}
	}
}

func BenchmarkFirstAllowedIP_Miss(b *testing.B) {
	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: "DE"},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: "US"},
	}
	ips := []netip.Addr{
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("172.16.0.1"),
	}
	allowed := filter.ParseAllowCountries("GB")
	lookup := geofeed.NewLookup(entries)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, ok := filter.FirstAllowed(lookup, ips, allowed, false)
		if ok {
			b.Fatal("expected miss")
		}
	}
}

func BenchmarkFirstAllowedIP_ManyIPs(b *testing.B) {
	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: "DE"},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: "US"},
		{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Country: "GB"},
	}
	// 10 IPs, last one matches
	ips := make([]netip.Addr, 10)
	for i := range 9 {
		ips[i] = netip.MustParseAddr("10.0.0.1")
	}
	ips[9] = netip.MustParseAddr("192.0.2.10")
	allowed := filter.ParseAllowCountries("GB")
	lookup := geofeed.NewLookup(entries)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ip, country, ok := filter.FirstAllowed(lookup, ips, allowed, false)
		if !ok || country != "GB" || ip != ips[9] {
			b.Fatalf("unexpected result")
		}
	}
}

func BenchmarkAllIPsAllowed_AllMatch(b *testing.B) {
	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: "DE"},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: "US"},
	}
	ips := []netip.Addr{
		netip.MustParseAddr("198.51.100.1"),
		netip.MustParseAddr("203.0.113.5"),
	}
	allowed := filter.ParseAllowCountries("DE,US")
	lookup := geofeed.NewLookup(entries)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, ok := filter.FirstAllowed(lookup, ips, allowed, true)
		if !ok {
			b.Fatal("expected all allowed")
		}
	}
}

func BenchmarkAllIPsAllowed_OneFails(b *testing.B) {
	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: "DE"},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: "US"},
	}
	ips := []netip.Addr{
		netip.MustParseAddr("198.51.100.1"),
		netip.MustParseAddr("10.0.0.1"),
	}
	allowed := filter.ParseAllowCountries("DE")
	lookup := geofeed.NewLookup(entries)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, ok := filter.FirstAllowed(lookup, ips, allowed, true)
		if ok {
			b.Fatal("expected not all allowed")
		}
	}
}

// BenchmarkFilterCore benchmarks the core inner filtering logic (no network I/O)
// by calling the pure functions that Filter orchestrates.
func BenchmarkFilterCore(b *testing.B) {
	entries := []geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: "DE"},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Country: "US"},
		{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Country: "GB"},
	}
	var sb strings.Builder
	for i := range 20 {
		sb.WriteString("vless://uuid@node")
		sb.WriteByte(byte('A' + i%26))
		sb.WriteString(".example.com:443?security=tls#Node ")
		sb.WriteByte(byte('0' + i%10))
		sb.WriteString("\n")
	}
	body := []byte(sb.String())

	allowed := filter.ParseAllowCountries("DE,US")
	lookup := geofeed.NewLookup(entries)
	syntheticIPs := []netip.Addr{
		netip.MustParseAddr("198.51.100.42"),
		netip.MustParseAddr("203.0.113.10"),
	}

	b.ReportAllocs()
	b.ResetTimer()
	var output bytes.Buffer
	output.Grow(4096)

	for range b.N {
		output.Reset()
		first := true
		subscription.Parse(body, func(node subscription.Node) bool {
			chosenIP, chosenCountry, ok := filter.FirstAllowed(lookup, syntheticIPs, allowed, false)
			if !ok {
				return true
			}
			if !first {
				output.WriteByte('\n')
			}
			first = false
			rewrite.NodeName(&output, node, chosenCountry, chosenIP)
			return true
		})
		if output.Len() == 0 {
			b.Fatal("expected output")
		}
	}
}
