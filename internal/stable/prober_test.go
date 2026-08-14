package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
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

// precheckProxy exposes only Type and Addr, the two methods filterReachable
// reads. The embedded nil interface panics on anything else, which pins that
// read set.
type precheckProxy struct {
	mihomo.Proxy
	typ  mihomo.AdapterType
	addr string
}

func (p *precheckProxy) Type() mihomo.AdapterType { return p.typ }
func (p *precheckProxy) Addr() string             { return p.addr }

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

// A TCP verdict is only sound for adapters mihomo reaches over TCP. hysteria2,
// tuic and mieru reach their server with ListenPacket over UDP, so condemning
// them on a refused TCP connect would delete those protocols from the output
// wholesale — the failure mode this repo has paid for before.
//
// The UDP cases deliberately sit on the SAME address as a condemned TCP one:
// Merge dedupes on server:port, which does not stop a mierus:// port list from
// colliding with another entry, so the verdict must follow the adapter type
// rather than the address.
func TestFilterReachableCondemnsOnlyDeadTCPEndpoints(t *testing.T) {
	t.Parallel()

	live, dead := liveTCPAddr(t), deadTCPAddr(t)
	pxs := []mihomo.Proxy{
		&precheckProxy{typ: mihomo.Vless, addr: live},
		&precheckProxy{typ: mihomo.Vless, addr: dead},
		&precheckProxy{typ: mihomo.Trojan, addr: dead},
		&precheckProxy{typ: mihomo.Shadowsocks, addr: dead},
		&precheckProxy{typ: mihomo.Hysteria2, addr: dead},
		&precheckProxy{typ: mihomo.Tuic, addr: dead},
		&precheckProxy{typ: mihomo.Mieru, addr: dead},
	}
	want := []mihomo.AdapterType{mihomo.Vless, mihomo.Hysteria2, mihomo.Tuic, mihomo.Mieru}
	wantCondemned := []mihomo.AdapterType{mihomo.Vless, mihomo.Trojan, mihomo.Shadowsocks}

	got, condemned := testProber(t).filterReachable(context.Background(), zerolog.Nop(), pxs)
	if len(got) != len(want) {
		t.Fatalf("kept %d proxies, want %d", len(got), len(want))
	}
	for i, px := range got {
		if px.Type() != want[i] {
			t.Errorf("kept[%d] = %v, want %v", i, px.Type(), want[i])
		}
	}
	// The condemned set is what seeds StageCondemned, so a proxy silently
	// dropped from BOTH slices would lose its dead-cache verdict.
	if len(condemned) != len(wantCondemned) {
		t.Fatalf("condemned %d proxies, want %d", len(condemned), len(wantCondemned))
	}
	for i, px := range condemned {
		if px.Type() != wantCondemned[i] {
			t.Errorf("condemned[%d] = %v, want %v", i, px.Type(), wantCondemned[i])
		}
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
				Timeout:        50 * time.Millisecond,
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
