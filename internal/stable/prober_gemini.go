package stable

import (
	"context"
	"net/url"
	"strings"
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

// GeminiCheck sends a real Gemini API GET through every node in payload and
// classifies it. This is the algorithm mihomo's HEAD-only URLTest cannot do: a
// geo-block appears only in the API response body, not the status code.
func (m *MihomoProber) GeminiCheck(ctx context.Context, payload []byte) map[string]APIOutcome {
	proxies, err := m.parseProxies(payload)
	if err != nil {
		m.logger.Warn().Err(err).Msg("gemini check: no proxies")
		return nil
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	return m.apiCheck(ctx, "stable.GeminiCheck", "gemini check", proxies,
		m.geminiURL(), nil, m.gemini.Timeout, m.gemini.Concurrency,
		func(body string) bool { return geminiBlocked(body, m.gemini.Marker) })
}

// geminiBlocked reports whether a Gemini API response body indicates the
// caller's location is geo-blocked.
func geminiBlocked(body, marker string) bool {
	return marker != "" && strings.Contains(body, marker)
}
