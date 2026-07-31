package subscription_test

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/subscription"
)

// An ssr payload is "host:port:protocol:method:obfs:password/?query" in base64,
// where password and every base64-valued query param use the url-safe alphabet.
const ssrHead = "1.2.3.4:8388:origin:aes-256-cfb:plain:c2VjcmV0"

func b64u(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func ssrQuery() string {
	return "obfsparam=" + b64u("obfs.example.com") +
		"&protoparam=" + b64u("auth-token") +
		"&remarks=" + b64u("Tokyo Node") +
		"&group=" + b64u("grp") +
		"&udpport=0&uot=1"
}

func ssrLine(payload string) string {
	return "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func TestParseSSRDecodesPayload(t *testing.T) {
	t.Parallel()

	line := ssrLine(ssrHead + "/?" + ssrQuery())
	node := mustParseOne(t, line)
	if node.Scheme != subscription.SchemeSSR {
		t.Errorf("scheme: got %q, want %q", node.Scheme, subscription.SchemeSSR)
	}
	if node.Server != "1.2.3.4" {
		t.Errorf("server: got %q, want 1.2.3.4", node.Server)
	}
	if node.Port != "8388" {
		t.Errorf("port: got %q, want 8388", node.Port)
	}
	if node.Name != "Tokyo Node" {
		t.Errorf("name: got %q, want the decoded remarks %q", node.Name, "Tokyo Node")
	}
	if node.FragmentIdx != -1 {
		t.Errorf("fragmentIdx: got %d, want -1 for a fragmentless link", node.FragmentIdx)
	}
}

func TestParseSSRName(t *testing.T) {
	t.Parallel()

	stdRemarks := base64.StdEncoding.EncodeToString([]byte("Ünïtéd ÿÿÿ"))
	if !strings.ContainsAny(stdRemarks, "+/") {
		t.Fatalf("fixture %q does not exercise the std base64 alphabet", stdRemarks)
	}

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"decoded remarks", "remarks=" + b64u("Tokyo Node"), "Tokyo Node"},
		{"absent remarks falls back to the host", "obfsparam=" + b64u("x"), "1.2.3.4"},
		{"undecodable remarks falls back to the host", "remarks=!!!", "1.2.3.4"},
		{"empty remarks falls back to the host", "remarks=", "1.2.3.4"},
		// mihomo maps the std alphabet onto the url-safe one before decoding,
		// so a producer using '+' and '/' is still readable.
		{"std alphabet remarks", "remarks=" + stdRemarks, "Ünïtéd ÿÿÿ"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			node := mustParseOne(t, ssrLine(ssrHead+"/?"+tc.query))
			if node.Name != tc.want {
				t.Errorf("name: got %q, want %q", node.Name, tc.want)
			}
		})
	}
}

// TestParseSSRWithFragment: mihomo base64-decodes everything after "ssr://" and
// chokes on a fragment, but a source that ships one still describes a real
// node; both of our output paths re-emit it through RewriteSSRName, which drops
// the fragment.
func TestParseSSRWithFragment(t *testing.T) {
	t.Parallel()

	line := ssrLine(ssrHead+"/?"+ssrQuery()) + "#stale label"
	node := mustParseOne(t, line)
	if node.Server != "1.2.3.4" || node.Port != "8388" {
		t.Errorf("server:port: got %s:%s, want 1.2.3.4:8388", node.Server, node.Port)
	}
	if node.Name != "Tokyo Node" {
		t.Errorf("name: got %q, want the decoded remarks", node.Name)
	}
	if want := strings.IndexByte(line, '#'); node.FragmentIdx != want {
		t.Errorf("fragmentIdx: got %d, want %d", node.FragmentIdx, want)
	}
}

// TestParseSSRRejects mirrors mihomo's accept set. A node we keep but the
// prober cannot convert burns probe budget and can never be published, so every
// case here must be counted as rejected rather than silently skipped.
func TestParseSSRRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
	}{
		{"undecodable payload", "ssr://!!!not-base64!!!"},
		{"missing /? separator", ssrLine(ssrHead)},
		{"query separator without the leading slash", ssrLine(ssrHead + "?" + ssrQuery())},
		{"five fields", ssrLine("1.2.3.4:8388:origin:aes-256-cfb:c2VjcmV0/?" + ssrQuery())},
		{"seven fields", ssrLine("1.2.3.4:8388:origin:aes-256-cfb:plain:extra:c2VjcmV0/?" + ssrQuery())},
		{"empty host", ssrLine(":8388:origin:aes-256-cfb:plain:c2VjcmV0/?" + ssrQuery())},
		{"empty port", ssrLine("1.2.3.4::origin:aes-256-cfb:plain:c2VjcmV0/?" + ssrQuery())},
		// url.ParseQuery is the last of mihomo's three payload requirements
		// (converter.go:501-504) and the one our own RewriteSSRName shares:
		// without it we publish, through rewrite.NodeName's raw fallback, a
		// link convert.ConvertsV2Ray answers "format invalid" to — while
		// /stable.txt drops it with nothing counted, hiding the loss from
		// Stats.Unsupported.
		{"unparseable query escape", ssrLine(ssrHead + "/?remarks=%zz")},
		{"semicolon separator", ssrLine(ssrHead + "/?remarks=" + b64u("x") + ";group=" + b64u("g"))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rejectOne(t, tc.line)
		})
	}
}

func TestRewriteSSRNameSetsRemarks(t *testing.T) {
	t.Parallel()

	const newName = "[GEO:JP][IP:1.2.3.4] Tokyo Node"
	out, ok := subscription.RewriteSSRName(ssrLine(ssrHead+"/?"+ssrQuery()), newName)
	if !ok {
		t.Fatal("RewriteSSRName reported failure on a valid link")
	}
	head, query := decodeSSRResult(t, out)

	if head != ssrHead {
		t.Errorf("head: got %q, want %q", head, ssrHead)
	}
	// mihomo reads every value with RawURLEncoding, which rejects a
	// percent-escape as much as it rejects a '=' pad.
	if strings.ContainsRune(query, '%') {
		t.Errorf("query carries percent escapes: %q", query)
	}

	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse rewritten query %q: %v", query, err)
	}
	for _, tc := range []struct{ key, want string }{
		{"remarks", newName},
		{"obfsparam", "obfs.example.com"},
		{"protoparam", "auth-token"},
		{"group", "grp"},
	} {
		got, decErr := base64.RawURLEncoding.DecodeString(values.Get(tc.key))
		if decErr != nil {
			t.Errorf("%s=%q: %v", tc.key, values.Get(tc.key), decErr)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s: got %q, want %q", tc.key, got, tc.want)
		}
	}
	if values.Get("uot") != "1" || values.Get("udpport") != "0" {
		t.Errorf("plain params lost: uot=%q udpport=%q", values.Get("uot"), values.Get("udpport"))
	}

	node := mustParseOne(t, out)
	if node.Name != newName {
		t.Errorf("reparsed name: got %q, want %q", node.Name, newName)
	}
	if node.Server != "1.2.3.4" || node.Port != "8388" {
		t.Errorf("reparsed server:port: got %s:%s, want 1.2.3.4:8388", node.Server, node.Port)
	}
}

// TestRewriteSSRNameDropsFragment: the fragment is why the relabeled link used
// to come back as "convert: format invalid".
func TestRewriteSSRNameDropsFragment(t *testing.T) {
	t.Parallel()

	out, ok := subscription.RewriteSSRName(ssrLine(ssrHead+"/?"+ssrQuery())+"#src-001", "renamed")
	if !ok {
		t.Fatal("RewriteSSRName reported failure on a link with a fragment")
	}
	if strings.ContainsRune(out, '#') {
		t.Fatalf("output carries a fragment: %q", out)
	}
	_, query := decodeSSRResult(t, out)
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse rewritten query %q: %v", query, err)
	}
	got, err := base64.RawURLEncoding.DecodeString(values.Get("remarks"))
	if err != nil || string(got) != "renamed" {
		t.Errorf("remarks: got %q (%v), want %q", got, err, "renamed")
	}
}

func TestRewriteSSRNameRejectsGarbage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{"no scheme separator", "ssr:!!!"},
		{"undecodable payload", "ssr://!!!not-base64!!!"},
		{"missing /? separator", ssrLine(ssrHead)},
		{"wrong field count", ssrLine("1.2.3.4:8388:origin:aes-256-cfb:c2VjcmV0/?" + ssrQuery())},
		{"empty", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if out, ok := subscription.RewriteSSRName(tc.raw, "x"); ok {
				t.Errorf("got (%q, true), want false", out)
			}
		})
	}
}

// decodeSSRResult splits a RewriteSSRName result. RawURLEncoding accepts
// neither padding nor the std alphabet, so decoding with it also pins the
// unpadded url-safe requirement mihomo's decode depends on.
func decodeSSRResult(t *testing.T, out string) (head, query string) {
	t.Helper()
	payload, ok := strings.CutPrefix(out, "ssr://")
	if !ok {
		t.Fatalf("result is not an ssr:// link: %q", out)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload %q is not unpadded url-safe base64: %v", payload, err)
	}
	head, query, ok = strings.Cut(string(decoded), "/?")
	if !ok {
		t.Fatalf("payload lost its /? separator: %q", decoded)
	}
	return head, query
}
