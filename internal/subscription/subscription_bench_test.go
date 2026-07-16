package subscription_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// largeNormalizeInput creates a large base64 input to stress Normalize allocations.
func largeNormalizeInput() []byte {
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("vless://uuid@node")
		sb.WriteByte(byte('A' + i%26))
		sb.WriteString(".example.com:443?security=tls#Node ")
		sb.WriteByte(byte('0' + i%10))
		sb.WriteString("\n")
	}
	raw := sb.String()
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(encoded, []byte(raw))
	return encoded
}

func BenchmarkNormalize_AlreadyParsed(b *testing.B) {
	input := []byte("vless://uuid@example.com:443?security=tls#Example")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = subscription.Normalize(input)
	}
}

func BenchmarkNormalize_Base64Small(b *testing.B) {
	input := []byte(base64.StdEncoding.EncodeToString([]byte("vless://uuid@example.com:443?security=tls#Node 1\n")))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		subscription.Normalize(input)
	}
}

func BenchmarkNormalize_Base64Large(b *testing.B) {
	input := largeNormalizeInput()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		subscription.Normalize(input)
	}
}

func BenchmarkNormalize_Base64Dirty(b *testing.B) {
	input := []byte("  \n\t" + base64.StdEncoding.EncodeToString([]byte("vless://uuid@example.com:443?security=tls#Node 1\n")) + "\n\t  ")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		subscription.Normalize(input)
	}
}

func BenchmarkParse_SingleNode(b *testing.B) {
	input := []byte("vless://uuid@example.com:443?security=tls#Example")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		subscription.Parse(input, func(_ subscription.Node) bool {
			return true
		})
	}
}

func BenchmarkParse_MultiNode(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("vless://uuid@node")
		sb.WriteByte(byte('A' + i%26))
		sb.WriteString(".example.com:443?security=tls#Node ")
		sb.WriteByte(byte('0' + i%10))
		sb.WriteString("\n")
	}
	input := []byte(sb.String())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		subscription.Parse(input, func(_ subscription.Node) bool {
			return true
		})
	}
}

func BenchmarkParse_SkipsNonURILines(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("some-other-proto-node\n")
	}
	input := []byte(sb.String())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		subscription.Parse(input, func(_ subscription.Node) bool {
			return true
		})
	}
}

var (
	sinkStr string
	sinkInt int
)

// vmessPayload builds the base64 JSON payload for a vmess node with display name.
func vmessPayload(name string) string {
	return `{"v":"2","add":"1.2.3.4","port":"443","ps":"` + name +
		`","id":"b831381d-6324-4d53-ad4f-8cda48b30811","net":"ws"}`
}

func BenchmarkParse_Vmess(b *testing.B) {
	var sb strings.Builder
	for range 50 {
		sb.WriteString(vmessLine(vmessPayload("Name")))
		sb.WriteString("\n")
	}
	input := []byte(sb.String())
	b.ReportAllocs()
	for b.Loop() {
		count := 0
		subscription.Parse(input, func(_ subscription.Node) bool {
			count++
			return true
		})
		sinkInt = count
	}
}

func BenchmarkRewriteVmessName(b *testing.B) {
	raw := vmessLine(vmessPayload("Name"))
	const newName = "[GEO:FI][IP:1.2.3.4] mifa-001"
	b.ReportAllocs()
	for b.Loop() {
		out, _ := subscription.RewriteVmessName(raw, newName)
		sinkStr = out
	}
}
