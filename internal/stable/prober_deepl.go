package stable

import (
	"context"
	"net/http"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

// deeplURL returns the probe URL. Unlike its siblings the configured endpoint
// carries the whole path: the verdict lives on the translator page itself.
func (m *MihomoProber) deeplURL() string {
	return strings.TrimRight(m.geo.Deepl.Endpoint, "/")
}

// DeeplCheck sends a keyless DeepL page GET through each of the supplied
// proxies and classifies it. DeepL decides the region server-side on the
// translator page: from a refused egress (Russia and Belarus since 2025-06-17)
// it answers HTTP 451 without JavaScript and whatever session cookies are sent
// (measured 2026-10-23), while an accepted egress gets 200 and the page. The
// verdict is the status, so the body is never read. No API key is required, so
// the gate is always active when the filter is configured. The caller owns the
// proxies' lifecycle (parse once, close once).
func (m *MihomoProber) DeeplCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	c := m.geo.Deepl
	return m.apiCheck(ctx, "stable.DeeplCheck", "deepl check", proxies,
		m.deeplURL(), nil, c.Timeout, c.Concurrency, 0, false,
		func(status int, _ string, _ bool) apiVerdict {
			return deeplVerdict(status)
		})
}

// deeplVerdict reads the status and nothing else: 451 is the refusal, every
// other answer reached DeepL and is an acceptance.
//
// The status-only rule is measured, not assumed: the refusal page arrives from
// a refusing egress without JavaScript and with any session cookie, so the
// verdict is server-side and the body carries nothing the status does not
// (measured 2026-10-23). Where Tidal fails CLOSED on non-2xx, DeepL answers a
// rate limit or a CDN fault with statuses of its own, and neither says anything
// about the egress; dropping on them would discard healthy nodes. The price is
// the failure mode prober_chatgpt.go records: a status the egress did not earn
// -- a rate limit, a CDN fault, an unfollowed redirect -- reads as accepted.
// The gate stays honest about that through its coverage numbers: a 451 that
// stops arriving shows up as stable_filter_dropped_nodes{filter="deepl",
// reason="blocked"} at 0 while every survivor is verified, not as a wrong drop.
func deeplVerdict(status int) apiVerdict {
	if status == http.StatusUnavailableForLegalReasons {
		return apiBlocked
	}
	return apiAccepted
}
