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

// traceBody is a real /cdn-cgi/trace answer (captured live). colo is the
// Cloudflare edge that served us; loc is the country of the CLIENT address,
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

	got, moved := swapTagValues("[GEO:CA][IP:104.21.0.119][SPD:20M]", TraceResult{Country: "DE", IP: "1.2.3.4"})
	if want := "[GEO:DE][IP:1.2.3.4][SPD:20M]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !moved {
		t.Error("CA -> DE is a country correction")
	}
	// A name with no GEO/IP is left exactly as it was: annotation is the
	// operator's choice and this filter does not introduce it.
	if kept, _ := swapTagValues("[SPD:5M]", TraceResult{Country: "DE", IP: "1.2.3.4"}); kept != "[SPD:5M]" {
		t.Fatalf("unrelated tags must survive untouched: %q", kept)
	}
}

// The tag run reaching this filter in the configured chain is NOT space-free:
// config.yaml runs bandwidth first and annotateSpeed prepends "[SPD:<n>M] ",
// which rewrite.LeadingTags then hands over gap and all. A scan that stops at
// the first blank leaves every production survivor untouched — silently, since
// this filter drops nothing and warns about nothing.
func TestSwapTagValuesSpansInterTagBlanks(t *testing.T) {
	t.Parallel()

	res := TraceResult{Country: "DE", IP: "5.6.7.8"}
	for _, tc := range []struct{ in, want string }{
		{"[SPD:20M] [GEO:CA][IP:104.21.0.119]", "[SPD:20M] [GEO:DE][IP:5.6.7.8]"},
		{"[GEO:CA] [IP:104.21.0.119] [SPD:20M]", "[GEO:DE] [IP:5.6.7.8] [SPD:20M]"},
		{"[SPD:20M]\t[GEO:CA]", "[SPD:20M]\t[GEO:DE]"},
	} {
		got, _ := swapTagValues(tc.in, res)
		if got != tc.want {
			t.Errorf("swapTagValues(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// swapTagValues reports a correction only when the COUNTRY moved. An egress
// whose address changed but whose country was already right is a retag, not a
// correction, and conflating the two inflates the one number this filter
// exists to publish.
func TestSwapTagValuesReportsOnlyCountryMoves(t *testing.T) {
	t.Parallel()

	res := TraceResult{Country: "DE", IP: "5.6.7.8"}
	if _, moved := swapTagValues("[GEO:DE][IP:104.21.0.119]", res); moved {
		t.Error("same country, moved address: not a correction")
	}
	if _, moved := swapTagValues("[IP:104.21.0.119]", res); moved {
		t.Error("no GEO tag at all: nothing to correct")
	}
	if _, moved := swapTagValues("[GEO:??][IP:104.21.0.119]", res); !moved {
		t.Error("?? -> DE is the correction this filter was built for")
	}
}

// The offline chain places the resolved anycast address; the node reports where
// it actually exits. This is the whole point of the filter.
func TestGeotraceFilterCorrectsAnycastTag(t *testing.T) {
	t.Parallel()

	survivors := []Survivor{
		{Entry: Entry{Label: "s-001", Addr: "h1:443", Country: "CA", Tagged: "vless://u@h1:443#[GEO:CA][IP:104.21.0.119] s-001"}},
		{Entry: Entry{Label: "s-002", Addr: "h2:443", Country: "SE", Tagged: "vless://u@h2:443#[GEO:SE][IP:2.2.2.2] s-002"}},
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
	// checker.go reads Entry.Country AFTER this pass to build the kept-country
	// and geo-unknown gauges, so a stale field publishes [GEO:DE] while
	// counting the node as CA.
	if kept[0].Country != "DE" {
		t.Errorf("Entry.Country = %q, want DE", kept[0].Country)
	}
	// A node the trace could not reach keeps the offline guess rather than
	// losing its label.
	if !strings.Contains(kept[1].Tagged, "[GEO:SE][IP:2.2.2.2]") {
		t.Fatalf("unanswered node must keep its tag: %q", kept[1].Tagged)
	}
	if kept[1].Country != "SE" {
		t.Errorf("unanswered node's country changed: %q", kept[1].Country)
	}
	if rep.Notes[noteUnanswered] != 1 || rep.Notes[noteCorrected] != 1 {
		t.Fatalf("notes = %+v", rep.Notes)
	}
	// Nothing was dropped, so the drops channel stays empty: it feeds a chart
	// whose whole subject is survivors thrown away.
	if len(rep.Dropped) != 0 || rep.In != rep.Kept {
		t.Errorf("report = %+v", rep)
	}
}

// The shape that actually reaches this filter in production: bandwidth runs
// first, so the survivor arrives with "[SPD:20M] " already in front of the tags
// the trace has to correct. The space-free fixture above passed while every
// real node went untouched.
func TestGeotraceFilterCorrectsSpeedTaggedName(t *testing.T) {
	t.Parallel()

	survivors := []Survivor{{Entry: Entry{
		Label: "s-001", Addr: "h1:443", Country: "CA",
		Tagged: "vless://u@h1:443#[SPD:20M] [GEO:CA][IP:104.21.0.119] s-001",
	}}}
	check := func(context.Context, []mihomo.Proxy) map[string]TraceResult {
		return map[string]TraceResult{"s-001": {Country: "DE", IP: "5.6.7.8"}}
	}

	f := &geotraceFilter{check: check, annotate: true, logger: zerolog.Nop()}
	kept, rep := f.apply(context.Background(), survivors, nil)
	want := "vless://u@h1:443#[SPD:20M] [GEO:DE][IP:5.6.7.8] s-001"
	if kept[0].Tagged != want {
		t.Fatalf("got %q, want %q", kept[0].Tagged, want)
	}
	if kept[0].Country != "DE" {
		t.Errorf("Entry.Country = %q, want DE", kept[0].Country)
	}
	if rep.Notes[noteCorrected] != 1 {
		t.Errorf("notes = %+v", rep.Notes)
	}
}

func TestGeotraceFilterRespectsAnnotateOff(t *testing.T) {
	t.Parallel()

	called := false
	check := func(context.Context, []mihomo.Proxy) map[string]TraceResult {
		called = true

		return nil
	}
	const line = "vless://u@h1:443#[GEO:CA][IP:104.21.0.119] s-001"
	survivors := []Survivor{{Entry: Entry{Label: "s-001", Country: "CA", Tagged: line}}}
	f := &geotraceFilter{check: check, annotate: false, logger: zerolog.Nop()}
	kept, rep := f.apply(context.Background(), survivors, nil)
	if called {
		t.Error("annotate=false must not spend a through-node request")
	}
	// Against the literal, not against survivors[0]: apply returns the input
	// slice, so the two are the same element and would compare equal however
	// badly it was rewritten.
	if kept[0].Tagged != line {
		t.Errorf("line changed: %q", kept[0].Tagged)
	}
	if kept[0].Country != "CA" {
		t.Errorf("country changed: %q", kept[0].Country)
	}
	if len(rep.Notes) != 0 {
		t.Errorf("no trace ran, so nothing to note: %+v", rep.Notes)
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

	got, moved := retagTraced(vmess, TraceResult{Country: "DE", IP: "5.6.7.8"})
	if !moved {
		t.Error("CA -> DE inside the payload is a correction")
	}
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

	gotSSR, movedSSR := retagTraced(ssr, TraceResult{Country: "DE", IP: "5.6.7.8"})
	if !movedSSR {
		t.Error("CA -> DE inside the ssr payload is a correction")
	}
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

// corrected is the headline number: how often the offline chain named the wrong
// COUNTRY. Deciding it by searching the published line for "[GEO:DE]" books
// every changed vmess/ssr node as a correction, because their display name is
// inside the base64 payload and the literal never appears in the line at all.
func TestGeotraceCountsOnlyCountryCorrections(t *testing.T) {
	t.Parallel()

	// Country already right, only the anycast address moved.
	vm := map[string]any{"v": "2", "ps": "[GEO:DE][IP:104.21.0.119] s-001", "add": "1.2.3.4", "port": "443", "id": "u"}
	raw, err := json.Marshal(vm)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	survivors := []Survivor{{Entry: Entry{
		Label: "s-001", Addr: "1.2.3.4:443", Country: "DE",
		Tagged: "vmess://" + base64.StdEncoding.EncodeToString(raw),
	}}}
	before := survivors[0].Tagged
	check := func(context.Context, []mihomo.Proxy) map[string]TraceResult {
		return map[string]TraceResult{"s-001": {Country: "DE", IP: "5.6.7.8"}}
	}

	f := &geotraceFilter{check: check, annotate: true, logger: zerolog.Nop()}
	kept, rep := f.apply(context.Background(), survivors, nil)
	if kept[0].Tagged == before {
		t.Fatal("the IP tag must still be rewritten")
	}
	if rep.Notes[noteCorrected] != 0 {
		t.Errorf("country did not move, so nothing was corrected: %+v", rep.Notes)
	}
}

// A survivor the trace confirms outright: same country, same address. Nothing
// is rewritten, so it counts as neither corrected nor unanswered — the two
// notes are not a partition of the survivor set.
func TestGeotraceConfirmedTagCountsInNoNote(t *testing.T) {
	t.Parallel()

	const line = "vless://u@h1:443#[GEO:DE][IP:5.6.7.8] s-001"
	survivors := []Survivor{{Entry: Entry{Label: "s-001", Addr: "h1:443", Country: "DE", Tagged: line}}}
	check := func(context.Context, []mihomo.Proxy) map[string]TraceResult {
		return map[string]TraceResult{"s-001": {Country: "DE", IP: "5.6.7.8"}}
	}

	f := &geotraceFilter{check: check, annotate: true, logger: zerolog.Nop()}
	kept, rep := f.apply(context.Background(), survivors, nil)
	if kept[0].Tagged != line {
		t.Errorf("nothing to change, yet the line moved: %q", kept[0].Tagged)
	}
	if rep.Notes[noteCorrected] != 0 || rep.Notes[noteUnanswered] != 0 {
		t.Errorf("notes = %+v", rep.Notes)
	}
}

// An untagged survivor is the operator's choice (annotate off upstream, or a
// source name this pipeline never annotated): the filter does not introduce a
// tag, and with no [GEO:] tag to mirror it must not invent an Entry.Country
// either — the kept-country gauge counts tags, not traces.
func TestGeotraceLeavesUntaggedNameAlone(t *testing.T) {
	t.Parallel()

	const line = "vless://u@h1:443#s-001"
	survivors := []Survivor{{Entry: Entry{Label: "s-001", Addr: "h1:443", Tagged: line}}}
	check := func(context.Context, []mihomo.Proxy) map[string]TraceResult {
		return map[string]TraceResult{"s-001": {Country: "DE", IP: "5.6.7.8"}}
	}

	f := &geotraceFilter{check: check, annotate: true, logger: zerolog.Nop()}
	kept, rep := f.apply(context.Background(), survivors, nil)
	if kept[0].Tagged != line {
		t.Errorf("untagged name annotated: %q", kept[0].Tagged)
	}
	if kept[0].Country != "" {
		t.Errorf("Entry.Country = %q, want empty: the published name carries no GEO tag", kept[0].Country)
	}
	if rep.Notes[noteCorrected] != 0 {
		t.Errorf("notes = %+v", rep.Notes)
	}
}

func TestRetagTracedReturnsUnparseableLineUnchanged(t *testing.T) {
	t.Parallel()

	res := TraceResult{Country: "DE", IP: "5.6.7.8"}
	for _, line := range []string{
		"# a comment, not a node",
		"[GEO:CA][IP:104.21.0.119] s-001",
		"://u@h1:443#[GEO:CA] s-001",
		"",
	} {
		got, moved := retagTraced(line, res)
		if got != line || moved {
			t.Errorf("retagTraced(%q) = %q, %v; annotation is best-effort, never fatal", line, got, moved)
		}
	}
}

// rewrite.LeadingTags returns a TrimSpace'd run, so on a name that begins with
// a blank it is not a literal prefix of that name and TrimPrefix removes
// nothing — the tag run gets emitted twice.
//
// vmess is the only scheme that can produce such a name: parseNode and
// ssrRemarks both TrimSpace their display name, while parseVmess takes "ps"
// through jsonFieldString, which does not. No source writes a leading blank
// there today, which is why this stayed latent.
func TestRetagTracedHandlesLeadingBlankInName(t *testing.T) {
	t.Parallel()

	vm := map[string]any{"v": "2", "ps": " [GEO:CA][IP:104.21.0.119] n", "add": "1.2.3.4", "port": "443", "id": "u"}
	raw, err := json.Marshal(vm)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	got, moved := retagTraced("vmess://"+base64.StdEncoding.EncodeToString(raw), TraceResult{Country: "DE", IP: "5.6.7.8"})
	if !moved {
		t.Fatal("CA -> DE is a correction")
	}
	payload, decErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "vmess://"))
	if decErr != nil {
		t.Fatalf("vmess payload not base64 after retag: %v", decErr)
	}
	var back map[string]any
	if jsonErr := json.Unmarshal(payload, &back); jsonErr != nil {
		t.Fatalf("vmess payload not JSON after retag: %v", jsonErr)
	}
	if back["ps"] != "[GEO:DE][IP:5.6.7.8] n" {
		t.Errorf("vmess ps = %q, want the tag run once", back["ps"])
	}
}
