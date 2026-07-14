package stable

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
)

// ClaudeOutcome is the per-node result of the through-node Anthropic check.
type ClaudeOutcome struct {
	Server    string // node host (no port); the geoblock key
	Reachable bool   // an HTTP response came back through the node
	Blocked   bool   // the response body carried the geo-block marker
}

func (m *MihomoProber) claudeURL() string {
	return strings.TrimRight(m.claude.Endpoint, "/") + "/v1/models"
}

// ClaudeCheck sends a keyless Anthropic API GET through every node in payload
// and classifies it. Anthropic geo-blocks before authentication: a blocked
// region gets HTTP 403 with the "Request not allowed" marker in the body,
// while an allowed region gets an authentication error instead. No API key is
// required, so the gate is always active when the filter is configured.
func (m *MihomoProber) ClaudeCheck(ctx context.Context, payload []byte) map[string]ClaudeOutcome {
	proxies, err := m.parseProxies(payload)
	if err != nil {
		m.logger.Warn().Err(err).Msg("claude check: no proxies")
		return nil
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	target := m.claudeURL()
	header := http.Header{"Anthropic-Version": []string{m.claude.Version}}
	out := make(map[string]ClaudeOutcome, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, m.claude.Concurrency)
	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reachable, body := apiProbeOne(ctx, px, target, header, m.claude.Timeout)
			host, _, splitErr := net.SplitHostPort(px.Addr())
			if splitErr != nil {
				host = px.Addr()
			}
			mu.Lock()
			out[px.Name()] = ClaudeOutcome{
				Server:    host,
				Reachable: reachable,
				Blocked:   reachable && claudeBlocked(body, m.claude.Marker),
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// claudeBlocked reports whether an Anthropic API response body indicates the
// caller's location is geo-blocked.
func claudeBlocked(body, marker string) bool {
	return marker != "" && strings.Contains(body, marker)
}
