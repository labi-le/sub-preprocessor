package subscription_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
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

// TestParseVmessEmptyPortRefused pins the end of the fabricated 443: a JSON
// body whose "port" reads as nothing — absent, "", null, or a token that is
// neither string nor number — is refused and booked Unsupported rather than
// published. mihomo's structure decode fails on the empty string ("cannot
// parse 'port' as int", common/structure/structure.go:143-148) and leaves an
// absent or null port at zero, so the fabricated 443 sent a probe to a port
// the node does not have, then a stage="unknown" probe and a dead-cache entry
// where an honest Unsupported booking belongs.
func TestParseVmessEmptyPortRefused(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{
		`{"add":"srv.example","ps":"n"}`,
		`{"add":"srv.example","port":"","ps":"n"}`,
		`{"add":"srv.example","port":true,"ps":"n"}`,
		`{"add":"srv.example","port":null,"ps":"n"}`,
		`{"add":"srv.example","port":{"p":443},"ps":"n"}`,
	} {
		_, count := parseOne(t, vmessLine(payload))
		if count != 0 {
			t.Errorf("payload %s parsed %d nodes, want 0", payload, count)
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

// TestParseVmessAEADAllocatesNothing prices the '@' cue that skips the doomed
// base64 attempt on the AEAD arm: an AEAD line must cost what the generic
// path costs (BenchmarkParse_SingleNode, 0 allocs/op), not the decode buffer
// a decode-first dispatch would allocate before failing per node.
func TestParseVmessAEADAllocatesNothing(t *testing.T) {
	line := []byte("vmess://b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:443?encryption=auto&security=tls#Name\n")
	if allocs := testing.AllocsPerRun(100, func() {
		subscription.Parse(line, func(_ subscription.Node) bool {
			return true
		})
	}); allocs != 0 {
		t.Fatalf("AEAD parse allocated %.0f times per run, want 0", allocs)
	}
}

// aeadLine is the Xray VMessAEAD share-link form — a vmess:// body that is
// not base64 but the authority <user>@<host>:<port>?<params>#<name>.
func aeadLine(rest string) string {
	return "vmess://" + rest
}

// TestParseVmessAEADExtractsServerPortName pins the accepted AEAD shape: the
// server and port come from the authority, the name from the fragment, and
// the query survives verbatim in Raw for the client's own mihomo to dial.
func TestParseVmessAEADExtractsServerPortName(t *testing.T) {
	t.Parallel()

	line := aeadLine("b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:8443?encryption=auto&security=tls&type=tcp#Tokyo AEAD")
	got, count := parseOne(t, line)
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Scheme != subscription.SchemeVmess {
		t.Errorf("scheme: got %q, want vmess", got.Scheme)
	}
	if got.Server != "1.2.3.4" {
		t.Errorf("server: got %q, want 1.2.3.4", got.Server)
	}
	if got.Port != "8443" {
		t.Errorf("port: got %q, want 8443", got.Port)
	}
	if got.Name != "Tokyo AEAD" {
		t.Errorf("name: got %q, want Tokyo AEAD", got.Name)
	}
	if got.FragmentIdx < 0 || got.Raw[:got.FragmentIdx] != line[:strings.IndexByte(line, '#')] {
		t.Errorf("FragmentIdx = %d, want the '#' position", got.FragmentIdx)
	}
	if got.Raw != line {
		t.Errorf("raw: got %q, want the original line", got.Raw)
	}
}

// TestParseVmessAEADRefusedShapes pins the gates the AEAD fallback is refused
// under: handleVShareLink requires both a hostname and a port
// (convert/v.go:17-21), url.Parse must accept the port text (any non-digit
// port makes it fail and the line is skipped, converter.go:239-241), and the
// two slices identify the form by an '@' in the authority — so a portless,
// hostless, non-digit-port or '@'-less body is refused and booked
// Unsupported, never published.
func TestParseVmessAEADRefusedShapes(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		aeadLine("uuid@1.2.3.4?type=tcp#No Port"),
		aeadLine("uuid@1.2.3.4#No Port"),
		aeadLine("uuid@:443#No Host"),
		aeadLine("uuid@1.2.3.4:abc#Bad Port"),
		aeadLine("1.2.3.4:443?type=tcp#No At"),
	} {
		_, count := parseOne(t, line)
		if count != 0 {
			t.Errorf("%q parsed %d nodes, want 0", line, count)
		}
	}
}

// TestParseVmessAEADAcceptsNonUUIDUser pins the user-part verdict: mihomo's
// vmess adapter maps whatever userinfo the link carries through UUIDMap,
// which turns a string uuid.FromString rejects into a deterministic UUIDv5
// rather than refusing it (transport/vmess/vmess.go:87,
// common/utils/uuid.go:46-51), so a body whose "uuid" is not one still dials
// and must parse.
func TestParseVmessAEADAcceptsNonUUIDUser(t *testing.T) {
	t.Parallel()

	got, count := parseOne(t, aeadLine("not-a-uuid@1.2.3.4:443?encryption=none#N"))
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Server != "1.2.3.4" || got.Port != "443" {
		t.Errorf("server:port = %s:%s, want 1.2.3.4:443", got.Server, got.Port)
	}
	if got.Name != "N" {
		t.Errorf("name: got %q, want N", got.Name)
	}
}

// TestParseVmessURLSafeBodyReencoded: decodeBase64Tolerant reads a url-safe
// alphabet body, but the client's mihomo decodes vmess bodies with the STD
// alphabets only (RawStd then Std, convert/base64.go:24-33), so a published
// url-safe line would fail that decode and be misparsed as an AEAD authority.
// parseVmess re-encodes such a body in the STD alphabet at parse, so Raw must
// decode under StdEncoding to the same document, keep the fragment, and
// re-parse to the same node.
func TestParseVmessURLSafeBodyReencoded(t *testing.T) {
	t.Parallel()

	doc := `{"v":"2","add":"1.2.3.4","port":"443","ps":"Nam~e","id":"b831381d","net":"ws"}`
	urlSafe := base64.RawURLEncoding.EncodeToString([]byte(doc))
	if !strings.ContainsAny(urlSafe, "-_") {
		t.Fatalf("fixture body %q does not exercise the url-safe alphabet", urlSafe)
	}

	line := "vmess://" + urlSafe + "#Frag"
	got, count := parseOne(t, line)
	if count != 1 {
		t.Fatalf("got %d nodes, want 1", count)
	}
	if got.Raw == line {
		t.Fatal("url-safe body was published verbatim")
	}
	const prefix = "vmess://"
	const frag = "#Frag"
	if !strings.HasSuffix(got.Raw, frag) {
		t.Fatalf("fragment lost: %q", got.Raw)
	}
	plain, err := base64.StdEncoding.DecodeString(got.Raw[len(prefix) : len(got.Raw)-len(frag)])
	if err != nil {
		t.Fatalf("published body is not STD base64: %v", err)
	}
	if string(plain) != doc {
		t.Errorf("published body decodes to %q, want %q", plain, doc)
	}

	// Re-parsing the published line must yield the same node — this is the
	// property that makes parse the right seam for both endpoints.
	again, count := parseOne(t, got.Raw)
	if count != 1 {
		t.Fatalf("re-parse got %d nodes, want 1", count)
	}
	if again.Server != got.Server || again.Port != got.Port || again.Name != got.Name {
		t.Errorf("re-parse node %+v differs from %+v", again, got)
	}
}

// TestRewriteVmessAEADNameReplacesFragment pins the relabel the AEAD form
// needs: the display name lives in the fragment, so the new label replaces it
// and the relabeled line parses back to the same node under the new name —
// the property stable.Merge's relabel depends on.
func TestRewriteVmessAEADNameReplacesFragment(t *testing.T) {
	t.Parallel()

	line := aeadLine("b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:8443?type=tcp#Old Name")
	out, ok := subscription.RewriteVmessAEADName(line, "avia-003")
	if !ok {
		t.Fatal("rewrite failed")
	}
	want := aeadLine("b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:8443?type=tcp#avia-003")
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}

	got, count := parseOne(t, out)
	if count != 1 {
		t.Fatalf("re-parse got %d nodes, want 1", count)
	}
	if got.Name != "avia-003" {
		t.Errorf("name: got %q, want avia-003", got.Name)
	}
	if got.Server != "1.2.3.4" || got.Port != "8443" {
		t.Errorf("server:port = %s:%s, want 1.2.3.4:8443", got.Server, got.Port)
	}
}

// TestRewriteVmessAEADNameAppendsMissingFragment: a fragmentless AEAD line
// names its node after its server, so the relabel appends the fragment mihomo
// reads the name from.
func TestRewriteVmessAEADNameAppendsMissingFragment(t *testing.T) {
	t.Parallel()

	line := aeadLine("b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:443?type=tcp")
	out, ok := subscription.RewriteVmessAEADName(line, "avia-003")
	if !ok {
		t.Fatal("rewrite failed")
	}
	if want := line + "#avia-003"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// TestRewriteVmessAEADNameRefusesOtherShapes: the AEAD rewriter owns the
// non-base64 body only, the JSON body is RewriteVmessName's, and the two
// refuse exactly what parseVmess refuses between them — a relabel flow that
// tries both drops nothing the parser accepted.
func TestRewriteVmessAEADNameRefusesOtherShapes(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		vmessLine(`{"v":"2","ps":"Old","add":"1.2.3.4","port":"443","id":"uuid"}`),
		"vmess://!!!not-base64!!!",
		aeadLine("uuid@1.2.3.4"),
	} {
		if _, ok := subscription.RewriteVmessAEADName(raw, "x"); ok {
			t.Errorf("RewriteVmessAEADName accepted %q", raw)
		}
	}
	if _, ok := subscription.RewriteVmessName(aeadLine("uuid@1.2.3.4:443#n"), "x"); ok {
		t.Error("RewriteVmessName accepted an AEAD body")
	}
}
