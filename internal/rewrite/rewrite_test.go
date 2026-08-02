package rewrite_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
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

// ssrLine builds an ssr:// link whose display name is the base64 "remarks"
// query param inside the base64 payload.
func ssrLine(remarks string) string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	payload := "1.2.3.4:8388:origin:aes-256-cfb:plain:" + b64("secret") +
		"/?obfsparam=" + b64("obfs.example") + "&remarks=" + b64(remarks)
	return "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// ssrQueryOf asserts that out is a fragment-free, unpadded url-safe ssr link —
// the only shape mihomo can decode — and returns its parsed query.
func ssrQueryOf(t *testing.T, out string) url.Values {
	t.Helper()

	if strings.ContainsRune(out, '#') {
		t.Fatalf("output carries a fragment: %q", out)
	}
	payload, ok := strings.CutPrefix(out, "ssr://")
	if !ok {
		t.Fatalf("expected an ssr:// link, got %q", out)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload is not unpadded url-safe base64: %v", err)
	}
	_, query, found := strings.Cut(string(decoded), "/?")
	if !found {
		t.Fatalf("payload lost its /? separator: %q", decoded)
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse rewritten query %q: %v", query, err)
	}
	return values
}

// TestNodeNameSSRRewritesRemarks pins the reason ssr needs its own branch:
// mihomo base64-decodes everything after "ssr://", so appending "#name" — what
// every other scheme gets — turns the node into "convert: format invalid".
func TestNodeNameSSRRewritesRemarks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
	}{
		{"fragmentless", ssrLine("Old")},
		{"fragment stripped", ssrLine("Old") + "#stale label"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			node := parseNode(t, tc.line)
			var buf bytes.Buffer
			rewrite.NodeName(&buf, node, "[GEO:JP][IP:1.2.3.4]")
			out := buf.String()

			values := ssrQueryOf(t, out)
			for _, want := range []struct{ key, value string }{
				{"remarks", "[GEO:JP][IP:1.2.3.4] Old"},
				{"obfsparam", "obfs.example"},
			} {
				got, decErr := base64.RawURLEncoding.DecodeString(values.Get(want.key))
				if decErr != nil {
					t.Errorf("%s=%q: %v", want.key, values.Get(want.key), decErr)
					continue
				}
				if string(got) != want.value {
					t.Errorf("%s: got %q, want %q", want.key, got, want.value)
				}
			}
		})
	}
}

// TestNodeNameUndecodableSSRWrittenVerbatim: unannotated beats mangled.
func TestNodeNameUndecodableSSRWrittenVerbatim(t *testing.T) {
	t.Parallel()

	node := subscription.Node{
		Raw:         "ssr://!!!not-base64!!!",
		Scheme:      subscription.SchemeSSR,
		Name:        "Old",
		Server:      "1.2.3.4",
		Port:        "8388",
		FragmentIdx: -1,
	}
	var buf bytes.Buffer
	rewrite.NodeName(&buf, node, "[GEO:JP]")
	if buf.String() != node.Raw {
		t.Errorf("got %q, want the raw line %q", buf.String(), node.Raw)
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
}
