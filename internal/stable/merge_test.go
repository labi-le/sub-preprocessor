package stable_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"

	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/stable"
)

// sourceBody turns a subscription body into the per-node results the preprocess
// IP stage hands the worker, so these fixtures stay written as the source text
// they model. Every non-blank line becomes one node, unparseable ones included:
// Merge must still shrug those off.
func sourceBody(name, body string) stable.SourceBody {
	var nodes []preprocess.NodeResult
	for line := range strings.SplitSeq(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			nodes = append(nodes, preprocess.NodeResult{Raw: line})
		}
	}

	return stable.SourceBody{Name: name, Nodes: nodes}
}

func TestMergeDedupesAndRelabels(t *testing.T) {
	t.Parallel()

	alpha := "vless://uuid-a@host.example:443?type=tcp#Old Name\n" +
		"garbage line without scheme\n" +
		"vless://uuid-b@other.example:8443\n"
	beta := "vless://uuid-c@host.example:443#Dup Of Alpha\n" +
		"vless://uuid-d@beta.example:443#Beta Node\n"

	entries := stable.Merge([]stable.SourceBody{
		sourceBody("alpha", alpha),
		sourceBody("beta", beta),
	})

	want := []stable.Entry{
		{Label: "alpha-001", Raw: "vless://uuid-a@host.example:443?type=tcp#alpha-001", Addr: "host.example:443"},
		{Label: "alpha-002", Raw: "vless://uuid-b@other.example:8443#alpha-002", Addr: "other.example:8443"},
		{Label: "beta-001", Raw: "vless://uuid-d@beta.example:443#beta-001", Addr: "beta.example:443"},
	}

	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("entry %d: got %+v, want %+v", i, entries[i], w)
		}
	}
}

// TestMergeCarriesNodeIP: the address the IP-filters judged travels with the
// node so the offline GEO chain annotates THAT address — the worker never
// resolves a hostname a second time. No tag prints the address itself (the IP
// annotate tag is gone), which is exactly why the carry is easy to mistake for
// dead weight: it feeds step.prov.Lookup(), so dropping it kills GEO
// annotation in the worker. The country is not merge's business: nothing has
// annotated the node yet.
func TestMergeCarriesNodeIP(t *testing.T) {
	t.Parallel()

	ip := addr(t, "203.0.113.7")
	entries := stable.Merge([]stable.SourceBody{{
		Name:  "src",
		Nodes: []preprocess.NodeResult{{Raw: "vless://u@h.example:443#[GEO:FI][IP:1.2.3.4] orig", IP: ip}},
	}})
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.IP != ip {
		t.Errorf("IP = %v, want %v", e.IP, ip)
	}
	if e.Country != "" {
		t.Errorf("Country = %q, want empty: merge decides no country", e.Country)
	}
	// The upstream's own tags are part of the name it authored, and the name is
	// replaced wholesale by the probe label. Tags are built once, at publication.
	if e.Raw != "vless://u@h.example:443#src-001" {
		t.Errorf("Raw must be the clean probe label, got %q", e.Raw)
	}
}

func TestMergeEmptyInput(t *testing.T) {
	t.Parallel()

	if got := stable.Merge(nil); len(got) != 0 {
		t.Fatalf("expected no entries, got %+v", got)
	}
}

func TestMergeRelabelsVmessViaPs(t *testing.T) {
	t.Parallel()

	body := "vmess://" +
		base64.StdEncoding.EncodeToString([]byte(`{"add":"1.2.3.4","port":"443","ps":"Original","id":"uuid"}`))

	entries := stable.Merge([]stable.SourceBody{sourceBody("avia", body)})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Label != "avia-001" {
		t.Errorf("label: got %q, want avia-001", entries[0].Label)
	}

	const prefix = "vmess://"
	raw := entries[0].Raw
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("expected vmess:// entry, got %q", raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw[len(prefix):])
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	var m map[string]any
	if err = json.Unmarshal(decoded, &m); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if m["ps"] != "avia-001" {
		t.Errorf("ps: got %v, want avia-001", m["ps"])
	}
	if m["add"] != "1.2.3.4" {
		t.Errorf("add lost: got %v", m["add"])
	}
}

// ssrLink builds a share link whose payload is base64 of
// "host:port:protocol:method:obfs:password/?query" — an ssr node carries its
// server, port and display name there and nowhere in the URI.
func ssrLink(remarks string) string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	payload := "1.2.3.4:8388:origin:aes-256-cfb:plain:" + b64("secret") +
		"/?obfsparam=" + b64("obfs.example.com") + "&remarks=" + b64(remarks)
	return "ssr://" + b64(payload)
}

// mihomoProxyMap runs one already-relabeled share link through the production
// convert path and returns the single proxy map it must yield.
//
// The count assertion is the point: ConvertsV2Ray drops a link it cannot
// decode with `continue` and returns no error, so a relabel that corrupts the
// link surfaces here as zero proxies rather than as a failure — and in
// production as a permanently unselectable node plus a poisoned dead cache.
func mihomoProxyMap(t *testing.T, link string) map[string]any {
	t.Helper()

	mappings, err := convert.ConvertsV2Ray([]byte(link))
	if err != nil {
		t.Fatalf("ConvertsV2Ray(%q): %v", link, err)
	}
	if len(mappings) != 1 {
		t.Fatalf("got %d proxies from %q, want 1", len(mappings), link)
	}
	px, parseErr := adapter.ParseProxy(mappings[0])
	if parseErr != nil {
		t.Fatalf("ParseProxy(%q): %v", link, parseErr)
	}
	t.Cleanup(func() { _ = px.Close() })

	return mappings[0]
}

// TestMergeRelabelsSSRViaRemarks: ssr keeps its display name in the base64
// payload's "remarks", and mihomo base64-decodes EVERYTHING after "ssr://" —
// so the generic "<raw>#<label>" relabel produces a link that converts to
// nothing at all, not merely to a wrongly named proxy.
func TestMergeRelabelsSSRViaRemarks(t *testing.T) {
	t.Parallel()

	entries := stable.Merge([]stable.SourceBody{sourceBody("src", ssrLink("Original"))})
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Label != "src-001" {
		t.Errorf("label: got %q, want src-001", e.Label)
	}
	if e.Addr != "1.2.3.4:8388" {
		t.Errorf("addr: got %q, want the decoded 1.2.3.4:8388", e.Addr)
	}
	if strings.ContainsRune(e.Raw, '#') {
		t.Errorf("a relabeled ssr link must carry no fragment, got %q", e.Raw)
	}

	m := mihomoProxyMap(t, e.Raw)
	wantString(t, m, "name", "src-001")
	wantString(t, m, "server", "1.2.3.4")
	wantString(t, m, "type", "ssr")
}

func TestMergeMixedCaseHostsCollapse(t *testing.T) {
	t.Parallel()

	// Hostnames are case-insensitive: mixed-case duplicates must collapse and
	// share one lowercased dead-cache key, while Raw keeps the original casing.
	body := "vless://a@HOST.Example:443#one\nvless://b@host.EXAMPLE:443#two\n"
	entries := stable.Merge([]stable.SourceBody{sourceBody("src", body)})
	if len(entries) != 1 {
		t.Fatalf("mixed-case duplicates must collapse to 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Addr != "host.example:443" {
		t.Errorf("Addr must be the lowercased dead-cache key, got %q", e.Addr)
	}
	if e.Raw != "vless://a@HOST.Example:443#src-001" {
		t.Errorf("Raw must keep original casing (first source wins), got %q", e.Raw)
	}
}
