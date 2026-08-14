package stable_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/rewrite"
	"domains.lst/sub-preprocessor/internal/stable"
)

func entry(label string) stable.Entry {
	return stable.Entry{Label: label, Raw: "vless://u@" + label + ".example:443#" + label}
}

func country(t *testing.T, code string) geofeed.CountryCode {
	t.Helper()

	if len(code) != 2 {
		t.Fatalf("country(%q): want a 2-letter code", code)
	}

	return geofeed.CountryCode{code[0], code[1]}
}

// tagAnnotator stands in for the configured annotate chain, narrowed to what
// the publication contract is about. It renders through the very
// rewrite.NodeName the production chain uses, so a test here judges the
// PUBLISHED form — vmess tags folded into the base64 ps, ssr emitted without a
// fragment — instead of a stub's idea of it. offline is what the offline chain
// resolves for any address; a valid Egress answers first, exactly as the
// cloudflare provider does in front of the offline ones.
type tagAnnotator struct {
	offline geofeed.CountryCode
}

func (a tagAnnotator) Annotate(
	_ context.Context, dst, _ *bytes.Buffer, req preprocess.AnnotateRequest,
) geofeed.CountryCode {
	code := a.offline
	if req.Egress.Valid() {
		code = req.Egress.Country
	}
	// "??" on an all-miss, as the production chain renders it.
	geo := "??"
	if code != (geofeed.CountryCode{}) {
		geo = code.String()
	}
	rewrite.NodeName(dst, req.Node, req.Prefix+"[GEO:"+geo+"][IP:"+req.IP.String()+"]")

	return code
}

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()

	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}

	return a
}

func TestSelectSurvivorsFiltersAndSorts(t *testing.T) {
	t.Parallel()

	entries := []stable.Entry{
		entry("a"), // all rounds ok, slow-ish
		entry("b"), // one failure
		entry("c"), // missing from results entirely
		entry("d"), // mean exactly at limit
		entry("e"), // mean above limit
	}
	res := map[string]stable.ProbeResult{
		"a": {Successes: 5, MeanMs: 200},
		"b": {Successes: 4, MeanMs: 100},
		"d": {Successes: 5, MeanMs: 1000},
		"e": {Successes: 5, MeanMs: 1001},
	}

	got := stable.SelectSurvivors(entries, res, 5, 0, 1000)
	wantLabels := []string{"a", "d"}
	if len(got) != len(wantLabels) {
		t.Fatalf("got %d survivors %+v, want %v", len(got), got, wantLabels)
	}
	for i, w := range wantLabels {
		if got[i].Label != w {
			t.Errorf("survivor %d: got %q, want %q", i, got[i].Label, w)
		}
	}

	// maxFail=1 admits b, which sorts first by mean.
	got = stable.SelectSurvivors(entries, res, 5, 1, 1000)
	wantLabels = []string{"b", "a", "d"}
	if len(got) != len(wantLabels) {
		t.Fatalf("got %d survivors %+v, want %v", len(got), got, wantLabels)
	}
	for i, w := range wantLabels {
		if got[i].Label != w {
			t.Errorf("survivor %d: got %q, want %q", i, got[i].Label, w)
		}
	}
}

// TestSelectSurvivorsNeverPublishesZeroSuccess turns a cross-package invariant
// into a local one. A node that answered no round must never be published, and
// today that holds only because config.go clamps max_fail to [0, rounds) --
// two packages away, where a loosening would silently start publishing nodes
// that never answered. It matters now because the probe map is about to carry
// an entry per label (Successes: 0 for a total failure) instead of successes
// alone, so the case stops being unreachable-by-construction.
func TestSelectSurvivorsNeverPublishesZeroSuccess(t *testing.T) {
	t.Parallel()

	entries := []stable.Entry{entry("dead"), entry("live")}
	res := map[string]stable.ProbeResult{
		"dead": {Successes: 0, MeanMs: 0},
		"live": {Successes: 3, MeanMs: 100},
	}

	const rounds = 3
	for maxFail := range rounds { // every value the validator admits
		got := stable.SelectSurvivors(entries, res, rounds, maxFail, 1000)
		for _, s := range got {
			if s.Label == "dead" {
				t.Errorf("max_fail=%d published a node with zero successful rounds", maxFail)
			}
		}
		if len(got) != 1 {
			t.Errorf("max_fail=%d: got %d survivors, want only the live node", maxFail, len(got))
		}
	}
}

// TestBuildPayloadWithoutAnnotator: annotation off publishes the merged line as
// it stands, and no chain ran to judge a country — "" rather than "??", which
// the geo-unknown gauge counts.
func TestBuildPayloadWithoutAnnotator(t *testing.T) {
	t.Parallel()

	survivors := []stable.Survivor{
		{Entry: stable.Entry{Label: "x", Raw: "vless://u@x:443#x", IP: addr(t, "1.1.1.1")}, MeanMs: 10, Mbps: 20},
		{Entry: stable.Entry{Label: "y", Raw: "vless://u@y:443#y"}, MeanMs: 20},
	}
	want := []byte("vless://u@x:443#x\nvless://u@y:443#y\n")
	if got := stable.BuildPayload(context.Background(), nil, survivors); !bytes.Equal(got, want) {
		t.Fatalf("payload mismatch:\ngot  %q\nwant %q", got, want)
	}
	for i, s := range survivors {
		if s.Country != "" {
			t.Errorf("survivors[%d].Country = %q, want empty (no chain judged it)", i, s.Country)
		}
	}
	if got := stable.BuildPayload(context.Background(), nil, nil); len(got) != 0 {
		t.Fatalf("empty survivors should give empty payload, got %q", got)
	}
}

// TestBuildPayloadAnnotatesEgress pins the published form the worker has always
// emitted — speed tag, then the configured tags, then the label — and the two
// halves of the address decision: a node that reported its own egress is
// described by THAT address, one that did not by the address the IP-filters
// judged. No second resolve happens either way.
func TestBuildPayloadAnnotatesEgress(t *testing.T) {
	t.Parallel()

	survivors := []stable.Survivor{
		{
			Entry:  stable.Entry{Label: "s-001", Raw: "vless://u@h1:443#s-001", IP: addr(t, "1.2.3.4")},
			Mbps:   20,
			Egress: preprocess.Egress{IP: addr(t, "5.6.7.8"), Country: country(t, "DE")},
		},
		{
			Entry: stable.Entry{Label: "s-002", Raw: "vless://u@h2:443#s-002", IP: addr(t, "9.9.9.9")},
			Mbps:  20,
		},
	}

	got := string(stable.BuildPayload(context.Background(), tagAnnotator{offline: country(t, "NL")}, survivors))
	want := "vless://u@h1:443#[SPD:20M] [GEO:DE][IP:5.6.7.8] s-001\n" +
		"vless://u@h2:443#[SPD:20M] [GEO:NL][IP:9.9.9.9] s-002\n"
	if got != want {
		t.Fatalf("payload:\ngot  %q\nwant %q", got, want)
	}
	if survivors[0].Country != "DE" || survivors[1].Country != "NL" {
		t.Errorf("countries = %q/%q, want DE/NL", survivors[0].Country, survivors[1].Country)
	}
}

// A node the bandwidth filter never measured carries no speed tag: [SPD:0M]
// would claim a measurement that did not happen.
func TestBuildPayloadOmitsSpeedTagWithoutMeasurement(t *testing.T) {
	t.Parallel()

	survivors := []stable.Survivor{
		{Entry: stable.Entry{Label: "s-001", Raw: "vless://u@h1:443#s-001", IP: addr(t, "1.2.3.4")}},
	}

	got := string(stable.BuildPayload(context.Background(), tagAnnotator{offline: country(t, "NL")}, survivors))
	if want := "vless://u@h1:443#[GEO:NL][IP:1.2.3.4] s-001\n"; got != want {
		t.Fatalf("payload:\ngot  %q\nwant %q", got, want)
	}
}

// A miss from every provider publishes [GEO:??] and books the node under the
// same "??" the geo-unknown gauge counts — never a second spelling.
func TestBuildPayloadUnknownCountry(t *testing.T) {
	t.Parallel()

	survivors := []stable.Survivor{
		{Entry: stable.Entry{Label: "s-001", Raw: "vless://u@h1:443#s-001", IP: addr(t, "1.2.3.4")}},
	}

	stable.BuildPayload(context.Background(), tagAnnotator{}, survivors)
	if survivors[0].Country != "??" {
		t.Errorf("Country = %q, want ??", survivors[0].Country)
	}
}

// TestBuildPayloadVmessTagsRideInsidePs: vmess keeps its display name in the
// base64 payload, so a published tag run that landed in a URI fragment would be
// invisible to every consumer — and mihomo would name the proxy from ps anyway.
func TestBuildPayloadVmessTagsRideInsidePs(t *testing.T) {
	t.Parallel()

	raw := "vmess://" + base64.StdEncoding.EncodeToString(
		[]byte(`{"v":"2","ps":"s-001","add":"1.2.3.4","port":"443","id":"uuid","net":"ws"}`))
	survivors := []stable.Survivor{
		{
			Entry:  stable.Entry{Label: "s-001", Raw: raw, IP: addr(t, "1.2.3.4")},
			Mbps:   20,
			Egress: preprocess.Egress{IP: addr(t, "5.6.7.8"), Country: country(t, "DE")},
		},
	}

	line := strings.TrimSuffix(string(
		stable.BuildPayload(context.Background(), tagAnnotator{offline: country(t, "NL")}, survivors)), "\n")
	if strings.ContainsRune(line, '#') {
		t.Fatalf("vmess tags must ride inside ps, not in a fragment: %q", line)
	}
	const prefix = "vmess://"
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, prefix))
	if err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	var m map[string]any
	if err = json.Unmarshal(decoded, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if want := "[SPD:20M] [GEO:DE][IP:5.6.7.8] s-001"; m["ps"] != want {
		t.Errorf("ps = %v, want %q", m["ps"], want)
	}
}

// TestBuildPayloadSSRPublishesNoFragment: mihomo base64-decodes EVERYTHING
// after "ssr://", an appended "#name" included, so a published ssr line with a
// fragment converts to nothing at all rather than to a wrongly named proxy.
func TestBuildPayloadSSRPublishesNoFragment(t *testing.T) {
	t.Parallel()

	survivors := []stable.Survivor{
		{
			Entry:  stable.Entry{Label: "s-001", Raw: ssrLink("s-001"), IP: addr(t, "1.2.3.4")},
			Egress: preprocess.Egress{IP: addr(t, "5.6.7.8"), Country: country(t, "DE")},
		},
	}

	line := strings.TrimSuffix(string(
		stable.BuildPayload(context.Background(), tagAnnotator{offline: country(t, "NL")}, survivors)), "\n")
	if strings.ContainsRune(line, '#') {
		t.Fatalf("a published ssr link must carry no fragment: %q", line)
	}
	wantString(t, mihomoProxyMap(t, line), "name", "[GEO:DE][IP:5.6.7.8] s-001")
}

// Annotation is best-effort and always has been: a line the parser refuses is
// published as it stands rather than dropped, and stays out of the country
// gauges.
func TestBuildPayloadKeepsUnparseableLine(t *testing.T) {
	t.Parallel()

	survivors := []stable.Survivor{
		{Entry: stable.Entry{Label: "s-001", Raw: "not a node at all"}},
	}

	got := string(stable.BuildPayload(context.Background(), tagAnnotator{offline: country(t, "NL")}, survivors))
	if want := "not a node at all\n"; got != want {
		t.Fatalf("payload:\ngot  %q\nwant %q", got, want)
	}
	if survivors[0].Country != "" {
		t.Errorf("Country = %q, want empty", survivors[0].Country)
	}
}
