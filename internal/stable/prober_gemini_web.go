package stable

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"

	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/log"
)

const (
	// geminiWebJSONPPrefix is YouTube's XSSI guard in front of every sw.js_data
	// body. A body without it is not this endpoint's answer, whatever JSON it
	// might otherwise contain.
	geminiWebJSONPPrefix = ")]}'"

	// geminiWebDataURL is deliberately NOT a config key, for the same reason as
	// cloudflareTraceURL: parseGeminiWebRegion encodes this one vendor's answer
	// down to its exact positional JSON shape, so no other vendor's endpoint
	// could satisfy it, and an `endpoint:` key would be a false affordance that
	// silently keeps every node when it points at a different service.
	//
	// Why YouTube and not AI Studio: the point of the gate is the region the
	// EGRESS is seen from, and that needs a keyless endpoint whose body names
	// it. aistudio.google.com answers a 302 to a sign-in page and names no
	// region (its ~34 KB /welcome page names none either); accounts.google.com's
	// sign-in flow is ~1.4 MB and names none; gemini.google.com/app does carry
	// the egress region, but ~139 KB into a ~950 KB page. sw.js_data is ~2.6 KB
	// of JSONP whose first record carries the region AND the address the
	// endpoint saw the request arrive from, at the top of the body. It is the
	// same vendor's view of the same egress Google's own region list describes.
	// Sizes measured 2026-11-20.
	geminiWebDataURL = "https://www.youtube.com/sw.js_data"
)

// geminiWebURL is the endpoint GeminiWebCheck probes. m.geminiWebEndpoint is
// the test seam and is empty in every production build; see MihomoProber.
func (m *MihomoProber) geminiWebURL() string {
	if m.geminiWebEndpoint != "" {
		return m.geminiWebEndpoint
	}

	return geminiWebDataURL
}

// GeminiWebCheck asks each node which region YouTube sees its traffic arrive
// from and drops the ones in the configured blocked set. This is the keyless
// sibling of GeminiCheck, and its verdict is DIFFERENT IN KIND: GeminiCheck
// reads Google's per-request refusal, while this one reads a region and
// compares it against a list derived from Google's published availability
// (geoblock.gemini_web.blocked_regions). A region list can be wrong in both
// directions where an explicit refusal cannot, which is why a refusal here is
// a guess about a country, never a fact about the service.
//
// What is conclusive is deliberately narrow: a 2xx whose body was read whole
// and whose region parses. Everything else -- non-2xx, an unfollowed redirect
// (apiProbeOne never follows), a cut-short body, a body without the JSONP
// prefix or the positional record, or a region that is not a country code -- is
// unverified, so the node is kept and not credited.
func (m *MihomoProber) GeminiWebCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	c := m.geo.GeminiWeb
	// Upper-folded once per check, not per response: the wire value is
	// uppercase, while config validation accepts either case
	// (validateCountryCode), so an operator's "ru" must still match.
	blocked := make(map[string]struct{}, len(c.BlockedRegions))
	for _, r := range c.BlockedRegions {
		blocked[strings.ToUpper(strings.TrimSpace(r))] = struct{}{}
	}
	opLog := log.Op(m.logger, "stable.GeminiWebCheck")

	return m.apiCheck(ctx, "stable.GeminiWebCheck", "gemini_web check", proxies,
		m.geminiWebURL(), nil, c.Timeout, c.Concurrency, 0, true,
		func(status int, body []byte, complete bool) apiVerdict {
			if !complete || status < http.StatusOK || status >= http.StatusMultipleChoices {
				return apiUnverified
			}
			region, egress, parsed := parseGeminiWebRegion(body)
			if !parsed {
				return apiUnverified
			}
			if egress != "" {
				opLog.Debug().Str("region", region).Str("egress", egress).Int("status", status).
					Msg("gemini_web probe location")
			}

			if _, refused := blocked[region]; refused {
				return apiBlocked
			}
			return apiAccepted
		})
}

// parseGeminiWebRegion reads the egress region and, best effort, the egress
// address out of YouTube's sw.js_data body: the JSONP prefix, then a JSON array
// whose [0][2][0][0] row is a positional record where [1] is the two-letter
// region and [3] the address the endpoint saw the request arrive from.
//
// The token walk avoids copying nested subtrees just to extract two fields.
//
// ok means the region is valid; the egress address is NOT part of the verdict,
// so an absent or malformed one returns "" and leaves ok alone.
func parseGeminiWebRegion(body []byte) (region, egress string, ok bool) {
	payload, found := bytes.CutPrefix(body, []byte(geminiWebJSONPPrefix))
	if !found || !json.Valid(payload) {
		return "", "", false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	// Descend [0][2][0][0]: top array, first element (the record), its third
	// element, then the first element of each array below. Every step requires
	// an array opener, so a body of another shape cannot reach the row.
	if !enterJSONArray(dec) {
		return "", "", false
	}
	if !enterJSONArray(dec) {
		return "", "", false
	}
	if !skipJSONValue(dec) { // record[0]
		return "", "", false
	}
	if !skipJSONValue(dec) { // record[1]
		return "", "", false
	}
	if !enterJSONArray(dec) { // record[2]
		return "", "", false
	}
	if !enterJSONArray(dec) {
		return "", "", false
	}
	if !enterJSONArray(dec) { // the row
		return "", "", false
	}
	if !skipJSONValue(dec) { // row[0]
		return "", "", false
	}
	if dec.Decode(&region) != nil || !validGeminiWebRegion(region) {
		return "", "", false
	}
	var raw string
	if skipJSONValue(dec) && dec.Decode(&raw) == nil { // row[2], row[3]
		if addr, addrErr := netip.ParseAddr(raw); addrErr == nil {
			egress = addr.String()
		}
	}

	return region, egress, true
}

// enterJSONArray consumes the '[' opening the next value. It is how the walk
// above refuses a positional path it does not recognise: an element that is not
// an array cannot be the record or the row.
func enterJSONArray(dec *json.Decoder) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	delim, ok := tok.(json.Delim)

	return ok && delim == '['
}

// skipJSONValue consumes exactly one value, balancing nested arrays and
// objects, so the walk can step over the fields it does not read.
//
// A closing delimiter where a value was expected is a FAILURE, not a value: the
// array ended early, and swallowing the ']' would let the walk go on reading the
// next top-level element as if it were this record's. Without that guard a body
// whose record is short can borrow a region from a sibling array -- a missing
// index would then parse instead of staying unverified.
func skipJSONValue(dec *json.Decoder) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case ']', '}':
		return false
	case '[', '{':
	default:
		return true
	}
	for depth := 1; depth > 0; {
		if tok, err = dec.Token(); err != nil {
			return false
		}
		if delim, ok = tok.(json.Delim); ok {
			if delim == '[' || delim == '{' {
				depth++
			} else {
				depth--
			}
		}
	}

	return true
}

// validGeminiWebRegion accepts only a region a verdict may be based on: exactly
// two UPPERCASE ASCII letters that name a country, as validCountry does for
// Cloudflare's loc. The letters test is case-EXACT: a lowercase region is a
// shape this endpoint does not emit, and accepting it would compare against a
// blocked set that is upper-folded.
//
// ZZ, XX and EU are not countries and support no verdict: ZZ is ISO 3166-1's
// user-assigned "unknown", XX is "no country data" (Cloudflare's reserved loc
// with the same meaning), and EU is reserved for the European Union. T1 (Tor)
// needs no guard here: its digit fails the letters test.
func validGeminiWebRegion(r string) bool {
	if len(r) != countryCodeLen || !upperLetter(r[0]) || !upperLetter(r[1]) {
		return false
	}
	switch r {
	case "ZZ", "XX", "EU":
		return false
	}

	return true
}
