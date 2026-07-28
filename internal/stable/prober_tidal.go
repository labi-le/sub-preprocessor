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

// tidalBlocked keeps a node only when Tidal actually answered it: HTTP 2xx with
// a readable countryCode. Anything else is a refusal.
//
// That is the inverse of markerBlocked's fail-open, deliberately. Where Tidal
// refuses an egress the request never reaches the API — CloudFront answers 403
// with an HTML error page and no JSON (measured from a Russian egress) — so
// classifying on the body alone would keep precisely the nodes this gate exists
// to drop. Rejecting every non-2xx also covers refusal shapes we have not seen
// (451, a future interstitial) with no status enum to maintain, and requiring a
// parseable countryCode proves the 200 came from Tidal rather than from a
// captive portal on the node.
//
// The country itself is NOT compared against Tidal's list of markets: that list
// gates where a subscription can be bought, while an existing subscriber
// streams fine from a country Tidal merely does not sell in. Only a refusal
// makes a node unusable.
func tidalBlocked(status int, body string) bool {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return true
	}
	_, ok := parseCountryCode(body)
	return !ok
}

// parseCountryCode returns the ISO code from a {"countryCode": "DE" } body.
// Hand-rolled instead of encoding/json: the shape is fixed, the body is two
// dozen bytes, and a scan still finds the code in a body truncated at
// maxAPIBody. The code is returned as a slice of body -- no copy, original case
// kept -- because nothing compares it: the gate only needs proof that the
// answer came from Tidal.
//
// The scan is anchored to the key position -- `"countryCode"` followed by `:`
// and a quoted value -- rather than grabbing the next quoted token: the literal
// also occurs as a field VALUE in error payloads, where the token after it
// belongs to an unrelated field.
func parseCountryCode(body string) (string, bool) {
	const jsonSpace = " \t\r\n"
	_, rest, found := strings.Cut(body, `"countryCode"`)
	if !found {
		return "", false
	}
	rest, isKey := strings.CutPrefix(strings.TrimLeft(rest, jsonSpace), ":")
	if !isKey {
		return "", false
	}
	rest, quoted := strings.CutPrefix(strings.TrimLeft(rest, jsonSpace), `"`)
	if !quoted {
		return "", false
	}
	value, _, closed := strings.Cut(rest, `"`)
	if !closed || len(value) != 2 || !isASCIILetter(value[0]) || !isASCIILetter(value[1]) {
		return "", false
	}
	return value, true
}

func isASCIILetter(b byte) bool {
	b |= 0x20 // ASCII case bit; setting it lower-folds letters
	return b >= 'a' && b <= 'z'
}
