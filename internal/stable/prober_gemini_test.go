package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

func TestGeminiBlocked(t *testing.T) {
	t.Parallel()

	const marker = "User location is not supported for the API use"
	if !markerBlocked(`{"error":{"message":"User location is not supported for the API use."}}`, marker) {
		t.Fatal("geo-block body should be detected")
	}
	if markerBlocked(`{"models":[{"name":"gemini-2.0-flash"}]}`, marker) {
		t.Fatal("a normal response body must not be flagged")
	}
	if markerBlocked("anything", "") {
		t.Fatal("empty marker must never match")
	}
}

func TestGeminiURLAndEnabled(t *testing.T) {
	t.Parallel()

	p, err := NewMihomoProber(
		config.CheckConfig{ExpectedStatus: "204"},
		config.BandwidthConfig{},
		config.GeoBlockConfig{Gemini: config.GeminiConfig{Endpoint: "https://generativelanguage.googleapis.com/", Model: "gemini-2.0-flash"}},
		config.CloudflareConfig{},
		"SECRET",
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !p.GeminiEnabled() {
		t.Fatal("should be enabled with a key")
	}
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash?key=SECRET"
	if got := p.geminiURL(); got != want {
		t.Fatalf("geminiURL = %q, want %q", got, want)
	}

	off, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if off.GeminiEnabled() {
		t.Fatal("no key must disable the gate")
	}
}

// TestGeminiInconclusive pins the classification of the responses that reach us
// BEFORE Gemini evaluates the caller's location (all bodies measured against
// generativelanguage.googleapis.com): they must never be read as "this egress
// is fine", or a rotated key silently disables the gate.
func TestGeminiInconclusive(t *testing.T) {
	t.Parallel()

	const (
		keyInvalid = `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.",` +
			`"status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`
		unregistered = `{"error":{"code":403,"message":"Method doesn't allow unregistered callers ` +
			`(callers without established identity).","status":"PERMISSION_DENIED"}}`
		geoBlocked = `{"error":{"code":400,"message":"User location is not supported for the API use.",` +
			`"status":"FAILED_PRECONDITION"}}`
		ok = `{"name":"models/gemini-2.0-flash","version":"2.0"}`
	)

	tests := map[string]struct {
		status int
		body   string
		want   bool
	}{
		"rotated key":      {400, keyInvalid, true},
		"no key":           {403, unregistered, true},
		"wrong model":      {404, `{"error":{"code":404,"status":"NOT_FOUND"}}`, true},
		"quota":            {429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, true},
		"location refused": {400, geoBlocked, false},
		"egress fine":      {200, ok, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := geminiInconclusive(tc.status, tc.body); got != tc.want {
				t.Fatalf("geminiInconclusive(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}

	// The two 400s must stay distinguishable: only the location one is a block.
	const marker = "User location is not supported for the API use"
	if !markerBlocked(geoBlocked, marker) || markerBlocked(keyInvalid, marker) {
		t.Fatal("only the FAILED_PRECONDITION body is a geo-block")
	}
}

// TestGeminiCheckAccountsWhatItCouldNotVerify drives the real fan-out and pins
// the pair the metric publishes. It is the count, not the classification, that
// TestGeminiInconclusive above cannot see: geminiInconclusive was already
// correct while a rotated key still degraded the gate silently, because the
// only thing the count reached was a WARN nothing scrapes.
//
// The denominator is classifier CALLS, not len(proxies): a proxy that never
// answered is short-circuited before the classifier, so it belongs to no term
// of this ratio. Nor is it accounted for as its own drop: apiFilter.apply
// counts reason="unreachable" per SURVIVOR, off an outcome betterAPIOutcome
// already folded best-of-ports, so only a node whose EVERY proxy was
// unreachable reaches that reason.
func TestGeminiCheckAccountsWhatItCouldNotVerify(t *testing.T) {
	t.Parallel()

	const marker = "User location is not supported for the API use"
	for name, tc := range map[string]struct {
		status     int
		body       string
		live, dead int
		want       GeminiReport
	}{
		"quota answers before the location check": {
			429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, 1, 0,
			GeminiReport{State: GeminiGateRan, Checks: 1, Unverified: 1},
		},
		"a rotated key answers before it too": {
			400, `{"error":{"status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`, 1, 0,
			GeminiReport{State: GeminiGateRan, Checks: 1, Unverified: 1},
		},
		"a refused location is a verdict": {
			400, `{"error":{"message":"User location is not supported for the API use."}}`, 1, 0,
			GeminiReport{State: GeminiGateRan, Checks: 1, Unverified: 0},
		},
		"a served model is a verdict": {
			200, `{"name":"models/gemini-2.0-flash"}`, 1, 0,
			GeminiReport{State: GeminiGateRan, Checks: 1, Unverified: 0},
		},
		"every rejected response counts, not just the first": {
			403, `{"error":{"status":"PERMISSION_DENIED"}}`, 3, 0,
			GeminiReport{State: GeminiGateRan, Checks: 3, Unverified: 3},
		},
		"an unreachable proxy is in neither term": {
			429, `{"error":{"code":429}}`, 1, 2,
			GeminiReport{State: GeminiGateRan, Checks: 1, Unverified: 1},
		},
		"a gate that ran clean is still a gate that ran": {
			200, `{"name":"models/gemini-2.0-flash"}`, 0, 0,
			GeminiReport{State: GeminiGateRan, Checks: 0, Unverified: 0},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			m, err := NewMihomoProber(
				config.CheckConfig{ExpectedStatus: "204"},
				config.BandwidthConfig{},
				config.GeoBlockConfig{Gemini: config.GeminiConfig{
					Endpoint: srv.URL, Model: "gemini-2.0-flash", Marker: marker,
					Timeout: 5 * time.Second, Concurrency: 4,
				}},
				config.CloudflareConfig{},
				"KEY",
				zerolog.Nop(),
			)
			if err != nil {
				t.Fatal(err)
			}

			_, got := m.GeminiCheck(context.Background(), geminiTestProxies(tc.live, tc.dead))
			if got != tc.want {
				t.Fatalf("GeminiCheck report = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// geminiTestProxies builds live proxies (a DIRECT outbound, which dials the
// address apiProbeOne put in the metadata) followed by dead ones that refuse
// the dial. Distinct names keep them distinct labels in the outcome fold.
func geminiTestProxies(live, dead int) []mihomo.Proxy {
	pxs := make([]mihomo.Proxy, 0, live+dead)
	for i := range live {
		pxs = append(pxs, &mieruPort{
			Proxy: adapter.NewProxy(outbound.NewDirect()),
			name:  fmt.Sprintf("live-%03d:2999/TCP", i), addr: "1.2.3.4:2999",
		})
	}
	for i := range dead {
		pxs = append(pxs, &mieruPort{name: fmt.Sprintf("dead-%03d:2999/TCP", i), addr: "5.6.7.8:2999"})
	}
	return pxs
}
