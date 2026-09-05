package stable

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/log"
)

// GeminiEnabled reports whether the Gemini gate is active (configured with a
// resolved API key).
func (m *MihomoProber) GeminiEnabled() bool {
	return m.geminiKey != ""
}

func (m *MihomoProber) geminiURL() string {
	return strings.TrimRight(m.geo.Gemini.Endpoint, "/") + "/v1beta/models/" +
		m.geo.Gemini.Model + "?key=" + url.QueryEscape(m.geminiKey)
}

// GeminiCheck sends a real Gemini API GET through each of the supplied proxies
// and classifies it. This is the algorithm mihomo's HEAD-only URLTest cannot
// do: a geo-block appears only in the API response body, not the status code.
// The caller owns the proxies' lifecycle (parse once, close once).
//
// The check paces its own request starts when RateLimit > 0 (interval =
// time.Minute / RateLimit): Google's metadata-read quota,
// generativelanguage.googleapis.com/model_requests, is 200/min per project per
// region — measured 2026-09-03 against the live API with the production key,
// 500 GETs at concurrency 8 finished in 20s with 202 answering
// 429 RESOURCE_EXHAUSTED. A 429 is geminiInconclusive, and an inconclusive
// node is KEPT and published, so an unpaced gate does not merely lose
// coverage: it publishes geo-blocked nodes.
//
// A response that never reached the location check is counted and warned about
// instead of passing silently as "not blocked": see geminiInconclusive. That
// count is also RETURNED, because a warning nothing scrapes is how a rotated
// key stayed invisible on the dashboard -- see GeminiReport.
//
// The two counters are atomic because the classifier runs on every fan-out
// goroutine; they are read once, after apiCheck has joined them all. Both
// count classifier CALLS, so an unreachable proxy is in neither: apiCheck
// short-circuits blocked() when nothing came back.
func (m *MihomoProber) GeminiCheck(ctx context.Context, proxies []mihomo.Proxy) (map[string]APIOutcome, GeminiReport) {
	g := m.geo.Gemini
	var pace time.Duration
	if g.RateLimit > 0 {
		pace = time.Minute / time.Duration(g.RateLimit)
	}
	var checks, inconclusive atomic.Int64
	out := m.apiCheck(ctx, "stable.GeminiCheck", "gemini check", proxies,
		m.geminiURL(), nil, g.Timeout, g.Concurrency, pace,
		func(status int, body string) bool {
			checks.Add(1)
			if geminiInconclusive(status, body, g.Marker) {
				inconclusive.Add(1)
			}
			return markerBlocked(body, g.Marker)
		})
	rep := GeminiReport{
		State:      GeminiGateRan,
		Checks:     int(checks.Load()),
		Unverified: int(inconclusive.Load()),
	}
	if rep.Unverified > 0 {
		opLog := log.Op(m.logger, "stable.GeminiCheck")
		// classified is the metric's denominator (stable_gemini_gate_checks);
		// of= stays the proxy count the fan-out was handed, so the gap between
		// them is the PROXIES that never answered -- not the node-level
		// reason="unreachable" drop, which needs every proxy of a node dead --
		// and the log still matches the series. The leading count is classifier
		// calls, not nodes (see GeminiReport), so it is keyed like the metric.
		opLog.Warn().Int("unverified_checks", rep.Unverified).Int("classified", rep.Checks).Int("of", len(proxies)).
			Msg("gemini gate verified nothing for these checks: the API answered without a location " +
				"verdict (key rotated/restricted, wrong model, quota, a server fault, or a reworded refusal) -- their nodes were kept")
	}

	return out, rep
}

// geminiInconclusive reports whether a Gemini API response says nothing about
// the egress location. Measured against generativelanguage.googleapis.com from
// a geo-blocked egress, the API answers in this order: caller identity (403,
// "unregistered callers", when no key is sent), then key validity (400
// API_KEY_INVALID for a junk key), and only then the location precondition (400
// FAILED_PRECONDITION, the marker). So only two responses carry the verdict: a
// 2xx (every gate, the location one included, passed) and the marker-400 (the
// location gate refused -- a real block, which the caller drops via
// markerBlocked). Everything else -- a rejected credential, a wrong model path
// (404), throttling (429), a 5xx server fault, an unfollowed redirect, or a 400
// whose wording no longer carries the marker -- was answered BEFORE the location
// verdict existed or without readable evidence of it; reading those as "not
// blocked" turns the whole gate into a silent no-op the moment the key rotates
// or the marker drifts -- the node then egresses to a country where Gemini
// refuses it, which is exactly what this gate exists to prevent. The marker is
// an argument because it is per-check configurable.
func geminiInconclusive(status int, body, marker string) bool {
	switch {
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		return false
	case status == http.StatusBadRequest && markerBlocked(body, marker):
		return false
	default:
		return true
	}
}
