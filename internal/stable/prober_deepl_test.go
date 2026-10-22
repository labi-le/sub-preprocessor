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
	// deeplRegionBody is the refusal DeepL renders before authentication on the
	// API as well as on the site (measured 2026-10-22 from a Russian egress:
	// HTTP 451, 572 bytes, `server: cloudflare`).
	deeplRegionBody = `<title>Unavailable in your region</title><p>This service is unavailable in your region.</p>`
	// deeplAuthBody is what the same keyless request answers from an accepted
	// egress: the request reached the API and only the credential is wrong.
	deeplAuthBody = `{"message":"Authentication failed, provided API key is invalid."}`
)

// TestDeeplVerdict pins the classifier the gate's fail-open shape rests on: a
// 451 or the region marker is a refusal, every other complete answer is an
// acceptance (a rate limit or CDN fault says nothing about the egress), and a
// body the check could not read whole without the marker is unverified.
func TestDeeplVerdict(t *testing.T) {
	t.Parallel()

	const marker = "unavailable in your region"
	cases := map[string]struct {
		status   int
		body     string
		complete bool
		want     apiVerdict
	}{
		"451 with the region page":           {http.StatusUnavailableForLegalReasons, deeplRegionBody, true, apiBlocked},
		"451 with an empty body":             {http.StatusUnavailableForLegalReasons, "", true, apiBlocked},
		"451 in a partial body":              {http.StatusUnavailableForLegalReasons, "prefix", false, apiBlocked},
		"region marker on another status":    {http.StatusOK, deeplRegionBody, true, apiBlocked},
		"region marker in a partial body":    {http.StatusForbidden, "prefix unavailable in your region", false, apiBlocked},
		"auth error reaches the API":         {http.StatusForbidden, deeplAuthBody, true, apiAccepted},
		"missing auth header":                {http.StatusForbidden, `{"message":"Missing Authorization header"}`, true, apiAccepted},
		"rate limit is not a refusal":        {http.StatusTooManyRequests, "", true, apiAccepted},
		"cdn fault is not a refusal":         {http.StatusBadGateway, "<html>bad gateway</html>", true, apiAccepted},
		"empty accepted answer":              {http.StatusOK, "", true, apiAccepted},
		"unreadable body without the marker": {http.StatusForbidden, "partial auth", false, apiUnverified},
		"unreadable empty body":              {http.StatusOK, "", false, apiUnverified},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := deeplVerdict(tc.status, tc.body, marker, tc.complete); got != tc.want {
				t.Fatalf("deeplVerdict(%d, %q, complete=%v) = %v, want %v", tc.status, tc.body, tc.complete, got, tc.want)
			}
		})
	}
}

// TestDeeplURLCarriesTheWholePath pins the endpoint contract: unlike its
// siblings the configured value IS the probe URL, not a base to append to.
func TestDeeplURLCarriesTheWholePath(t *testing.T) {
	t.Parallel()

	m := &MihomoProber{geo: config.GeoBlockConfig{
		Deepl: config.DeeplConfig{Endpoint: "https://api-free.deepl.com/v2/usage/"},
	}}
	if got, want := m.deeplURL(), "https://api-free.deepl.com/v2/usage"; got != want {
		t.Fatalf("deeplURL() = %q, want %q", got, want)
	}
}

// TestDeeplCheckThroughNode drives the whole through-node path — URL build,
// apiCheck fan-out, apiProbeOne dial, status and body classification — against
// a local server via mihomo's DIRECT outbound, so the verdict is proven to
// reach the filter rather than assumed.
func TestDeeplCheckThroughNode(t *testing.T) {
	t.Parallel()

	var (
		status  = http.StatusForbidden
		payload = deeplAuthBody
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
				Endpoint:    srv.URL + "/v2/usage",
				Marker:      "unavailable in your region",
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

	// The accepted shape: the API answered, and only the credential is wrong.
	if o := check(); !o.Reachable || o.Blocked || !o.Verified {
		t.Fatalf("an auth error must classify as a verified acceptance: %+v", o)
	}
	if gotPath != "/v2/usage" {
		t.Fatalf("probed path = %q, want /v2/usage", gotPath)
	}

	// The refusal shape: 451 with the region page.
	status, payload = http.StatusUnavailableForLegalReasons, deeplRegionBody
	if o := check(); !o.Reachable || !o.Blocked || !o.Verified {
		t.Fatalf("a 451 region page must classify as a verified refusal: %+v", o)
	}

	// A marker in a body past maxAPIBody: the read is incomplete, but the bytes
	// received already carry the refusal, which stays conclusive.
	status, payload = http.StatusForbidden, deeplRegionBody+strings.Repeat("x", maxAPIBody)
	if o := check(); !o.Blocked || !o.Verified {
		t.Fatalf("an observed marker in a truncated body must stay a verified refusal: %+v", o)
	}

	// A truncated body with no marker supports no verdict: kept unverified.
	status, payload = http.StatusForbidden, strings.Repeat("x", maxAPIBody+1)
	if o := check(); !o.Reachable || o.Blocked || o.Verified {
		t.Fatalf("a truncated body without the marker must stay unverified: %+v", o)
	}
}
