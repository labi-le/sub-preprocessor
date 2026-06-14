package subscription

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseDefaultsPort(t *testing.T) {
	t.Parallel()

	nodes, err := Parse([]byte("vless://uuid@example.com?security=tls#Example\nvmess://ignored\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("unexpected count: %d", len(nodes))
	}
	if nodes[0].Port != "443" {
		t.Fatalf("unexpected port: %q", nodes[0].Port)
	}
}

func TestNormalizeBase64(t *testing.T) {
	t.Parallel()

	raw := "vless://uuid@example.com:443?security=tls#Node 1\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	if got := string(normalize([]byte(encoded))); got != strings.TrimSpace(raw) {
		t.Fatalf("unexpected normalize result: %q", got)
	}
}
