package stable

import (
	"context"
	"net/url"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

// GeminiEnabled reports whether the Gemini gate is active (configured with a
// resolved API key).
func (m *MihomoProber) GeminiEnabled() bool {
	return m.geminiKey != ""
}

func (m *MihomoProber) geminiURL() string {
	return strings.TrimRight(m.geo.Gemini.Endpoint, "/") + "/v1beta/models/" +
		m.geo.Gemini.Model + "?key=" + url.QueryEscape(m.geminiKey)
}

// GeminiCheck sends a real Gemini API GET through each of the supplied proxies
// and classifies it. This is the algorithm mihomo's HEAD-only URLTest cannot
// do: a geo-block appears only in the API response body, not the status code.
// The caller owns the proxies' lifecycle (parse once, close once).
func (m *MihomoProber) GeminiCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	g := m.geo.Gemini
	return m.apiCheck(ctx, "stable.GeminiCheck", "gemini check", proxies,
		m.geminiURL(), nil, g.Timeout, g.Concurrency,
		func(_ int, body string) bool { return markerBlocked(body, g.Marker) })
}
