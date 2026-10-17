package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// startConnectProxy serves HTTP CONNECT then a 204 for the tunneled GET, which
// is the minimum an HTTP outbound URL test and apiProbeOne need to pass. It
// listens on host so a test can put the selected address and the name's system
// answer on different loopback addresses.
func startConnectProxy(t *testing.T, host string) string {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				requestLine, readErr := br.ReadString('\n')
				if readErr != nil {
					return
				}
				readHeaders(br)
				if strings.HasPrefix(requestLine, "CONNECT ") {
					_, _ = fmt.Fprint(c, "HTTP/1.1 200 Connection established\r\n\r\n")
					readHeaders(br)
				}
				_, _ = fmt.Fprint(c, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			}(conn)
		}
	}()

	return ln.Addr().String()
}

func readHeaders(br *bufio.Reader) {
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			return
		}
	}
}

func newBoundProber(t *testing.T) *MihomoProber {
	t.Helper()

	return testProberWith(t, config.CheckConfig{
		Rounds: 1, Concurrency: 2, Timeout: 2 * time.Second,
		TestURL: "http://example.invalid/", ExpectedStatus: "204",
	}, config.BandwidthConfig{}, zerolog.Nop())
}

// TestProbeBoundKeepsALiveSelectedAddressOutOfTheDeadCache is the issue #5
// regression: preprocessing selects 127.0.0.2, the URI still names localhost,
// and localhost resolves to a different loopback address where the port is
// closed. The probe must dial the selected address rather than whichever address
// its own resolver answers, or recordDead stores a zero-success verdict under an
// address neither path tested and the live node stays skipped for the whole
// dead-cache TTL.
func TestProbeBoundKeepsALiveSelectedAddressOutOfTheDeadCache(t *testing.T) {
	proxyAddr := startConnectProxy(t, "127.0.0.2")
	_, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	selected := netip.MustParseAddr("127.0.0.2")
	entries := []Entry{
		{
			Label: "named",
			Raw:   "http://localhost:" + port + "#named",
			Addr:  "localhost:" + port,
			IP:    selected,
		},
		{
			Label: "control",
			Raw:   "http://" + proxyAddr + "#control",
			Addr:  proxyAddr,
			IP:    selected,
		},
	}
	p := newBoundProber(t)
	t.Cleanup(p.ReleaseBindings)

	res, probeErr := probeEntries(context.Background(), p, entries)
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	if got := res["named"].Successes; got != 1 {
		t.Fatalf("named entry Successes = %d, want 1: the probe did not dial the selected address", got)
	}
	if got := res["control"].Successes; got != 1 {
		t.Fatalf("control entry Successes = %d, want 1", got)
	}
	if report := p.PrecheckReport(); report.Refused != 0 {
		t.Fatalf("precheck refused %d endpoints, want 0: %+v", report.Refused, report)
	}

	dead := NewDeadSet(time.Hour)
	(&Checker{dead: dead, logger: zerolog.Nop()}).recordDead(entries, res, RefusalReport{}, p.PrecheckReport())
	for _, e := range entries {
		if dead.Blocked(e.Addr, e.IP) {
			t.Errorf("%s was dead-cached against selected IP %s", e.Label, e.IP)
		}
	}
}

// TestProbeBoundHoldsTheBindingForRetainedAdapters covers the egress lifetime:
// the checker reuses the probe's adapters for the API/trace/bandwidth stages,
// so a binding released when ProbeBound returns would let those dials resolve
// the hostname normally and fail a node the URL test just proved live.
func TestProbeBoundHoldsTheBindingForRetainedAdapters(t *testing.T) {
	proxyAddr := startConnectProxy(t, "127.0.0.2")
	_, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{{
		Label: "named",
		Raw:   "http://localhost:" + port + "#named",
		Addr:  "localhost:" + port,
		IP:    netip.MustParseAddr("127.0.0.2"),
	}}
	p := newBoundProber(t)
	t.Cleanup(p.ReleaseBindings)

	res, probeErr := probeEntries(context.Background(), p, entries)
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	if got := res["named"].Successes; got != 1 {
		t.Fatalf("named entry Successes = %d, want 1", got)
	}
	proxies := p.TakeProbedAdapters()
	if len(proxies) != 1 {
		t.Fatalf("retained %d adapters, want 1", len(proxies))
	}
	t.Cleanup(func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	})

	if reachable, status, _ := apiProbeOne(context.Background(), proxies[0], "http://example.invalid/", nil, 2*time.Second, true); !reachable || status != 204 {
		t.Fatalf("retained adapter after ProbeBound: reachable=%v status=%d, want true/204", reachable, status)
	}

	p.ReleaseBindings()
	if reachable, _, _ := apiProbeOne(context.Background(), proxies[0], "http://example.invalid/", nil, 2*time.Second, true); reachable {
		t.Fatal("the unpinned adapter still reached the listener; the binding was not what made the first dial work")
	}
}

// TestProbeBoundDoesNotAttributeAConflictingSelection covers two entries on one
// hostname that selected different addresses. One resolver host table cannot
// hold both, so the URL tests are left unpinned and marked unbound; the
// per-entry pre-check still dials each selected address, and neither entry may
// be condemned or dead-cached from the other's answer.
func TestProbeBoundDoesNotAttributeAConflictingSelection(t *testing.T) {
	firstAddr := startConnectProxy(t, "127.0.0.2")
	secondAddr := startConnectProxy(t, "127.0.0.3")
	_, firstPort, err := net.SplitHostPort(firstAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, secondPort, err := net.SplitHostPort(secondAddr)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{
			Label: "first",
			Raw:   "http://localhost:" + firstPort + "#first",
			Addr:  "localhost:" + firstPort,
			IP:    netip.MustParseAddr("127.0.0.2"),
		},
		{
			Label: "second",
			Raw:   "http://localhost:" + secondPort + "#second",
			Addr:  "localhost:" + secondPort,
			IP:    netip.MustParseAddr("127.0.0.3"),
		},
	}
	p := newBoundProber(t)
	t.Cleanup(p.ReleaseBindings)

	res, probeErr := probeEntries(context.Background(), p, entries)
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	for _, label := range []string{"first", "second"} {
		r := res[label]
		if r.Stage == StageCondemned {
			t.Errorf("%s was condemned: the pre-check did not dial its own selected address", label)
		}
		if !r.Unbound {
			t.Errorf("%s is not marked unbound; its URL test could not be pinned to both selections", label)
		}
	}
	// Both selected addresses are live, so the per-entry pre-check judged two
	// endpoints and refused none.
	if report := p.PrecheckReport(); report.Dialled != 2 || report.Refused != 0 {
		t.Errorf("precheck = %+v, want Dialled=2 Refused=0", report)
	}

	dead := NewDeadSet(time.Hour)
	(&Checker{dead: dead, logger: zerolog.Nop()}).recordDead(entries, res, RefusalReport{}, p.PrecheckReport())
	for _, e := range entries {
		if dead.Blocked(e.Addr, e.IP) {
			t.Errorf("%s was dead-cached under %s, an address its URL test never dialled", e.Label, e.IP)
		}
	}
}

// TestProbeBoundMatchesATrailingDotHostname covers the spelling mismatch: the
// resolver host trie refuses a dotted name, so the pin has to be installed
// under the undotted spelling while the configured hostname stays the source of
// TLS SNI and Host derivation.
func TestProbeBoundMatchesATrailingDotHostname(t *testing.T) {
	proxyAddr := startConnectProxy(t, "127.0.0.2")
	_, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{{
		Label: "dotted",
		Raw:   "http://localhost.:" + port + "#dotted",
		Addr:  "localhost.:" + port,
		IP:    netip.MustParseAddr("127.0.0.2"),
	}}
	p := newBoundProber(t)
	t.Cleanup(p.ReleaseBindings)

	res, probeErr := probeEntries(context.Background(), p, entries)
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	if got := res["dotted"].Successes; got != 1 {
		t.Fatalf("dotted entry Successes = %d, want 1: the pin did not match the adapter lookup", got)
	}
	if res["dotted"].Unbound {
		t.Error("dotted entry is unbound, want a pinned URL test")
	}

	// The normalisation must leave SNI/Host derivation on the configured name.
	tlsEntry := Entry{
		Label: "tls",
		Raw: "vless://b831381d-6324-4d53-ad4f-8cda48b30811@localhost.:443" +
			"?encryption=none&security=tls&type=ws#tls",
		Addr: "localhost.:443",
		IP:   netip.MustParseAddr("127.0.0.2"),
	}
	b := newBinding([]Entry{tlsEntry})
	mappings, convErr := convert.ConvertsV2Ray(entriesPayload([]Entry{tlsEntry}))
	if convErr != nil {
		t.Fatal(convErr)
	}
	normalisePinnedServers(mappings, b)
	if got := mappings[0]["server"]; got != "localhost" {
		t.Errorf("mapping server = %v, want localhost", got)
	}
	if got := mappings[0]["servername"]; got != "localhost." {
		t.Errorf("TLS servername = %v, want the configured localhost.", got)
	}
	if got := mappings[0]["sni"]; got != "localhost." {
		t.Errorf("sni = %v, want the configured localhost.", got)
	}
	px, parseErr := adapter.ParseProxy(mappings[0])
	if parseErr != nil {
		t.Fatalf("ParseProxy after normalisation: %v", parseErr)
	}
	_ = px.Close()
	if got, want := px.Addr(), "localhost:443"; got != want {
		t.Errorf("adapter Addr() = %q, want %q", got, want)
	}
}

// TestProbeBoundPinsResolutionAndRestoresTheTable pins the mechanism itself:
// the selected-ip.invalid name (which cannot resolve in the system) answers the
// pin while installed, the pre-check dials the selected address, and the
// previous table is put back afterwards.
func TestProbeBoundPinsResolutionAndRestoresTheTable(t *testing.T) {
	const host = "selected-ip.invalid"
	selected := netip.MustParseAddr("127.0.0.2")
	entries := []Entry{{
		Label: "s-001",
		Raw: "vless://b831381d-6324-4d53-ad4f-8cda48b30811@" + host + ":443" +
			"?encryption=none&security=tls&sni=front.example&type=ws&host=front.example#s-001",
		Addr: host + ":443",
		IP:   selected,
	}}

	b := newBinding(entries)
	if got, ok := b.hosts[host]; !ok || got != selected {
		t.Fatalf("host pin = %v/%v, want %s", got, ok, selected)
	}
	restore := pinHosts(b.hosts)
	pinned := true
	t.Cleanup(func() {
		if pinned {
			restore()
		}
	})

	ips, err := resolver.LookupIP(context.Background(), host)
	if err != nil {
		t.Fatalf("pinned lookup: %v", err)
	}
	if !slices.Equal(ips, []netip.Addr{selected}) {
		t.Fatalf("pinned lookup = %v, want [%s]", ips, selected)
	}

	mappings, err := convert.ConvertsV2Ray(entriesPayload(entries))
	if err != nil {
		t.Fatal(err)
	}
	nodes := probeNodes(mappings, b)
	if got := nodes[0].addr; got != net.JoinHostPort(selected.String(), "443") {
		t.Errorf("precheck addr = %q, want the selected address", got)
	}
	if !nodes[0].precheckBound || !nodes[0].bound {
		t.Errorf("position bounds = precheck:%v url:%v, want both true", nodes[0].precheckBound, nodes[0].bound)
	}
	if got := mappings[0]["server"]; got != host {
		t.Errorf("mapping server = %v, want the hostname", got)
	}
	if got := mappings[0]["servername"]; got != "front.example" {
		t.Errorf("TLS servername = %v, want front.example", got)
	}
	px, parseErr := adapter.ParseProxy(mappings[0])
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	t.Cleanup(func() { _ = px.Close() })
	if got, want := px.Addr(), net.JoinHostPort(host, "443"); got != want {
		t.Errorf("adapter Addr() = %q, want %q: the binding must not rewrite the dial target's name", got, want)
	}

	restore()
	pinned = false
	ips, err = resolver.LookupIP(context.Background(), host)
	if err == nil && slices.Contains(ips, selected) {
		t.Fatalf("lookup still returns the pin after restore: %v", ips)
	}
}
