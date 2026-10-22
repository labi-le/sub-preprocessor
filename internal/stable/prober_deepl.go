package stable

import (
	"context"
	"net/http"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

// deeplURL returns the probe URL. Unlike its siblings the configured endpoint
// carries the whole path: the keyless verdict lives on one API route.
func (m *MihomoProber) deeplURL() string {
	return strings.TrimRight(m.geo.Deepl.Endpoint, "/")
}

// DeeplCheck sends a keyless DeepL API GET through each of the supplied proxies
// and classifies it. DeepL renders its region verdict before authentication, so
// from a refused egress (Russia and Belarus since 2025-06-17) both the site and
// /v2/usage answer HTTP 451 with the "unavailable in your region" page, while an
// accepted egress answers the API's authentication error and only the
// credential is wrong. No API key is required, so the gate is always active
// when the filter is configured. The caller owns the proxies' lifecycle (parse
// once, close once).
func (m *MihomoProber) DeeplCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	c := m.geo.Deepl
	return m.apiCheck(ctx, "stable.DeeplCheck", "deepl check", proxies,
		m.deeplURL(), nil, c.Timeout, c.Concurrency, 0, true,
		func(status int, body string, complete bool) apiVerdict {
			return deeplVerdict(status, body, c.Marker, complete)
		})
}

// deeplVerdict classifies one DeepL response. The status is a refusal on its
// own, before the body is judged, and a marker found in a partial body is
// still a refusal, as markerVerdict reads it.
//
// Everything else that was read whole is an ACCEPTANCE, deliberately unlike
// tidal's fail-closed status rule. Where Tidal's refusal never reaches the API,
// DeepL answers a rate limit or a CDN fault as well, and neither says anything
// about the egress; dropping on them would discard healthy nodes. The price is
// the failure mode prober_chatgpt.go records: if the region verdict ever moves
// behind authentication, every complete answer reads as accepted and the gate
// goes silently blind instead of wrong. A body that could not be read whole and
// carries no marker is unverified and kept, like every other marker gate.
func deeplVerdict(status int, body, marker string, complete bool) apiVerdict {
	if status == http.StatusUnavailableForLegalReasons || markerBlocked(body, marker) {
		return apiBlocked
	}
	if !complete {
		return apiUnverified
	}
	return apiAccepted
}
