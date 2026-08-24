package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// vmessPayload builds a one-node vmess subscription payload on a port nothing
// listens on, so the test never touches the network — and so the reachability
// pre-check condemns it before any URL test.
func vmessPayload(t *testing.T) []byte {
	t.Helper()

	return vmessPayloadAt(t, deadTCPAddr(t))
}

func vmessPayloadAt(t *testing.T, addr string) []byte {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	node := `{"v":"2","ps":"node","add":"` + host + `","port":"` + port + `",` +
		`"id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"0","net":"tcp","type":"none","tls":"","scy":"auto"}`

	return []byte("vmess://" + base64.StdEncoding.EncodeToString([]byte(node)) + "\n")
}

// liveTCPAddr returns an address accepting TCP for the test's lifetime. It
// accepts and never answers, which is all the pre-check asks of it.
func liveTCPAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	return ln.Addr().String()
}

// deadTCPAddr claims a free port and releases it, so the address is one the
// kernel refuses rather than one that hangs.
func deadTCPAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	return addr
}

// probeNodesOf wraps already-parsed proxies as the positions the fold walks,
// for a test that starts from proxies rather than from a payload.
func probeNodesOf(pxs []mihomo.Proxy) []probeNode {
	nodes := make([]probeNode, len(pxs))
	for i, px := range pxs {
		nodes[i] = probeNode{proxy: px}
	}

	return nodes
}

// precheckNodes are the positions filterReachable reads, one per address, all
// of them TCP-dialled unless the caller says otherwise.
func precheckNodes(addrs ...string) []probeNode {
	nodes := make([]probeNode, len(addrs))
	for i, addr := range addrs {
		nodes[i] = probeNode{addr: addr, tcpServer: true}
	}

	return nodes
}

// refusedAddrs returns n DISTINCT addresses the kernel refuses. Every
// 127.0.0.0/8 address is local on Linux and port 1 is privileged, so no
// concurrent test can bind one and turn a refusal into a connection. Calling
// deadTCPAddr n times promises neither: the kernel may hand back a port it just
// released, and a collapsed dial set would make the breaker's SAMPLE SIZE the
// variable under test instead of its threshold.
func refusedAddrs(n int) []string {
	addrs := make([]string, n)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("127.0.1.%d:1", i+1)
	}

	return addrs
}

// precheckProber builds a prober whose check.timeout is the only field the
// pre-check reads, since precheckDialBudget derives from it. One second leaves
// the derived 500ms per attempt, ample for the loopback dials these tests make.
func precheckProber(t *testing.T) *MihomoProber {
	t.Helper()

	return testProberWith(t, config.CheckConfig{Timeout: time.Second, ExpectedStatus: "204"},
		config.BandwidthConfig{}, zerolog.Nop())
}

// accsByName maps the name-keyed accumulators a test writes onto the positional
// slice foldProbeResults reads, so no test has to know the order mihomo expands
// a mierus:// port list in. An unnamed proxy is fatal: a silent zero would look
// like a node that answered nothing.
func accsByName(t *testing.T, pxs []mihomo.Proxy, want map[string]delayAcc) []delayAcc {
	t.Helper()

	accs := make([]delayAcc, len(pxs))
	for i, px := range pxs {
		a, named := want[px.Name()]
		if !named {
			t.Fatalf("no accumulator for proxy %q; mihomo's naming changed", px.Name())
		}
		accs[i] = a
	}

	return accs
}

func TestProbeCancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	p := testProberWith(t, config.CheckConfig{
		Rounds:         2,
		Concurrency:    1,
		Timeout:        time.Second,
		TestURL:        "http://127.0.0.1:0/",
		ExpectedStatus: "204",
	}, config.BandwidthConfig{}, zerolog.Nop())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, probeErr := p.Probe(ctx, vmessPayload(t))
	if probeErr == nil {
		t.Fatal("a cancelled probe must be an error, not a truncated success")
	}
	if !errors.Is(probeErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", probeErr)
	}
	if res != nil {
		t.Fatalf("result map must be discarded on cancellation, got %v", res)
	}
}

func TestProbeZeroConcurrencyDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	// NewMihomoProber is exported and validates nothing but expected_status, so
	// a hand-built prober can carry Concurrency 0 — the residual case
	// fanoutSem's doc comment names. runRound acquires the semaphore on the
	// producer goroutine before any releasing worker exists, so an unbuffered
	// channel blocks forever. The failure mode is a HANG, not a wrong value,
	// hence the deadline + done channel: without them this wedges the suite.
	//
	// The node needs a LIVE listener: a condemned one never reaches runRound,
	// and this test would then pass with the semaphore unbounded.
	p := testProberWith(t, config.CheckConfig{
		Rounds:         1,
		Concurrency:    0,
		Timeout:        50 * time.Millisecond,
		TestURL:        "http://127.0.0.1:0/",
		ExpectedStatus: "204",
	}, config.BandwidthConfig{}, zerolog.Nop())
	payload := vmessPayloadAt(t, liveTCPAddr(t))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Deliberately not the deadline context: the blocked send ignores
		// cancellation, so only a bounded semaphore can let this return.
		_, _ = p.Probe(context.Background(), payload)
	}()

	timeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-done:
	case <-timeout.Done():
		t.Fatal("Probe deadlocked with concurrency 0; the fan-out semaphore must be normalised to >= 1")
	}
}

// vlessXHTTPLine renders a vless share link in xhttp mode. security=tls is
// load-bearing: convert only copies ?alpn= into the mapping under tls or
// reality (common/convert/v.go:29-38), so the same alpn without it reaches
// mihomo as an empty list.
func vlessXHTTPLine(addr, security, alpn, name string) string {
	q := "encryption=none&type=xhttp&security=" + security
	if alpn != "" {
		q += "&alpn=" + url.QueryEscape(alpn)
	}

	return "vless://" + benchUUID + "@" + addr + "?" + q + "#" + name
}

// testProbeNodes derives the probe positions for a payload exactly as Probe
// does, converter included.
func testProbeNodes(t *testing.T, payload string) []probeNode {
	t.Helper()

	mappings, err := convert.ConvertsV2Ray([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}

	return probeNodes(mappings)
}

// dialsAddrOverTCP is dialsServerOverTCP keyed on the PARSED adapter type, the
// form the verdict took while the parse still preceded the pre-check. It is the
// authority the mapping-keyed switch is checked against, so a mistyped dispatch
// key cannot condemn a node that reaches its server over UDP.
func dialsAddrOverTCP(typ mihomo.AdapterType, mapping map[string]any) bool {
	switch typ { //nolint:exhaustive // mirrors dialsServerOverTCP's default
	case mihomo.Vless, mihomo.Vmess, mihomo.Trojan, mihomo.Shadowsocks,
		mihomo.ShadowsocksR, mihomo.Socks5, mihomo.Http, mihomo.AnyTLS:
		return !dialsServerOverQUIC(mapping)
	default:
		return false
	}
}

// assertNodesMatchTheAdapter checks the reorder's whole premise: what probeNodes
// reads off a mapping is what the adapter it may now skip would have answered.
// An unreadable endpoint is not a defect but must cost the speedup, which the
// verdict comparison is what catches.
func assertNodesMatchTheAdapter(t *testing.T, payload string, nodes []probeNode) {
	t.Helper()

	mappings, err := convert.ConvertsV2Ray([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != len(nodes) {
		t.Fatalf("converted %d mappings for %d positions", len(mappings), len(nodes))
	}
	for i, mapping := range mappings {
		px, parseErr := adapter.ParseProxy(mapping)
		if parseErr != nil {
			t.Fatalf("ParseProxy(%v): %v", mapping, parseErr)
		}
		t.Cleanup(func() { _ = px.Close() })
		if got := nodes[i].label; got != entryLabel(px) {
			t.Errorf("position %d: label = %q, the adapter answers %q", i, got, entryLabel(px))
		}
		if got := nodes[i].addr; got != "" && got != px.Addr() {
			t.Errorf("position %d: addr = %q, the adapter dials %q", i, got, px.Addr())
		}
		if got, want := nodes[i].tcpServer, dialsAddrOverTCP(px.Type(), mapping); got != want {
			t.Errorf("position %d %v: tcp verdict = %v, want %v", i, px.Type(), got, want)
		}
	}
}

// The TCP verdict must come from the transport the raw mapping selects, not
// from the adapter type: one vless mapping carries both an ordinary TCP dial
// and xhttp's QUIC mode, where the only dial is ListenPacket over UDP and the
// TCP closure is never invoked. Condemning such a node costs it a URL test, a
// zero-success fold and deadcache.ttl in the dead cache.
//
// Going through the converter rather than calling the predicate directly is
// what proves the two keys survive convert, and the adapter comparison that
// every line here is one mihomo really parses.
func TestProbeNodesKeyTheTCPVerdictOnTheTransport(t *testing.T) {
	t.Parallel()

	const h, p = "203.0.113.7", "443"
	addr := net.JoinHostPort(h, p)
	for _, c := range []struct {
		desc string
		line string
		want bool
	}{
		{"xhttp with alpn exactly h3 dials only QUIC", vlessXHTTPLine(addr, "tls", "h3", "n1"), false},
		// Both orders, because mihomo's test is len(alpn) == 1, not "h3 is in
		// there" and not "h3 comes first".
		{"xhttp with h3 last among others stays on TCP", vlessXHTTPLine(addr, "tls", "h2,h3", "n2"), true},
		{"xhttp with h3 first among others stays on TCP", vlessXHTTPLine(addr, "tls", "h3,h2", "n10"), true},
		{"xhttp with a single non-h3 alpn stays on TCP", vlessXHTTPLine(addr, "tls", "h2", "n11"), true},
		{"xhttp without alpn stays on TCP", vlessXHTTPLine(addr, "tls", "", "n3"), true},
		// No tls, so convert drops the alpn and mihomo's h3 test cannot match.
		{"xhttp with h3 but no tls stays on TCP", vlessXHTTPLine(addr, "none", "h3", "n4"), true},
		{"h3 alpn outside xhttp is not the QUIC mode", strings.Replace(
			vlessXHTTPLine(addr, "tls", "h3", "n5"), "type=xhttp", "type=tcp", 1,
		), true},
		{"plain vless", benchVlessLine(h, p, "n6"), true},
		{"vmess", benchVmessLine(h, p, "n7"), true},
		{"anytls", "anytls://pass@" + addr + "#n8", true},
		{"hysteria2 reaches its server over UDP", "hy2://auth@" + addr + "#n9", false},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			nodes := testProbeNodes(t, c.line)
			if len(nodes) != 1 {
				t.Fatalf("converted %d mappings, want 1", len(nodes))
			}
			if nodes[0].tcpServer != c.want {
				t.Errorf("%q: tcp verdict = %v, want %v", nodes[0].label, nodes[0].tcpServer, c.want)
			}
			assertNodesMatchTheAdapter(t, c.line, nodes)
		})
	}
}

// numericPortVmessLine renders the vmess link a standard v2rayN client emits,
// whose base64 JSON carries "port" as a NUMBER: convert copies that value
// straight out of a json.Decoder (common/convert/converter.go:275), so the
// mapping holds a float64 where every other scheme holds a string.
func numericPortVmessLine(host string, port int, name string) string {
	node := fmt.Sprintf(`{"v":"2","ps":%q,"add":%q,"port":%d,`+
		`"id":%q,"aid":"0","net":"tcp","type":"none","tls":"","scy":"auto"}`,
		name, host, port, benchUUID)

	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(node))
}

// mappingPort decodes three shapes and the corpus drove one: every vmess
// fixture writes the port as a plain decimal string, so neither the float64
// branch the v2rayN link takes nor the base prefix ParseInt's base 0 accepts
// reached a test. Both feed the endpoint the pre-check DIALS, and tcpServer
// stays true either way, so a truncation or a wrong base condemns a live node
// unprobed and dead-caches it for deadcache.ttl.
func TestProbeNodesReadEveryPortShapeMihomoDoes(t *testing.T) {
	t.Parallel()

	const h = "203.0.113.11"
	for _, c := range []struct {
		desc string
		line string
		want string
	}{
		{"port as a JSON number", numericPortVmessLine(h, 443, "float-port"), net.JoinHostPort(h, "443")},
		// mihomo reads an octal prefix, so 0755 is port 493 for both sides.
		{"port with a base prefix", benchVlessLine(h, "0755", "octal-port"), net.JoinHostPort(h, "493")},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			nodes := testProbeNodes(t, c.line)
			if len(nodes) != 1 {
				t.Fatalf("converted %d mappings, want 1", len(nodes))
			}
			if nodes[0].addr != c.want {
				t.Errorf("addr = %q, want %q", nodes[0].addr, c.want)
			}
			assertNodesMatchTheAdapter(t, c.line, nodes)
		})
	}
}

// The third shape, a top-level int, is one mihomo's decoder accepts
// (common/structure/structure.go:135) and no payload can drive here: convert
// emits an int port for the mieru port list alone (converter.go:682), and
// dialsServerOverTCP does not judge mieru, so probeAddr is never called for it.
// It stays because adding a TCP-dialled type to that switch would make the
// branch live in one line, and the mapping is built by hand to keep the same
// authority over it.
func TestProbeAddrReadsAnIntPortLikeTheAdapter(t *testing.T) {
	t.Parallel()

	mapping := map[string]any{
		"name": "int-port", "type": "vmess", "server": "203.0.113.12",
		"port": 8443, "uuid": benchUUID, "alterId": 0, "cipher": "auto",
	}
	addr, addressable := probeAddr(mapping)
	if !addressable {
		t.Fatal("probeAddr refused a mapping the adapter reads")
	}
	px, err := adapter.ParseProxy(mapping)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = px.Close() })
	if addr != px.Addr() {
		t.Errorf("addr = %q, the adapter dials %q", addr, px.Addr())
	}
}

// Positions are the CONVERTER's, so a mapping mihomo refuses holds its own slot
// rather than shifting its neighbours: misalign them and the QUIC-only node
// inherits the verdict beside it and is condemned unprobed.
func TestProbeNodesKeepPositionsAcrossAnUnparsableNode(t *testing.T) {
	t.Parallel()

	const h, p = "203.0.113.9", "443"
	addr := net.JoinHostPort(h, p)
	payload := strings.Join([]string{
		benchVlessLine(h, p, "tcp-first"),
		// convert emits a mapping for this and adapter.ParseProxy then refuses
		// it: "unknown method: nope".
		"ss://nope:pass@" + addr + "#unparsable",
		vlessXHTTPLine(addr, "tls", "h3", "quic-only"),
	}, "\n")

	nodes := testProbeNodes(t, payload)
	want := []probeNode{
		{label: "tcp-first", addr: addr, tcpServer: true},
		{label: "unparsable", addr: addr, tcpServer: true},
		// No endpoint: the pre-check will not dial this position, so probeNodes
		// never derives one.
		{label: "quic-only"},
	}
	if len(nodes) != len(want) {
		t.Fatalf("converted %d mappings, want %d", len(nodes), len(want))
	}
	for i, w := range want {
		if nodes[i] != w {
			t.Errorf("position %d = %+v, want %+v", i, nodes[i], w)
		}
	}
}

// A proxy the pre-check may not judge must survive a refused dial on its OWN
// address: Merge dedupes on server:port, which does not stop a QUIC-only node
// from sharing an address with a TCP one, so the verdict cannot be looked up by
// address alone.
func TestFilterReachableSparesWhatItMayNotJudge(t *testing.T) {
	t.Parallel()

	dead := deadTCPAddr(t)
	nodes := precheckNodes(liveTCPAddr(t), dead, dead, dead)
	nodes[2].tcpServer = false

	live, condemned := precheckProber(t).
		filterReachable(context.Background(), zerolog.Nop(), nodes)
	if want := []int{0, 2}; !slices.Equal(live, want) {
		t.Errorf("live = %v, want %v", live, want)
	}
	// The condemned set is what seeds StageCondemned, so a position silently
	// dropped from BOTH slices would lose its dead-cache verdict.
	if want := []int{1, 3}; !slices.Equal(condemned, want) {
		t.Errorf("condemned = %v, want %v", condemned, want)
	}
}

// The pre-check earns its place only by not spending check.timeout on a node it
// condemned, so the assertion is the ABSENCE of a url-test event: the result
// map cannot show it, because both nodes fold to a zero-success entry either
// way. The stage is what tells them apart afterwards.
func TestProbeNeverURLTestsACondemnedNode(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name      string
		addr      string
		urlTested bool
		wantStage ProbeStage
	}{
		{name: "condemned", addr: deadTCPAddr(t), urlTested: false, wantStage: StageCondemned},
		// The listener accepts and never answers, so mihomo's dial succeeds
		// and the GET through the tunnel is what times out.
		{name: "reachable", addr: liveTCPAddr(t), urlTested: true, wantStage: StageFetch},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := zerolog.New(zerolog.SyncWriter(&buf)).Level(zerolog.DebugLevel)
			p := testProberWith(t, config.CheckConfig{
				Rounds:         2,
				Concurrency:    1,
				Timeout:        200 * time.Millisecond,
				TestURL:        "http://127.0.0.1:1/",
				ExpectedStatus: "204",
			}, config.BandwidthConfig{}, logger)

			res, probeErr := p.Probe(context.Background(), vmessPayloadAt(t, c.addr))
			if probeErr != nil {
				t.Fatal(probeErr)
			}
			if got := res["node"]; got != (ProbeResult{Stage: c.wantStage}) {
				t.Fatalf("result = %+v, want a zero-success entry at stage %v", got, c.wantStage)
			}
			if tested := strings.Contains(buf.String(), `"message":"url-test"`); tested != c.urlTested {
				t.Errorf("url-tested = %v, want %v; log: %s", tested, c.urlTested, buf.String())
			}
		})
	}
}

// The pre-check runs BEFORE the parse, so a condemned node costs no adapter
// object. What makes that observable is a node mihomo REFUSES sitting on a
// refused endpoint: parsed first it is dropped as unparsable and vanishes from
// the result map behind a warn, condemned first it never reaches the parser and
// reports the verdict the pre-check actually reached.
//
// Which of the two zero-success stages it lands in is deliberate: the pre-check
// judged its endpoint, and the endpoint is entirely the mapping's.
func TestProbeNeverParsesACondemnedNode(t *testing.T) {
	t.Parallel()

	live := liveTCPAddr(t)
	host, port, err := net.SplitHostPort(live)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Join([]string{
		// convert emits a mapping for this and adapter.ParseProxy then refuses
		// it: "unknown method: nope".
		"ss://nope:pass@" + deadTCPAddr(t) + "#condemned",
		benchVlessLine(host, port, "probed"),
	}, "\n")

	var buf bytes.Buffer
	logger := zerolog.New(zerolog.SyncWriter(&buf))
	p := testProberWith(t, config.CheckConfig{
		Rounds:         1,
		Concurrency:    1,
		Timeout:        200 * time.Millisecond,
		TestURL:        "http://127.0.0.1:1/",
		ExpectedStatus: "204",
	}, config.BandwidthConfig{}, logger)

	res, err := p.Probe(context.Background(), []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if got := res["condemned"]; got != (ProbeResult{Stage: StageCondemned}) {
		t.Errorf("condemned node = %+v, want a zero-success entry at stage condemned", got)
	}
	if _, ok := res["probed"]; !ok {
		t.Errorf("the reachable node must still be probed; results = %+v", res)
	}
	if strings.Contains(buf.String(), "skipped unparsable proxies") {
		t.Errorf("a condemned mapping reached the parser; log: %s", buf.String())
	}
}

// The probe path publishes four things per cycle: the result map SelectSurvivors
// reads, the payload BuildPayload renders from it, the stage histogram the
// exporter counts and the pre-check's own account. All four are pinned here for
// one fixed pool, so no reordering inside Probe can move a count silently.
//
// rounds and maxFail are 0, so no round runs and every label the prober NAMES
// survives selection: the membership of the result map is then visible in the
// published bytes, which no loadable config can be — max_fail is bounded to
// [0, rounds), so a zero-success node is always dropped, as the last assertion
// pins. The fixtures sit on refusedAddrs' network, which no concurrent test can
// bind, so every verdict below is fixed without a listener.
func TestProbeExpositionForAFixedPool(t *testing.T) {
	t.Parallel()

	ssLine := func(addr, name string) string {
		return "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret")) +
			"@" + addr + "#" + name
	}
	entries := []Entry{
		{Label: "src-001", Raw: benchVlessLine("127.0.1.1", "1", "src-001")},
		{Label: "src-002", Raw: vlessXHTTPLine("127.0.1.2:1", "tls", "h3", "src-002")},
		{Label: "src-003", Raw: "hy2://auth@127.0.1.3:1#src-003"},
		{Label: "src-004", Raw: benchVmessLine("127.0.1.4", "1", "src-004")},
		{Label: "src-005", Raw: mieruLine("127.0.1.5", "src-005", [2]string{"1", "TCP"})},
		{Label: "src-006", Raw: ssLine("127.0.1.6:1", "src-006")},
		// TCP-typed, refused AND unparsable, the one combination the reorder
		// moves: the pre-check condemns its endpoint before mihomo ever gets to
		// refuse the mapping, so it reports condemned where the eager parse
		// dropped it into the unknown bucket, and its endpoint joins Dialled.
		{Label: "src-007", Raw: "ss://nope:pass@127.0.1.7:1#src-007"},
		// Unparsable but NOT condemned: hysteria2 reaches its server over UDP,
		// so the pre-check may not judge it, mihomo then refuses the mapping
		// ("missing obfs password", adapter/outbound/hysteria2.go:146-148) and
		// it stays out of the result map exactly as it did when every mapping
		// was parsed up front.
		{Label: "src-008", Raw: "hy2://auth@127.0.1.8:1?obfs=salamander#src-008"},
	}
	payload := entriesPayload(entries)

	p := testProber(t)
	res, err := p.Probe(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}

	// Only the three TCP-dialled nodes are condemned: xhttp-over-h3 and
	// hysteria2 reach their server over UDP and mieru is unlisted, so the
	// pre-check may not judge them.
	wantRes := map[string]ProbeResult{
		"src-001": {Stage: StageCondemned},
		"src-002": {Stage: StageUnknown},
		"src-003": {Stage: StageUnknown},
		"src-004": {Stage: StageCondemned},
		"src-005": {Stage: StageUnknown},
		"src-006": {Stage: StageCondemned},
		"src-007": {Stage: StageCondemned},
	}
	if !maps.Equal(res, wantRes) {
		t.Errorf("probe results = %+v, want %+v", res, wantRes)
	}
	// A partial literal on purpose: the histogram must carry no key for a stage
	// nobody reached, which an exhaustive one cannot express.
	wantStages := map[ProbeStage]int{StageCondemned: 4, StageUnknown: 4} //nolint:exhaustive // see above
	if got := probeStages(entries, res); !maps.Equal(got, wantStages) {
		t.Errorf("probeStages = %v, want %v", got, wantStages)
	}
	if got, want := p.PrecheckReport(),
		(PrecheckReport{State: PrecheckRan, Dialled: 4, Refused: 4}); got != want {
		t.Errorf("PrecheckReport() = %+v, want %+v", got, want)
	}
	survivors := SelectSurvivors(entries, res, 0, 0, 0)
	// Every node but the unparsable one, which is named by nobody.
	if got, want := BuildPayload(context.Background(), nil, survivors),
		entriesPayload(entries[:len(entries)-1]); !bytes.Equal(got, want) {
		t.Errorf("published payload = %q, want %q", got, want)
	}
	// Nothing config.Load accepts publishes any of them: rounds >= 1 and
	// max_fail < rounds (CheckConfig.validate), so rounds-0 > max_fail always.
	if got := SelectSurvivors(entries, res, 5, 4, 1000); len(got) != 0 {
		t.Errorf("selection kept %d zero-success nodes; no loadable config may publish one", len(got))
	}
}

// If nearly every dial is refused the fault is far likelier to be ours than the
// pool's, and believing it writes the WHOLE probed set into the dead cache for
// deadcache.ttl — three cycles at the shipped 3h against a 1h interval. So the
// pre-check disbelieves itself and condemns nobody.
//
// Both sides of the threshold are pinned: lowering it would fail the case just
// under, raising it the case at 100%. precheckBreakerMin is pinned from the
// other side by TestProbeNeverURLTestsACondemnedNode, whose single node is 100%
// condemned and must still be judged on its merits.
func TestFilterReachableBreakerDisbelievesAnImplausibleVerdict(t *testing.T) {
	t.Parallel()

	// One over the floor, so the share is what decides and not the sample size.
	const total = precheckBreakerMin + 1
	for _, c := range []struct {
		desc          string
		live          int
		wantCondemned int
		wantRep       PrecheckReport
	}{
		{
			"every endpoint refused", 0, 0,
			PrecheckReport{State: PrecheckTripped, Dialled: total, Refused: total},
		},
		// 95 of 101 refused is 94.05%, just under the threshold.
		{
			"just under the threshold", total - 95, 95,
			PrecheckReport{State: PrecheckRan, Dialled: total, Refused: 95},
		},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			addrs := make([]string, 0, total)
			for range c.live {
				addrs = append(addrs, liveTCPAddr(t))
			}
			addrs = append(addrs, refusedAddrs(total-c.live)...)

			p := precheckProber(t)
			live, condemned := p.filterReachable(context.Background(), zerolog.Nop(), precheckNodes(addrs...))
			if len(condemned) != c.wantCondemned {
				t.Errorf("condemned %d of %d, want %d", len(condemned), total, c.wantCondemned)
			}
			if len(live)+len(condemned) != total {
				t.Errorf("live %d + condemned %d != %d probe positions", len(live), len(condemned), total)
			}
			// A tripped breaker condemns nobody, which is also what a pool of
			// reachable servers looks like: the state is the only thing that
			// says whether the verdict was believed.
			if got := p.PrecheckReport(); got != c.wantRep {
				t.Errorf("PrecheckReport() = %+v, want %+v", got, c.wantRep)
			}
		})
	}
}

// TestPrecheckBudgetCoversShippedLatencyGates pins the coupling that a constant
// broke: an endpoint's whole pre-check budget must be no tighter than the gate
// the instance publishes against, or the pre-check deletes nodes that instance
// is deliberately tuned to keep. The shipped 500ms constant was 8x tighter than
// config-vassago's max_avg_ms of 4000.
//
// Read from the configs themselves, as internal/metrics does for latencyBuckets,
// so re-tuning either instance fails here rather than in production.
func TestPrecheckBudgetCoversShippedLatencyGates(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{"config", "config-vassago"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join("..", "..", dir, "config.yaml")
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			check := cfg.Subscriptions.Check
			p := testProberWith(t, check, config.BandwidthConfig{}, zerolog.Nop())
			gate := time.Duration(check.MaxAvgMs) * time.Millisecond
			if budget := precheckAttempts * p.precheckDialBudget(); budget < gate {
				t.Errorf("%s: pre-check spends at most %v on an endpoint, under max_avg_ms of %v",
					path, budget, gate)
			}
		})
	}
}

// TestPrecheckBudgetTracksTheDefaultTimeout covers the timeout no shipped config
// exercises. Both set check.timeout explicitly, so Load never applies its
// default on either path and the derivation could be replaced by a constant
// equal to today's shipped value with the test above still green.
func TestPrecheckBudgetTracksTheDefaultTimeout(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join("..", "..", "config-vassago", "config.yaml"))
	if err != nil {
		t.Fatalf("read shipped config: %v", err)
	}
	var kept []string
	for line := range strings.SplitSeq(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "timeout: 5000ms") {
			continue
		}
		kept = append(kept, line)
	}
	// Guard the STRIP, not the loaded value: Load can never return 0 (normalize
	// substitutes the default, validate rejects anything <= 0), so a zero check
	// is dead code. The reachable vacuity is a strip that removes nothing.
	if len(kept) == len(strings.Split(string(src), "\n")) {
		t.Fatal("fixture strip removed no line; the test would pin the shipped timeout, not the default")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if writeErr := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600); writeErr != nil {
		t.Fatalf("write fixture: %v", writeErr)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	check := cfg.Subscriptions.Check
	p := testProberWith(t, check, config.BandwidthConfig{}, zerolog.Nop())
	if budget, want := precheckAttempts*p.precheckDialBudget(), check.Timeout; budget != want {
		t.Errorf("pre-check spends %v on an endpoint under the default check.timeout, want %v",
			budget, want)
	}
}

// The pre-check resolves through an uncached net.DefaultResolver inside its own
// budget, while mihomo's URL test dials through a stale-serving cache and the IP
// stage already resolved every kept name this cycle. A lookup failure here is
// therefore evidence about this path's resolver, not about the node, so it must
// fail OPEN: condemning on it dead-caches a live node for a jittered [3h, 4.5h)
// against a 1h interval.
func TestFilterReachableFailsOpenOnAnUnresolvableName(t *testing.T) {
	t.Parallel()

	// .invalid resolves for nobody (RFC 6761), so the dial fails at the lookup
	// and no SYN leaves the machine.
	nodes := precheckNodes("no-such-host.invalid:443", deadTCPAddr(t), liveTCPAddr(t))

	p := precheckProber(t)
	live, condemned := p.filterReachable(context.Background(), zerolog.Nop(), nodes)
	if want := []int{0, 2}; !slices.Equal(live, want) {
		t.Errorf("live = %v, want %v: an unresolvable name proves nothing about the endpoint", live, want)
	}
	if want := []int{1}; !slices.Equal(condemned, want) {
		t.Errorf("condemned = %v, want %v: only a refused or black-holed SYN is proof", condemned, want)
	}
	want := PrecheckReport{State: PrecheckRan, Dialled: 3, Refused: 1, Unresolved: 1}
	if got := p.PrecheckReport(); got != want {
		t.Errorf("PrecheckReport() = %+v, want %+v", got, want)
	}
}

// The breaker's share is taken over the endpoints the pre-check JUDGED, never
// over everything it dialled: a resolver outage adds fail-open endpoints that
// prove nothing, and counting them in the denominator would hold a wholly
// refused egress under the threshold and condemn the pool it was built to
// spare. 101 refused against 99 unresolvable is 100% of the judged set and
// 50.5% of the dialled one.
func TestFilterReachableBreakerIgnoresUnjudgedEndpoints(t *testing.T) {
	t.Parallel()

	const unresolvable = 99
	addrs := refusedAddrs(precheckBreakerMin + 1)
	for i := range unresolvable {
		addrs = append(addrs, fmt.Sprintf("no-such-host-%d.invalid:443", i))
	}

	p := precheckProber(t)
	_, condemned := p.filterReachable(context.Background(), zerolog.Nop(), precheckNodes(addrs...))
	if len(condemned) != 0 {
		t.Errorf("condemned %d endpoints, want 0: the breaker must fire on the judged share", len(condemned))
	}
	want := PrecheckReport{
		State: PrecheckTripped, Dialled: len(addrs), Refused: precheckBreakerMin + 1, Unresolved: unresolvable,
	}
	if got := p.PrecheckReport(); got != want {
		t.Errorf("PrecheckReport() = %+v, want %+v", got, want)
	}
}

// precheckingProber is the smallest publishing prober that also carries the
// pre-check capability, so the seam Probe -> CycleReport -> Reporter is
// exercised for a report no other fake reports.
type precheckingProber struct {
	oneNodeProber
	rep PrecheckReport
}

func (p precheckingProber) PrecheckReport() PrecheckReport { return p.rep }

// Every hand-off on the way to the Reporter can drop the account without
// failing anything: a lost one is the zero PrecheckReport, which renders as no
// series at all and so looks like a prober that runs no pre-check.
func TestPrecheckReportReachesTheCycleReport(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		desc   string
		prober Prober
		want   PrecheckReport
	}{
		{"verdict used", precheckingProber{rep: PrecheckReport{
			State: PrecheckRan, Dialled: 1907, Refused: 1119, Unresolved: 99,
		}}, PrecheckReport{State: PrecheckRan, Dialled: 1907, Refused: 1119, Unresolved: 99}},
		{"verdict discarded", precheckingProber{rep: PrecheckReport{
			State: PrecheckTripped, Dialled: 1907, Refused: 1889,
		}}, PrecheckReport{State: PrecheckTripped, Dialled: 1907, Refused: 1889}},
		{"prober runs no pre-check", oneNodeProber{}, PrecheckReport{}},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			rec := &cycleRecorder{}
			ch := NewChecker(CheckerSpec{
				Sources:       []config.SubscriptionSource{{Name: "src", Body: benchVlessLine("1.1.1.1", "443", "n")}},
				Interval:      time.Hour,
				Rounds:        5,
				MaxAvgMs:      1000,
				SourceTimeout: time.Minute,
				Prober:        c.prober,
			}, func() Filterer { return oneNodeFilterer{} }, nil, nil, NewHolder(), "", zerolog.Nop(), rec)

			if err := ch.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if rec.last == nil {
				t.Fatal("a published cycle must reach the Reporter")
			}
			if rec.last.Precheck != c.want {
				t.Fatalf("CycleReport.Precheck = %+v, want %+v", rec.last.Precheck, c.want)
			}
		})
	}
}

// A Probe that fails before the pre-check must publish PrecheckAbsent, never the
// previous cycle's verdict: a stale PrecheckRan claims a pre-check that this
// cycle never performed.
func TestProbeClearsLastCyclesPrecheckReport(t *testing.T) {
	t.Parallel()

	p := precheckProber(t)
	if _, err := p.Probe(context.Background(), vmessPayload(t)); err != nil {
		t.Fatalf("setup probe: %v", err)
	}
	if got := p.PrecheckReport(); got.State != PrecheckRan {
		t.Fatalf("setup: a completed pre-check must report PrecheckRan, got %+v", got)
	}

	if _, err := p.Probe(context.Background(), []byte("no nodes here\n")); err == nil {
		t.Fatal("a payload with no parsable proxy must fail the probe")
	}
	if got := p.PrecheckReport(); got != (PrecheckReport{}) {
		t.Errorf("PrecheckReport() = %+v, want the zero report", got)
	}
}
