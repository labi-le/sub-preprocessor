package stable

import (
	"context"
	"net/http"
	"strings"
)

func (m *MihomoProber) claudeURL() string {
	return strings.TrimRight(m.claude.Endpoint, "/") + "/v1/models"
}

// ClaudeCheck sends a keyless Anthropic API GET through every node in payload
// and classifies it. Anthropic geo-blocks before authentication: a blocked
// region gets HTTP 403 with the "Request not allowed" marker in the body,
// while an allowed region gets an authentication error instead. No API key is
// required, so the gate is always active when the filter is configured.
func (m *MihomoProber) ClaudeCheck(ctx context.Context, payload []byte) map[string]APIOutcome {
	proxies, err := m.parseProxies(payload)
	if err != nil {
		m.logger.Warn().Err(err).Msg("claude check: no proxies")
		return nil
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	header := http.Header{"Anthropic-Version": []string{m.claude.Version}}
	return m.apiCheck(ctx, "stable.ClaudeCheck", "claude check", proxies,
		m.claudeURL(), header, m.claude.Timeout, m.claude.Concurrency,
		func(body string) bool { return claudeBlocked(body, m.claude.Marker) })
}

// claudeBlocked reports whether an Anthropic API response body indicates the
// caller's location is geo-blocked.
func claudeBlocked(body, marker string) bool {
	return marker != "" && strings.Contains(body, marker)
}
