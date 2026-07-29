package stable

import (
	"context"
	"net/http"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"
)

func (m *MihomoProber) tidalURL() string {
	return strings.TrimRight(m.geo.Tidal.Endpoint, "/") + "/v1/country"
}

// TidalCheck sends a keyless Tidal GET through each of the supplied proxies and
// keeps the ones Tidal answers. /v1/country is the only keyless endpoint that
// reacts to the egress at all: the catalog API needs a token, and tidal.com
// itself sits behind DataDome, which would 403 a proxied probe for bot reasons
// rather than geography. The caller owns the proxies' lifecycle (parse once,
// close once).
func (m *MihomoProber) TidalCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	c := m.geo.Tidal
	return m.apiCheck(ctx, "stable.TidalCheck", "tidal check", proxies,
		m.tidalURL(), nil, c.Timeout, c.Concurrency, tidalBlocked)
}

// tidalBlocked reads the status and nothing else: a 2xx means the request got
// through, anything else is the refusal. Redirects are not followed, so a 3xx
// interstitial counts as refused too.
//
// That is the inverse of markerBlocked's fail-open, deliberately. Where Tidal
// refuses an egress the request never reaches the API -- CloudFront answers 403
// with an HTML error page (measured from a Russian egress) -- so there is no
// refusal marker to look for, and reading the body for one would keep precisely
// the nodes this gate exists to drop.
//
// The body is deliberately not inspected: the only question here is whether the
// request passed. The country it carries never gated anything anyway (Tidal's
// market list says where a subscription can be bought, not where an existing
// subscriber can stream), and the check that went with it -- a 2xx had to carry
// a parseable countryCode to prove the answer came from Tidal -- is gone with
// it. The accepted cost: a node whose upstream answers 2xx from something that
// is not Tidal (an ISP block page, a captive portal) now counts as passed.
func tidalBlocked(status int, _ string) bool {
	return status < http.StatusOK || status >= http.StatusMultipleChoices
}
