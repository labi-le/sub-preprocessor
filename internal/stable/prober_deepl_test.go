package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

const (
	// deeplRegionBody is the refusal DeepL renders server-side on the
	// translator page (measured 2026-10-23 from a Russian egress: HTTP 451,
	// 572 bytes, no JavaScript, unchanged by any session cookie).
	deeplRegionBody = `<title>Unavailable in your region</title><p>This service is unavailable in your region.</p>`
	// deeplAcceptedBody is the accepted page's head; the live page is 373398 B
	// (measured 2026-10-23 from an accepted egress) and is never read.
	deeplAcceptedBody = `<title>DeepL Переводчик | Самый точный переводчик в мире</title><link rel="canonical" href="https://www.deepl.com/ru/translator"/>`
)

// TestDeeplVerdict pins the status-only classifier: 451 is the refusal, every
// other answer reached DeepL and is an acceptance. Body and completeness are
// absent on purpose -- they carry no evidence on this surface, so a page that
// grows, shrinks or loses its marker cannot change the verdict.
func TestDeeplVerdict(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		status int
		want   apiVerdict
	}{
		"451 with the region page":       {http.StatusUnavailableForLegalReasons, apiBlocked},
		"451 with an empty body":         {http.StatusUnavailableForLegalReasons, apiBlocked},
		"200 translator page":            {http.StatusOK, apiAccepted},
		"206 partial page":               {http.StatusPartialContent, apiAccepted},
		"403 auth error reaches the API": {http.StatusForbidden, apiAccepted},
		"rate limit is not a refusal":    {http.StatusTooManyRequests, apiAccepted},
		"cdn fault is not a refusal":     {http.StatusBadGateway, apiAccepted},
		"unfollowed redirect":            {http.StatusFound, apiAccepted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := deeplVerdict(tc.status); got != tc.want {
				t.Fatalf("deeplVerdict(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestDeeplURLCarriesTheWholePath pins the endpoint contract: unlike its
// siblings the configured value IS the probe URL, not a base to append to.
func TestDeeplURLCarriesTheWholePath(t *testing.T) {
	t.Parallel()

	m := &MihomoProber{geo: config.GeoBlockConfig{
		Deepl: config.DeeplConfig{Endpoint: "https://www.deepl.com/ru/translator/"},
	}}
	if got, want := m.deeplURL(), "https://www.deepl.com/ru/translator"; got != want {
		t.Fatalf("deeplURL() = %q, want %q", got, want)
	}
}

// TestDeeplCheckReadsNoBody pins the readBody=false half of the gate: the
// server sends headers and then stalls, so a probe that waited for the body
// would hold the worker until the request deadline while this one must return
// at the headers.
func TestDeeplCheckReadsNoBody(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	px := adapter.NewProxy(outbound.NewDirect())
	m := &MihomoProber{logger: zerolog.Nop(), geo: config.GeoBlockConfig{
		Deepl: config.DeeplConfig{Endpoint: srv.URL, Timeout: 5 * time.Second, Concurrency: 1},
	}}

	start := time.Now()
	o, ok := m.DeeplCheck(context.Background(), []mihomo.Proxy{px})[px.Name()]
	elapsed := time.Since(start)
	if !ok || !o.Reachable || o.Blocked || !o.Verified {
		t.Fatalf("a headers-only 200 must be a verified acceptance: ok=%v %+v", ok, o)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("probe waited %v for a body it must not read (request timeout 5s)", elapsed)
	}
}

// TestDeeplCheckThroughNode drives the whole through-node path -- URL build,
// apiCheck fan-out, apiProbeOne dial, status classification -- against a local
// server via mihomo's DIRECT outbound, so the verdict is proven to reach the
// filter rather than assumed.
func TestDeeplCheckThroughNode(t *testing.T) {
	t.Parallel()

	var (
		status  = http.StatusOK
		payload = deeplAcceptedBody
		gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	px := adapter.NewProxy(outbound.NewDirect())
	check := func() APIOutcome {
		t.Helper()
		m := &MihomoProber{logger: zerolog.Nop(), geo: config.GeoBlockConfig{
			Deepl: config.DeeplConfig{
				Endpoint:    srv.URL + "/ru/translator",
				Timeout:     5 * time.Second,
				Concurrency: 1,
			},
		}}
		o, ok := m.DeeplCheck(context.Background(), []mihomo.Proxy{px})[px.Name()]
		if !ok {
			t.Fatalf("no outcome for %q", px.Name())
		}
		return o
	}

	// The accepted shape: 200 with the translator page.
	if o := check(); !o.Reachable || o.Blocked || !o.Verified {
		t.Fatalf("a 200 page must classify as a verified acceptance: %+v", o)
	}
	if gotPath != "/ru/translator" {
		t.Fatalf("probed path = %q, want /ru/translator", gotPath)
	}

	// An accepted page past the old body cap: the body is never read, so the
	// 373 KB page (and anything larger) still classifies on the status alone.
	status, payload = http.StatusOK, deeplAcceptedBody+strings.Repeat("x", maxAPIBody)
	if o := check(); !o.Reachable || o.Blocked || !o.Verified {
		t.Fatalf("an oversized accepted page must stay a verified acceptance: %+v", o)
	}

	// The refusal shape: 451 with the region page.
	status, payload = http.StatusUnavailableForLegalReasons, deeplRegionBody
	if o := check(); !o.Reachable || !o.Blocked || !o.Verified {
		t.Fatalf("a 451 region page must classify as a verified refusal: %+v", o)
	}

	// A refusal page larger than any marker scan: still the status line.
	status, payload = http.StatusUnavailableForLegalReasons, deeplRegionBody+strings.Repeat("x", maxAPIBody)
	if o := check(); !o.Reachable || !o.Blocked || !o.Verified {
		t.Fatalf("an oversized 451 must stay a verified refusal: %+v", o)
	}

	// A 200 that happens to carry the refusal text is an acceptance: the body
	// is not read, so a reworded or grown page has no vote.
	status, payload = http.StatusOK, deeplRegionBody
	if o := check(); !o.Reachable || o.Blocked || !o.Verified {
		t.Fatalf("a 200 must classify as accepted whatever its body: %+v", o)
	}
}
