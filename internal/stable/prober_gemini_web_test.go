package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// readGeminiWebFixture loads one of the bodies captured live on 2026-11-20:
// ru.jsonp is what youtube.com/sw.js_data answers through a Russian egress and
// us.jsonp through a US one. Full bodies, not excerpts: the parser walks the
// real positional shape, so a hand-trimmed fixture would prove nothing about
// where [0][2][0][0][1] actually sits. The one edited value is YouTube's public
// InnerTube API key, replaced with a placeholder because secret scanners flag
// it; the parser never reads that field.
func readGeminiWebFixture(t *testing.T, name string) string {
	t.Helper()

	b, err := os.ReadFile("testdata/gemini_web/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return string(b)
}

// TestParseGeminiWebRegionFixtures pins the real captured answers: the region
// is read from [0][2][0][0][1] and the egress address from [3].
func TestParseGeminiWebRegionFixtures(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		fixture    string
		wantRegion string
		wantEgress string
	}{
		"blocked egress": {fixture: "ru.jsonp", wantRegion: "RU", wantEgress: "93.100.194.40"},
		"allowed egress": {fixture: "us.jsonp", wantRegion: "US", wantEgress: "45.61.185.154"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			region, egress, ok := parseGeminiWebRegion(readGeminiWebFixture(t, tc.fixture))
			if !ok {
				t.Fatalf("%s: a real answer must parse", tc.fixture)
			}
			if region != tc.wantRegion {
				t.Fatalf("region = %q, want %q", region, tc.wantRegion)
			}
			if egress != tc.wantEgress {
				t.Fatalf("egress = %q, want %q", egress, tc.wantEgress)
			}
		})
	}
}

// geminiWebBody builds the positional shape the parser walks, so a broken
// body can be expressed as one field instead of a pasted fixture.
func geminiWebBody(region, egress string) string {
	return `[["yt.sw.adr",null,[[["en",` + region + `,null,` + egress + `]]]]]`
}

// TestParseGeminiWebRegionRejects pins what makes a body unusable. Every case
// is unverified, never a pass: the gate exists to drop regions, and a body it
// cannot read must not be credited as one that agreed.
func TestParseGeminiWebRegionRejects(t *testing.T) {
	t.Parallel()

	const prefixed = geminiWebJSONPPrefix + "\n\n"
	cases := map[string]struct {
		body       string
		wantRegion string
		wantEgress string
		wantOK     bool
	}{
		"empty body":       {body: ""},
		"prefix only":      {body: geminiWebJSONPPrefix},
		"missing prefix":   {body: `[["yt.sw.adr",null,[[["en","US",null,"1.2.3.4"]]]]]`},
		"invalid json":     {body: prefixed + `not json at all`},
		"empty array":      {body: prefixed + `[]`},
		"record too short": {body: prefixed + `[["yt.sw.adr",null]]`},
		"no row":           {body: prefixed + `[["yt.sw.adr",null,[[]]]]`},
		// A short record followed by another array: the walk must not read the
		// sibling's region as this record's.
		"short record, sibling array": {body: prefixed + `[["yt.sw.adr"],[[["en","RU",null,"1.2.3.4"]]]]`},
		"empty row":                   {body: prefixed + `[["yt.sw.adr",null,[[[]]]]]`},
		"region number":               {body: prefixed + geminiWebBody(`5`, `"1.2.3.4"`)},
		"region lowercase":            {body: prefixed + geminiWebBody(`"us"`, `"1.2.3.4"`)},
		"region too long":             {body: prefixed + geminiWebBody(`"USA"`, `"1.2.3.4"`)},
		"region reserved":             {body: prefixed + geminiWebBody(`"ZZ"`, `"1.2.3.4"`)},
		"region no data":              {body: prefixed + geminiWebBody(`"XX"`, `"1.2.3.4"`)},
		"region union":                {body: prefixed + geminiWebBody(`"EU"`, `"1.2.3.4"`)},
		"region tor":                  {body: prefixed + geminiWebBody(`"T1"`, `"1.2.3.4"`)},
		"valid region":                {body: prefixed + geminiWebBody(`"US"`, `"1.2.3.4"`), wantRegion: "US", wantEgress: "1.2.3.4", wantOK: true},
		// The address is debug detail: a row that ends after the region, or
		// carries something that is not an address, still yields the verdict.
		"egress absent":  {body: prefixed + `[["yt.sw.adr",null,[[["en","US",null]]]]]`, wantRegion: "US", wantOK: true},
		"egress invalid": {body: prefixed + geminiWebBody(`"US"`, `"not-an-ip"`), wantRegion: "US", wantOK: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			region, egress, ok := parseGeminiWebRegion(tc.body)
			if ok != tc.wantOK || region != tc.wantRegion || egress != tc.wantEgress {
				t.Fatalf("parse = (%q, %q, %v), want (%q, %q, %v)",
					region, egress, ok, tc.wantRegion, tc.wantEgress, tc.wantOK)
			}
		})
	}
}

// TestGeminiWebVerdict pins the classification: only a complete 2xx whose valid
// region parses can be conclusive. A redirect is not followed by apiProbeOne,
// so a 3xx reaches the classifier as a status and must stay unverified.
func TestGeminiWebVerdict(t *testing.T) {
	t.Parallel()

	blocked := map[string]struct{}{"RU": {}}
	cases := map[string]struct {
		status   int
		region   string
		parsed   bool
		complete bool
		want     apiVerdict
	}{
		"blocked region":          {status: http.StatusOK, region: "RU", parsed: true, complete: true, want: apiBlocked},
		"allowed region":          {status: http.StatusOK, region: "US", parsed: true, complete: true, want: apiAccepted},
		"blocked, incomplete":     {status: http.StatusOK, region: "RU", parsed: true, want: apiUnverified},
		"unparsed body":           {status: http.StatusOK, complete: true, want: apiUnverified},
		"server error":            {status: http.StatusForbidden, region: "RU", parsed: true, complete: true, want: apiUnverified},
		"unfollowed redirect":     {status: http.StatusFound, complete: true, want: apiUnverified},
		"no content":              {status: http.StatusNoContent, complete: true, want: apiUnverified},
		"blocked, region but 404": {status: http.StatusNotFound, region: "RU", parsed: true, complete: true, want: apiUnverified},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := geminiWebVerdict(tc.status, tc.region, tc.parsed, tc.complete, blocked); got != tc.want {
				t.Fatalf("verdict = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGeminiWebCheckThroughNode drives URL build, the apiCheck fan-out,
// apiProbeOne's dial and body read, the parser and the classification through
// one real DIRECT adapter, so the body path is proven to reach the verdict
// rather than assumed.
func TestGeminiWebCheckThroughNode(t *testing.T) {
	t.Parallel()

	ruBody := readGeminiWebFixture(t, "ru.jsonp")
	usBody := readGeminiWebFixture(t, "us.jsonp")
	cases := map[string]struct {
		status     int
		body       string
		truncate   bool
		wantBlock  bool
		wantVerify bool
	}{
		"blocked egress":  {status: http.StatusOK, body: ruBody, wantBlock: true, wantVerify: true},
		"allowed egress":  {status: http.StatusOK, body: usBody, wantVerify: true},
		"refused request": {status: http.StatusForbidden, body: "blocked"},
		// A promised body the server does not deliver is the read error
		// apiProbeOne reports as incomplete: the region may already be in the
		// bytes, and it still must not be credited.
		"cut-short body":      {status: http.StatusOK, body: ruBody, truncate: true},
		"body without prefix": {status: http.StatusOK, body: usBody[len(geminiWebJSONPPrefix):]},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if tc.truncate {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+64))
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			px := adapter.NewProxy(outbound.NewDirect())
			m := &MihomoProber{
				logger:            zerolog.Nop(),
				geminiWebEndpoint: srv.URL + "/sw.js_data",
				geo: config.GeoBlockConfig{GeminiWeb: config.GeminiWebConfig{
					Timeout: 5 * time.Second, Concurrency: 1, BlockedRegions: []string{"RU"},
				}},
			}
			o, ok := m.GeminiWebCheck(context.Background(), []mihomo.Proxy{px})[px.Name()]
			if !ok {
				t.Fatalf("no outcome for %q", px.Name())
			}
			if gotPath != "/sw.js_data" {
				t.Fatalf("probe path = %q, want /sw.js_data", gotPath)
			}
			if o.Blocked != tc.wantBlock || o.Verified != tc.wantVerify {
				t.Fatalf("outcome = %+v, want blocked=%v verified=%v", o, tc.wantBlock, tc.wantVerify)
			}
			if !o.Reachable {
				t.Fatal("an answered probe must be reachable")
			}
		})
	}
}

// TestGeminiWebBlockedRegionsFoldCase pins the config-to-wire comparison: config
// validation accepts lowercase (validateCountryCode), while the endpoint emits
// uppercase, so a lowercase entry must still match.
func TestGeminiWebBlockedRegionsFoldCase(t *testing.T) {
	t.Parallel()

	body := readGeminiWebFixture(t, "ru.jsonp")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	px := adapter.NewProxy(outbound.NewDirect())
	m := &MihomoProber{
		logger:            zerolog.Nop(),
		geminiWebEndpoint: srv.URL,
		geo: config.GeoBlockConfig{GeminiWeb: config.GeminiWebConfig{
			Timeout: 5 * time.Second, Concurrency: 1, BlockedRegions: []string{"ru"},
		}},
	}
	o := m.GeminiWebCheck(context.Background(), []mihomo.Proxy{px})[px.Name()]
	if !o.Blocked || !o.Verified {
		t.Fatalf("a lowercase blocked_regions entry must match the uppercase wire region: %+v", o)
	}
}

// TestGeminiWebURL pins the production endpoint and the seam contract: the
// parser is bound to YouTube's body, so nothing an operator writes can move it.
func TestGeminiWebURL(t *testing.T) {
	t.Parallel()

	if got := (&MihomoProber{}).geminiWebURL(); got != geminiWebDataURL {
		t.Fatalf("geminiWebURL() = %q, want the compile-time constant %q", got, geminiWebDataURL)
	}

	p, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"},
		config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if p.geminiWebEndpoint != "" {
		t.Fatalf("NewMihomoProber must leave the test seam empty, got %q", p.geminiWebEndpoint)
	}
}

// BenchmarkParseGeminiWebRegion measures the parser on the real body. Reported
// in the change along with the alternative's figures (see the parser comment):
// a network probe per node dwarfs this, but the walk is what keeps the parse
// off the 19 KB/op the nested-RawMessage shape costs.
func BenchmarkParseGeminiWebRegion(b *testing.B) {
	body, err := os.ReadFile("testdata/gemini_web/ru.jsonp")
	if err != nil {
		b.Fatal(err)
	}
	s := string(body)
	b.ReportAllocs()
	for range b.N {
		region, _, ok := parseGeminiWebRegion(s)
		if !ok || region != "RU" {
			b.Fatalf("parse = (%q, %v)", region, ok)
		}
	}
}
