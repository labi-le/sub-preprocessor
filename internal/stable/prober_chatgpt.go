package stable

import (
	"context"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

func (m *MihomoProber) chatgptURL() string {
	return strings.TrimRight(m.geo.ChatGPT.Endpoint, "/") + "/compliance/cookie_requirements"
}

// ChatGPTCheck sends a keyless OpenAI compliance GET through each of the
// supplied proxies and classifies it: a refused egress answers HTTP 403 with
// code "unsupported_country", an accepted one answers 200
// {"cookie_consent_required": ...}.
//
// The plain API (/v1/models) cannot serve as this gate: it answers 401 for the
// missing bearer token without ever consulting the region, so it reports every
// node as fine. Measured across 325 live nodes, the platform.openai.com
// origin/authorization headers the reference implementation
// (lmc999/RegionRestrictionCheck) sends changed nothing, so the request stays a
// plain GET. The caller owns the proxies' lifecycle (parse once, close once).
func (m *MihomoProber) ChatGPTCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	c := m.geo.ChatGPT
	return m.apiCheck(ctx, "stable.ChatGPTCheck", "chatgpt check", proxies,
		m.chatgptURL(), nil, c.Timeout, c.Concurrency,
		func(_ int, body string) bool { return markerBlocked(body, c.Marker) })
}
