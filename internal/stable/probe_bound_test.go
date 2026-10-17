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
// is the minimum an HTTP outbound URL test needs to pass. It listens on host so
// a test can put the selected address and the name's system answer on different
// loopback addresses.
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
	p := testProberWith(t, config.CheckConfig{
		Rounds: 1, Concurrency: 2, Timeout: 2 * time.Second,
		TestURL: "http://example.invalid/", ExpectedStatus: "204",
	}, config.BandwidthConfig{}, zerolog.Nop())

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

// TestProbeBoundPinsResolutionAndPreservesTheHostname pins both halves of the
// binding contract: the pre-check dials the selected address, while the mapping
// and adapter keep the hostname so TLS SNI and transport Host headers derived
// from it are unchanged. selected-ip.invalid cannot resolve in the system, so a
// lookup returning the pin is the pin doing the work, not the environment.
func TestProbeBoundPinsResolutionAndPreservesTheHostname(t *testing.T) {
	const host = "selected-ip.invalid"
	selected := netip.MustParseAddr("127.0.0.2")
	entries := []Entry{{
		Label: "s-001",
		Raw: "vless://b831381d-6324-4d53-ad4f-8cda48b30811@" + host + ":443" +
			"?encryption=none&security=tls&sni=front.example&type=ws&host=front.example#s-001",
		Addr: host + ":443",
		IP:   selected,
	}}

	pins := selectedIPs(entries)
	if len(pins) != 1 || pins[host] != selected {
		t.Fatalf("selectedIPs = %v, want {%s: %s}", pins, host, selected)
	}
	restore := pinHosts(pins)
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
	nodes := probeNodes(mappings, pins)
	if got := nodes[0].addr; got != net.JoinHostPort(selected.String(), "443") {
		t.Errorf("precheck addr = %q, want the selected address", got)
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
