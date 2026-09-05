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

// TestParseVmessMissingPsUsesFragment pins PP-08: a share link that omits "ps"
// and carries its label in the URI fragment is naming the node there. Before
// the fallback the fragment was stripped by decodeVmessJSON and the node was
// published under its bare host.
func TestParseVmessMissingPsUsesFragment(t *testing.T) {
	t.Parallel()

	got, count := parseOne(t, vmessLine(`{"add":"srv.example","port":"443"}`)+"#Tokyo Relay")
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Name != "Tokyo Relay" {
		t.Errorf("name: got %q, want the fragment", got.Name)
	}

	// An empty fragment is not a name; the server fallback still applies.
	got, count = parseOne(t, vmessLine(`{"add":"srv.example","port":"443"}`)+"#")
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Name != "srv.example" {
		t.Errorf("blank-fragment name: got %q, want srv.example", got.Name)
	}
}

// TestParseVmessPsWinsOverFragment: the payload stays authoritative, so a link
// carrying both keeps the "ps" value.
func TestParseVmessPsWinsOverFragment(t *testing.T) {
	t.Parallel()

	got, count := parseOne(t, vmessLine(`{"add":"srv.example","port":"443","ps":"Payload Name"}`)+"#Stale Fragment")
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Name != "Payload Name" {
		t.Errorf("name: got %q, want Payload Name", got.Name)
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

// TestParseVmessRejectsNonStringAdd: a JSON null/bool/object/array "add" is not
// a hostname, and mihomo's own decode (values["add"], convert/converter.go:274)
// turns it into a mapping adapter.ParseProxy refuses. jsonValueString's raw
// fallback exists for bare NUMBERS only, so any of these must reject the node
// instead of publishing it with a literal "true"/"{...}" server.
func TestParseVmessRejectsNonStringAdd(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{
		`{"add":true,"port":"443","ps":"n"}`,
		`{"add":null,"port":"443","ps":"n"}`,
		`{"add":{"host":"x"},"port":"443","ps":"n"}`,
		`{"add":["x"],"port":"443","ps":"n"}`,
	} {
		_, count := parseOne(t, vmessLine(payload))
		if count != 0 {
			t.Errorf("payload %s parsed %d nodes, want 0", payload, count)
		}
	}
}

// TestParseVmessNonStringPortDefaults: a "port" that is not a JSON string or
// number must not become literal port text ("true", "null"). mihomo reads
// values["port"] and structure-decodes it into an int, so "true" would publish
// a node that converts to nothing; the empty string the reader now returns
// takes the same 443 default an absent port does.
func TestParseVmessNonStringPortDefaults(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{
		`{"add":"srv.example","port":true,"ps":"n"}`,
		`{"add":"srv.example","port":null,"ps":"n"}`,
		`{"add":"srv.example","port":{"p":443},"ps":"n"}`,
	} {
		got, count := parseOne(t, vmessLine(payload))
		if count != 1 {
			t.Errorf("payload %s parsed %d nodes, want 1", payload, count)
			continue
		}
		if got.Port != "443" {
			t.Errorf("payload %s: port = %q, want the 443 default", payload, got.Port)
		}
		if got.Server != "srv.example" {
			t.Errorf("payload %s: server = %q, want srv.example", payload, got.Server)
		}
	}
}

// TestParseVmessUppercaseSchemeDecodes pins scheme normalization: mihomo
// lowercases the scheme before dispatching (convert/converter.go:35), so a
// "VMESS://" line must reach the payload decoder instead of slipping onto the
// generic authority path, which would publish the base64 payload as the server.
func TestParseVmessUppercaseSchemeDecodes(t *testing.T) {
	t.Parallel()

	line := "VMESS://" + base64.StdEncoding.EncodeToString([]byte(`{"add":"srv.example","port":"8443","ps":"Up"}`))
	got, count := parseOne(t, line)
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Scheme != subscription.SchemeVmess {
		t.Errorf("scheme: got %q, want vmess", got.Scheme)
	}
	if got.Server != "srv.example" {
		t.Errorf("server: got %q, want srv.example", got.Server)
	}
	if got.Port != "8443" {
		t.Errorf("port: got %q, want 8443", got.Port)
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
