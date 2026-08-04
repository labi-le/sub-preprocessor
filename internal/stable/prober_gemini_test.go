package stable //nolint:testpackage // exercises unexported stable internals

import (
	"testing"

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
