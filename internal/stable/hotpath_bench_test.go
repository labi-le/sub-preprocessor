package stable //nolint:testpackage // benchmarks unexported stable internals (parseProxies)

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/config"
)

// Package-level sinks keep the compiler from eliding benchmarked work.
var (
	benchEntriesSink []Entry
	benchSurvSink    []Survivor
	benchBytesSink   []byte
	benchProxSink    []mihomo.Proxy
)

const benchUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

// benchVlessLine renders a parseable vless URI for host:port with a #fragment name.
func benchVlessLine(host, port, name string) string {
	return fmt.Sprintf("vless://%s@%s:%s?encryption=none&security=none&type=tcp#%s", //nolint:nosprintfhostport // synthetic bench URI, not a network dial
		benchUUID, host, port, name)
}

// benchVmessLine renders a parseable vmess URI (name lives in the base64 ps field).
func benchVmessLine(host, port, name string) string {
	node := fmt.Sprintf(`{"v":"2","ps":%q,"add":%q,"port":%q,`+
		`"id":%q,"aid":"0","net":"tcp","type":"none","tls":"","scy":"auto"}`,
		name, host, port, benchUUID)
	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(node))
}

// benchSourceBodies builds 4 sources of ~150 mixed vless/vmess lines each, with
// ~20% of lines reusing a shared server:port pool so ~20% collapse cross-source.
func benchSourceBodies() []SourceBody {
	const perSource = 150
	names := []string{"alpha", "beta", "gamma", "delta"}
	bodies := make([]SourceBody, len(names))
	for si, name := range names {
		var sb strings.Builder
		for i := range perSource {
			var host, port string
			if i%5 == 0 {
				// Shared address pool -> duplicate server:port across sources.
				host = fmt.Sprintf("10.0.%d.%d", i/256, i%256)
				port = "443"
			} else {
				host = fmt.Sprintf("192.168.%d.%d", si, i)
				port = "443"
			}
			nodeName := fmt.Sprintf("%s node %d", name, i)
			if i%7 == 0 {
				sb.WriteString(benchVmessLine(host, port, nodeName))
			} else {
				sb.WriteString(benchVlessLine(host, port, nodeName))
			}
			sb.WriteByte('\n')
		}
		bodies[si] = SourceBody{Name: name, Body: []byte(sb.String())}
	}
	return bodies
}

// benchSelectData builds ~500 entries with a res map covering ~80% of labels
// with varied mean delays.
func benchSelectData() ([]Entry, map[string]ProbeResult) {
	const n = 500
	entries := make([]Entry, n)
	res := make(map[string]ProbeResult, n)
	for i := range n {
		label := fmt.Sprintf("alpha-%03d", i)
		entries[i] = Entry{
			Label:  label,
			Raw:    benchVlessLine(fmt.Sprintf("10.1.%d.%d", i/256, i%256), "443", label),
			Tagged: benchVlessLine(fmt.Sprintf("10.1.%d.%d", i/256, i%256), "443", label),
			Addr:   fmt.Sprintf("10.1.%d.%d:443", i/256, i%256),
		}
		if i%5 != 0 { // ~80% coverage
			res[label] = ProbeResult{Successes: 3, MeanMs: 40 + (i % 200)}
		}
	}
	return entries, res
}

// benchSurvivors builds ~300 survivors with ~80-byte Tagged URIs.
func benchSurvivors() []Survivor {
	const n = 300
	surv := make([]Survivor, n)
	for i := range n {
		tagged := benchVlessLine(fmt.Sprintf("203.0.%d.%d", i/256, i%256), "443",
			fmt.Sprintf("alpha-%03d-published", i))
		surv[i] = Survivor{Entry: Entry{Tagged: tagged}, MeanMs: i, Mbps: i}
	}
	return surv
}

// benchParsePayload builds a ~300-node merged payload (entriesPayload shape) of
// parseable nodes for parseProxies.
func benchParsePayload() []byte {
	const n = 300
	entries := make([]Entry, n)
	for i := range n {
		label := fmt.Sprintf("alpha-%03d", i)
		host := fmt.Sprintf("127.0.%d.%d", i/256, (i%256)+1)
		if i%7 == 0 {
			entries[i] = Entry{Raw: benchVmessLine(host, "10086", label)}
		} else {
			entries[i] = Entry{Raw: benchVlessLine(host, "443", label)}
		}
	}
	return entriesPayload(entries)
}

func BenchmarkMerge(b *testing.B) {
	bodies := benchSourceBodies() // built once
	b.ReportAllocs()
	for b.Loop() {
		benchEntriesSink = Merge(bodies)
	}
}

func BenchmarkSelectSurvivors(b *testing.B) {
	entries, res := benchSelectData()
	b.ReportAllocs()
	for b.Loop() {
		benchSurvSink = SelectSurvivors(entries, res, 3, 1, 300)
	}
}

func BenchmarkBuildPayload(b *testing.B) {
	survivors := benchSurvivors()
	b.ReportAllocs()
	for b.Loop() {
		benchBytesSink = BuildPayload(survivors)
	}
}

func BenchmarkParseProxies(b *testing.B) {
	prober, err := NewMihomoProber(
		config.CheckConfig{ExpectedStatus: "204"},
		config.GeminiConfig{}, "", config.ClaudeConfig{}, zerolog.Nop(),
	)
	if err != nil {
		b.Fatal(err)
	}
	payload := benchParsePayload()

	// Sanity check + fail loud if the payload isn't parseable.
	warm, err := prober.parseProxies(payload)
	if err != nil {
		b.Fatal(err)
	}
	for _, px := range warm {
		_ = px.Close()
	}

	b.ReportAllocs()
	for b.Loop() {
		proxies, perr := prober.parseProxies(payload)
		if perr != nil {
			b.Fatal(perr)
		}
		benchProxSink = proxies
		for _, px := range proxies {
			_ = px.Close()
		}
	}
}
