package subscription_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

func TestParseDefaultsPort(t *testing.T) {
	t.Parallel()

	var nodes []subscription.Node
	subscription.Parse([]byte("vless://uuid@example.com?security=tls#Example\ntrojan://uuid@example.net#Other\nplain-text-node\n"), func(n subscription.Node) bool {
		nodes = append(nodes, n)
		return true
	})
	if len(nodes) != 2 {
		t.Fatalf("unexpected count: %d", len(nodes))
	}
	if nodes[0].Port != "443" {
		t.Fatalf("unexpected port: %q", nodes[0].Port)
	}
	if nodes[1].Scheme != "trojan" {
		t.Fatalf("unexpected scheme: %q", nodes[1].Scheme)
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

	node = mustParseOne(t, "vless://uuid@[2001:db8::1]#v6")
	if node.Server != "2001:db8::1" {
		t.Errorf("no-port server: got %q, want %q", node.Server, "2001:db8::1")
	}
	if node.Port != "443" {
		t.Errorf("no-port port: got %q, want default %q", node.Port, "443")
	}
}

func TestParseIPv6UnbracketedIsWholeHost(t *testing.T) {
	t.Parallel()

	node := mustParseOne(t, "vless://uuid@2001:db8::1#v6")
	if node.Server != "2001:db8::1" {
		t.Errorf("server: got %q, want %q", node.Server, "2001:db8::1")
	}
	if node.Port != "443" {
		t.Errorf("port: got %q, want default %q", node.Port, "443")
	}
}
