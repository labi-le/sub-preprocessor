package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// precheckProxy exposes only Addr, the one method filterReachable reads now
// that the TCP verdict is taken in parseProxies. The embedded nil interface
// panics on anything else, which pins that read set.
type precheckProxy struct {
	mihomo.Proxy
	addr string
}

func (p *precheckProxy) Addr() string { return p.addr }

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

	p, err := NewMihomoProber(config.CheckConfig{Timeout: time.Second, ExpectedStatus: "204"},
		config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	return p
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

	p, err := NewMihomoProber(config.CheckConfig{
		Rounds:         2,
		Concurrency:    1,
		Timeout:        time.Second,
		TestURL:        "http://127.0.0.1:0/",
		ExpectedStatus: "204",
	}, config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

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
	p, err := NewMihomoProber(config.CheckConfig{
		Rounds:         1,
		Concurrency:    0,
		Timeout:        50 * time.Millisecond,
		TestURL:        "http://127.0.0.1:0/",
		ExpectedStatus: "204",
	}, config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
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

// The TCP verdict must come from the transport the raw mapping selects, not
// from the adapter type: one mihomo.Vless carries both an ordinary TCP dial and
// xhttp's QUIC mode, where the only dial is ListenPacket over UDP and the TCP
// closure is never invoked. Condemning such a node costs it a URL test, a
// zero-success fold and deadcache.ttl in the dead cache.
//
// Going through parseProxies rather than calling the predicate directly is what
// proves the two keys survive convert AND that every line here is one mihomo
// really parses: an unparsable line would shift the index alignment, which the
// length check catches.
func TestParseProxiesKeysTheTCPVerdictOnTheTransport(t *testing.T) {
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
			vlessXHTTPLine(addr, "tls", "h3", "n5"), "type=xhttp", "type=tcp", 1), true},
		{"plain vless", benchVlessLine(h, p, "n6"), true},
		{"vmess", benchVmessLine(h, p, "n7"), true},
		{"anytls", "anytls://pass@" + addr + "#n8", true},
		{"hysteria2 reaches its server over UDP", "hy2://auth@" + addr + "#n9", false},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			pxs, tcpServer, err := precheckProber(t).parseProxies([]byte(c.line))
			if err != nil {
				t.Fatal(err)
			}
			if len(pxs) != 1 || len(tcpServer) != 1 {
				t.Fatalf("parsed %d proxies and %d verdicts, want 1 and 1", len(pxs), len(tcpServer))
			}
			if tcpServer[0] != c.want {
				t.Errorf("%v %q: tcp verdict = %v, want %v", pxs[0].Type(), pxs[0].Name(), tcpServer[0], c.want)
			}
		})
	}
}

// The verdict slice is appended only for a mapping mihomo actually builds, so an
// unparsable line BETWEEN two nodes is what catches an off-by-one: misalign the
// two and the QUIC-only node inherits its neighbour's verdict and is condemned
// unprobed.
func TestParseProxiesAlignsTheVerdictAcrossAnUnparsableNode(t *testing.T) {
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

	pxs, tcpServer, err := precheckProber(t).parseProxies([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"tcp-first": true, "quic-only": false}
	if len(pxs) != len(want) || len(tcpServer) != len(pxs) {
		t.Fatalf("parsed %d proxies and %d verdicts, want %d of each", len(pxs), len(tcpServer), len(want))
	}
	for i, px := range pxs {
		if tcpServer[i] != want[px.Name()] {
			t.Errorf("%q: tcp verdict = %v, want %v", px.Name(), tcpServer[i], want[px.Name()])
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
	pxs := []mihomo.Proxy{
		&precheckProxy{addr: liveTCPAddr(t)},
		&precheckProxy{addr: dead},
		&precheckProxy{addr: dead},
		&precheckProxy{addr: dead},
	}
	tcpServer := []bool{true, true, false, true}

	live, condemned := precheckProber(t).
		filterReachable(context.Background(), zerolog.Nop(), pxs, tcpServer)
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
			p, err := NewMihomoProber(config.CheckConfig{
				Rounds:         2,
				Concurrency:    1,
				Timeout:        200 * time.Millisecond,
				TestURL:        "http://127.0.0.1:1/",
				ExpectedStatus: "204",
			}, config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", logger)
			if err != nil {
				t.Fatal(err)
			}

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
	}{
		{"every endpoint refused", 0, 0},
		// 95 of 101 refused is 94.05%, just under the threshold.
		{"just under the threshold", total - 95, 95},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			pxs := make([]mihomo.Proxy, 0, total)
			for range c.live {
				pxs = append(pxs, &precheckProxy{addr: liveTCPAddr(t)})
			}
			for _, addr := range refusedAddrs(total - c.live) {
				pxs = append(pxs, &precheckProxy{addr: addr})
			}
			tcpServer := make([]bool, total)
			for i := range tcpServer {
				tcpServer[i] = true
			}

			live, condemned := precheckProber(t).
				filterReachable(context.Background(), zerolog.Nop(), pxs, tcpServer)
			if len(condemned) != c.wantCondemned {
				t.Errorf("condemned %d of %d, want %d", len(condemned), total, c.wantCondemned)
			}
			if len(live)+len(condemned) != total {
				t.Errorf("live %d + condemned %d != %d parsed proxies", len(live), len(condemned), total)
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
			p, err := NewMihomoProber(check, config.BandwidthConfig{}, config.GeoBlockConfig{},
				config.CloudflareConfig{}, "", zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
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
	p, err := NewMihomoProber(check, config.BandwidthConfig{}, config.GeoBlockConfig{},
		config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if budget, want := precheckAttempts*p.precheckDialBudget(), check.Timeout; budget != want {
		t.Errorf("pre-check spends %v on an endpoint under the default check.timeout, want %v",
			budget, want)
	}
}
