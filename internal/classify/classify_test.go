package classify_test

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/classify"
)

func TestBodyCountsNodes(t *testing.T) {
	t.Parallel()

	raw := "vless://u@1.1.1.1:443?security=reality#a\nvless://u@2.2.2.2:443#b\n"
	body := []byte(base64.StdEncoding.EncodeToString([]byte(raw)))

	got := classify.Body(body, "", 1000)
	if got.Nodes != 2 {
		t.Fatalf("Nodes = %d, want 2", got.Nodes)
	}
	if !got.Live() {
		t.Fatalf("expected Live for a 2-node body")
	}
}

func TestBodyPlainNotBase64(t *testing.T) {
	t.Parallel()

	body := []byte("vless://u@1.1.1.1:443#a\n")
	if got := classify.Body(body, "", 1000); got.Nodes != 1 || !got.Live() {
		t.Fatalf("plain body: got %+v, want 1 live node", got)
	}
}

func TestBodyUppercaseSchemeCounts(t *testing.T) {
	t.Parallel()

	// Schemes are case-insensitive (RFC 3986); VLESS:// must count as vless.
	body := []byte("VLESS://u@1.1.1.1:443#a\n")
	if got := classify.Body(body, "", 1000); got.Nodes != 1 || !got.Live() {
		t.Fatalf("uppercase scheme: got %+v, want 1 live node", got)
	}
}

func TestBodyFloatExpire(t *testing.T) {
	t.Parallel()

	// Some panels emit expire as a float ("expire=1786085295.0"); it must
	// still be parsed (truncated) instead of being ignored.
	body := []byte("vless://u@1.1.1.1:443#a\n")
	got := classify.Body(body, "upload=0; download=0; total=0; expire=500.0", 1000)
	if !got.Expired || got.Live() {
		t.Fatalf("past float expiry: got %+v, want expired not live", got)
	}
	got = classify.Body(body, "expire=2000.5", 1000)
	if got.Expired || !got.Live() {
		t.Fatalf("future float expiry: got %+v, want live non-expired", got)
	}
}

func TestBodyExpiredNotLive(t *testing.T) {
	t.Parallel()

	body := []byte(base64.StdEncoding.EncodeToString([]byte("vless://u@1.1.1.1:443#a\n")))
	// expire=500 is before now=1000 → expired.
	got := classify.Body(body, "upload=0; download=0; total=0; expire=500", 1000)
	if !got.Expired {
		t.Fatalf("expected Expired for past expiry")
	}
	if got.Live() {
		t.Fatalf("expired body must not be Live")
	}
}

func TestBodyFutureExpiryLive(t *testing.T) {
	t.Parallel()

	body := []byte(base64.StdEncoding.EncodeToString([]byte("vless://u@1.1.1.1:443#a\n")))
	got := classify.Body(body, "expire=2000", 1000)
	if got.Expired || !got.Live() {
		t.Fatalf("future expiry: got %+v, want live non-expired", got)
	}
}

func TestBodyNoNodesNotLive(t *testing.T) {
	t.Parallel()

	if got := classify.Body([]byte("just some prose, no nodes"), "", 1000); got.Nodes != 0 || got.Live() {
		t.Fatalf("prose body: got %+v, want 0 nodes not live", got)
	}
}

func TestBodyRejectsHTMLLinks(t *testing.T) {
	t.Parallel()

	// An HTML page full of http(s):// links must not look like a subscription,
	// even though the generic node parser would accept those authorities.
	body := []byte(`<html><a href="https://kod.ru/article">x</a>` +
		`<link href="https://cdn.example.com:443/s.css"><a href="https://t.me/chan">y</a></html>`)
	if got := classify.Body(body, "", 1000); got.Nodes != 0 || got.Live() {
		t.Fatalf("HTML page must not classify as subscription, got %+v", got)
	}
}

// TestBodyCountsMierusNodes: a mierus:// body is a real subscription — the node
// parser reads the server and the "port" query value out of it — so leaving
// mierus out of proxySchemes made such a source classify as nodeless and the
// crawler never adopt it.
func TestBodyCountsMierusNodes(t *testing.T) {
	t.Parallel()

	raw := "mierus://user:pass@1.2.3.4?port=2999&protocol=TCP#m1\n" +
		"mierus://user:pass@5.6.7.8?port=9998-9999&protocol=UDP#m2\n"
	body := []byte(base64.StdEncoding.EncodeToString([]byte(raw)))

	got := classify.Body(body, "", 1000)
	if got.Nodes != 2 {
		t.Fatalf("Nodes = %d, want 2", got.Nodes)
	}
	if !got.Live() {
		t.Fatalf("a mierus-only body must classify as live, got %+v", got)
	}
}

// TestBodyRejectsPortfulProxyURLs: parseNode accepts a portful http/socks link,
// so only proxySchemes keeps a docs page or a client-setup snippet from reading
// as a subscription. Adding those schemes to the gate would break exactly that.
func TestBodyRejectsPortfulProxyURLs(t *testing.T) {
	t.Parallel()

	body := []byte("Set your client to socks5://127.0.0.1:1080\n" +
		"Docs: https://example.com:8443/docs\n")
	if got := classify.Body(body, "", 1000); got.Nodes != 0 || got.Live() {
		t.Fatalf("portful http/socks URLs must not classify as a subscription, got %+v", got)
	}
}

// TestStatusErrorGone pins which statuses prove a subscription is gone. Callers
// delete a source on a Gone verdict, so a status that merely means "not now"
// (WAF challenge, back-pressure, origin failure) must never report true.
func TestStatusErrorGone(t *testing.T) {
	t.Parallel()

	gone := []int{http.StatusNotFound, http.StatusGone, http.StatusUnavailableForLegalReasons}
	transient := []int{
		http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooEarly,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}
	for _, code := range gone {
		if !(&classify.StatusError{Code: code, Status: http.StatusText(code)}).Gone() {
			t.Errorf("status %d must be definitive", code)
		}
	}
	for _, code := range transient {
		if (&classify.StatusError{Code: code, Status: http.StatusText(code)}).Gone() {
			t.Errorf("status %d must not be treated as definitive", code)
		}
	}
}

// TestResultReasonAgreesWithLive: Reason is what lets a caller distinguish the
// two very different ways a body fails Live — an origin-advertised expiry is a
// death verdict, a nodeless 2xx is not — so the two must never disagree about
// which one it is. Expired outranks a zero node count: a body that is both is
// dead, not merely nodeless.
func TestResultReasonAgreesWithLive(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		res    classify.Result
		reason classify.Reason
		name   string
	}{
		{classify.Result{Nodes: 3}, classify.ReasonLive, "live"},
		{classify.Result{}, classify.ReasonNodeless, "nodeless-2xx"},
		{classify.Result{Nodes: 3, Expired: true}, classify.ReasonExpired, "expired"},
		{classify.Result{Expired: true}, classify.ReasonExpired, "expired"},
	} {
		got := tc.res.Reason()
		if got != tc.reason {
			t.Errorf("%+v: Reason = %v, want %v", tc.res, got, tc.reason)
		}
		if got.String() != tc.name {
			t.Errorf("%+v: Reason.String = %q, want %q", tc.res, got.String(), tc.name)
		}
		if live := tc.res.Live(); live != (got == classify.ReasonLive) {
			t.Errorf("%+v: Live = %v but Reason = %v", tc.res, live, got)
		}
	}
}

// TestBodyReportsWhyItIsNotLive walks Body end to end: the reason has to come
// out of a parsed body, not only out of a hand-built Result.
func TestBodyReportsWhyItIsNotLive(t *testing.T) {
	t.Parallel()

	const node = "vless://uuid@host.example:443?type=tcp#n"
	const now = 1786085295

	if got := classify.Body([]byte(node), "", now).Reason(); got != classify.ReasonLive {
		t.Errorf("a one-node body reads %v, want live", got)
	}
	if got := classify.Body([]byte("<html>login</html>"), "", now).Reason(); got != classify.ReasonNodeless {
		t.Errorf("a login page reads %v, want nodeless-2xx", got)
	}
	if got := classify.Body([]byte(node), "expire=1786085294", now).Reason(); got != classify.ReasonExpired {
		t.Errorf("an expired subscription reads %v, want expired", got)
	}
}

// TestBodyRejectsNilUUIDPlaceholder pins the measured dead-panel shape: prose
// saying the subscription expired, one node whose credential is the Nil UUID,
// and a subscription-userinfo whose expire= is still in the FUTURE — so the
// header gate cannot see the death and only the node carries the evidence.
func TestBodyRejectsNilUUIDPlaceholder(t *testing.T) {
	t.Parallel()

	body := []byte("# This subscription was valid for exactly 24 hours and has now expired.\n" +
		"# A new free link is announced at https://example.com\n\n" +
		"vless://00000000-0000-0000-0000-000000000000@192.0.2.1:1111" +
		"?encryption=none&security=none&type=tcp&headerType=none#get%20a%20new%20link\n")

	got := classify.Body(body, "upload=0; download=0; total=1099511627776; expire=2000", 1000)
	if got.Nodes != 0 {
		t.Fatalf("Nodes = %d, want 0: a Nil-UUID node is not a usable node", got.Nodes)
	}
	if got.Reason() != classify.ReasonNodeless {
		t.Fatalf("reason = %v, want nodeless-2xx", got.Reason())
	}
	// Nodeless, never a death verdict: Prune must not delete on this body.
	if got.Expired {
		t.Fatalf("placeholder body read as a death verdict: %+v", got)
	}
}

// TestBodyPlaceholderDoesNotPoisonRealNodes: panels pad a working list with a
// notice node, so the guard has to drop that one node and keep the rest.
func TestBodyPlaceholderDoesNotPoisonRealNodes(t *testing.T) {
	t.Parallel()

	raw := "vless://00000000-0000-0000-0000-000000000000@192.0.2.1:1111#notice\n" +
		"vless://a1b2c3d4-0000-4000-8000-000000000001@198.51.100.2:443#a\n" +
		"trojan://user@203.0.113.3:443#b\n"
	body := []byte(base64.StdEncoding.EncodeToString([]byte(raw)))

	got := classify.Body(body, "", 1000)
	if got.Nodes != 2 || !got.Live() {
		t.Fatalf("got %+v, want 2 nodes and live", got)
	}
}

// TestBodyRejectsUnspecifiedServerPlaceholder: the other placeholder spelling
// carries its error text in the node NAME and points at the unspecified
// address. The credential is ordinary here, so only the address can fire.
func TestBodyRejectsUnspecifiedServerPlaceholder(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"vless://a1b2c3d4-1111-4000-8000-000000000009@0.0.0.0:1#subscription%20has%20died\n",
		"vless://a1b2c3d4-1111-4000-8000-000000000009@[::]:1#traffic%20limit%20exceeded\n",
	} {
		got := classify.Body([]byte(raw), "", 1000)
		if got.Nodes != 0 || got.Reason() != classify.ReasonNodeless {
			t.Errorf("%q: got %+v reason %v, want 0 nodes and nodeless-2xx", raw, got, got.Reason())
		}
	}
}

// TestBodyCountsUUIDContainingZeros: the guard demands the whole Nil UUID, one
// zero less included, or it would drop the working node of a live source.
func TestBodyCountsUUIDContainingZeros(t *testing.T) {
	t.Parallel()

	raw := "vless://00000000-0000-0000-0000-000000000001@192.0.2.1:443#a\n" +
		"vless://10000000-0000-0000-0000-000000000000@198.51.100.2:443#b\n"

	got := classify.Body([]byte(raw), "", 1000)
	if got.Nodes != 2 || !got.Live() {
		t.Fatalf("got %+v, want 2 nodes and live", got)
	}
}

// TestBodyCountsShortAllZeroCredential: "0" is a legal password, so the guard
// demands the Nil UUID's full digit count instead of any all-zero credential.
func TestBodyCountsShortAllZeroCredential(t *testing.T) {
	t.Parallel()

	raw := "trojan://0@192.0.2.1:443#a\n" +
		"vless://0-0@198.51.100.2:443#b\n"

	got := classify.Body([]byte(raw), "", 1000)
	if got.Nodes != 2 || !got.Live() {
		t.Fatalf("got %+v, want 2 nodes and live", got)
	}
}

// TestBodyRejectsDashLeadingNilCredential: the guard counts zeros and ignores
// dashes wherever they fall, so a credential may legally START with one. The
// first-byte fast path in front of that scan has to admit '-' for that reason,
// and dropping it would silently narrow the guard.
func TestBodyRejectsDashLeadingNilCredential(t *testing.T) {
	t.Parallel()

	raw := "vless://-00000000000000000000000000000000@192.0.2.1:443#x\n"

	got := classify.Body([]byte(raw), "", 1000)
	if got.Nodes != 0 || got.Reason() != classify.ReasonNodeless {
		t.Fatalf("got %+v reason %v, want 0 nodes and nodeless-2xx", got, got.Reason())
	}
}

// benchNodes is the node count the wave's A/B used, so a delta measured here is
// comparable with the numbers recorded against it.
const benchNodes = 1000

// benchBody builds a plain URI-list body whose credentials cycle through all 16
// leading hex digits: a real UUID starts with '0' one time in sixteen, and a
// corpus that never does would hide the cost of the Nil-UUID scan entirely.
func benchBody(nodes int, server func(i int) string) []byte {
	var sb strings.Builder
	for i := range nodes {
		sb.WriteString("vless://")
		sb.WriteByte("0123456789abcdef"[i%16])
		sb.WriteString("1b2c3d4-1111-4000-8000-0000")
		sb.WriteString(strconv.Itoa(100000000 + i))
		sb.WriteByte('@')
		sb.WriteString(server(i))
		sb.WriteString(":443?security=tls#node")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

func benchBodyDigitHost() []byte {
	return benchBody(benchNodes, func(int) string { return "0.tcp.example.com" })
}

func benchBodyHostnames() []byte {
	return benchBody(benchNodes, func(i int) string {
		return "node" + strconv.Itoa(i) + ".example.com"
	})
}

var benchSink int

func benchClassify(b *testing.B, body []byte) {
	b.ReportAllocs()
	for b.Loop() {
		r := classify.Body(body, "", 1000)
		if r.Nodes != benchNodes {
			b.Fatalf("Nodes = %d, want %d", r.Nodes, benchNodes)
		}
		benchSink = r.Nodes
	}
}

// BenchmarkBodyDigitLeadingHostnames is the shape a server-prefix gate mistakes
// for an address: a tunnel host like "0.tcp.example.com" reached
// netip.ParseAddr, whose error is heap-boxed, and cost 46.88 KiB and 1000 allocs
// per body. The ordinary-hostname twin below cannot see that at all.
func BenchmarkBodyDigitLeadingHostnames(b *testing.B) {
	benchClassify(b, benchBodyDigitHost())
}

func BenchmarkBodyHostnames(b *testing.B) {
	benchClassify(b, benchBodyHostnames())
}

// TestBodyAllocatesNothingPerNode is the standing guard the benchmarks above can
// only report: classify.Body walks views into the caller's body, so its cost per
// node must be zero allocations regardless of what the servers spell. The
// threshold is loose because -race and coverage add their own; the regression it
// exists to catch was one alloc per node.
func TestBodyAllocatesNothingPerNode(t *testing.T) {
	for name, body := range map[string][]byte{
		"digit-leading hostnames": benchBodyDigitHost(),
		"ordinary hostnames":      benchBodyHostnames(),
	} {
		got := testing.AllocsPerRun(3, func() {
			if r := classify.Body(body, "", 1000); r.Nodes != benchNodes {
				t.Fatalf("%s: Nodes = %d, want %d", name, r.Nodes, benchNodes)
			}
		})
		if got > benchNodes/10 {
			t.Errorf("%s: %.0f allocs for %d nodes, want per-node zero", name, got, benchNodes)
		}
	}
}
