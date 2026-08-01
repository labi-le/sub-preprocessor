package stable

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"
)

// traceBody is a real /cdn-cgi/trace answer (captured live, 211 bytes). colo is
// the Cloudflare edge that served us; loc is the country of the CLIENT address,
// which through a proxy is the node's egress.
const traceBody = "fl=1169f8\nh=cloudflare.com\nip=93.100.194.40\nts=1785592190.041\n" +
	"visit_scheme=https\nuag=curl/8.21.0\ncolo=HEL\nsliver=none\nhttp=http/2\n" +
	"loc=RU\ntls=TLSv1.3\nsni=plaintext\n"

func TestParseTrace(t *testing.T) {
	t.Parallel()

	trace, ok := parseTrace(traceBody)
	if !ok {
		t.Fatal("a real trace body must parse")
	}
	if trace.IP != "93.100.194.40" || trace.Country != "RU" {
		t.Fatalf("got %+v", trace)
	}

	// Both fields or nothing: a half-answer means the response did not come
	// from the trace endpoint, and a tag is only worth replacing when the
	// replacement is complete.
	for name, body := range map[string]string{
		"only loc":     "colo=HEL\nloc=RU\n",
		"only ip":      "ip=1.2.3.4\ncolo=HEL\n",
		"empty":        "",
		"html":         "<html><body>403 Forbidden</body></html>",
		"loc too long": "ip=1.2.3.4\nloc=XYZ\n",
		"loc empty":    "ip=1.2.3.4\nloc=\n",
	} {
		if _, bad := parseTrace(body); bad {
			t.Errorf("%s: must not parse", name)
		}
	}
}

func TestSwapTagValuesKeepsOtherTagsAndOrder(t *testing.T) {
	t.Parallel()

	got := swapTagValues("[GEO:CA][IP:104.21.0.119][SPD:20M]", TraceResult{Country: "DE", IP: "1.2.3.4"})
	if want := "[GEO:DE][IP:1.2.3.4][SPD:20M]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// A name with no GEO/IP is left exactly as it was: annotation is the
	// operator's choice and this filter does not introduce it.
	if kept := swapTagValues("[SPD:5M]", TraceResult{Country: "DE", IP: "1.2.3.4"}); kept != "[SPD:5M]" {
		t.Fatalf("unrelated tags must survive untouched: %q", kept)
	}
}

// The offline chain places the resolved anycast address; the node reports where
// it actually exits. This is the whole point of the filter.
func TestGeotraceFilterCorrectsAnycastTag(t *testing.T) {
	t.Parallel()

	survivors := []Survivor{
		{Entry: Entry{Label: "s-001", Addr: "h1:443", Tagged: "vless://u@h1:443#[GEO:CA][IP:104.21.0.119] s-001"}},
		{Entry: Entry{Label: "s-002", Addr: "h2:443", Tagged: "vless://u@h2:443#[GEO:SE][IP:2.2.2.2] s-002"}},
	}
	check := func(context.Context, []mihomo.Proxy) map[string]TraceResult {
		return map[string]TraceResult{"s-001": {Country: "DE", IP: "5.6.7.8"}}
	}

	f := &geotraceFilter{check: check, annotate: true, logger: zerolog.Nop()}
	kept, rep := f.apply(context.Background(), survivors, nil)
	if len(kept) != 2 {
		t.Fatalf("geotrace must drop nothing, got %d", len(kept))
	}
	if !strings.Contains(kept[0].Tagged, "[GEO:DE][IP:5.6.7.8]") {
		t.Fatalf("tag not corrected: %q", kept[0].Tagged)
	}
	// A node the trace could not reach keeps the offline guess rather than
	// losing its label.
	if !strings.Contains(kept[1].Tagged, "[GEO:SE][IP:2.2.2.2]") {
		t.Fatalf("unanswered node must keep its tag: %q", kept[1].Tagged)
	}
	if rep.Dropped["unanswered"] != 1 || rep.Dropped["corrected"] != 1 {
		t.Fatalf("report = %+v", rep.Dropped)
	}
}

func TestGeotraceFilterRespectsAnnotateOff(t *testing.T) {
	t.Parallel()

	called := false
	check := func(context.Context, []mihomo.Proxy) map[string]TraceResult {
		called = true

		return nil
	}
	survivors := []Survivor{{Entry: Entry{Label: "s-001", Tagged: "vless://u@h1:443#s-001"}}}
	f := &geotraceFilter{check: check, annotate: false, logger: zerolog.Nop()}
	kept, _ := f.apply(context.Background(), survivors, nil)
	if called {
		t.Error("annotate=false must not spend a through-node request")
	}
	if kept[0].Tagged != survivors[0].Tagged {
		t.Errorf("line changed: %q", kept[0].Tagged)
	}
}

// vmess and ssr keep the display name inside their payload, not in a #fragment.
// Retagging has to reach it there, which is why this path goes through
// relabelNode instead of touching the fragment.
func TestGeotraceRetagsVmessAndSSR(t *testing.T) {
	t.Parallel()

	vm := map[string]any{"v": "2", "ps": "[GEO:CA][IP:104.21.0.119] n", "add": "1.2.3.4", "port": "443", "id": "u"}
	raw, _ := json.Marshal(vm)
	vmess := "vmess://" + base64.StdEncoding.EncodeToString(raw)

	got := retagTraced(vmess, TraceResult{Country: "DE", IP: "5.6.7.8"})
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "vmess://"))
	if err != nil {
		t.Fatalf("vmess payload not base64 after retag: %v", err)
	}
	var back map[string]any
	if jsonErr := json.Unmarshal(payload, &back); jsonErr != nil {
		t.Fatalf("vmess payload not JSON after retag: %v", jsonErr)
	}
	if back["ps"] != "[GEO:DE][IP:5.6.7.8] n" {
		t.Errorf("vmess ps = %v", back["ps"])
	}
	if back["add"] != "1.2.3.4" {
		t.Errorf("vmess add lost: %v", back["add"])
	}

	head := "1.2.3.4:8388:origin:aes-256-cfb:plain:" + base64.RawURLEncoding.EncodeToString([]byte("pw"))
	q := "/?remarks=" + base64.RawURLEncoding.EncodeToString([]byte("[GEO:CA][IP:104.21.0.119] n"))
	ssr := "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(head+q))

	gotSSR := retagTraced(ssr, TraceResult{Country: "DE", IP: "5.6.7.8"})
	if strings.Contains(gotSSR, "#") {
		t.Fatalf("ssr must stay fragment-free or mihomo refuses it: %q", gotSSR)
	}
	decoded, decErr := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(gotSSR, "ssr://"))
	if decErr != nil {
		t.Fatalf("ssr payload not base64 after retag: %v", decErr)
	}
	_, query, _ := strings.Cut(string(decoded), "/?")
	values, parseErr := url.ParseQuery(query)
	if parseErr != nil {
		t.Fatalf("ssr query: %v", parseErr)
	}
	remarks, remErr := base64.RawURLEncoding.DecodeString(values.Get("remarks"))
	if remErr != nil {
		t.Fatalf("ssr remarks not base64: %v", remErr)
	}
	if string(remarks) != "[GEO:DE][IP:5.6.7.8] n" {
		t.Errorf("ssr remarks = %q", remarks)
	}
}
