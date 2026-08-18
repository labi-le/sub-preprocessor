package rewrite_test

import (
	"bytes"
	"encoding/base64"
	"testing"

	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// BenchmarkNodeNamePayloadArms prices the two arms the fragment path's
// separate-write trick does not reach: both fold the display name into a base64
// payload, so both need it as one string. preprocess.BenchmarkRewriteNodeName
// covers the fragment path itself.
func BenchmarkNodeNamePayloadArms(b *testing.B) {
	const tags = "[GEO:NL][IP:198.51.100.10]"
	vmessLine := "vmess://" + base64.StdEncoding.EncodeToString(
		[]byte(`{"v":"2","ps":"Old Name","add":"192.0.2.1","port":"443","id":"uuid","net":"ws"}`))
	ssrPayload := "192.0.2.1:8388:origin:aes-256-cfb:plain:" + base64.RawURLEncoding.EncodeToString([]byte("secret")) +
		"/?remarks=" + base64.RawURLEncoding.EncodeToString([]byte("Old Name"))

	for _, tc := range []struct{ name, line string }{
		{"vmess", vmessLine},
		{"ssr", "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(ssrPayload))},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var node subscription.Node
			subscription.Parse([]byte(tc.line), func(n subscription.Node) bool {
				node = n

				return false
			})
			if node.Scheme == "" {
				b.Fatalf("no node parsed from %q", tc.line)
			}
			var buf bytes.Buffer
			buf.Grow(512)

			b.ReportAllocs()
			for b.Loop() {
				buf.Reset()
				rewrite.NodeName(&buf, node, tags)
			}
		})
	}
}

var sinkString string

func BenchmarkStripKnownTags(b *testing.B) {
	const tagged = "[GEO:FI][IP:1.2.3.4][SPD:20M] Real Node Name"
	const plain = "Plain Name"

	b.Run("tagged", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkString = rewrite.StripKnownTags(tagged)
		}
	})

	b.Run("untagged", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkString = rewrite.StripKnownTags(plain)
		}
	})
}
