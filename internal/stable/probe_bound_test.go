package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/common/convert"
	"github.com/metacubex/mihomo/common/structure"
	"github.com/metacubex/mihomo/component/resolver"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/geoblock"
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

// selfSignedCert is enough for the capture: the adapter sets skip-cert-verify,
// so only the handshake and the ClientHello matter.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost", "front.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type wsWire struct {
	sni  string
	host string
}

// captureVlessWS runs the adapter's real StreamConnContext against a local TLS
// listener and reports what went on the wire: the ClientHello SNI and the WS
// upgrade Host header. It is the observable form of the precedence rules, which
// mapping-field assertions cannot pin.
func captureVlessWS(t *testing.T, streamer interface {
	StreamConnContext(context.Context, net.Conn, *mihomo.Metadata) (net.Conn, error)
}) wsWire {
	t.Helper()

	cert := selfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	result := make(chan wsWire, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var sni string
		tlsConn := tls.Server(conn, &tls.Config{
			Certificates: []tls.Certificate{cert},
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				sni = hello.ServerName

				//nolint:nilnil // a nil config makes the server use its base config
				return nil, nil
			},
		})
		if handshakeErr := tlsConn.Handshake(); handshakeErr != nil {
			result <- wsWire{sni: sni}

			return
		}
		reader := bufio.NewReader(tlsConn)
		if _, readErr := reader.ReadString('\n'); readErr != nil {
			result <- wsWire{sni: sni}

			return
		}
		var host string
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || line == "\r\n" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "host:") {
				host = strings.TrimSpace(line[len("host:"):])
			}
		}
		result <- wsWire{sni: sni, host: host}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var meta mihomo.Metadata
	if metaErr := meta.SetRemoteAddress("example.com:443"); metaErr != nil {
		t.Fatal(metaErr)
	}
	_, _ = streamer.StreamConnContext(context.Background(), conn, &meta)

	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the TLS/WS capture")
	}

	return wsWire{}
}

// TestProbeBoundPreservesDottedSNIAndHost is the wire regression for the
// trailing-dot rewrite: mihomo's VLESS ws precedence is servername, then the WS
// Host header, then the adapter address. Injecting servername for a dotted
// origin overrides an explicit host=front.example and changes the transmitted
// SNI; leaving the Host unset changes the default WS Host as well.
func TestProbeBoundPreservesDottedSNIAndHost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hostQ    string
		wantSNI  string
		wantHost string
	}{
		{
			name: "no explicit host preserves the dotted server in both",
			// crypto/tls strips a trailing dot before putting the name in the
			// ClientHello (handshake_client.go hostnameInSNI), so the wire SNI
			// is undotted even when the effective name was dotted. The WS Host
			// is not normalized and is what distinguishes the two paths.
			wantSNI:  "localhost",
			wantHost: "localhost.",
		},
		{
			name:     "explicit ws host still drives SNI and Host",
			hostQ:    "&host=front.example",
			wantSNI:  "front.example",
			wantHost: "front.example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@localhost.:443" +
				"?encryption=none&security=tls&type=ws" + tc.hostQ + "#wire"
			entries := []Entry{{
				Label: "wire",
				Raw:   raw,
				Addr:  "localhost.:443",
				IP:    netip.MustParseAddr("127.0.0.2"),
			}}
			b := newBinding(entries)
			mappings, convErr := convert.ConvertsV2Ray(entriesPayload(entries))
			if convErr != nil {
				t.Fatal(convErr)
			}
			normalisePinnedServers(mappings, allPositions(len(mappings)), b)
			if got := mappings[0]["server"]; got != "localhost" {
				t.Fatalf("mapping server = %v, want localhost", got)
			}
			mappings[0]["skip-cert-verify"] = true
			delete(mappings[0], "client-fingerprint") // use crypto/tls, not uTLS
			decoder := structure.NewDecoder(structure.Option{
				TagName: "proxy", WeaklyTypedInput: true, KeyReplacer: structure.DefaultKeyReplacer,
			})
			var opt outbound.VlessOption
			if decodeErr := decoder.Decode(mappings[0], &opt); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			px, parseErr := outbound.NewVless(opt)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			t.Cleanup(func() { _ = px.Close() })

			got := captureVlessWS(t, px)
			if got.sni != tc.wantSNI {
				t.Errorf("ClientHello SNI = %q, want %q", got.sni, tc.wantSNI)
			}
			if got.host != tc.wantHost {
				t.Errorf("WS Host = %q, want %q", got.host, tc.wantHost)
			}
		})
	}
}

// capturePlainWS reports the Host the WS upgrade request carries, over a
// plaintext pipe. It is the non-TLS counterpart of captureVlessWS.
func capturePlainWS(t *testing.T, streamer interface {
	StreamConnContext(context.Context, net.Conn, *mihomo.Metadata) (net.Conn, error)
}) string {
	t.Helper()

	client, server := net.Pipe()
	result := make(chan string, 1)
	go func() {
		defer server.Close()
		req, readErr := http.ReadRequest(bufio.NewReader(server))
		if readErr != nil {
			result <- ""

			return
		}
		result <- req.Host
	}()

	var meta mihomo.Metadata
	if metaErr := meta.SetRemoteAddress("example.com:80"); metaErr != nil {
		t.Fatal(metaErr)
	}
	_, _ = streamer.StreamConnContext(context.Background(), client, &meta)
	_ = client.Close()

	select {
	case host := <-result:
		return host
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the plaintext WS request")
	}

	return ""
}

// TestProbeBoundPreservesPlainVMessWSHost covers the VMess difference the VLESS
// path does not have: without TLS, mihomo's VMess WS keeps the server-derived
// Host (vmess.go:104-148), while VLESS replaces an empty Host with a random
// camouflage name. Stripping the dot must therefore pin the Host for VMess even
// without TLS.
func TestProbeBoundPreservesPlainVMessWSHost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hostQ    string
		wantHost string
	}{
		{name: "default host keeps the dotted server", wantHost: "localhost."},
		{name: "explicit host is unchanged", hostQ: "front.example", wantHost: "front.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			px := vmessWSAdapter(t, tc.hostQ)
			if got := capturePlainWS(t, px); got != tc.wantHost {
				t.Errorf("plaintext VMess WS Host = %q, want %q", got, tc.wantHost)
			}
		})
	}
}

// vmessWSAdapter converts, normalises and parses one dotted plaintext VMess WS
// mapping, the shape the wire test drives.
func vmessWSAdapter(t *testing.T, hostQ string) *outbound.Vmess {
	t.Helper()

	node := map[string]any{
		"v": "2", "ps": "wire", "add": "localhost.", "port": "443",
		"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "aid": "0",
		"net": "ws", "type": "none", "tls": "", "path": "/", "scy": "auto",
	}
	if hostQ != "" {
		node["host"] = hostQ
	}
	body, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{{
		Label: "vmess",
		Raw:   "vmess://" + base64.StdEncoding.EncodeToString(body),
		Addr:  "localhost.:443",
		IP:    netip.MustParseAddr("127.0.0.2"),
	}}
	b := newBinding(entries)
	mappings, err := convert.ConvertsV2Ray(entriesPayload(entries))
	if err != nil {
		t.Fatal(err)
	}
	normalisePinnedServers(mappings, allPositions(len(mappings)), b)
	if got := mappings[0]["server"]; got != "localhost" {
		t.Fatalf("mapping server = %v, want localhost", got)
	}
	decoder := structure.NewDecoder(structure.Option{
		TagName: "proxy", WeaklyTypedInput: true, KeyReplacer: structure.DefaultKeyReplacer,
	})
	var opt outbound.VmessOption
	if err = decoder.Decode(mappings[0], &opt); err != nil {
		t.Fatal(err)
	}
	px, err := outbound.NewVmess(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = px.Close() })

	return px
}

// TestNormaliseSkipsCondemnedMappings pins the allocation gate: the pre-check
// runs before normalisation, so a mapping it condemns must keep its original
// spelling and allocate no transport-preservation carrier.
func TestNormaliseSkipsCondemnedMappings(t *testing.T) {
	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@localhost.:443" +
		"?encryption=none&security=none&type=tcp&headerType=http#condemned"
	entries := []Entry{{
		Label: "condemned",
		Raw:   raw,
		Addr:  "localhost.:443",
		IP:    netip.MustParseAddr("127.0.0.2"),
	}}
	b := newBinding(entries)
	mappings, convErr := convert.ConvertsV2Ray(entriesPayload(entries))
	if convErr != nil {
		t.Fatal(convErr)
	}

	normalisePinnedServers(mappings, nil, b)
	if got := mappings[0]["server"]; got != "localhost." {
		t.Errorf("condemned server = %v, want the untouched localhost.", got)
	}
	if httpHeaderHostPresent(mappings[0]) {
		t.Error("condemned mapping allocated an http Host carrier")
	}

	normalisePinnedServers(mappings, []int{0}, b)
	if got := mappings[0]["server"]; got != "localhost" {
		t.Errorf("live server = %v, want localhost", got)
	}
	if !httpHeaderHostPresent(mappings[0]) {
		t.Error("live mapping is missing the preserved http Host")
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
	normalisePinnedServers(mappings, allPositions(len(mappings)), b)
	if got := mappings[0]["server"]; got != "localhost" {
		t.Errorf("mapping server = %v, want localhost", got)
	}
	if got, present := mappings[0]["servername"]; present {
		t.Errorf("servername = %v, want absent: the WS Host carries the derived name", got)
	}
	if host, ok := wsHeaderHost(mappings[0]); !ok || host != "localhost." {
		t.Errorf("WS Host = %q/%v, want the configured localhost.", host, ok)
	}
	px, parseErr := adapter.ParseProxy(mappings[0])
	if parseErr != nil {
		t.Fatalf("ParseProxy after normalisation: %v", parseErr)
	}
	_ = px.Close()
	if got, want := px.Addr(), "localhost:443"; got != want {
		t.Errorf("adapter Addr() = %q, want %q", got, want)
	}

	// The adapter's dial spelling feeds the geoblock writer, while the raw
	// entry keeps the dotted name. Both must address one store entry.
	store, err := geoblock.Open(filepath.Join(t.TempDir(), "gb.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if blockErr := store.Block(proxyHost(px)); blockErr != nil {
		t.Fatal(blockErr)
	}
	if !store.Blocked("localhost.") {
		t.Fatal("the normalized adapter host did not block the raw dotted host")
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
