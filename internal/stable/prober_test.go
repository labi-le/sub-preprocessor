package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
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
	"domains.lst/sub-preprocessor/internal/preprocess"
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

// vmessNamedPayloadAt is vmessPayloadAt with an explicit node name, so a
// payload can carry several nodes that stay distinguishable in the result map.
func vmessNamedPayloadAt(t *testing.T, name, addr string) []byte {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	node := `{"v":"2","ps":"` + name + `","add":"` + host + `","port":"` + port + `",` +
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
	// fanoutSem's doc comment names. runRound acquires the semaphore in its
	// spawning loop before any worker exists, so an unbuffered channel would
	// block the spawner forever; only fanoutSem's >= 1 clamp keeps that shape
	// safe. The failure mode is a HANG, not a wrong value, hence the deadline
	// + done channel: without them this wedges the suite.
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
// verdict comparison is what catches. The label is deliberately NOT compared:
// the fold asks the adapter for a live position and probeSet derives the
// condemned position's label from the raw mapping, so probeNodes must leave it
// empty everywhere.
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
		if got := nodes[i].label; got != "" {
			t.Errorf("position %d: probeNodes derived label %q; the fold derives labels (the adapter answers %q)", i, got, entryLabel(px))
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
				t.Errorf("%s: tcp verdict = %v, want %v", c.desc, nodes[0].tcpServer, c.want)
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
	// No labels: probeNodes leaves them empty, the fold derives them from the
	// adapter or from the raw mapping of a condemned position (see probeNodes).
	want := []probeNode{
		{addr: addr, tcpServer: true},
		{addr: addr, tcpServer: true},
		// No endpoint: the pre-check will not dial this position, so probeNodes
		// never derives one.
		{},
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

// A node the pre-check condemns is never URL-tested, and the condemned
// verdict is only believed on a PARTIAL refusal: a single refused endpoint is
// a total refusal and the breaker disbelieves it (see breakerTrips), so the
// refused node here sits beside a live one and is condemned unprobed.
func TestProbeNeverURLTestsACondemnedNode(t *testing.T) {
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

	// The listener accepts and never answers, so mihomo's dial succeeds
	// and the GET through the tunnel is what times out.
	payload := append(vmessNamedPayloadAt(t, "condemned", deadTCPAddr(t)),
		vmessNamedPayloadAt(t, "probed", liveTCPAddr(t))...)
	res, probeErr := p.Probe(context.Background(), payload)
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	if got := res["condemned"]; got != (ProbeResult{Stage: StageCondemned}) {
		t.Fatalf("condemned node = %+v, want a zero-success entry at stage condemned", got)
	}
	if got := res["probed"]; got != (ProbeResult{Stage: StageFetch}) {
		t.Fatalf("probed node = %+v, want a zero-success entry at stage fetch", got)
	}
	// The url-test log lines name their node: the condemned one must not be
	// among them.
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if !strings.Contains(line, `"message":"url-test"`) {
			continue
		}
		if strings.Contains(line, `"node":"condemned"`) {
			t.Errorf("a condemned node reached the URL test; log: %s", line)
		}
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
// pins. The refused fixtures sit on refusedAddrs' network, which no concurrent
// test can bind, so their verdicts are fixed without a listener; src-009 binds
// one real listener so the pool is not a total refusal (see breakerTrips).
func TestProbeExpositionForAFixedPool(t *testing.T) {
	t.Parallel()

	ssLine := func(addr, name string) string {
		return "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret")) +
			"@" + addr + "#" + name
	}
	liveHost, livePort, err := net.SplitHostPort(liveTCPAddr(t))
	if err != nil {
		t.Fatal(err)
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
		// The one live endpoint. A pool whose every judged endpoint refused is
		// a total refusal and the breaker disbelieves it (see breakerTrips),
		// so this fixture needs a reachable endpoint to stay in the believed
		// regime whose exposition it pins.
		{Label: "src-009", Raw: benchVlessLine(liveHost, livePort, "src-009")},
	}
	payload := entriesPayload(entries)

	p := precheckProber(t)
	res, err := p.Probe(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}

	// Only the three TCP-dialled refused nodes are condemned: xhttp-over-h3 and
	// hysteria2 reach their server over UDP and mieru is unlisted, so the
	// pre-check may not judge them. src-009's dial succeeds, so it is probed
	// (rounds-0 folds it to a named zero-success entry).
	wantRes := map[string]ProbeResult{
		"src-001": {Stage: StageCondemned},
		"src-002": {Stage: StageUnknown},
		"src-003": {Stage: StageUnknown},
		"src-004": {Stage: StageCondemned},
		"src-005": {Stage: StageUnknown},
		"src-006": {Stage: StageCondemned},
		"src-007": {Stage: StageCondemned},
		"src-009": {Stage: StageUnknown},
	}
	if !maps.Equal(res, wantRes) {
		t.Errorf("probe results = %+v, want %+v", res, wantRes)
	}
	// A partial literal on purpose: the histogram must carry no key for a stage
	// nobody reached, which an exhaustive one cannot express. src-008 is the
	// entry whose label no result names: the parse-refusal account attributes
	// it (unparsable) instead of leaving it to index as unknown, which is the
	// whole difference this pool pins. The named-but-zero-stage entries --
	// rounds-0 folds a live proxy without a round -- keep their StageUnknown.
	wantStages := map[ProbeStage]int{StageCondemned: 4, StageUnknown: 4} //nolint:exhaustive // see above
	if got := probeStages(entries, res, p.ParseRefusalReport()); !maps.Equal(got, wantStages) {
		t.Errorf("probeStages = %v, want %v", got, wantStages)
	}
	// The account both classes of miss fold into. src-008's mapping exists and
	// ParseProxy refused it; nothing in this pool was never mapped at all.
	refusals := attributeRefusals(entries, res, p.ParseRefusalReport())
	if refusals.State != RefusalRan || refusals.Unparsable != 1 || refusals.Unconvertible != 0 {
		t.Errorf("refusal account = %+v, want State=RefusalRan Unparsable=1 Unconvertible=0", refusals)
	}
	if got, want := p.PrecheckReport(),
		(PrecheckReport{State: PrecheckRan, Dialled: 5, Refused: 4}); got != want {
		t.Errorf("PrecheckReport() = %+v, want %+v", got, want)
	}
	survivors := SelectSurvivors(entries, res, 0, 0, 0)
	// Every node but the unparsable one (src-008), which is named by nobody.
	wantPayload := entriesPayload(append(slices.Clone(entries[:7]), entries[8]))
	if got, want := BuildPayload(context.Background(), nil, survivors),
		wantPayload; !bytes.Equal(got, want) {
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
// Both sides of the share threshold are pinned: lowering it would fail the
// case just under, raising it the case at 100%. The sample-size floor bounds
// only partial refusal — a verdict that refused EVERYTHING it judged is
// disbelieved below it too (breakerTrips), which the two small-pool cases
// below pin.
func TestFilterReachableBreakerDisbelievesAnImplausibleVerdict(t *testing.T) {
	t.Parallel()

	// One over the floor, so the share is what decides and not the sample size.
	const total = precheckBreakerMin + 1
	for _, c := range []struct {
		desc          string
		live          int
		total         int
		wantCondemned int
		wantRep       PrecheckReport
	}{
		{
			"every endpoint refused", 0, total, 0,
			PrecheckReport{State: PrecheckTripped, Dialled: total, Refused: total},
		},
		// 95 of 101 refused is 94.05%, just under the threshold.
		{
			"just under the threshold", total - 95, total, 95,
			PrecheckReport{State: PrecheckRan, Dialled: total, Refused: 95},
		},
		// Total refusal below the floor is disbelieved too: a cold-start pool
		// of three refused endpoints carries the same ~100% signature as a big
		// one, and believing it dead-caches the whole pool for deadcache.ttl.
		{
			"total refusal below the floor", 0, 3, 0,
			PrecheckReport{State: PrecheckTripped, Dialled: 3, Refused: 3},
		},
		// ... but a small pool that is not wholly refused keeps its verdict:
		// one dead endpoint beside two live ones is an ordinary pool.
		{
			"partial refusal below the floor", 2, 3, 1,
			PrecheckReport{State: PrecheckRan, Dialled: 3, Refused: 1},
		},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			addrs := make([]string, 0, c.total)
			for range c.live {
				addrs = append(addrs, liveTCPAddr(t))
			}
			addrs = append(addrs, refusedAddrs(c.total-c.live)...)

			p := precheckProber(t)
			live, condemned := p.filterReachable(context.Background(), zerolog.Nop(), precheckNodes(addrs...))
			if len(condemned) != c.wantCondemned {
				t.Errorf("condemned %d of %d, want %d", len(condemned), c.total, c.wantCondemned)
			}
			if len(live)+len(condemned) != c.total {
				t.Errorf("live %d + condemned %d != %d probe positions", len(live), len(condemned), c.total)
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
// the instance publishes against, or the pre-check deletes nodes the instance is
// deliberately tuned to keep. A hardcoded 500ms was 8x tighter than the
// max_avg_ms of 4000 measured on the second instance, retired 2026-08-26.
//
// Read from the config itself, as internal/metrics does for latencyBuckets, so
// re-tuning check.timeout or max_avg_ms fails here rather than in production.
func TestPrecheckBudgetCoversShippedLatencyGates(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "config", "config.yaml")
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
}

// TestPrecheckBudgetTracksTheDefaultTimeout covers the timeout no shipped config
// exercises, and is the only test separating the derivation from a constant: the
// shipped check.timeout of 1000ms derives to exactly the 500ms that used to be
// hardcoded, so the test above passes either way.
func TestPrecheckBudgetTracksTheDefaultTimeout(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join("..", "..", "config", "config.yaml"))
	if err != nil {
		t.Fatalf("read shipped config: %v", err)
	}
	var kept []string
	for line := range strings.SplitSeq(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "timeout: 1000ms") {
			continue
		}
		kept = append(kept, line)
	}
	// Guard the STRIP, not the loaded value: Load can never return 0 (normalize
	// substitutes the default, validate rejects anything <= 0), so a zero check
	// is dead code. The reachable vacuities are a strip that removes nothing and
	// one that removes more than check.timeout, the config carrying ten other
	// timeout keys.
	if removed := len(strings.Split(string(src), "\n")) - len(kept); removed != 1 {
		t.Fatalf("fixture strip removed %d lines, want exactly check.timeout", removed)
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

// A Probe that fails before the pre-check must publish PrecheckAbsent, never
// the previous cycle's verdict: a stale report claims a pre-check that this
// cycle never performed. The setup's single refused endpoint is a total
// refusal, so the breaker trips — Tripped is still a completed pre-check's
// report, and the failing probe must clear it all the same.
func TestProbeClearsLastCyclesPrecheckReport(t *testing.T) {
	t.Parallel()

	p := precheckProber(t)
	if _, err := p.Probe(context.Background(), vmessPayload(t)); err != nil {
		t.Fatalf("setup probe: %v", err)
	}
	if got := p.PrecheckReport(); got.State != PrecheckTripped {
		t.Fatalf("setup: a completed pre-check must report the tripped breaker, got %+v", got)
	}

	if _, err := p.Probe(context.Background(), []byte("no nodes here\n")); err == nil {
		t.Fatal("a payload with no parsable proxy must fail the probe")
	}
	if got := p.PrecheckReport(); got != (PrecheckReport{}) {
		t.Errorf("PrecheckReport() = %+v, want the zero report", got)
	}
}

// A successful Probe keeps the adapters it built alive past its return, which
// is the whole premise of the egress-stage handover (probedAdapterSource): the
// survivor set's raw must not be converted and parsed a second time. The take
// clears the field, so a second take owes nothing, and the caller closes each
// proxy exactly once.
func TestProbeRetainsAdaptersUntilTaken(t *testing.T) {
	t.Parallel()

	p := testProberWith(t, config.CheckConfig{
		Rounds:         1,
		Concurrency:    1,
		Timeout:        200 * time.Millisecond,
		TestURL:        "http://127.0.0.1:1/",
		ExpectedStatus: "204",
	}, config.BandwidthConfig{}, zerolog.Nop())
	payload := vmessNamedPayloadAt(t, "n1", liveTCPAddr(t))

	if _, err := p.Probe(context.Background(), payload); err != nil {
		t.Fatalf("probe: %v", err)
	}
	taken := p.TakeProbedAdapters()
	if len(taken) != 1 {
		t.Fatalf("a one-node probe retained %d adapters, want 1", len(taken))
	}
	if got := taken[0].Name(); got != "n1" {
		t.Errorf("retained adapter name = %q, want the probed node's n1", got)
	}
	if again := p.TakeProbedAdapters(); again != nil {
		t.Errorf("second take returned %d adapters; the take must clear the field", len(again))
	}
	for _, px := range taken {
		_ = px.Close()
	}
}

// The checker only takes after a successful Probe, so every failure shape must
// leave nothing behind: a cancelled probe closes its own adapters before
// returning, and a probe that never parsed retains nothing to close.
func TestProbeFailureRetainsNothing(t *testing.T) {
	t.Parallel()

	p := precheckProber(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Probe(ctx, vmessNamedPayloadAt(t, "n1", liveTCPAddr(t))); err == nil {
		t.Fatal("a cancelled probe must fail")
	}
	if got := p.TakeProbedAdapters(); got != nil {
		t.Errorf("cancelled probe retained %d adapters, want none", len(got))
	}

	if _, err := p.Probe(context.Background(), []byte("no nodes here\n")); err == nil {
		t.Fatal("a payload with no parsable proxy must fail the probe")
	}
	if got := p.TakeProbedAdapters(); got != nil {
		t.Errorf("a failed probe retained %d adapters, want none", len(got))
	}
}

// recordingDead mirrors fakeDeadCache from the external checker tests, which
// this internal package cannot import; it records what recordDead writes.
type recordingDead struct {
	blocked []deadKey
}

func (d *recordingDead) Blocked(string, netip.Addr) bool { return false }
func (d *recordingDead) Block(addr string, ip netip.Addr) error {
	d.blocked = append(d.blocked, deadKey{addr: addr, ip: ip})
	return nil
}
func (d *recordingDead) Prune() error { return nil }

// TestRecordDeadExcludesAttributedRefusals pins the dead-cache decision in
// code: a probed node whose label the result map never names carries no
// liveness verdict when the prober can attribute the miss. Whether the
// converter never mapped it or adapter.ParseProxy refused the mapping, the
// endpoint was never judged (or, having passed the TCP pre-check, was judged
// ALIVE), so caching server:port would shadow any working sibling Merge admits
// on it for the jittered [3h, 4.5h) TTL -- the ssr-relabel regression's shape
// (TestSchemeContractSSRSurvivesRelabelFragmentFree) -- and a mihomo bump that
// learns the scheme or cipher would find the same line skipped past the bump.
// Only the URL-tested zero-success entry is a death, and only it is written.
func TestRecordDeadExcludesAttributedRefusals(t *testing.T) {
	t.Parallel()

	dead := &recordingDead{}
	c := NewChecker(CheckerSpec{}, nil, nil, dead, NewHolder(), "", zerolog.Nop(), nil)
	probe := []Entry{
		{Label: "src-001", Addr: "192.0.2.1:443"}, // URL-tested; every round failed
		{Label: "src-002", Addr: "192.0.2.2:443"}, // mapping existed; ParseProxy refused it
		{Label: "src-003", Addr: "192.0.2.3:443"}, // converter never emitted a mapping
		{Label: "src-004", Addr: "192.0.2.4:443"}, // healthy
	}
	res := map[string]ProbeResult{
		"src-001": {Stage: StageConnect},
		"src-004": {Successes: 5, MeanMs: 100, Stage: StagePassed},
	}
	refusals := RefusalReport{State: RefusalRan, refused: map[string]struct{}{"src-002": {}}}
	c.recordDead(probe, res, refusals)

	want := []string{"192.0.2.1:443"}
	got := make([]string, len(dead.blocked))
	for i, k := range dead.blocked {
		got[i] = k.addr
	}
	if !slices.Equal(got, want) {
		t.Errorf("recordDead wrote %v, want only the URL-tested-dead %v", got, want)
	}
}

// TestRecordDeadAbsenceKeepsBlockingWithoutTheAccount pins the other half of
// the same rule: nothing obliges a Prober implementation to name every label,
// so a prober WITHOUT the parse-refusal account keeps the absence-means-death
// write. Removing it would silently empty the dead cache the moment such a
// prober stops naming labels, with every counter still reading plausible.
func TestRecordDeadAbsenceKeepsBlockingWithoutTheAccount(t *testing.T) {
	t.Parallel()

	dead := &recordingDead{}
	c := NewChecker(CheckerSpec{}, nil, nil, dead, NewHolder(), "", zerolog.Nop(), nil)
	probe := []Entry{
		{Label: "src-001", Addr: "192.0.2.1:443"},
		{Label: "src-002", Addr: "192.0.2.2:443"},
		{Label: "src-003", Addr: "192.0.2.3:443"},
		{Label: "src-004", Addr: "192.0.2.4:443"},
	}
	res := map[string]ProbeResult{
		"src-001": {Stage: StageConnect},
		"src-004": {Successes: 5, MeanMs: 100, Stage: StagePassed},
	}
	c.recordDead(probe, res, RefusalReport{}) // State Absent: no account

	want := []string{"192.0.2.1:443", "192.0.2.2:443", "192.0.2.3:443"}
	got := make([]string, len(dead.blocked))
	for i, k := range dead.blocked {
		got[i] = k.addr
	}
	if !slices.Equal(got, want) {
		t.Errorf("recordDead wrote %v, want the zero-success and both absent %v", got, want)
	}
}

// TestRecordDeadBreakerIgnoresRefusedEntries: the write's plausibility breaker
// must judge only the entries a write could block, exactly as the pre-check's
// breaker judges only the endpoints it dialled. A probed set that is wholly
// converter-unmapped has no verdict to disbelieve -- nothing is written and
// nothing trips -- while a set whose judged entries are all dead still trips
// and keeps the cache unchanged.
func TestRecordDeadBreakerIgnoresRefusedEntries(t *testing.T) {
	t.Parallel()

	allRefused := make([]Entry, 8)
	for i := range allRefused {
		allRefused[i] = Entry{Label: fmt.Sprintf("src-%03d", i), Addr: fmt.Sprintf("192.0.2.%d:443", i+1)}
	}
	dead := &recordingDead{}
	c := NewChecker(CheckerSpec{}, nil, nil, dead, NewHolder(), "", zerolog.Nop(), nil)
	c.recordDead(allRefused, map[string]ProbeResult{}, RefusalReport{State: RefusalRan})
	if len(dead.blocked) != 0 {
		t.Errorf("a wholly unconvertible set must write nothing, wrote %v", dead.blocked)
	}

	allDead := make([]Entry, 8)
	res := make(map[string]ProbeResult, 8)
	for i := range allDead {
		label := fmt.Sprintf("src-%03d", i)
		allDead[i] = Entry{Label: label, Addr: fmt.Sprintf("192.0.2.%d:443", i+1)}
		res[label] = ProbeResult{Stage: StageConnect}
	}
	dead = &recordingDead{}
	c = NewChecker(CheckerSpec{}, nil, nil, dead, NewHolder(), "", zerolog.Nop(), nil)
	c.recordDead(allDead, res, RefusalReport{State: RefusalRan})
	if len(dead.blocked) != 0 {
		t.Errorf("an all-dead judged set must trip the breaker and write nothing, wrote %v", dead.blocked)
	}
}

// TestAttributeRefusalsSplitsTheTwoClasses pins the attribution seam itself:
// result-map misses sort into the parse-refused labels the prober recorded and
// everything else -- the converter never mapped it. A prober without the
// account gets no attribution at all: its misses keep the legacy meaning
// rather than being guessed at.
func TestAttributeRefusalsSplitsTheTwoClasses(t *testing.T) {
	t.Parallel()

	probe := []Entry{
		{Label: "src-001"}, {Label: "src-002"}, {Label: "src-003"}, {Label: "src-004"},
	}
	res := map[string]ProbeResult{
		"src-001": {Successes: 5, MeanMs: 100, Stage: StagePassed},
		"src-004": {Stage: StageCondemned},
	}
	refusals := RefusalReport{State: RefusalRan, refused: map[string]struct{}{"src-002": {}}}
	got := attributeRefusals(probe, res, refusals)
	if got.State != RefusalRan || got.Unparsable != 1 || got.Unconvertible != 1 {
		t.Errorf("attributeRefusals = %+v, want State=Ran Unparsable=1 Unconvertible=1", got)
	}
	if unaccounted := attributeRefusals(probe, res, RefusalReport{}); unaccounted.State != RefusalAbsent ||
		unaccounted.Unparsable != 0 || unaccounted.Unconvertible != 0 {
		t.Errorf("an absent account must not be attributed, got %+v", unaccounted)
	}
}

// twoNodeFilterer yields two nodes from one source, so a cycle can carry one
// entry the result map names and one the refusal account carries.
type twoNodeFilterer struct{}

func (twoNodeFilterer) FilterNodes(
	context.Context, preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	return []preprocess.NodeResult{
		{Raw: benchVlessLine("1.1.1.1", "443", "a")},
		{Raw: benchVlessLine("2.2.2.2", "443", "b")},
	}, preprocess.Stats{}, nil
}

//nolint:ireturn // implements Filterer; handing out the interface is the point
func (twoNodeFilterer) Annotator() preprocess.Annotator { return nil }

// refusingProber names src-001 as passed and reports whatever parse-refusal
// account the test built, so the checker's attribution seam is exercised for a
// report no other fake reports -- the precheckingProber pattern for
// RefusalReport.
type refusingProber struct {
	oneNodeProber
	refused map[string]struct{}
}

func (p *refusingProber) Probe(context.Context, []byte) (map[string]ProbeResult, error) {
	return map[string]ProbeResult{"src-001": {Successes: 5, MeanMs: 100, Stage: StagePassed}}, nil
}

func (p *refusingProber) ParseRefusalReport() RefusalReport {
	return RefusalReport{State: RefusalRan, refused: p.refused}
}

// passedOnlyProber names src-001 as passed and implements no refusal
// capability: the legacy shape whose result-map miss must keep indexing as
// stage="unknown".
type passedOnlyProber struct{}

func (passedOnlyProber) Probe(context.Context, []byte) (map[string]ProbeResult, error) {
	return map[string]ProbeResult{"src-001": {Successes: 5, MeanMs: 100, Stage: StagePassed}}, nil
}

func (passedOnlyProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

// TestRefusalReportReachesTheCycleReport walks the whole seam the refusal
// metric rides: prober -> attributeRefusals -> CycleReport -> Reporter. The
// result-map miss is attributed to the class the prober recorded (or to the
// converter when it recorded none), and is then absent from the stage counts;
// a prober without the account leaves the miss indexing as StageUnknown with
// an absent Refusals, exactly the pre-account behaviour.
func TestRefusalReportReachesTheCycleReport(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		prober   Prober
		wantRef  RefusalReport
		wantUnkn int
	}{
		{
			desc:    "parse refused",
			prober:  &refusingProber{refused: map[string]struct{}{"src-002": {}}},
			wantRef: RefusalReport{State: RefusalRan, Unparsable: 1},
		},
		{
			desc:    "converter never mapped",
			prober:  &refusingProber{},
			wantRef: RefusalReport{State: RefusalRan, Unconvertible: 1},
		},
		{
			desc:     "prober runs no refusal account",
			prober:   passedOnlyProber{},
			wantUnkn: 1,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			rec := &cycleRecorder{}
			ch := NewChecker(CheckerSpec{
				Sources:       []config.SubscriptionSource{{Name: "src", Body: "ignored\n"}},
				Interval:      time.Hour,
				Rounds:        5,
				MaxAvgMs:      1000,
				SourceTimeout: time.Minute,
				Prober:        tc.prober,
			}, func() Filterer { return twoNodeFilterer{} }, nil, nil, NewHolder(), "", zerolog.Nop(), rec)

			if err := ch.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if rec.last == nil {
				t.Fatal("a published cycle must reach the Reporter")
			}
			r := rec.last.Refusals
			if r.State != tc.wantRef.State || r.Unparsable != tc.wantRef.Unparsable ||
				r.Unconvertible != tc.wantRef.Unconvertible {
				t.Errorf("CycleReport.Refusals = %+v, want %+v", r, tc.wantRef)
			}
			wantStages := map[ProbeStage]int{StagePassed: 1} //nolint:exhaustive // only the reached stage may carry a key
			if tc.wantUnkn > 0 {
				wantStages[StageUnknown] = tc.wantUnkn
			}
			if got := rec.last.ProbeStages; !maps.Equal(got, wantStages) {
				t.Errorf("CycleReport.ProbeStages = %v, want %v", got, wantStages)
			}
		})
	}
}
