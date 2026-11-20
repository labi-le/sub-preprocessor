package stable

import (
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
		func(status int, body string, complete bool) apiVerdict {
			region, egress, parsed := parseGeminiWebRegion(body)
			// The egress address is debug detail only: it is what the endpoint
			// saw, useful when a verdict looks wrong for a node, and it is
			// absent from some answers, so it never affects the verdict.
			if parsed && egress != "" {
				opLog.Debug().Str("region", region).Str("egress", egress).Int("status", status).
					Msg("gemini_web probe location")
			}

			return geminiWebVerdict(status, region, parsed, complete, blocked)
		})
}

// geminiWebVerdict classifies one response: a complete 2xx carrying a valid
// region in the blocked set is a refusal, the same answer outside it a pass,
// and anything that did not deliver that evidence is unverified. The incomplete
// check comes before the region comparison on purpose -- a body can be cut
// after the region (the fields follow it), and a truncated body is not proof
// that the rest of the answer was well-formed.
func geminiWebVerdict(status int, region string, parsed, complete bool, blocked map[string]struct{}) apiVerdict {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return apiUnverified
	}
	if !complete || !parsed {
		return apiUnverified
	}
	if _, ok := blocked[region]; ok {
		return apiBlocked
	}

	return apiAccepted
}

// parseGeminiWebRegion reads the egress region and, best effort, the egress
// address out of YouTube's sw.js_data body: the JSONP prefix, then a JSON array
// whose [0][2][0][0] row is a positional record where [1] is the two-letter
// region and [3] the address the endpoint saw the request arrive from.
//
// The decoder is driven with Token/Decode rather than json.Unmarshal into
// nested []json.RawMessage. Each RawMessage level copies its whole subtree, so
// the obvious shape measured 88 allocs/op and 19328 B/op on the real 2.6 KB
// fixture, against 15 allocs/op and 689 B/op for this walk, which stops at the
// row and never materializes the ~1 KB blob beside the two fields it needs
// (medians of -count=3, 2026-11-20; the walk's one extra allocation over the
// raw count is the validated egress address formatted for the debug log).
//
// ok means the region is valid; the egress address is NOT part of the verdict,
// so an absent or malformed one returns "" and leaves ok alone.
func parseGeminiWebRegion(body string) (region, egress string, ok bool) {
	payload, found := strings.CutPrefix(body, geminiWebJSONPPrefix)
	if !found {
		return "", "", false
	}
	dec := json.NewDecoder(strings.NewReader(payload))
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
