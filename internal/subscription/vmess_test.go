package subscription_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

func vmessLine(payload string) string {
	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(payload))
}

func parseOne(t *testing.T, line string) (subscription.Node, int) {
	t.Helper()
	var got subscription.Node
	count := 0
	subscription.Parse([]byte(line), func(n subscription.Node) bool {
		got = n
		count++
		return true
	})
	return got, count
}

func TestParseVmessExtractsServerPortName(t *testing.T) {
	t.Parallel()

	line := vmessLine(`{"v":"2","ps":"Tokyo Node","add":"1.2.3.4","port":"443","id":"uuid","net":"ws","tls":"tls"}`)
	got, count := parseOne(t, line+"\n")
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Scheme != subscription.SchemeVmess {
		t.Errorf("scheme: got %q, want vmess", got.Scheme)
	}
	if got.Server != "1.2.3.4" {
		t.Errorf("server: got %q, want 1.2.3.4", got.Server)
	}
	if got.Port != "443" {
		t.Errorf("port: got %q, want 443", got.Port)
	}
	if got.Name != "Tokyo Node" {
		t.Errorf("name: got %q, want Tokyo Node", got.Name)
	}
	if got.FragmentIdx != -1 {
		t.Errorf("fragmentIdx: got %d, want -1", got.FragmentIdx)
	}
}

func TestParseVmessNumericPort(t *testing.T) {
	t.Parallel()

	got, count := parseOne(t, vmessLine(`{"add":"h.example","port":8080,"ps":"n"}`))
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Port != "8080" {
		t.Errorf("port: got %q, want 8080", got.Port)
	}
}

func TestParseVmessMissingPsFallsBackToServer(t *testing.T) {
	t.Parallel()

	got, count := parseOne(t, vmessLine(`{"add":"srv.example","port":"443"}`))
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Name != "srv.example" {
		t.Errorf("name: got %q, want srv.example", got.Name)
	}
}

func TestParseVmessMalformedSkipped(t *testing.T) {
	t.Parallel()

	lines := "vmess://not!base64!!!\n" +
		"vmess://" + base64.StdEncoding.EncodeToString([]byte("not json")) + "\n" +
		"vmess://" + base64.StdEncoding.EncodeToString([]byte("null")) + "\n" +
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"port":"443","ps":"no server"}`)) + "\n"
	count := 0
	subscription.Parse([]byte(lines), func(subscription.Node) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("expected 0 nodes, got %d", count)
	}
}

func TestRewriteVmessNameReplacesPs(t *testing.T) {
	t.Parallel()

	line := vmessLine(`{"v":"2","ps":"Old","add":"1.2.3.4","port":"443","id":"uuid","net":"ws"}`)
	out, ok := subscription.RewriteVmessName(line, "avia-003")
	if !ok {
		t.Fatal("rewrite failed")
	}

	decoded, err := base64.StdEncoding.DecodeString(out[len("vmess://"):])
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var m map[string]any
	if err = json.Unmarshal(decoded, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if m["ps"] != "avia-003" {
		t.Errorf("ps: got %v, want avia-003", m["ps"])
	}
	if m["add"] != "1.2.3.4" {
		t.Errorf("add lost: got %v", m["add"])
	}
	if m["port"] != "443" {
		t.Errorf("port lost: got %v", m["port"])
	}
	if m["id"] != "uuid" {
		t.Errorf("id lost: got %v", m["id"])
	}
}

func TestRewriteVmessNameRejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, ok := subscription.RewriteVmessName("vmess://not!base64!!!", "x"); ok {
		t.Fatal("expected failure on undecodable payload")
	}
}
