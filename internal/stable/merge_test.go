package stable_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"

	"domains.lst/sub-preprocessor/internal/stable"
)

func TestMergeDedupesAndRelabels(t *testing.T) {
	t.Parallel()

	alpha := []byte("vless://uuid-a@host.example:443?type=tcp#Old Name\n" +
		"garbage line without scheme\n" +
		"vless://uuid-b@other.example:8443\n")
	beta := []byte("vless://uuid-c@host.example:443#Dup Of Alpha\n" +
		"vless://uuid-d@beta.example:443#Beta Node\n")

	entries := stable.Merge([]stable.SourceBody{
		{Name: "alpha", Body: alpha},
		{Name: "beta", Body: beta},
	})

	want := []stable.Entry{
		{Label: "alpha-001", Raw: "vless://uuid-a@host.example:443?type=tcp#alpha-001", Tagged: "vless://uuid-a@host.example:443?type=tcp#alpha-001", Addr: "host.example:443"},
		{Label: "alpha-002", Raw: "vless://uuid-b@other.example:8443#alpha-002", Tagged: "vless://uuid-b@other.example:8443#alpha-002", Addr: "other.example:8443"},
		{Label: "beta-001", Raw: "vless://uuid-d@beta.example:443#beta-001", Tagged: "vless://uuid-d@beta.example:443#beta-001", Addr: "beta.example:443"},
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

func TestMergeEmptyInput(t *testing.T) {
	t.Parallel()

	if got := stable.Merge(nil); len(got) != 0 {
		t.Fatalf("expected no entries, got %+v", got)
	}
}

func TestMergeRelabelsVmessViaPs(t *testing.T) {
	t.Parallel()

	body := []byte("vmess://" +
		base64.StdEncoding.EncodeToString([]byte(`{"add":"1.2.3.4","port":"443","ps":"Original","id":"uuid"}`)) + "\n")

	entries := stable.Merge([]stable.SourceBody{{Name: "avia", Body: body}})
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

	entries := stable.Merge([]stable.SourceBody{{Name: "src", Body: []byte(ssrLink("Original") + "\n")}})
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
	if e.Tagged != e.Raw {
		t.Errorf("untagged ssr node: Tagged (%q) must equal Raw (%q)", e.Tagged, e.Raw)
	}

	m := mihomoProxyMap(t, e.Raw)
	wantString(t, m, "name", "src-001")
	wantString(t, m, "server", "1.2.3.4")
	wantString(t, m, "type", "ssr")
}

// TestMergeKeepsGeoTagSSR: the published copy folds the [GEO][IP] tags into
// "remarks" too — an ssr node must never reach the fragment path, tagged or not.
func TestMergeKeepsGeoTagSSR(t *testing.T) {
	t.Parallel()

	body := []byte(ssrLink("[GEO:FI][IP:1.2.3.4] orig") + "\n")
	entries := stable.Merge([]stable.SourceBody{{Name: "src", Body: body}})
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Country != "FI" {
		t.Errorf("country: got %q, want FI", e.Country)
	}
	wantString(t, mihomoProxyMap(t, e.Raw), "name", "src-001")
	wantString(t, mihomoProxyMap(t, e.Tagged), "name", "[GEO:FI][IP:1.2.3.4] src-001")
}

func TestMergeKeepsGeoTag(t *testing.T) {
	t.Parallel()

	body := []byte("vless://u@h.example:443#[GEO:FI][IP:1.2.3.4] orig\n")
	entries := stable.Merge([]stable.SourceBody{{Name: "src", Body: body}})
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Raw != "vless://u@h.example:443#src-001" {
		t.Errorf("Raw must be the clean probe label, got %q", e.Raw)
	}
	if e.Tagged != "vless://u@h.example:443#[GEO:FI][IP:1.2.3.4] src-001" {
		t.Errorf("Tagged must keep the geo tag, got %q", e.Tagged)
	}
}

// TestMergeExtractsCountry: Entry.Country mirrors the carried [GEO:xx] tag so
// the checker can report publication-time coverage and per-country counts
// without re-parsing names (vmess hides tags inside base64 ps). "??" = the
// chain resolved nothing; "" = annotation off, or a tag whose payload is not a
// country code.
func TestMergeExtractsCountry(t *testing.T) {
	t.Parallel()

	body := []byte("vless://u@unknown.example:443#[GEO:??][IP:1.2.3.4] a\n" +
		"vless://u@known.example:443#[GEO:FI][IP:1.2.3.4] b\n" +
		"vless://u@untagged.example:443#plain c\n" +
		"vless://u@iptag.example:443#[IP:1.2.3.4][GEO:NL] d\n" +
		// Source-authored junk in a GEO tag: with annotation off the upstream
		// name reaches Merge verbatim, and Country becomes a Prometheus label
		// value, so anything that is not two ASCII letters must be discarded
		// rather than forwarded.
		"vless://u@quote.example:443#[GEO:a\"] e\n" +
		"vless://u@digit.example:443#[GEO:12] f\n" +
		"vless://u@brace.example:443#[GEO:{}] g\n")
	entries := stable.Merge([]stable.SourceBody{{Name: "src", Body: body}})
	if len(entries) != 7 {
		t.Fatalf("want 7 entries, got %d", len(entries))
	}
	for i, want := range []string{"??", "FI", "", "NL", "", "", ""} {
		if got := entries[i].Country; got != want {
			t.Errorf("entries[%d].Country = %q, want %q", i, got, want)
		}
	}
}

func TestMergeKeepsGeoTagVmess(t *testing.T) {
	t.Parallel()

	body := []byte("vmess://" +
		base64.StdEncoding.EncodeToString([]byte(`{"add":"1.2.3.4","port":"443","ps":"[GEO:FI][IP:1.2.3.4] orig","id":"uuid"}`)) + "\n")
	entries := stable.Merge([]stable.SourceBody{{Name: "src", Body: body}})
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	psOf := func(raw string) string {
		const prefix = "vmess://"
		decoded, err := base64.StdEncoding.DecodeString(raw[len(prefix):])
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		var m map[string]any
		if unmarshalErr := json.Unmarshal(decoded, &m); unmarshalErr != nil {
			t.Fatalf("unmarshal: %v", unmarshalErr)
		}
		s, _ := m["ps"].(string)
		return s
	}
	if got := psOf(entries[0].Raw); got != "src-001" {
		t.Errorf("Raw ps must be the clean probe label, got %q", got)
	}
	if got := psOf(entries[0].Tagged); got != "[GEO:FI][IP:1.2.3.4] src-001" {
		t.Errorf("Tagged ps must keep the geo tag, got %q", got)
	}
}

func TestMergeUntaggedNameCleanTagged(t *testing.T) {
	t.Parallel()

	// No [GEO][IP] tag (annotation off upstream) -> Tagged == Raw (both clean).
	body := []byte("vless://u@h.example:443#Original Name\n")
	entries := stable.Merge([]stable.SourceBody{{Name: "src", Body: body}})
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Raw != "vless://u@h.example:443#src-001" {
		t.Errorf("Raw must be clean, got %q", e.Raw)
	}
	if e.Tagged != e.Raw {
		t.Errorf("untagged node: Tagged (%q) must equal Raw (%q)", e.Tagged, e.Raw)
	}
}

func TestMergeMixedCaseHostsCollapse(t *testing.T) {
	t.Parallel()

	// Hostnames are case-insensitive: mixed-case duplicates must collapse and
	// share one lowercased dead-cache key, while Raw keeps the original casing.
	body := []byte("vless://a@HOST.Example:443#one\nvless://b@host.EXAMPLE:443#two\n")
	entries := stable.Merge([]stable.SourceBody{{Name: "src", Body: body}})
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
