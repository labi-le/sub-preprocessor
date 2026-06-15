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
