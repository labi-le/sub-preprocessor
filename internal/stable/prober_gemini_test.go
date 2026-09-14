package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

	off := testProber(t)
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

	const marker = "User location is not supported for the API use"
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
		"server fault":     {503, `{"error":{"code":503,"status":"UNAVAILABLE"}}`, true},
		"gateway fault":    {502, `{"error":{"code":502,"status":"BAD_GATEWAY"}}`, true},
		"internal fault":   {500, `{"error":{"code":500,"status":"INTERNAL"}}`, true},
		"redirect":         {302, "", true},
		"reworded refusal": {400, `{"error":{"message":"User location is not supported in your region.","status":"FAILED_PRECONDITION"}}`, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := geminiInconclusive(tc.status, tc.body, marker); got != tc.want {
				t.Fatalf("geminiInconclusive(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}

	// The two 400s must stay distinguishable: only the location one is a block.
	if !markerBlocked(geoBlocked, marker) || markerBlocked(keyInvalid, marker) {
		t.Fatal("only the FAILED_PRECONDITION body is a geo-block")
	}
}

// TestGeminiCheckClassifiesThroughTheFanOut drives the real fan-out and pins
// what each response class becomes in the outcome map. TestGeminiInconclusive
// above pins the predicate in isolation; this pins that the fan-out actually
// applies it, which is the difference between a gate that keeps an
// unverifiable node and one that publishes a geo-blocked one.
//
// An unreachable proxy is Reachable=false and no verdict at all: apiFilter
// counts reason="unreachable" per SURVIVOR, off an outcome betterAPIOutcome
// already folded best-of-ports, so only a node whose EVERY proxy was
// unreachable reaches that reason.
func TestGeminiCheckClassifiesThroughTheFanOut(t *testing.T) {
	t.Parallel()

	const marker = "User location is not supported for the API use"
	for name, tc := range map[string]struct {
		status      int
		body        string
		live, dead  int
		wantBlocked bool
		wantLive    int
	}{
		"quota answers before the location check": {
			429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, 1, 0, false, 1,
		},
		"a rotated key answers before it too": {
			400, `{"error":{"status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`, 1, 0, false, 1,
		},
		"a server fault answers before it too": {
			503, `{"error":{"code":503,"status":"UNAVAILABLE"}}`, 1, 0, false, 1,
		},
		"a refused location is a verdict": {
			400, `{"error":{"message":"User location is not supported for the API use."}}`, 1, 0, true, 1,
		},
		"a served model is a verdict": {
			200, `{"name":"models/gemini-2.0-flash"}`, 1, 0, false, 1,
		},
		"an unreachable proxy carries no verdict": {
			429, `{"error":{"code":429}}`, 1, 2, false, 1,
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

			out := m.GeminiCheck(context.Background(), geminiTestProxies(tc.live, tc.dead))
			reachable, blocked := countOutcomes(out)
			if reachable != tc.wantLive {
				t.Fatalf("reachable outcomes = %d, want %d (%d dead proxies carry no verdict)", reachable, tc.wantLive, tc.dead)
			}
			if (blocked > 0) != tc.wantBlocked {
				t.Fatalf("blocked outcomes = %d, want blocked=%v for status %d", blocked, tc.wantBlocked, tc.status)
			}
		})
	}
}

// countOutcomes folds an outcome map into the two numbers the fan-out test
// asserts on: how many proxies answered at all, and how many of those answers
// were read as a refusal of this egress.
func countOutcomes(out map[string]APIOutcome) (reachable, blocked int) {
	for _, o := range out {
		if o.Reachable {
			reachable++
		}
		if o.Blocked {
			blocked++
		}
	}
	return reachable, blocked
}

// TestGeminiCheckPacesRequestStarts pins the pacing contract: request starts
// observed by the server are spread at least the configured interval apart,
// and every proxy still gets a verdict — pacing must throttle, never starve.
func TestGeminiCheckPacesRequestStarts(t *testing.T) {
	t.Parallel()

	// 600/min = one start per 100ms; 5 proxies keep the paced run well under
	// a second, and gaps of ~100ms are nothing like the ~0 an unpaced check
	// produces, so half the interval is a flake-safe floor.
	m, arrivals := geminiPacingProber(t, 600)
	out := m.GeminiCheck(context.Background(), geminiTestProxies(5, 0))

	times := arrivals.all()
	if len(times) != 5 {
		t.Fatalf("server saw %d requests, want 5 (one per live proxy)", len(times))
	}
	const minGap = 50 * time.Millisecond
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < minGap {
			t.Fatalf("request starts %v apart, want >= %v: the shared ticker hands out one start per interval", gap, minGap)
		}
	}
	if len(out) != 5 {
		t.Fatalf("GeminiCheck returned %d outcomes, want one per proxy: pacing must throttle, never skip", len(out))
	}
}

// TestGeminiCheckRateLimitZeroStaysUnpaced: the zero value means "no pacing",
// so the check behaves exactly as before the field existed — five loopback
// requests land as a burst, not spread over paced intervals.
func TestGeminiCheckRateLimitZeroStaysUnpaced(t *testing.T) {
	t.Parallel()

	m, arrivals := geminiPacingProber(t, 0)
	out := m.GeminiCheck(context.Background(), geminiTestProxies(5, 0))

	times := arrivals.all()
	if len(times) != 5 {
		t.Fatalf("server saw %d requests, want 5 (one per live proxy)", len(times))
	}
	spread := times[len(times)-1].Sub(times[0])
	// Even the smallest paced rate (60/min) would spread five starts over a
	// second; a burst of loopback requests lands in a few ms, so 100ms
	// separates paced from unpaced without a loaded machine blurring them.
	if spread >= 100*time.Millisecond {
		t.Fatalf("request starts spread over %v with RateLimit 0, want an unpaced burst", spread)
	}
	if len(out) != 5 {
		t.Fatalf("GeminiCheck returned %d outcomes, want one per proxy", len(out))
	}
}

// TestGeminiCheckCancelDoesNotWaitOutThePace: every worker is parked on the
// shared ticker when the context dies (pace 1s, cancellation at 50ms), and
// the check must return at once instead of sitting out the remaining ticks —
// a cancelled cycle that hung on the ticker would stall the shutdown path.
func TestGeminiCheckCancelDoesNotWaitOutThePace(t *testing.T) {
	t.Parallel()

	m, _ := geminiPacingProber(t, 60) // one start per second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.GeminiCheck(ctx, geminiTestProxies(3, 0))
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // every worker parked on the ticker
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("GeminiCheck outlived ctx cancellation: a worker sat on the ticker instead of selecting ctx.Done()")
	}
}

// geminiArrivalRecorder timestamps each request as the server sees it. Request
// STARTS are what apiCheck's pace bounds, so the arrival interval is the
// observable that must spread out (or not).
type geminiArrivalRecorder struct {
	mu    sync.Mutex
	times []time.Time
}

func (r *geminiArrivalRecorder) add() {
	r.mu.Lock()
	r.times = append(r.times, time.Now())
	r.mu.Unlock()
}

func (r *geminiArrivalRecorder) all() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.times...)
}

// geminiPacingProber starts an httptest server that records arrivals and
// answers 200 with a served-model body, then builds a prober gated on it with
// the given rate_limit.
func geminiPacingProber(t *testing.T, rateLimit int) (*MihomoProber, *geminiArrivalRecorder) {
	t.Helper()

	arrivals := &geminiArrivalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrivals.add()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"name":"models/gemini-2.0-flash"}`)
	}))
	t.Cleanup(srv.Close)

	m, err := NewMihomoProber(
		config.CheckConfig{ExpectedStatus: "204"},
		config.BandwidthConfig{},
		config.GeoBlockConfig{Gemini: config.GeminiConfig{
			Endpoint: srv.URL, Model: "gemini-2.0-flash",
			Timeout: 5 * time.Second, Concurrency: 8, RateLimit: rateLimit,
		}},
		config.CloudflareConfig{},
		"KEY",
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return m, arrivals
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
