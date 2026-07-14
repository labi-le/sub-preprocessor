package stable

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
)

// GeminiOutcome is the per-node result of the through-node Gemini check.
type GeminiOutcome struct {
	Server    string // node host (no port); the geoblock key
	Reachable bool   // an HTTP response came back through the node
	Blocked   bool   // the response body carried the geo-block marker
}

// GeminiEnabled reports whether the Gemini gate is active (configured with a
// resolved API key).
func (m *MihomoProber) GeminiEnabled() bool {
	return m.geminiKey != ""
}

func (m *MihomoProber) geminiURL() string {
	return strings.TrimRight(m.gemini.Endpoint, "/") + "/v1beta/models/" +
		m.gemini.Model + "?key=" + url.QueryEscape(m.geminiKey)
}

// GeminiCheck sends a real Gemini API GET through every node in payload and
// classifies it. This is the algorithm mihomo's HEAD-only URLTest cannot do: a
// geo-block appears only in the API response body, not the status code.
func (m *MihomoProber) GeminiCheck(ctx context.Context, payload []byte) map[string]GeminiOutcome {
	proxies, err := m.parseProxies(payload)
	if err != nil {
		m.logger.Warn().Err(err).Msg("gemini check: no proxies")
		return nil
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	target := m.geminiURL()
	out := make(map[string]GeminiOutcome, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, m.gemini.Concurrency)
	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reachable, body := apiProbeOne(ctx, px, target, nil, m.gemini.Timeout)
			host, _, splitErr := net.SplitHostPort(px.Addr())
			if splitErr != nil {
				host = px.Addr()
			}
			mu.Lock()
			out[px.Name()] = GeminiOutcome{
				Server:    host,
				Reachable: reachable,
				Blocked:   reachable && geminiBlocked(body, m.gemini.Marker),
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// geminiBlocked reports whether a Gemini API response body indicates the
// caller's location is geo-blocked.
func geminiBlocked(body, marker string) bool {
	return marker != "" && strings.Contains(body, marker)
}
