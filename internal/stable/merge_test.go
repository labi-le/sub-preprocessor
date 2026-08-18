package stable_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// TestMergeDropsPlaceholderNodes: the worker never calls classify, so the pool
// it probes is filtered here or nowhere. A Nil-UUID credential authenticates
// nobody and a server naming the dialling machine reaches no remote, so both
// cost a probe slot and can never be published. The real node beside them must
// survive, and the labels must stay contiguous: a gap would mean a dropped node
// still consumed a number.
func TestMergeDropsPlaceholderNodes(t *testing.T) {
	t.Parallel()

	body := "vless://00000000-0000-0000-0000-000000000000@192.0.2.1:1111#notice\n" +
		"vless://a1b2c3d4-1111-4000-8000-000000000009@0.0.0.0:8080#died\n" +
		"vless://a1b2c3d4-1111-4000-8000-000000000009@[::]:443#limit\n" +
		"vless://a1b2c3d4-0000-4000-8000-000000000001@198.51.100.2:443#real\n" +
		"trojan://0@203.0.113.3:443#short zero password is a password\n"

	entries := stable.Merge([]stable.SourceBody{sourceBody("src", body)})

	want := []stable.Entry{
		{Label: "src-001", Raw: "vless://a1b2c3d4-0000-4000-8000-000000000001@198.51.100.2:443#src-001", Addr: "198.51.100.2:443"},
		{Label: "src-002", Raw: "trojan://0@203.0.113.3:443#src-002", Addr: "203.0.113.3:443"},
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

// TestMergeDropsLocalAddressNodes: the address rule stands on its own, so an
// ordinary credential does not save a node that names the machine dialling it,
// and the loopback spellings are the ones the pool actually carries — measured
// over the 98 configured source URLs with the worker's UA on 2026-08-14, 24 of
// the 100 local-address nodes were loopback and 4 of those sat on the shipped
// listener's own :8080, where a probe reaches the service itself. A
// documentation-range node with the SAME credential must survive: this is a
// rule about which machine an address names, not a reserved-range filter.
func TestMergeDropsLocalAddressNodes(t *testing.T) {
	t.Parallel()

	const cred = "vless://a1b2c3d4-1111-4000-8000-000000000009@"
	body := cred + "127.0.0.1:8080#listener\n" +
		cred + "0.0.0.0:8080#listener\n" +
		cred + "127.0.0.53:80#resolver stub\n" +
		cred + "[::1]:443#loopback6\n" +
		cred + "192.0.2.10:8080#real\n"

	entries := stable.Merge([]stable.SourceBody{sourceBody("src", body)})

	want := []stable.Entry{
		{Label: "src-001", Raw: cred + "192.0.2.10:8080#src-001", Addr: "192.0.2.10:8080"},
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

// TestMergePlaceholderDoesNotShadowRealNode: the drop happens before the dedupe
// key is interned, so a placeholder cannot book a server:port a working node of
// another source arrives on later — the shadowing hazard relabelNode's doc
// describes for the ssr case.
func TestMergePlaceholderDoesNotShadowRealNode(t *testing.T) {
	t.Parallel()

	entries := stable.Merge([]stable.SourceBody{
		sourceBody("alpha", "vless://00000000-0000-0000-0000-000000000000@192.0.2.1:443#notice\n"),
		sourceBody("beta", "vless://a1b2c3d4-0000-4000-8000-000000000001@192.0.2.1:443#real\n"),
	})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].Label != "beta-001" || entries[0].Addr != "192.0.2.1:443" {
		t.Errorf("got %+v, want the beta node on the shared server:port", entries[0])
	}
}

// TestMergeAddrsSurviveArenaBlockBoundaries walks past the first arena block
// Entry.Addr views into. Recycling a filled block in place — the obvious "we
// already own this buffer" shortcut — rewrites the bytes every earlier Addr
// still points at, and no other test in this file reaches a second block, so
// nothing else in the suite can see it. Letting a block GROW instead is not the
// hazard: append leaves the abandoned array untouched.
func TestMergeAddrsSurviveArenaBlockBoundaries(t *testing.T) {
	t.Parallel()

	const nodes = 2000 // ~19 bytes a key, so ~37 keyArenaBlock blocks
	var (
		body strings.Builder
		want = make([]string, 0, nodes)
	)
	for i := range nodes {
		addr := fmt.Sprintf("198.51.%d.%d:8443", i/256, i%256)
		body.WriteString("vless://a1b2c3d4-0000-4000-8000-000000000001@" + addr + "#n\n")
		want = append(want, addr)
	}

	entries := stable.Merge([]stable.SourceBody{sourceBody("alpha", body.String())})

	if len(entries) != nodes {
		t.Fatalf("got %d entries, want %d", len(entries), nodes)
	}
	for i, w := range want {
		if entries[i].Addr != w {
			t.Fatalf("entry %d: Addr = %q, want %q", i, entries[i].Addr, w)
		}
	}
}

// TestMergeVmessLabelsSurviveArenaBlockBoundaries covers the other arena view a
// kept Entry holds: a vmess node has no relabeled line tail for its Label to
// point at, so relabelNode interns the label beside the dedupe key. Handing back
// the caller's reused label buffer instead — the shortcut interning exists to
// refuse — makes every Label read the last node's, which no single-vmess-node
// case in this file can see, and the run walks past the first arena block so a
// block recycled in place cannot hide behind it either.
func TestMergeVmessLabelsSurviveArenaBlockBoundaries(t *testing.T) {
	t.Parallel()

	const nodes = 600 // ~24 arena bytes a node (key plus label), so ~14 blocks
	var body strings.Builder
	for i := range nodes {
		doc := fmt.Sprintf(`{"add":"198.51.%d.%d","port":"8443","ps":"Original","id":"uuid"}`, i/256, i%256)
		body.WriteString("vmess://" + base64.StdEncoding.EncodeToString([]byte(doc)) + "\n")
	}

	entries := stable.Merge([]stable.SourceBody{sourceBody("alpha", body.String())})

	if len(entries) != nodes {
		t.Fatalf("got %d entries, want %d", len(entries), nodes)
	}
	for i, e := range entries {
		label := fmt.Sprintf("alpha-%03d", i+1)
		if e.Label != label {
			t.Fatalf("entry %d: Label = %q, want %q", i, e.Label, label)
		}
		payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(e.Raw, "vmess://"))
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if want := `"ps":"` + label + `"`; !strings.Contains(string(payload), want) {
			t.Fatalf("entry %d: payload %q missing %s", i, payload, want)
		}
	}
}
