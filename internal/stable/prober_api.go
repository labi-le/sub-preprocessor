package stable

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
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
	Blocked   bool   // the check read the response as a refusal of this egress
}

// fanoutSem returns the semaphore bounding a through-node fan-out. The acquire
// runs on the caller's goroutine before the worker that releases it exists, so
// a zero or negative bound would deadlock on the first node rather than degrade
// to serial execution. Config defaults and validation keep the real values >=1;
// this guards hand-constructed probers.
func fanoutSem(concurrency int) chan struct{} {
	if concurrency < 1 {
		concurrency = 1
	}
	return make(chan struct{}, concurrency)
}

// apiCheck fans a through-node API GET out over proxies (bounded by
// concurrency) and classifies each response with blocked, which sees both the
// status and the body: a service can refuse an egress with either (a marker in
// the body, or a bare CDN 403). Every node logs a debug outcome and the
// progress logger reports each completed 10% decade.
//
// Outcomes are keyed by entry label, which is what a filter looks them up by.
// Pass at most one proxy per label: applyFilters' shared map has already
// collapsed a mieru node's per-port proxies, and a second one here would
// overwrite the first's outcome instead of folding with it.
func (m *MihomoProber) apiCheck(
	ctx context.Context,
	op, msg string,
	proxies []mihomo.Proxy,
	target string,
	header http.Header,
	timeout time.Duration,
	concurrency int,
	blocked func(status int, body string) bool,
) map[string]APIOutcome {
	opLog := log.Op(m.logger, op)
	prog := newProgress(opLog, msg+" progress", len(proxies))

	out := make(map[string]APIOutcome, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := fanoutSem(concurrency)
	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reachable, status, body := apiProbeOne(ctx, px, target, header, timeout)
			host, _, splitErr := net.SplitHostPort(px.Addr())
			if splitErr != nil {
				host = px.Addr()
			}
			o := APIOutcome{Server: host, Reachable: reachable, Blocked: reachable && blocked(status, body)}
			n := prog.step()
			opLog.Debug().Str("node", px.Name()).Str("server", host).
				Bool("reachable", o.Reachable).Int("status", status).Bool("blocked", o.Blocked).
				Int64("n", n).Int64("of", prog.total).Msg(msg)
			mu.Lock()
			out[entryLabel(px)] = o
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// markerBlocked reports whether an API response body carries the service's
// geo-block marker. An empty marker never matches, so a check with no
// configured marker keeps every node instead of dropping all of them.
func markerBlocked(body, marker string) bool {
	return marker != "" && strings.Contains(body, marker)
}

// apiProbeOne dials target through px, issues a GET with header, and returns
// whether a response came back, its status, and its (capped) body. Mirrors
// mihomo's URLTest transport (a fixed pre-dialed conn) but reads the status and
// body, neither of which a HEAD-only URLTest exposes to a classifier.
func apiProbeOne(
	ctx context.Context,
	px mihomo.Proxy,
	target string,
	header http.Header,
	timeout time.Duration,
) (reachable bool, status int, body string) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// One dial-address rule for both probe paths: hostPort defaults the port
	// from the scheme, so an http:// endpoint override is dialled on 80
	// instead of being pinned to 443 and timing every node out. It also
	// refuses a target with no host, which must never reach the node.
	addr, ok := hostPort(target)
	if !ok {
		return false, 0, ""
	}
	var meta mihomo.Metadata
	if addrErr := meta.SetRemoteAddress(addr); addrErr != nil {
		return false, 0, ""
	}
	conn, err := px.DialContext(tctx, &meta)
	if err != nil {
		return false, 0, ""
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
		return false, 0, ""
	}
	req.Header.Set("User-Agent", browserUserAgent)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, ""
	}
	defer func() { _ = resp.Body.Close() }()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	return true, resp.StatusCode, string(b)
}
