package subscription_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// TestParsePortlessProxySchemesRejected: mihomo's converter defaults a portless
// link to 443 for exactly ONE scheme, hysteria2 (convert/converter.go:85-89).
// Every other portless line dies in mihomo: handleVShareLink refuses vless on
// url.Port() == "" (common/convert/v.go:20-22), the structure decoder refuses
// the empty-string port trojan/tuic/hysteria put in "port", and an unknown
// scheme has no converter case at all. Publishing them under a fabricated 443
// would spend a probe slot and a dead-cache entry on a port the node does not
// have.
func TestParsePortlessProxySchemesRejected(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"vless://uuid@example.com?security=tls#Example",
		"trojan://pw@example.net#Other",
		"tuic://uuid:secret@example.org#T",
		"hysteria://pw@example.org#H",
		"anytls://pw@example.net#A",
		"future-scheme://u@example.com#U",
	} {
		t.Run(line, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, line)
		})
	}
}

// TestParsePortlessHysteria2Defaults443 mirrors the one default mihomo's own
// converter applies (convert/converter.go:85-89), so a portless hysteria2 link
// is served on 443 rather than dropped here. The uppercase row pins scheme
// normalization: the default must fire for "HYSTERIA2://" too.
func TestParsePortlessHysteria2Defaults443(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		line string
		name string
	}{
		{"hysteria2://auth@example.com#H2", "H2"},
		{"hy2://auth@example.net#Y", "Y"},
		{"HYSTERIA2://auth@example.org#Upper", "Upper"},
	} {
		node := mustParseOne(t, tc.line)
		if node.Port != "443" {
			t.Errorf("%q: port = %q, want 443", tc.line, node.Port)
		}
		if node.Name != tc.name {
			t.Errorf("%q: name = %q, want %q", tc.line, node.Name, tc.name)
		}
	}
}

func TestNormalizeBase64(t *testing.T) {
	t.Parallel()

	raw := "vless://uuid@example.com:443?security=tls#Node 1\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	if got := string(subscription.Normalize([]byte(encoded))); got != strings.TrimSpace(raw) {
		t.Fatalf("unexpected normalize result: %q", got)
	}
}

func TestNormalizeRawBase64(t *testing.T) {
	t.Parallel()

	raw := "vless://uuid@example.com:443?security=tls#Node 1\n"
	encoded := base64.RawStdEncoding.EncodeToString([]byte(raw))
	if got := string(subscription.Normalize([]byte(encoded))); got != strings.TrimSpace(raw) {
		t.Fatalf("unexpected normalize result: %q", got)
	}
}

func TestNormalizeBase64WithWhitespace(t *testing.T) {
	t.Parallel()

	raw := "vless://uuid@example.com:443?security=tls#Node 1\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	spaced := "  \n\t" + encoded[:8] + "\n" + encoded[8:] + "\t  "
	if got := string(subscription.Normalize([]byte(spaced))); got != strings.TrimSpace(raw) {
		t.Fatalf("unexpected normalize result: %q", got)
	}
}

func TestNormalizeInvalidReturnsOriginal(t *testing.T) {
	t.Parallel()

	input := []byte("this is not base64!!!")
	if got := string(subscription.Normalize(input)); got != string(input) {
		t.Fatalf("unexpected normalize fallback: %q", got)
	}
}

func TestNormalizeInvalidWithWhitespaceReturnsOriginal(t *testing.T) {
	t.Parallel()

	input := []byte("  this is\nnot\tbase64!!!  ")
	want := strings.TrimSpace(string(input))
	if got := string(subscription.Normalize(input)); got != want {
		t.Fatalf("unexpected normalize fallback: %q", got)
	}
}

func mustParseOne(t *testing.T, line string) subscription.Node {
	t.Helper()
	node, count := parseOne(t, line)
	if count != 1 {
		t.Fatalf("got %d nodes from %q, want 1", count, line)
	}
	return node
}

// rejectOne asserts that a URI-shaped line yields no node AND is counted as
// rejected, so a regression that silently drops the line — invisible in
// Stats.Unsupported — cannot pass.
func rejectOne(t *testing.T, line string) {
	t.Helper()
	kept := 0
	rejected := subscription.Parse([]byte(line), func(subscription.Node) bool {
		kept++
		return true
	})
	if kept != 0 || rejected != 1 {
		t.Fatalf("%q: kept %d nodes and counted %d rejected, want 0 kept and 1 rejected", line, kept, rejected)
	}
}

func TestNormalizeURLSafeBase64(t *testing.T) {
	t.Parallel()

	raw := "vless://uuid@example.com:443?security=tls#Node ??>??>\n"
	encoded := base64.URLEncoding.EncodeToString([]byte(raw))
	if !strings.ContainsAny(encoded, "-_") {
		t.Fatal("test payload does not exercise the URL-safe alphabet")
	}
	if got := string(subscription.Normalize([]byte(encoded))); got != strings.TrimSpace(raw) {
		t.Fatalf("unexpected normalize result: %q", got)
	}
}

func TestNormalizeRawURLSafeBase64(t *testing.T) {
	t.Parallel()

	raw := "vless://uuid@example.com:443?security=tls#Node ??>??>\n"
	encoded := base64.RawURLEncoding.EncodeToString([]byte(raw))
	if !strings.ContainsAny(encoded, "-_") {
		t.Fatal("test payload does not exercise the URL-safe alphabet")
	}
	if got := string(subscription.Normalize([]byte(encoded))); got != strings.TrimSpace(raw) {
		t.Fatalf("unexpected normalize result: %q", got)
	}
}

func TestParseFragmentUsesFirstHash(t *testing.T) {
	t.Parallel()

	line := "trojan://p@example.com:443#My#Node"
	node := mustParseOne(t, line)
	if node.Name != "My#Node" {
		t.Errorf("name: got %q, want %q", node.Name, "My#Node")
	}
	wantIdx := strings.IndexByte(line, '#')
	if node.FragmentIdx != wantIdx {
		t.Errorf("fragmentIdx: got %d, want %d", node.FragmentIdx, wantIdx)
	}
}

// TestParseRejectsProseSchemes pins PP-07: the text before the first "://" must
// have the RFC 3986 scheme shape. Before the check, every one of these lines
// became a Node whose Scheme was the surrounding prose and whose Server was the
// URL's host, so an HTML error page or a Clash YAML document parsed as a
// perfectly healthy subscription.
func TestParseRejectsProseSchemes(t *testing.T) {
	t.Parallel()

	lines := []string{
		`<a href="https://example.com/login">Sign in</a>`,
		"- url: https://example.com/sub",
		"see https://example.com",
		"x#note vless://uuid@example.com:443",
		"://example.com",
	}
	for _, line := range lines {
		if _, count := parseOne(t, line); count != 0 {
			t.Errorf("%q parsed as a node, want rejected", line)
		}
	}
}

// TestParseHTMLErrorPageYieldsNoNodes is the end-to-end shape of PP-07: a
// source that starts answering with an error page must produce zero nodes so
// the caller reports "no supported URI nodes found" instead of republishing
// markup.
func TestParseHTMLErrorPageYieldsNoNodes(t *testing.T) {
	t.Parallel()

	page := "<!DOCTYPE html>\n<html><body>\n" +
		`<p>Your token expired. <a href="https://panel.example/renew">Renew</a></p>` + "\n" +
		"<p>Support: https://t.me/support</p>\n</body></html>\n"

	count := 0
	rejected := subscription.Parse([]byte(page), func(subscription.Node) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("HTML page yielded %d nodes, want 0", count)
	}
	if rejected != 2 {
		t.Fatalf("rejected = %d, want 2 (the two URI-bearing markup lines)", rejected)
	}
}

// TestParseAcceptsRealSchemes guards the other side of PP-07: the shape check
// must stay scheme-generic, including the digit-bearing and unknown schemes
// this service deliberately forwards without understanding.
func TestParseAcceptsRealSchemes(t *testing.T) {
	t.Parallel()

	for _, scheme := range []string{"vless", "trojan", "ss", "hysteria2", "hy2", "anytls", "some.new+scheme-1"} {
		line := scheme + "://uuid@example.com:443#n"
		node, count := parseOne(t, line)
		if count != 1 {
			t.Fatalf("%q yielded %d nodes, want 1", line, count)
		}
		if string(node.Scheme) != scheme {
			t.Errorf("scheme: got %q, want %q", node.Scheme, scheme)
		}
	}
}

// TestParseUppercaseSchemesNormalized pins scheme normalization
// (convert/converter.go:35): parseNode lowercases the scheme once and both the
// dispatch and Node.Scheme use the lowercased value, so an uppercase line still
// reaches its dedicated decoder (ssr, mierus), and rewrite's and merge's
// switches on node.Scheme see a value they match.
func TestParseUppercaseSchemesNormalized(t *testing.T) {
	t.Parallel()

	node := mustParseOne(t, "VLESS://uuid@example.com:443#N")
	if node.Scheme != "vless" {
		t.Errorf("vless scheme: got %q, want vless", node.Scheme)
	}
	if node.Server != "example.com" || node.Port != "443" {
		t.Errorf("vless server:port: got %s:%s, want example.com:443", node.Server, node.Port)
	}

	ssr := "SSR://" + base64.RawURLEncoding.EncodeToString([]byte(ssrHead+"/?"+ssrQuery()))
	node = mustParseOne(t, ssr)
	if node.Scheme != subscription.SchemeSSR {
		t.Errorf("ssr scheme: got %q, want ssr", node.Scheme)
	}
	if node.Server != "1.2.3.4" || node.Port != "8388" {
		t.Errorf("ssr server:port: got %s:%s, want 1.2.3.4:8388", node.Server, node.Port)
	}

	node = mustParseOne(t, "MIERUS://user:pass@1.2.3.4?port=2999&protocol=TCP#M")
	if node.Scheme != subscription.SchemeMieru {
		t.Errorf("mierus scheme: got %q, want mierus", node.Scheme)
	}
	if node.Port != "2999" {
		t.Errorf("mierus port: got %q, want 2999", node.Port)
	}
}

// TestParseCountsRejectedLines pins PP-05: URI-shaped lines the parser refuses
// are reported, so a source that quietly starts returning junk is visible in
// stats instead of just yielding fewer nodes.
func TestParseCountsRejectedLines(t *testing.T) {
	t.Parallel()

	body := []byte("vless://uuid@example.com:443#ok\n" +
		"vless://@:443#no-host\n" + // empty authority host
		"not a scheme://example.com\n" + // prose prefix
		"vmess://!!!not-base64!!!\n" + // undecodable payload
		"plain text line\n") // no "://" at all: not URI-shaped, not counted

	count := 0
	rejected := subscription.Parse(body, func(subscription.Node) bool {
		count++
		return true
	})
	if count != 1 {
		t.Fatalf("parsed %d nodes, want 1", count)
	}
	if rejected != 3 {
		t.Fatalf("rejected = %d, want 3", rejected)
	}
}

func TestParseIPv6BracketedStripsBrackets(t *testing.T) {
	t.Parallel()

	node := mustParseOne(t, "vless://uuid@[2001:db8::1]:8443#v6")
	if node.Server != "2001:db8::1" {
		t.Errorf("server: got %q, want %q", node.Server, "2001:db8::1")
	}
	if node.Port != "8443" {
		t.Errorf("port: got %q, want %q", node.Port, "8443")
	}

	// The bracketed form without a port reads the host cleanly and then dies on
	// the portless reject — mihomo refuses it the same way (v.go:20-22) — so it
	// must not be published under a fabricated 443.
	rejectOne(t, "vless://uuid@[2001:db8::1]#v6")
}

// TestParseIPv6UnbracketedIsWholeHost: an unbracketed IPv6 literal is kept
// whole — never split at its last colon — which means no port can be read, and
// the portless veto refuses the line. The second row would PARSE if
// splitHostPort sliced at the last colon ("…:1" host, "8443" port), so
// rejection pins the no-split property. The refusal is NOT mihomo parity:
// net/url's parseHost splits a non-bracketed multi-colon host at its LAST
// colon outside http(s), so mihomo parses row 1 as server "2001:db8:" port "1"
// and row 2 as a genuine v6 node — it refuses neither, and this IPv4-only
// pipeline rejects them because neither is an endpoint it can probe.
func TestParseIPv6UnbracketedIsWholeHost(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"vless://uuid@2001:db8::1#v6",
		"vless://uuid@2001:db8::1:8443#v6",
	} {
		t.Run(line, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, line)
		})
	}
}

// TestParseProxySchemesRequireExplicitPort: an HTTP/SOCKS proxy node is
// host:port by definition and mihomo drops a portless one, so a bare web URL in
// a source body — a channel link, a panel notice — must not be published as a
// node. The "HTTPS://" rows pin scheme normalization: mihomo lowercases the
// scheme (convert/converter.go:35), so an uppercase bare web URL must hit the
// same portless reject instead of the 443 default that used to publish it.
func TestParseProxySchemesRequireExplicitPort(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"https://t.me/somechannel",
		"https://example.com",
		"http://example.com/sub",
		"http://user:pass@example.com#Proxy",
		"socks://1.2.3.4",
		"socks5://1.2.3.4#Proxy",
		"socks5h://proxy.example?udp=true",
		"HTTPS://t.me/somechannel",
		"HTTP://example.com",
		"HTTPS://user:pass@example.com#Proxy",
	} {
		t.Run(line, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, line)
		})
	}
}

func TestParseProxySchemesWithPort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		line       string
		wantServer string
		wantPort   string
		wantName   string
	}{
		{"https://example.com:8443/docs", "example.com", "8443", "example.com"},
		{"http://user:pass@1.2.3.4:8080#Proxy", "1.2.3.4", "8080", "Proxy"},
		{"socks5://1.2.3.4:1080", "1.2.3.4", "1080", "1.2.3.4"},
		{"socks5h://proxy.example:9050#S", "proxy.example", "9050", "S"},
		{"socks://proxy.example:1080?udp=true#S", "proxy.example", "1080", "S"},
	}

	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()

			node := mustParseOne(t, tc.line)
			if node.Server != tc.wantServer {
				t.Errorf("server: got %q, want %q", node.Server, tc.wantServer)
			}
			if node.Port != tc.wantPort {
				t.Errorf("port: got %q, want %q", node.Port, tc.wantPort)
			}
			if node.Name != tc.wantName {
				t.Errorf("name: got %q, want %q", node.Name, tc.wantName)
			}
		})
	}
}
