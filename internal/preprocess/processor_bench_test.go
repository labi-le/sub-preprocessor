package preprocess_test

import (
	"bytes"
	"testing"

	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

func BenchmarkParseAllowed(b *testing.B) {
	countries := "DE,US,JP,GB,FR,  nl  "
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		allowed := filter.ParseAllowed(countries)
		if !allowed.Has(geofeed.CountryCode{'D', 'E'}) || !allowed.Has(geofeed.CountryCode{'N', 'L'}) {
			b.Fatalf("unexpected result")
		}
	}
}

func BenchmarkParseAllowed_Single(b *testing.B) {
	countries := "DE"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filter.ParseAllowed(countries)
	}
}

func BenchmarkRewriteNodeName(b *testing.B) {
	var nodes []subscription.Node
	subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#Old Name"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})
	node := nodes[0]
	const tags = "[GEO:NL][IP:198.51.100.10]"
	var sb bytes.Buffer
	sb.Grow(256)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sb.Reset()
		rewrite.NodeName(&sb, node, tags)
	}
}

func BenchmarkRewriteNodeName_EmptyName(b *testing.B) {
	var nodes []subscription.Node
	subscription.Parse([]byte("vless://uuid@example.com:443?security=tls#"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})
	node := nodes[0]
	const tags2 = "[GEO:US][IP:203.0.113.5]"
	var sb bytes.Buffer
	sb.Grow(256)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sb.Reset()
		rewrite.NodeName(&sb, node, tags2)
	}
}
