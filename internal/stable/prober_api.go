package stable

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
)

// maxAPIBody caps the response body read when scanning for a block marker;
// the error JSON is tiny, so this only guards against a hostile node.
const maxAPIBody = 64 << 10

// apiTLSHandshakeTimeout bounds the through-node TLS handshake to an API
// endpoint; the per-request deadline still comes from the check's timeout.
const apiTLSHandshakeTimeout = 10 * time.Second

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
