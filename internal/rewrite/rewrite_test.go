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

// TestNodeNameUndecodableVmessWrittenVerbatim is the ssr fallback's twin: a
// payload the re-encoder cannot read is published unannotated, never appended
// to as if it named its node in a fragment.
func TestNodeNameUndecodableVmessWrittenVerbatim(t *testing.T) {
	t.Parallel()

	node := subscription.Node{
		Raw:         "vmess://!!!not-base64!!!",
		Scheme:      subscription.SchemeVmess,
		Name:        "Old",
		Server:      "192.0.2.1",
		Port:        "443",
		FragmentIdx: -1,
	}
	var buf bytes.Buffer
	rewrite.NodeName(&buf, node, "[GEO:JP]")
	if buf.String() != node.Raw {
		t.Errorf("got %q, want the raw line %q", buf.String(), node.Raw)
	}
}

// ssLegacyLine is the pre-SIP002 ss form, whose whole authority is base64 of
// "method:password@host:port". It is returned split at the '#' because the
// fragment path rewrites exactly that prefix.
func ssLegacyLine() (authority, line string) {
	authority = "ss://" + base64.RawStdEncoding.EncodeToString([]byte("aes-256-gcm:pass@192.0.2.1:8388"))

	return authority, authority + "#Old Name"
}

// TestNodeNameFragmentBytes pins the bytes the fragment path writes for the
// schemes rewrite_test does not already pin, and for both shapes where a
// separator could appear where none belongs: an empty prefix, and a line with
// no fragment to replace.
func TestNodeNameFragmentBytes(t *testing.T) {
	t.Parallel()

	ssAuthority, ssLine := ssLegacyLine()
	const bare = "vless://uuid@192.0.2.1:443?type=tcp"
	cases := []struct {
		name, line, tags, want string
	}{
		{"ss legacy", ssLine, "[GEO:SE]", ssAuthority + "#[GEO:SE] Old Name"},
		{
			"mierus", "mierus://u@192.0.2.1?port=2999&protocol=TCP#Old Name", "[GEO:DE]",
			"mierus://u@192.0.2.1?port=2999&protocol=TCP#[GEO:DE] Old Name",
		},
		{
			"empty tags strip the upstream prefix",
			"vless://uuid@host.example:443#[GEO:RU][SPD:20M] Old Name", "",
			"vless://uuid@host.example:443#Old Name",
		},
		{"no fragment names the node after its server", bare, "[GEO:FI]", bare + "#[GEO:FI] 192.0.2.1"},
		{"no fragment and no tags", bare, "", bare + "#192.0.2.1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			rewrite.NodeName(&buf, parseNode(t, tc.line), tc.tags)
			if got := buf.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

type nodeCase struct {
	name string
	node subscription.Node
}

// nodeNameCases spans every arm of NodeName: the three schemes that name a node
// in a URI fragment, the two that fold the name into a base64 payload, both
// undecodable-payload fallbacks and a line with no scheme at all.
func nodeNameCases(t *testing.T) []nodeCase {
	t.Helper()

	_, ssLine := ssLegacyLine()
	vmessLine := "vmess://" + base64.StdEncoding.EncodeToString(
		[]byte(`{"v":"2","ps":"Old","add":"192.0.2.1","port":"443","id":"uuid","net":"ws"}`))

	return []nodeCase{
		{"vless", parseNode(t, "vless://uuid@host.example:443?type=tcp#Old Name")},
		{"vless without a fragment", parseNode(t, "vless://uuid@192.0.2.1:443?type=tcp")},
		{"vless behind an upstream tag", parseNode(t, "vless://uuid@host.example:443#[GEO:RU][SPD:20M] Old Name")},
		{"ss legacy", parseNode(t, ssLine)},
		{"mierus", parseNode(t, "mierus://u@192.0.2.1?port=2999&protocol=TCP#Old Name")},
		{"vmess", parseNode(t, vmessLine)},
		{"ssr", parseNode(t, ssrLine("Old Node"))},
		{"ssr behind a stale fragment", parseNode(t, ssrLine("Old Node")+"#stale label")},
		{"undecodable vmess", subscription.Node{
			Raw: "vmess://!!!not-base64!!!", Scheme: subscription.SchemeVmess,
			Name: "Old", Server: "192.0.2.1", Port: "443", FragmentIdx: -1,
		}},
		{"undecodable ssr", subscription.Node{
			Raw: "ssr://!!!not-base64!!!", Scheme: subscription.SchemeSSR,
			Name: "Old", Server: "192.0.2.1", Port: "8388", FragmentIdx: -1,
		}},
		{"no scheme", subscription.Node{Raw: "not-a-uri", FragmentIdx: -1}},
	}
}

// preSplitNodeName is NodeName as it stood before the prefix and the clean name
// became separate writes on the fragment path: one joined string handed to
// every arm. It is the oracle for byte identity — dropping that allocation may
// not move a byte of published output, whatever the arm or the prefix.
func preSplitNodeName(node subscription.Node, tags string) string {
	if node.Scheme == "" {
		return node.Raw
	}
	cleanName := rewrite.StripKnownTags(node.Name)
	if cleanName == "" {
		cleanName = node.Server
	}
	name := cleanName
	if tags != "" {
		name = tags + " " + cleanName
	}
	switch node.Scheme { //nolint:exhaustive // mirrors NodeName: every other scheme takes the fragment path below
	case subscription.SchemeVmess:
		if out, ok := subscription.RewriteVmessName(node.Raw, name); ok {
			return out
		}

		return node.Raw
	case subscription.SchemeSSR:
		if out, ok := subscription.RewriteSSRName(node.Raw, name); ok {
			return out
		}

		return node.Raw
	}
	if node.FragmentIdx >= 0 {
		return node.Raw[:node.FragmentIdx] + "#" + name
	}

	return node.Raw + "#" + name
}

func TestNodeNameMatchesPreSplitJoin(t *testing.T) {
	t.Parallel()

	// The shipped prefixes: none at all (a clean relabel), the annotator's
	// [GEO:xx], and the stable worker's speed prefix ahead of it.
	prefixes := []string{"", "[GEO:JP]", "[SPD:60M] [GEO:JP]"}
	for _, tc := range nodeNameCases(t) {
		for _, tags := range prefixes {
			var buf bytes.Buffer
			rewrite.NodeName(&buf, tc.node, tags)
			if got, want := buf.String(), preSplitNodeName(tc.node, tags); got != want {
				t.Errorf("%s with tags %q: got %q, want %q", tc.name, tags, got, want)
			}
		}
	}
}

// TestNodeNameFragmentPathAllocationFree pins the point of the split: a
// production cycle annotates ~300000 nodes, and the fragment path is what
// nearly all of them take.
func TestNodeNameFragmentPathAllocationFree(t *testing.T) {
	node := parseNode(t, "vless://uuid@host.example:443?type=tcp#[GEO:RU] Old Name")
	var buf bytes.Buffer
	buf.Grow(256)

	allocs := testing.AllocsPerRun(50, func() {
		buf.Reset()
		rewrite.NodeName(&buf, node, "[GEO:NL]")
	})
	if allocs != 0 {
		t.Errorf("allocated %.0f times per run, want 0", allocs)
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
