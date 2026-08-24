package stable //nolint:testpackage // benchmarks unexported stable internals (probeNodes, parseLive)

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/netip"
	"testing"

	"github.com/rs/zerolog"

	"github.com/metacubex/mihomo/common/convert"
	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/rewrite"
)

// Package-level sinks keep the compiler from eliding benchmarked work.
var (
	benchEntriesSink   []Entry
	benchSurvSink      []Survivor
	benchBytesSink     []byte
	benchProxSink      []mihomo.Proxy
	benchProbeSink     map[string]ProbeResult
	benchProbeNodeSink []probeNode
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

// benchSSRLine renders a parseable ssr URI: six colon-separated head fields
// and a query, the whole thing base64, with the name in the "remarks" value.
func benchSSRLine(host, port, name string) string {
	b64 := base64.RawURLEncoding.EncodeToString
	payload := host + ":" + port + ":origin:aes-256-cfb:plain:" + b64([]byte("secret")) +
		"/?obfsparam=" + b64([]byte("obfs.example.com")) + "&remarks=" + b64([]byte(name))
	return "ssr://" + b64([]byte(payload))
}

// benchSourceBodies builds 4 sources of ~150 mixed vless/vmess lines each, with
// ~20% of lines reusing a shared server:port pool so ~20% collapse cross-source.
func benchSourceBodies() []SourceBody {
	return benchMixedSourceBodies(benchVmessLine)
}

// benchMixedSourceBodies is benchSourceBodies with the scheme of every 7th line
// left open: that slot decides which relabel path Merge takes for it (a payload
// rewrite for vmess and ssr, a #fragment for the vless rest). Node count,
// addresses and duplicate rate are held fixed so two Merge benchmarks over it
// differ in that one variable.
func benchMixedSourceBodies(everySeventh func(host, port, name string) string) []SourceBody {
	const perSource = 150
	names := []string{"alpha", "beta", "gamma", "delta"}
	bodies := make([]SourceBody, len(names))
	for si, name := range names {
		nodes := make([]preprocess.NodeResult, 0, perSource)
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
			line := benchVlessLine(host, port, nodeName)
			if i%7 == 0 {
				line = everySeventh(host, port, nodeName)
			}
			nodes = append(nodes, preprocess.NodeResult{
				Raw: line,
				IP:  netip.AddrFrom4([4]byte{10, 1, byte(si), byte(i)}),
			})
		}
		bodies[si] = SourceBody{Name: name, Nodes: nodes}
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
			Label: label,
			Raw:   benchVlessLine(fmt.Sprintf("10.1.%d.%d", i/256, i%256), "443", label),
			Addr:  fmt.Sprintf("10.1.%d.%d:443", i/256, i%256),
			IP:    netip.AddrFrom4([4]byte{10, 1, byte(i / 256), byte(i % 256)}),
		}
		if i%5 != 0 { // ~80% coverage
			res[label] = ProbeResult{Successes: 3, MeanMs: 40 + (i % 200)}
		}
	}
	return entries, res
}

// benchAnnotator prices the publication as production runs it: through
// rewrite.NodeName, over a tag run of the shape the configured chain emits.
type benchAnnotator struct{}

func (benchAnnotator) Annotate(
	_ context.Context, dst, _ *bytes.Buffer, req preprocess.AnnotateRequest,
) geofeed.CountryCode {
	code := geofeed.CountryCode{'D', 'E'}
	rewrite.NodeName(dst, req.Node, req.Prefix+"[GEO:"+code.String()+"][IP:"+req.IP.String()+"]")

	return code
}

// benchSurvivors builds ~300 survivors with ~80-byte URIs.
func benchSurvivors() []Survivor {
	const n = 300
	surv := make([]Survivor, n)
	for i := range n {
		raw := benchVlessLine(fmt.Sprintf("203.0.%d.%d", i/256, i%256), "443",
			fmt.Sprintf("alpha-%03d-published", i))
		surv[i] = Survivor{
			Entry:  Entry{Raw: raw, IP: netip.AddrFrom4([4]byte{203, 0, byte(i / 256), byte(i % 256)})},
			MeanMs: i,
			Mbps:   i,
		}
	}
	return surv
}

// benchParsePayload builds a ~300-node merged payload (entriesPayload shape) of
// parseable nodes for the parse benchmarks.
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

// BenchmarkMergeSSR is BenchmarkMerge with ssr where the vmess lines sit, so
// the difference between the two is what relabelling an ssr node costs against
// its vmess twin in the one place both actually run. Nothing measured it
// before: BenchmarkRewriteSSRName prices the rewriter alone, off the merge
// path. This is a floor, not a comparison.
func BenchmarkMergeSSR(b *testing.B) {
	bodies := benchMixedSourceBodies(benchSSRLine) // built once
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
	ctx := context.Background()
	ann := benchAnnotator{}
	b.ReportAllocs()
	for b.Loop() {
		benchBytesSink = BuildPayload(ctx, ann, survivors)
	}
}

// BenchmarkParseProxies prices the survivor-set parse (checker.go:464), which
// is the whole payload: those nodes are live by definition.
func BenchmarkParseProxies(b *testing.B) {
	prober, payload := benchParseProber(b), benchParsePayload()

	// Sanity check + fail loud if the payload isn't parseable.
	warm, err := prober.ParseProxies(payload)
	if err != nil {
		b.Fatal(err)
	}
	for _, px := range warm {
		_ = px.Close()
	}

	b.ReportAllocs()
	for b.Loop() {
		proxies, perr := prober.ParseProxies(payload)
		if perr != nil {
			b.Fatal(perr)
		}
		benchProxSink = proxies
		for _, px := range proxies {
			_ = px.Close()
		}
	}
}

// BenchmarkProbeParseCondemned prices what Probe now spends on the same payload
// at the production condemned share (benchCondemnedPercent): the converter, the
// pre-check's whole input, and adapter objects for the survivors alone. Against
// BenchmarkParseProxies over the same 300 nodes, the difference is the reorder.
//
// The condemned set is taken by stride rather than by dialling: a benchmark
// cannot own the network, and which positions are condemned does not change the
// work.
func BenchmarkProbeParseCondemned(b *testing.B) {
	prober, payload := benchParseProber(b), benchParsePayload()

	b.ReportAllocs()
	for b.Loop() {
		mappings, err := convert.ConvertsV2Ray(payload)
		if err != nil {
			b.Fatal(err)
		}
		nodes := probeNodes(mappings)
		live := benchSpareEvery(len(nodes), benchCondemnedPercent)
		live = prober.parseLive(mappings, nodes, live)
		if len(live) == 0 {
			b.Fatal("every spared position failed to parse")
		}
		benchProbeNodeSink = nodes
		for _, i := range live {
			_ = nodes[i].proxy.Close()
		}
	}
}

// benchCondemnedPercent is the NODE share the pre-check condemns on a healthy
// egress: stable_probe_outcome_nodes{stage="condemned"} over
// stable_probed_nodes. NOT precheckBreakerPercent's 58.9%, which is
// refused/judged over distinct ENDPOINTS -- PrecheckReport is explicit that the
// two are not interchangeable. Derivation, and what seeding this from the
// endpoint share cost: docs/guides/benchmarks.md.
const benchCondemnedPercent = 55.8

// benchSpareEvery returns the positions a pre-check condemning pct of n leaves
// live, spread evenly so no run of positions is either wholly parsed or wholly
// skipped.
func benchSpareEvery(n int, pct float64) []int {
	live := make([]int, 0, n)
	stride := 100 / (100 - pct)
	for i := range n {
		if float64(len(live)) < float64(i+1)/stride {
			live = append(live, i)
		}
	}

	return live
}

func benchParseProber(b *testing.B) *MihomoProber {
	b.Helper()

	p := testProberWith(&testing.T{}, config.CheckConfig{ExpectedStatus: "204"},
		config.BandwidthConfig{}, zerolog.Nop())

	return p
}

// foldProxy carries the two methods entryLabel reads, so this prices the
// probe's bookkeeping instead of mihomo's parser.
type foldProxy struct {
	mihomo.Proxy
	name string
}

func (p *foldProxy) Name() string             { return p.name }
func (p *foldProxy) Type() mihomo.AdapterType { return mihomo.Vless }

// BenchmarkFoldProbeResults prices the probe's per-node bookkeeping at the
// production pool size: the accumulators every round writes into, plus the
// result map the cycle carries from Probe into SelectSurvivors.
func BenchmarkFoldProbeResults(b *testing.B) {
	const n = 8817
	nodes := make([]probeNode, n)
	for i := range nodes {
		nodes[i] = probeNode{proxy: &foldProxy{name: fmt.Sprintf("alpha-%05d", i)}}
	}

	b.ReportAllocs()
	for b.Loop() {
		accs := make([]delayAcc, len(nodes))
		for i := range accs {
			accs[i] = delayAcc{succ: 2, sum: 300, stage: StagePassed}
		}
		benchProbeSink = foldProbeResults(nodes, accs)
	}
}
