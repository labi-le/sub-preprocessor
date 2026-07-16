package stable

import (
	"context"
	"net/http"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

func (m *MihomoProber) claudeURL() string {
	return strings.TrimRight(m.claude.Endpoint, "/") + "/v1/models"
}

// ClaudeCheck sends a keyless Anthropic API GET through each of the supplied
// proxies and classifies it. Anthropic geo-blocks before authentication: a
// blocked region gets HTTP 403 with the "Request not allowed" marker in the
// body, while an allowed region gets an authentication error instead. No API
// key is required, so the gate is always active when the filter is configured.
// The caller owns the proxies' lifecycle (parse once, close once).
func (m *MihomoProber) ClaudeCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
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
