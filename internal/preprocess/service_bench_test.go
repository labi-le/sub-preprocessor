package preprocess_test

import (
	"net/netip"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/subscription"
)

func BenchmarkParseAllowCountries(b *testing.B) {
	countries := []string{"DE", "US", "JP", "GB", "FR", "  nl  "}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		allowed := preprocess.ParseAllowCountries(countries)
		if len(allowed) != 6 {
			b.Fatalf("unexpected count: %d", len(allowed))
		}
	}
}

func BenchmarkParseAllowCountries_Single(b *testing.B) {
	countries := []string{"DE"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		preprocess.ParseAllowCountries(countries)
	}
}

func BenchmarkRewriteNodeName(b *testing.B) {
	nodes, err := subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#Old Name"))
	if err != nil {
		b.Fatal(err)
	}
	node := nodes[0]
	ip := netip.MustParseAddr("198.51.100.10")
	country := "NL"
	var sb strings.Builder
	sb.Grow(256)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sb.Reset()
		preprocess.RewriteNodeName(&sb, node, country, ip)
	}
}

func BenchmarkRewriteNodeName_EmptyName(b *testing.B) {
	nodes, err := subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#"))
	if err != nil {
		b.Fatal(err)
	}
	node := nodes[0]
	ip := netip.MustParseAddr("203.0.113.5")
	country := "US"
	var sb strings.Builder
	sb.Grow(256)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sb.Reset()
		preprocess.RewriteNodeName(&sb, node, country, ip)
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
	allowed := preprocess.ParseAllowCountries([]string{"GB"})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ip, country, ok := preprocess.FilterAndFirstAllowed(entries, ips, allowed, false)
		if !ok || country != "GB" || ip.String() != "192.0.2.10" {
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
	allowed := preprocess.ParseAllowCountries([]string{"GB"})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, ok := preprocess.FilterAndFirstAllowed(entries, ips, allowed, false)
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
	allowed := preprocess.ParseAllowCountries([]string{"GB"})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ip, country, ok := preprocess.FilterAndFirstAllowed(entries, ips, allowed, false)
		if !ok || country != "GB" || ip.String() != "192.0.2.10" {
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
	allowed := preprocess.ParseAllowCountries([]string{"DE", "US"})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, ok := preprocess.FilterAndFirstAllowed(entries, ips, allowed, true)
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
	allowed := preprocess.ParseAllowCountries([]string{"DE"})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, ok := preprocess.FilterAndFirstAllowed(entries, ips, allowed, true)
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

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		allowed := preprocess.ParseAllowCountries([]string{"DE", "US"})
		nodes, err := subscription.Parse(body)
		if err != nil {
			b.Fatal(err)
		}
		// Simulate the inner filter loop with synthetic IPs (no DNS)
		// Each node gets the same IPs as if resolveIPv4 returned them
		syntheticIPs := []netip.Addr{
			netip.MustParseAddr("198.51.100.42"),
			netip.MustParseAddr("203.0.113.10"),
		}
		var output strings.Builder
		output.Grow(4096)
		first := true
		for _, node := range nodes {
			chosenIP, chosenCountry, ok := preprocess.FilterAndFirstAllowed(entries, syntheticIPs, allowed, false)
			if !ok {
				continue
			}
			if !first {
				output.WriteByte('\n')
			}
			first = false
			preprocess.RewriteNodeName(&output, node, chosenCountry, chosenIP)
		}
		_ = output.String()
	}
}
