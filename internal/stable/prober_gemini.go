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
	return strings.TrimRight(m.gemini.Endpoint, "/") + "/v1beta/models/" +
		m.gemini.Model + "?key=" + url.QueryEscape(m.geminiKey)
}

// GeminiCheck sends a real Gemini API GET through each of the supplied proxies
// and classifies it. This is the algorithm mihomo's HEAD-only URLTest cannot
// do: a geo-block appears only in the API response body, not the status code.
// The caller owns the proxies' lifecycle (parse once, close once).
func (m *MihomoProber) GeminiCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	return m.apiCheck(ctx, "stable.GeminiCheck", "gemini check", proxies,
		m.geminiURL(), nil, m.gemini.Timeout, m.gemini.Concurrency,
		func(body string) bool { return geminiBlocked(body, m.gemini.Marker) })
}

// geminiBlocked reports whether a Gemini API response body indicates the
// caller's location is geo-blocked.
func geminiBlocked(body, marker string) bool {
	return marker != "" && strings.Contains(body, marker)
}
