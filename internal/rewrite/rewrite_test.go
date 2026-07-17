package rewrite_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/subscription"
)

func parseNode(t *testing.T, line string) subscription.Node {
	t.Helper()
	var node subscription.Node
	found := false
	subscription.Parse([]byte(line), func(n subscription.Node) bool {
		node = n
		found = true
		return true
	})
	if !found {
		t.Fatalf("no node parsed from %q", line)
	}
	return node
}

func TestNodeNameVlessAppendsGeoIPFragment(t *testing.T) {
	t.Parallel()

	node := parseNode(t, "vless://uuid@host.example:443?type=tcp#Old Name")
	var buf bytes.Buffer
	rewrite.NodeName(&buf, node, "[GEO:US][IP:1.2.3.4]")

	want := "vless://uuid@host.example:443?type=tcp#[GEO:US][IP:1.2.3.4] Old Name"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestNodeNameVmessRewritesPsWithGeoIP(t *testing.T) {
	t.Parallel()

	line := "vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"Old","add":"1.2.3.4","port":"443","id":"uuid","net":"ws"}`))
	node := parseNode(t, line)
	var buf bytes.Buffer
	rewrite.NodeName(&buf, node, "[GEO:US][IP:1.2.3.4]")

	out := buf.String()
	const prefix = "vmess://"
	if len(out) < len(prefix) || out[:len(prefix)] != prefix {
		t.Fatalf("expected vmess:// output, got %q", out)
	}
	decoded, err := base64.StdEncoding.DecodeString(out[len(prefix):])
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var m map[string]any
	if err = json.Unmarshal(decoded, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if m["ps"] != "[GEO:US][IP:1.2.3.4] Old" {
		t.Errorf("ps: got %v, want [GEO:US][IP:1.2.3.4] Old", m["ps"])
	}
	if m["add"] != "1.2.3.4" {
		t.Errorf("add lost: got %v", m["add"])
	}
}

func TestLeadingTags(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"[GEO:FI][IP:1.2.3.4] my node": "[GEO:FI][IP:1.2.3.4]",
		"[OK][GEO:DE] name":            "[OK][GEO:DE]",
		"[GEO:LV]":                     "[GEO:LV]",
		"plain name":                   "",
		"[UNKNOWN:x][GEO:FI] n":        "",
		"":                             "",
		"[BAD][GEO:FI] x":              "[BAD][GEO:FI]",
	}
	for in, want := range cases {
		if got := rewrite.LeadingTags(in); got != want {
			t.Errorf("LeadingTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripKnownTags(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"[BAD] node":                   "node",
		"[OK] node":                    "node",
		"[GEO:FI][IP:1.2.3.4] my node": "my node",
		"[UNKNOWN:x] n":                "[UNKNOWN:x] n",
		"plain name":                   "plain name",
	}
	for in, want := range cases {
		if got := rewrite.StripKnownTags(in); got != want {
			t.Errorf("StripKnownTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKnownTagsIncludeSPD(t *testing.T) {
	t.Parallel()

	if got := rewrite.StripKnownTags("[SPD:45M] Tokyo"); got != "Tokyo" {
		t.Fatalf("StripKnownTags dropped SPD wrong: %q", got)
	}
	if got := rewrite.StripKnownTags("[GEO:FI][IP:1.2.3.4][SPD:5M] node"); got != "node" {
		t.Fatalf("StripKnownTags mixed tags: %q", got)
	}
	if got := rewrite.LeadingTags("[SPD:12M] node"); got != "[SPD:12M]" {
		t.Fatalf("LeadingTags = %q, want [SPD:12M]", got)
	}
}
