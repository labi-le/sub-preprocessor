package stable

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
)

// maxGeminiBody caps the response body we read when scanning for the geo-block
// marker; the error JSON is tiny, so this only guards against a hostile node.
const maxGeminiBody = 64 << 10

// geminiTLSHandshakeTimeout bounds the through-node TLS handshake to the
// Gemini endpoint; the per-request deadline still comes from gemini.Timeout.
const geminiTLSHandshakeTimeout = 10 * time.Second

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

			reachable, blocked := m.geminiProbeOne(ctx, px, target)
			host, _, splitErr := net.SplitHostPort(px.Addr())
			if splitErr != nil {
				host = px.Addr()
			}
			mu.Lock()
			out[px.Name()] = GeminiOutcome{Server: host, Reachable: reachable, Blocked: blocked}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// geminiProbeOne dials the Gemini endpoint through px and inspects the body.
// Mirrors mihomo's URLTest transport (a fixed pre-dialed conn) but issues a GET
// and reads the body instead of a HEAD.
func (m *MihomoProber) geminiProbeOne(ctx context.Context, px mihomo.Proxy, target string) (reachable, blocked bool) {
	tctx, cancel := context.WithTimeout(ctx, m.gemini.Timeout)
	defer cancel()

	u, err := url.Parse(target)
	if err != nil {
		return false, false
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	var meta mihomo.Metadata
	if addrErr := meta.SetRemoteAddress(net.JoinHostPort(u.Hostname(), port)); addrErr != nil {
		return false, false
	}
	conn, err := px.DialContext(tctx, &meta)
	if err != nil {
		return false, false
	}
	defer func() { _ = conn.Close() }()

	transport := &http.Transport{
		DialContext:         func(context.Context, string, string) (net.Conn, error) { return conn, nil },
		TLSHandshakeTimeout: geminiTLSHandshakeTimeout,
	}
	client := &http.Client{
		Timeout:   m.gemini.Timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(tctx, http.MethodGet, target, nil)
	if err != nil {
		return false, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxGeminiBody))
	return true, geminiBlocked(string(body), m.gemini.Marker)
}

// geminiBlocked reports whether a Gemini API response body indicates the
// caller's location is geo-blocked.
func geminiBlocked(body, marker string) bool {
	return marker != "" && strings.Contains(body, marker)
}
