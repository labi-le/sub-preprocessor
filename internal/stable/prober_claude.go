package stable

import (
	"context"
	"net/http"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

func (m *MihomoProber) claudeURL() string {
	return strings.TrimRight(m.geo.Claude.Endpoint, "/") + "/v1/models"
}

// ClaudeCheck sends a keyless Anthropic API GET through each of the supplied
// proxies and classifies it. Anthropic geo-blocks before authentication: a
// blocked region gets HTTP 403 with the "Request not allowed" marker in the
// body, while an allowed region gets an authentication error instead. No API
// key is required, so the gate is always active when the filter is configured.
// The caller owns the proxies' lifecycle (parse once, close once).
func (m *MihomoProber) ClaudeCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	c := m.geo.Claude
	header := http.Header{"Anthropic-Version": []string{c.Version}}
	return m.apiCheck(ctx, "stable.ClaudeCheck", "claude check", proxies,
		m.claudeURL(), header, c.Timeout, c.Concurrency,
		func(_ int, body string) bool { return markerBlocked(body, c.Marker) })
}
