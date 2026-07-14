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

func TestParseHashBeforeSchemeIsNotFragment(t *testing.T) {
	t.Parallel()

	// Lines starting with '#' are skipped by the iterator, so exercise a
	// '#' that sits before the scheme mid-line instead.
	node := mustParseOne(t, "x#note vless://uuid@example.com:443")
	if node.FragmentIdx != -1 {
		t.Errorf("fragmentIdx: got %d, want -1", node.FragmentIdx)
	}
	if node.Name != "example.com" {
		t.Errorf("name: got %q, want server fallback %q", node.Name, "example.com")
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
