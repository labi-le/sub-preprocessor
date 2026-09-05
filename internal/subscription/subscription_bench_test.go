package subscription_test

import (
	"encoding/base64"
	"strconv"
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

func vmessPayload(name string) string {
	return `{"v":"2","add":"1.2.3.4","port":"443","ps":"` + name +
		`","id":"b831381d-6324-4d53-ad4f-8cda48b30811","net":"ws"}`
}

// BenchmarkParse_Vmess prices the vmess decode under both port spellings the
// wild ships: "port":"443" quoted and "port":443 bare. The bare form used to
// pay a doomed json.Unmarshal — one heap &UnmarshalTypeError per node — before
// the number gate ran; the arms must stay close, or the gate has slipped back
// below the decoder.
func BenchmarkParse_Vmess(b *testing.B) {
	for _, tc := range []struct{ name, port string }{
		{"quoted", `"443"`},
		{"numeric", `443`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			payload := `{"v":"2","add":"1.2.3.4","port":` + tc.port +
				`,"ps":"Name","id":"b831381d-6324-4d53-ad4f-8cda48b30811","net":"ws"}`
			var sb strings.Builder
			for range benchNodes {
				sb.WriteString(vmessLine(payload))
				sb.WriteString("\n")
			}
			input := []byte(sb.String())
			nodes := 0
			if rejected := subscription.Parse(input, func(_ subscription.Node) bool {
				nodes++
				return true
			}); nodes != benchNodes || rejected != 0 {
				b.Fatalf("fixture: %d nodes, %d rejected; want %d, 0", nodes, rejected, benchNodes)
			}

			b.ReportAllocs()
			for b.Loop() {
				count := 0
				subscription.Parse(input, func(_ subscription.Node) bool {
					count++
					return true
				})
				sinkInt = count
			}
		})
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

// BenchmarkRewriteVmessName_UnicodeName prices the name shape sources actually
// publish. json.Marshal emits valid non-ASCII UTF-8 verbatim, so this must cost
// what the ASCII name above costs; a divergence means the splice fell back to
// marshalling.
func BenchmarkRewriteVmessName_UnicodeName(b *testing.B) {
	raw := vmessLine(vmessPayload("Name"))
	const newName = "[GEO:FI][IP:192.0.2.1] 🇫🇮 fast node (a)"
	b.ReportAllocs()
	for b.Loop() {
		out, _ := subscription.RewriteVmessName(raw, newName)
		sinkStr = out
	}
}

// The four benchmarks below are the first to execute the ss legacy, ssr and
// mierus decoders, so there is no earlier measurement to compare them with;
// they exist to fix a floor for the next change to this code.

// benchNodes is the payload size every Parse benchmark below measures, matching
// the 50-line inputs of the older ones above.
const benchNodes = 50

// ssLegacyBenchLine mirrors the pre-SIP002 form: the whole authority is
// unpadded std base64 of "method:password@host:port".
func ssLegacyBenchLine() string {
	return "ss://" + base64.RawStdEncoding.EncodeToString([]byte("aes-256-gcm:pass@1.2.3.4:8388")) + "#Name"
}

// ssrBenchLine mirrors a real ssr payload: six colon-separated head fields and
// a query whose values are themselves base64.
func ssrBenchLine(name string) string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	payload := "1.2.3.4:8388:origin:aes-256-cfb:plain:" + b64("secret") +
		"/?obfsparam=" + b64("obfs.example.com") + "&protoparam=" + b64("auth-token") +
		"&remarks=" + b64(name) + "&group=" + b64("grp")
	return "ssr://" + b64(payload)
}

// ssrWideBenchLine mirrors what a source can hand RewriteSSRName: a query of
// pairs distinct keys, descending, the worst order for a sort.
func ssrWideBenchLine(pairs int) string {
	var q strings.Builder
	q.Grow(pairs * len("k0000000=v&"))
	for i := pairs - 1; i >= 0; i-- {
		if q.Len() > 0 {
			q.WriteByte('&')
		}
		q.WriteString("k")
		q.WriteString(strconv.Itoa(1_000_000 + i))
		q.WriteString("=v")
	}
	payload := "1.2.3.4:8388:origin:aes-256-cfb:plain:" +
		base64.RawURLEncoding.EncodeToString([]byte("secret")) + "/?" + q.String()
	return "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// benchParseInput repeats line into a benchNodes-line payload and pins that
// every line of it parses. Parse's sink accepts a zero count, so without the
// check a fixture that stops parsing — a base64 encoding that lands a '/' in
// an ss authority and truncates it, a decoder narrowed by a later change —
// silently turns the benchmark into a measurement of benchNodes REJECTIONS and
// reports the faster number as an improvement.
func benchParseInput(b *testing.B, line string) []byte {
	b.Helper()

	var sb strings.Builder
	for range benchNodes {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	input := []byte(sb.String())

	nodes := 0
	rejected := subscription.Parse(input, func(subscription.Node) bool {
		nodes++
		return true
	})
	if nodes != benchNodes || rejected != 0 {
		b.Fatalf("fixture %q: %d nodes, %d rejected; want %d, 0", line, nodes, rejected, benchNodes)
	}

	return input
}

func BenchmarkParse_SSLegacy(b *testing.B) {
	input := benchParseInput(b, ssLegacyBenchLine())
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

func BenchmarkParse_SSR(b *testing.B) {
	input := benchParseInput(b, ssrBenchLine("Tokyo Node"))
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

func BenchmarkParse_Mieru(b *testing.B) {
	input := benchParseInput(b, "mierus://user:pass@1.2.3.4?port=2999&port=3000&protocol=TCP&protocol=UDP#Mieru")
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

// BenchmarkRewriteSSRName is the ssr twin of BenchmarkRewriteVmessName: both
// run once per published node on the annotated "/" path. What ssr adds is a
// base64 decode, a query parse, a query encode and a base64 encode; measured
// 2026-08-18 that is 5 allocs / 656 B against the vmess splice's 3 / 448, the
// extra one being the base64 remarks the vmess form has no equivalent of.
func BenchmarkRewriteSSRName(b *testing.B) {
	raw := ssrBenchLine("Tokyo Node")
	const newName = "[GEO:FI][IP:1.2.3.4] mifa-001"
	b.ReportAllocs()
	for b.Loop() {
		out, _ := subscription.RewriteSSRName(raw, newName)
		sinkStr = out
	}
}

// The five benchmarks below measure a whole SOURCE BODY through the seam the
// /stable.txt worker drives it through — Normalize then Parse — rather than one
// node at a time. Their shapes and sizes come from fetching all 163 configured
// sources on 2026-08-14: 157 answered non-empty, 20.51 MB in total over 72568
// nodes, median body 38 KB, p90 173 KB, largest 3.06 MB; by shape 79 bodies
// were base64-wrapped (3.16 MB), 44 plain URI lists (16.93 MB) and 25 Xray JSON
// (0.41 MB). Every one of the 79 base64 bodies arrived with NO whitespace, so
// stripWhitespace's copying branch is unreached in production.
//
// The 10 MiB pair is maxSubscriptionSize, which no configured source comes near:
// it bounds what one runaway source can cost, and it is where a growth chain
// hurts most.
const (
	benchMedianBody = 38 << 10
	benchLargeBody  = 10 << 20
	benchSources    = 157

	// benchXrayOutbounds is the 160 proxy outbounds the measured JSON links
	// carried, and it names BenchmarkNormalizeParse_XrayJSON160Outbounds.
	benchXrayOutbounds = 160
)

// benchURIList builds an already-normalized vless body of at least size bytes,
// at the corpus's measured 267 B/node.
func benchURIList(size int) []byte {
	var sb strings.Builder
	sb.Grow(size + 512)
	for i := 0; sb.Len() < size; i++ {
		sb.WriteString("vless://b831381d-6324-4d53-ad4f-8cda48b30811@node")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(".example.com:443?security=reality&sni=www.example.org")
		sb.WriteString("&fp=chrome&pbk=UO3EObgU3xUrhIGEE0gfCn5ZOz8YxNcwwW6ZaYzD3SA")
		sb.WriteString("&sid=4e9b0c2d1a3f5768&type=tcp&flow=xtls-rprx-vision#Node ")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString("\n")
	}
	return []byte(sb.String())
}

// benchBase64Body wraps a URI list so the ENCODED body is about size bytes,
// which is what the fetch actually carries.
func benchBase64Body(size int) []byte {
	raw := benchURIList(size * 3 / 4)
	out := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(out, raw)
	return out
}

// benchXrayJSON builds the Hiddify-style document Normalize has to convert:
// outbounds vless configs of the reality-over-tcp shape 85 of one measured
// post's 158 outbounds had.
func benchXrayJSON(outbounds int) []byte {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range outbounds {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strings.Replace(
			strings.TrimSuffix(strings.TrimPrefix(realityTCP, "["), "]"),
			"node-a", "node-"+strconv.Itoa(i), 1,
		))
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

// benchXrayHysteria2JSON is the hysteria2 shape 35 of one channel's 385 measured
// outbounds had. Its converter writes a userinfo credential, an alpn list and a
// name the vless fixture never reaches — the address, since every config calls
// its outbound "proxy".
func benchXrayHysteria2JSON(outbounds int) []byte {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range outbounds {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strings.ReplaceAll(hysteriaV2, "popa.example.ru", "node-"+strconv.Itoa(i)+".example"))
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

// benchXrayEscapedNameJSON names each node the way sources actually do — a flag,
// spaces and parentheses — so the fragment escaper is measured on a name that
// needs escaping rather than on the fixture's escape-free "node-N".
func benchXrayEscapedNameJSON(outbounds int) []byte {
	body := benchXrayJSON(outbounds)
	return []byte(strings.ReplaceAll(string(body), `"remarks":"node-`, `"remarks":"🇫🇮 fast node (a) `))
}

func benchNormalizeParse(b *testing.B, body []byte) {
	b.Helper()
	b.ReportAllocs()
	for b.Loop() {
		count := 0
		subscription.Parse(subscription.Normalize(body), func(subscription.Node) bool {
			count++
			return true
		})
		if count == 0 {
			b.Fatal("fixture parsed no nodes")
		}
		sinkInt = count
	}
}

func BenchmarkNormalizeParse_URIList10MiB(b *testing.B) {
	benchNormalizeParse(b, benchURIList(benchLargeBody))
}

func BenchmarkNormalizeParse_Base64Wrapped10MiB(b *testing.B) {
	benchNormalizeParse(b, benchBase64Body(benchLargeBody))
}

func BenchmarkNormalizeParse_MedianBody38KiB(b *testing.B) {
	benchNormalizeParse(b, benchURIList(benchMedianBody))
}

func BenchmarkNormalizeParse_XrayJSON160Outbounds(b *testing.B) {
	benchNormalizeParse(b, benchXrayJSON(benchXrayOutbounds))
}

func BenchmarkNormalizeParse_XrayJSONHysteria2(b *testing.B) {
	benchNormalizeParse(b, benchXrayHysteria2JSON(benchXrayOutbounds))
}

func BenchmarkNormalizeParse_XrayJSONEscapedNames(b *testing.B) {
	benchNormalizeParse(b, benchXrayEscapedNameJSON(benchXrayOutbounds))
}

// BenchmarkNormalizeParse_ManySmallSources is one cycle's worth of the median
// shape: 157 bodies of 38 KB, so the per-BODY costs (the JSON sniff, the "://"
// scan, the base64 decision) are measured at the count a cycle pays them.
func BenchmarkNormalizeParse_ManySmallSources(b *testing.B) {
	bodies := make([][]byte, benchSources)
	for i := range bodies {
		bodies[i] = benchURIList(benchMedianBody)
	}
	b.ReportAllocs()
	for b.Loop() {
		count := 0
		for _, body := range bodies {
			subscription.Parse(subscription.Normalize(body), func(subscription.Node) bool {
				count++
				return true
			})
		}
		sinkInt = count
	}
}

// benchMaxQueryParams is net/url's defaultMaxParams, the segment ceiling
// queryList.parse mirrors: the widest query an accepted ssr payload can carry.
const benchMaxQueryParams = 10000

// BenchmarkRewriteSSRName_MaxParams bounds what one crafted published node
// costs, the figure the insertion sort this replaced made quadratic. It runs
// LAST because its 1.8 MB/op moved whatever followed it by +5% (measured
// 2026-08-18 against the xray bodies) through the heap it leaves behind.
func BenchmarkRewriteSSRName_MaxParams(b *testing.B) {
	raw := ssrWideBenchLine(benchMaxQueryParams)
	const newName = "[GEO:FI][IP:1.2.3.4] mifa-001"
	if _, ok := subscription.RewriteSSRName(raw, newName); !ok {
		b.Fatal("fixture must rewrite")
	}
	b.ReportAllocs()
	for b.Loop() {
		out, _ := subscription.RewriteSSRName(raw, newName)
		sinkStr = out
	}
}
