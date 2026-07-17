package stable

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/log"
)

// maxAPIBody caps the response body read when scanning for a block marker;
// the error JSON is tiny, so this only guards against a hostile node.
const maxAPIBody = 64 << 10

// apiTLSHandshakeTimeout bounds the through-node TLS handshake to an API
// endpoint; the per-request deadline still comes from the check's timeout.
const apiTLSHandshakeTimeout = 10 * time.Second

// APIOutcome is the per-node result of a through-node API check.
type APIOutcome struct {
	Server    string // node host (no port); the geoblock key
	Reachable bool   // an HTTP response came back through the node
	Blocked   bool   // the response body carried the geo-block marker
}

// apiCheck fans a through-node API GET out over proxies (bounded by
// concurrency) and classifies each response with blocked. Every node logs a
// debug outcome and the progress logger reports each completed 10% decade.
func (m *MihomoProber) apiCheck(
	ctx context.Context,
	op, msg string,
	proxies []mihomo.Proxy,
	target string,
	header http.Header,
	timeout time.Duration,
	concurrency int,
	blocked func(body string) bool,
) map[string]APIOutcome {
	opLog := log.Op(m.logger, op)
	prog := newProgress(opLog, msg+" progress", len(proxies))

	out := make(map[string]APIOutcome, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reachable, body := apiProbeOne(ctx, px, target, header, timeout)
			host, _, splitErr := net.SplitHostPort(px.Addr())
			if splitErr != nil {
				host = px.Addr()
			}
			o := APIOutcome{Server: host, Reachable: reachable, Blocked: reachable && blocked(body)}
			n := prog.step()
			opLog.Debug().Str("node", px.Name()).Str("server", host).
				Bool("reachable", o.Reachable).Bool("blocked", o.Blocked).
				Int64("n", n).Int64("of", prog.total).Msg(msg)
			mu.Lock()
			out[px.Name()] = o
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// apiProbeOne dials target through px, issues a GET with header, and returns
// whether a response came back plus its (capped) body. Mirrors mihomo's
// URLTest transport (a fixed pre-dialed conn) but reads the body, which a
// HEAD-only URLTest cannot do.
func apiProbeOne(
	ctx context.Context,
	px mihomo.Proxy,
	target string,
	header http.Header,
	timeout time.Duration,
) (reachable bool, body string) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	u, err := url.Parse(target)
	if err != nil {
		return false, ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	var meta mihomo.Metadata
	if addrErr := meta.SetRemoteAddress(net.JoinHostPort(u.Hostname(), port)); addrErr != nil {
		return false, ""
	}
	conn, err := px.DialContext(tctx, &meta)
	if err != nil {
		return false, ""
	}
	defer func() { _ = conn.Close() }()

	transport := &http.Transport{
		DialContext:         func(context.Context, string, string) (net.Conn, error) { return conn, nil },
		TLSHandshakeTimeout: apiTLSHandshakeTimeout,
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(tctx, http.MethodGet, target, nil)
	if err != nil {
		return false, ""
	}
	req.Header.Set("User-Agent", browserUserAgent)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, ""
	}
	defer func() { _ = resp.Body.Close() }()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	return true, string(b)
}
