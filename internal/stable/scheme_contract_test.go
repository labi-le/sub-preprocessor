package stable //nolint:testpackage // drives the whole chain into entryLabel, an unexported internal

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"
	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// A node scheme is only "supported" if it survives the WHOLE production chain:
// subscription.Normalize -> subscription.Parse -> Merge (which relabels) ->
// convert.ConvertsV2Ray -> adapter.ParseProxy -> entryLabel. Each hop is
// covered by its own unit tests, but every bug this file was written for lived
// in a SEAM: a relabel the parser accepted and mihomo then refused, a proxy
// name mihomo emitted and entryLabel could not fold back. So the table drives
// one representative link per scheme end to end and pins what comes out at
// every hop.
//
// Every case asserts the mihomo mapping COUNT, never merely the absence of an
// error: convert.ConvertsV2Ray drops a line it cannot decode with `continue`
// and returns no error as long as some other line converted, so a mapping bug
// surfaces as a missing node rather than as a failure. In production that is a
// node which is parsed, merged, published and probed, never answers, and ends
// up in the 2h dead cache under its server:port — where Merge's first-wins
// dedupe lets it shadow a working node of another scheme.
//
// The chain is conversion and parsing only: adapter.ParseProxy builds an
// outbound without dialling anything, so the table stays a sub-second unit
// test rather than a probe.

const (
	contractSource = "src"
	// contractLabel is what Merge names the first kept node of contractSource;
	// the whole point of the relabel is that this string is what comes back out
	// of entryLabel at the far end.
	contractLabel = contractSource + "-001"
	// contractTags is the [GEO][IP] prefix the publication folds into a node's
	// name, and contractOriginName is the upstream name every fixture carries.
	// The tags are load-bearing: the published line is a strictly harder line
	// than Raw — a SECOND RewriteSSRName round trip for ssr, and for everything
	// else a fragment carrying '[', ']', a space and a ':' that mihomo's own
	// url.Parse and mieru's "<name>:<port>/<protocol>" naming both have to
	// survive.
	contractTags        = "[GEO:NL][IP:1.2.3.4]"
	contractOriginName  = contractTags + " Origin"
	contractTaggedLabel = contractTags + " " + contractLabel
)

// contractAnnotator renders exactly contractTags through the same
// rewrite.NodeName the production chain uses, so the published half of the
// table below is the line a consumer's mihomo really parses out of
// /stable.txt.
type contractAnnotator struct{}

func (contractAnnotator) Annotate(
	_ context.Context, dst, _ *bytes.Buffer, req preprocess.AnnotateRequest,
) geofeed.CountryCode {
	rewrite.NodeName(dst, req.Node, contractTags)

	return geofeed.CountryCode{'N', 'L'}
}

// sourceNodes turns a normalized subscription body into the per-node results
// the preprocess IP stage hands the worker, so these fixtures stay written as
// the source text they model.
func sourceNodes(payload []byte) SourceBody {
	var nodes []preprocess.NodeResult
	for line := range strings.SplitSeq(string(payload), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			nodes = append(nodes, preprocess.NodeResult{Raw: line})
		}
	}

	return SourceBody{Name: contractSource, Nodes: nodes}
}

// publishedLine is the single line BuildPayload puts on /stable.txt for e.
func publishedLine(t *testing.T, e Entry) string {
	t.Helper()

	payload := BuildPayload(context.Background(), contractAnnotator{}, []Survivor{{Entry: e}})

	return strings.TrimSuffix(string(payload), "\n")
}

// These credentials are structurally valid on purpose: adapter.ParseProxy
// validates a vless UUID, an x25519 public key and a hex short ID, so
// placeholder strings would fail the parse for a reason that has nothing to do
// with the chain under test.
const (
	contractUUID       = "ab7d5ea9-6eca-47c3-b14b-67378fc2d7c2"
	contractRealityKey = "pHQke94AM1SmUNRlA7CNyXL4dK9-O1mTZoNVJKVFTk0"
	contractShortID    = "1dbf8a58bc15d4b0"
)

// schemeContract is one representative share link plus everything the
// production chain must make of it.
type schemeContract struct {
	name   string
	line   string
	scheme subscription.Scheme
	server string
	port   string
	typ    mihomo.AdapterType
	// addrs holds one expected proxy Addr() per mihomo mapping the RELABELED
	// Entry.Raw must expand into, in mihomo's emission order. Exactly one for
	// every scheme but mierus://, which mihomo expands per configured port.
	addrs []string
}

// contractSSSIP002Line renders a SIP002 ss:// link: "<b64(method:password)>@host:port".
func contractSSSIP002Line(server, port string) string {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	return "ss://" + userinfo + "@" + server + ":" + port + "#" + contractOriginName
}

// contractSSLegacyLine renders a pre-SIP002 ss:// link, whose WHOLE authority
// is unpadded RawStd base64 of "method:password@host:port" — there is no host
// in the URI to read. mihomo takes this branch whenever url.Port() is empty
// (common/convert/converter.go:396-407).
func contractSSLegacyLine(t *testing.T, server, port string) string {
	t.Helper()

	authority := base64.RawStdEncoding.EncodeToString([]byte("aes-256-gcm:secret@" + server + ":" + port))
	// '/' ends a URI authority and '+' is not a host byte, for url.Parse and
	// for splitHostPort alike, so either one would break this fixture for a
	// reason unrelated to the chain. Guarded rather than commented because the
	// plaintext above is the obvious thing a maintainer edits.
	if strings.ContainsAny(authority, "+/") {
		t.Fatalf("legacy ss fixture encodes to %q, which is not URI-safe; pick another password", authority)
	}

	return "ss://" + authority + "#" + contractOriginName
}

// contractSSRLine renders an ssr:// link. Its base64 payload is
// "host:port:protocol:method:obfs:password/?query" and its display name is the
// query's base64 "remarks", so nothing the node needs is readable from the URI.
// frag, when set, is the plain-text fragment a source may append — the one
// mihomo chokes on, since it base64-decodes EVERYTHING after "ssr://".
func contractSSRLine(server, port, remarks, frag string) string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	payload := server + ":" + port + ":origin:aes-256-cfb:plain:" + b64("secret") +
		"/?obfsparam=" + b64("obfs.example.com") + "&remarks=" + b64(remarks)
	line := "ssr://" + b64(payload)
	if frag != "" {
		line += "#" + frag
	}

	return line
}

// contractVmessLine renders a v2rayN-styled vmess:// link: base64 of a JSON
// object whose "add"/"port"/"ps" carry the server, port and display name.
func contractVmessLine(server, port, name string) string {
	doc := `{"v":"2","ps":"` + name + `","add":"` + server + `","port":"` + port +
		`","id":"` + contractUUID + `","aid":"0","net":"tcp","type":"none","tls":"tls","sni":"a.example","scy":"auto"}`

	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(doc))
}

func schemeContracts(t *testing.T) []schemeContract {
	t.Helper()

	return []schemeContract{{
		name:   "vless_tls",
		line:   "vless://" + contractUUID + "@1.2.3.1:443?encryption=none&security=tls&sni=a.example&type=tcp#" + contractOriginName,
		scheme: "vless",
		server: "1.2.3.1",
		port:   "443",
		typ:    mihomo.Vless,
		addrs:  []string{"1.2.3.1:443"},
	}, {
		name: "vless_reality_grpc",
		line: "vless://" + contractUUID + "@1.2.3.2:8443?encryption=none&security=reality&type=grpc" +
			"&serviceName=grpc&sni=tesla.com&fp=chrome&pbk=" + contractRealityKey + "&sid=" + contractShortID + "#" + contractOriginName,
		scheme: "vless",
		server: "1.2.3.2",
		port:   "8443",
		typ:    mihomo.Vless,
		addrs:  []string{"1.2.3.2:8443"},
	}, {
		name:   "vmess_base64_json",
		line:   contractVmessLine("1.2.3.3", "443", contractOriginName),
		scheme: subscription.SchemeVmess,
		server: "1.2.3.3",
		port:   "443",
		typ:    mihomo.Vmess,
		addrs:  []string{"1.2.3.3:443"},
	}, {
		name:   "trojan",
		line:   "trojan://secret@1.2.3.4:443?sni=a.example&type=tcp#" + contractOriginName,
		scheme: "trojan",
		server: "1.2.3.4",
		port:   "443",
		typ:    mihomo.Trojan,
		addrs:  []string{"1.2.3.4:443"},
	}, {
		name:   "ss_sip002",
		line:   contractSSSIP002Line("1.2.3.5", "8388"),
		scheme: subscription.SchemeSS,
		server: "1.2.3.5",
		port:   "8388",
		typ:    mihomo.Shadowsocks,
		addrs:  []string{"1.2.3.5:8388"},
	}, {
		name:   "ss_legacy_base64_authority",
		line:   contractSSLegacyLine(t, "1.2.3.6", "8389"),
		scheme: subscription.SchemeSS,
		server: "1.2.3.6",
		port:   "8389",
		typ:    mihomo.Shadowsocks,
		addrs:  []string{"1.2.3.6:8389"},
	}, {
		name:   "ssr",
		line:   contractSSRLine("1.2.3.7", "8390", contractOriginName, ""),
		scheme: subscription.SchemeSSR,
		server: "1.2.3.7",
		port:   "8390",
		typ:    mihomo.ShadowsocksR,
		addrs:  []string{"1.2.3.7:8390"},
	}, {
		name:   "hysteria_v1",
		line:   "hysteria://1.2.3.8:443?auth=secret&peer=a.example&protocol=udp&upmbps=100&downmbps=100&alpn=hysteria#" + contractOriginName,
		scheme: "hysteria",
		server: "1.2.3.8",
		port:   "443",
		typ:    mihomo.Hysteria,
		addrs:  []string{"1.2.3.8:443"},
	}, {
		name:   "hysteria2",
		line:   "hysteria2://secret@1.2.3.9:8443?sni=a.example&alpn=h3#" + contractOriginName,
		scheme: "hysteria2",
		server: "1.2.3.9",
		port:   "8443",
		typ:    mihomo.Hysteria2,
		addrs:  []string{"1.2.3.9:8443"},
	}, {
		// hy2:// is an alias mihomo maps onto type "hysteria2", so the parsed
		// Scheme and the adapter type deliberately disagree here.
		name:   "hy2_alias",
		line:   "hy2://secret@1.2.3.10:8444?sni=a.example&alpn=h3#" + contractOriginName,
		scheme: "hy2",
		server: "1.2.3.10",
		port:   "8444",
		typ:    mihomo.Hysteria2,
		addrs:  []string{"1.2.3.10:8444"},
	}, {
		name:   "tuic",
		line:   "tuic://" + contractUUID + ":secret@1.2.3.11:443?congestion_control=bbr&alpn=h3&sni=a.example&udp_relay_mode=native#" + contractOriginName,
		scheme: "tuic",
		server: "1.2.3.11",
		port:   "443",
		typ:    mihomo.Tuic,
		addrs:  []string{"1.2.3.11:443"},
	}, {
		name:   "anytls",
		line:   "anytls://user:secret@1.2.3.12:8443?sni=a.example&insecure=1#" + contractOriginName,
		scheme: "anytls",
		server: "1.2.3.12",
		port:   "8443",
		typ:    mihomo.AnyTLS,
		addrs:  []string{"1.2.3.12:8443"},
	}, {
		name:   "socks5_with_port",
		line:   "socks5://user:secret@1.2.3.13:1080#" + contractOriginName,
		scheme: "socks5",
		server: "1.2.3.13",
		port:   "1080",
		typ:    mihomo.Socks5,
		addrs:  []string{"1.2.3.13:1080"},
	}, {
		name:   "http_with_port",
		line:   "http://user:secret@1.2.3.14:8080#" + contractOriginName,
		scheme: "http",
		server: "1.2.3.14",
		port:   "8080",
		typ:    mihomo.Http,
		addrs:  []string{"1.2.3.14:8080"},
	}, {
		// https:// only adds tls=true to the same mihomo "http" proxy; the row
		// exists because the portless https:// line is the negative boundary
		// below, and a gate that rejected BOTH forms would look just as green.
		name:   "https_with_port",
		line:   "https://user:secret@1.2.3.15:8443#" + contractOriginName,
		scheme: "https",
		server: "1.2.3.15",
		port:   "8443",
		typ:    mihomo.Http,
		addrs:  []string{"1.2.3.15:8443"},
	}, {
		name:   "mierus_single_port",
		line:   "mierus://user:secret@1.2.3.16?port=2999&protocol=TCP#" + contractOriginName,
		scheme: subscription.SchemeMieru,
		server: "1.2.3.16",
		port:   "2999",
		typ:    mihomo.Mieru,
		addrs:  []string{"1.2.3.16:2999"},
	}, {
		// The one row whose mapping count exceeds one: mihomo expands a
		// mierus:// link into one proxy per port/protocol pair, named
		// "<label>:<port>/<protocol>". Every one of them must fold back onto
		// the single Entry.Label, and the Entry keeps only the FIRST port as
		// its dedupe/dead-cache key — multi-port mieru is best-of-ports, so a
		// second key would let one dead port bury the node. A port RANGE
		// resolves to its first port at the adapter (mihomo
		// adapter/outbound/mieru.go:151-157).
		name:   "mierus_multi_port",
		line:   "mierus://user:secret@1.2.3.17?port=2999&protocol=TCP&port=9998-9999&protocol=UDP#" + contractOriginName,
		scheme: subscription.SchemeMieru,
		server: "1.2.3.17",
		port:   "2999",
		typ:    mihomo.Mieru,
		addrs:  []string{"1.2.3.17:2999", "1.2.3.17:9998"},
	}}
}

func TestSchemeContractEndToEnd(t *testing.T) {
	t.Parallel()

	for _, c := range schemeContracts(t) {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			payload := subscription.Normalize([]byte(c.line + "\n"))
			c.assertParsed(t, payload)
			e := c.assertMerged(t, payload)
			// Both published forms, because they are different lines: Raw is
			// what the prober converts, the annotated line is what a consumer's
			// mihomo parses out of /stable.txt.
			c.assertProxies(t, "Raw", e.Raw, e.Label)
			published := publishedLine(t, e)
			if published == e.Raw {
				t.Fatalf("BuildPayload published %q unannotated; the tagged half below would be a copy of Raw", published)
			}
			c.assertProxies(t, "published", published, contractTaggedLabel)
		})
	}
}

// assertParsed pins hop 2: the scheme, server and port subscription.Parse reads
// out of the link, and that it read them without rejecting anything.
func (c schemeContract) assertParsed(t *testing.T, payload []byte) {
	t.Helper()

	var nodes []subscription.Node
	rejected := subscription.Parse(payload, func(n subscription.Node) bool {
		nodes = append(nodes, n)

		return true
	})
	if len(nodes) != 1 || rejected != 0 {
		t.Fatalf("Parse(%q) = %d nodes, %d rejected; want 1, 0", c.line, len(nodes), rejected)
	}
	if got := nodes[0].Scheme; got != c.scheme {
		t.Errorf("Scheme = %q, want %q", got, c.scheme)
	}
	if got := nodes[0].Server; got != c.server {
		t.Errorf("Server = %q, want %q", got, c.server)
	}
	if got := nodes[0].Port; got != c.port {
		t.Errorf("Port = %q, want %q", got, c.port)
	}
}

// assertMerged pins hop 3 and returns the single relabeled Entry the rest of
// the chain runs on.
func (c schemeContract) assertMerged(t *testing.T, payload []byte) Entry {
	t.Helper()

	entries := Merge([]SourceBody{sourceNodes(payload)})
	if len(entries) != 1 {
		t.Fatalf("Merge(%q) = %d entries, want 1", c.line, len(entries))
	}
	e := entries[0]
	if e.Label != contractLabel {
		t.Errorf("Entry.Label = %q, want %q", e.Label, contractLabel)
	}
	// Merge keys the dead cache on the lowercased server:port it parsed, so
	// deriving the expectation from this row's own Server/Port expectations is
	// what catches a key built from something else — the mieru rows above,
	// which keep only the first of several ports, being the interesting case.
	if wantAddr := c.server + ":" + c.port; e.Addr != wantAddr {
		t.Errorf("Entry.Addr = %q, want %q", e.Addr, wantAddr)
	}

	return e
}

// assertProxies pins the last three hops for one published line: how many
// mihomo proxies it becomes, what each one is, and that every one of them
// carries wantName.
//
// entryLabel is the inverse of mihomo's naming, so one assertion covers both
// the single-proxy schemes and mierus://, which mihomo expands into one proxy
// per port named "<name>:<port>/<protocol>". Only the Raw call reproduces
// production — nothing folds an annotated proxy name back — but running it over
// a name containing '[', ']', a space and a ':' is what proves the fold cuts at
// the port suffix rather than at the first colon it finds.
func (c schemeContract) assertProxies(t *testing.T, which, line, wantName string) {
	t.Helper()

	for i, px := range contractProxies(t, line, len(c.addrs)) {
		if got := px.Type(); got != c.typ {
			t.Errorf("%s proxy[%d] %q: Type = %v, want %v", which, i, px.Name(), got, c.typ)
		}
		if got := px.Addr(); got != c.addrs[i] {
			t.Errorf("%s proxy[%d] %q: Addr = %q, want %q", which, i, px.Name(), got, c.addrs[i])
		}
		if got := entryLabel(px); got != wantName {
			t.Errorf("%s entryLabel(%q) = %q, want %q", which, px.Name(), got, wantName)
		}
	}
}

// contractProxies runs one published node line through the prober's own front
// end and returns the wantCount proxies it must yield, in emission order.
//
// adapter.ParseProxy hands back an outbound holding a dialer (and, for mieru, a
// client), so each one is closed exactly once via t.Cleanup — never dialled,
// only built.
func contractProxies(t *testing.T, raw string, wantCount int) []mihomo.Proxy {
	t.Helper()

	mappings, err := convert.ConvertsV2Ray([]byte(raw))
	if err != nil {
		t.Fatalf("ConvertsV2Ray(%q): %v", raw, err)
	}
	if len(mappings) != wantCount {
		t.Fatalf("ConvertsV2Ray(%q) = %d mappings, want %d", raw, len(mappings), wantCount)
	}

	pxs := make([]mihomo.Proxy, 0, wantCount)
	for _, m := range mappings {
		px, parseErr := adapter.ParseProxy(m)
		if parseErr != nil {
			t.Fatalf("ParseProxy(%v) from %q: %v", m, raw, parseErr)
		}
		t.Cleanup(func() { _ = px.Close() })
		pxs = append(pxs, px)
	}

	return pxs
}

// TestSchemeContractRejectsPortlessProxyLine: an http/https/socks node is
// host:port by definition and mihomo refuses a portless one
// (common/convert/converter.go:543-546), so a bare web URL in a source body — a
// Telegram channel link, a panel notice — must be counted as unsupported rather
// than published as a node. The rejected count is the assertion that matters:
// it is what surfaces as preprocess.Stats.Unsupported, so a silent skip would
// lose the line with nothing accounting for it.
func TestSchemeContractRejectsPortlessProxyLine(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"https://t.me/somechannel",
		"http://panel.example/sub?token=abc",
		"socks5://1.2.3.4",
	} {
		t.Run(line, func(t *testing.T) {
			t.Parallel()

			payload := subscription.Normalize([]byte(line + "\n"))
			nodes := 0
			rejected := subscription.Parse(payload, func(subscription.Node) bool {
				nodes++

				return true
			})
			if nodes != 0 || rejected != 1 {
				t.Fatalf("Parse(%q) = %d nodes, %d rejected; want 0, 1", line, nodes, rejected)
			}
			if entries := Merge([]SourceBody{sourceNodes(payload)}); len(entries) != 0 {
				t.Fatalf("Merge(%q) = %d entries, want 0: %+v", line, len(entries), entries)
			}
		})
	}
}

// TestSchemeContractWireguardConvertsToNothing pins a BOUNDARY, not a bug.
//
// wireguard:// is a well-formed node URI, so the scheme-generic parser accepts
// it and Merge publishes it — deliberately: refusing it would book it as
// Stats.Unsupported, blaming the source for a client-side gap. mihomo v1.19.27
// has no wireguard case in its share-link converter, so the node yields no
// proxy at all and is simply never selected. That is the honest state of the
// world; if a mihomo bump ever adds the case, this test fails and the row
// belongs in schemeContracts above.
//
// The two-line assertion is the production shape: a payload is converted whole,
// so the wireguard line vanishes with NO error as long as one sibling line
// converted — which is exactly why every case in this file counts mappings.
func TestSchemeContractWireguardConvertsToNothing(t *testing.T) {
	t.Parallel()

	const wgLine = "wireguard://ZGVhZGJlZWZkZWFkYmVlZg@1.2.3.20:51820?reserved=0,0,0#" + contractOriginName
	vlessLine := "vless://" + contractUUID + "@1.2.3.21:443?encryption=none&security=tls&type=tcp#" + contractOriginName

	entries := Merge([]SourceBody{sourceNodes([]byte(wgLine + "\n" + vlessLine + "\n"))})
	if len(entries) != 2 {
		t.Fatalf("Merge = %d entries, want 2 (wireguard is parsed, not rejected): %+v", len(entries), entries)
	}
	wg, vless := entries[0], entries[1]
	if wg.Addr != "1.2.3.20:51820" {
		t.Errorf("wireguard Entry.Addr = %q, want 1.2.3.20:51820", wg.Addr)
	}

	if mappings, err := convert.ConvertsV2Ray([]byte(wg.Raw)); err == nil || len(mappings) != 0 {
		t.Fatalf("ConvertsV2Ray(%q) = %d mappings, err %v; want 0 and the format-invalid error",
			wg.Raw, len(mappings), err)
	}

	mappings, err := convert.ConvertsV2Ray([]byte(wg.Raw + "\n" + vless.Raw + "\n"))
	if err != nil {
		t.Fatalf("ConvertsV2Ray(wireguard+vless): %v", err)
	}
	if len(mappings) != 1 {
		t.Fatalf("ConvertsV2Ray(wireguard+vless) = %d mappings, want 1 (the vless alone)", len(mappings))
	}
	if got, _ := mappings[0]["name"].(string); got != vless.Label {
		t.Errorf("surviving mapping name = %q, want %q", got, vless.Label)
	}
}

// TestSchemeContractSSRSurvivesRelabelFragmentFree is the regression that
// motivated RewriteSSRName. mihomo base64-decodes EVERYTHING after "ssr://"
// (common/convert/converter.go:476-479), so the generic "<raw>#<label>" relabel
// yields a link that converts to NOTHING: the label then misses in the
// probe-result map, SelectSurvivors drops the entry, and the checker books
// server:port into the 2h dead cache where it can shadow a working node.
//
// Both halves are asserted, because the published line being fragment-free is
// only meaningful if a fragment is what would break it.
func TestSchemeContractSSRSurvivesRelabelFragmentFree(t *testing.T) {
	t.Parallel()

	// The remarks carry the [GEO][IP] tags so the published line is a second
	// RewriteSSRName product rather than a copy of Entry.Raw; the published
	// fragment assertion below is vacuous otherwise.
	line := contractSSRLine("1.2.3.22", "8388", contractOriginName, "Original")
	if !strings.Contains(line, "#") {
		t.Fatalf("fixture %q carries no fragment, so it proves nothing", line)
	}

	entries := Merge([]SourceBody{sourceNodes([]byte(line + "\n"))})
	if len(entries) != 1 {
		t.Fatalf("Merge = %d entries, want 1", len(entries))
	}
	e := entries[0]
	if strings.Contains(e.Raw, "#") {
		t.Errorf("relabeled ssr Raw carries a fragment, which mihomo cannot decode: %q", e.Raw)
	}
	if published := publishedLine(t, e); strings.Contains(published, "#") {
		t.Errorf("published ssr line carries a fragment, which mihomo cannot decode: %q", published)
	}

	pxs := contractProxies(t, e.Raw, 1)
	if got := pxs[0].Name(); got != e.Label {
		t.Errorf("proxy name = %q, want the relabeled %q; remarks was not rewritten", got, e.Label)
	}

	// The counterfactual: the same payload with a fragment appended is what the
	// generic relabel path would have published.
	if mappings, err := convert.ConvertsV2Ray([]byte(e.Raw + "#" + e.Label)); err == nil || len(mappings) != 0 {
		t.Fatalf("a fragment-carrying ssr link converted to %d mappings (err %v); mihomo's TryDecodeBase64 no longer "+
			"chokes on it, so RewriteSSRName's reason for existing has changed", len(mappings), err)
	}
}
