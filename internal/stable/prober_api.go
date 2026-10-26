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
// the error JSON is tiny, so this only guards against a hostile node. One byte
// past the cap is read to tell a body that ends exactly at it from one that
// continues: a cap hit is an incomplete read, not a complete response.
const maxAPIBody = 64 << 10

// apiTLSHandshakeTimeout bounds the through-node TLS handshake to an API
// endpoint; the per-request deadline still comes from the check's timeout.
const apiTLSHandshakeTimeout = 10 * time.Second

// APIOutcome is the per-node result of a through-node API check.
type APIOutcome struct {
	Server    string // node host (no port); the geoblock key
	Reachable bool   // an HTTP response came back through the node
	Blocked   bool   // the check read the response as a refusal of this egress
	// Verified marks a conclusive outcome: the check read the evidence its
	// classifier needs. A marker check needs the whole body, so a body cut
	// short by a read error or by maxAPIBody leaves the node reachable but
	// unverified unless the marker was already in the bytes read: the marker
	// may sit in the bytes that never arrived. Tidal classifies every response;
	// DeepL needs either an explicit refusal or acceptance on both transports.
	Verified bool
}

// apiVerdict is a classifier's reading of one through-node response.
type apiVerdict uint8

const (
	// apiUnverified is a response that did not carry the evidence the
	// classifier reads, so it supports neither a pass nor a refusal.
	apiUnverified apiVerdict = iota
	apiAccepted
	apiBlocked
)

// API fold ranks, weakest first.
const (
	apiRankUnreachable = iota
	apiRankUnverified
	apiRankBlocked
	apiRankPassed
)

// betterAPIOutcome reports whether a is the outcome to keep when two proxies
// of one entry fold onto the same label — a mierus:// link, whose ports are
// checked independently. Best-of-ports, mirroring betterProbe: what we publish
// is the link, and its consumer is mihomo, which expands it back into one
// health-checked proxy per port. A dead port therefore costs the consumer one
// member of a proxy group, whereas worst-of-N here would discard a whole
// multi-port node because one obscure UDP port is filtered on OUR egress, not
// the consumer's.
//
// Blocked outranks unreachable because every port of a link shares one host:
// a service that refused the egress gave a verdict about that host, worth
// persisting to the geoblock store, while an unreachable port says nothing.
// Unverified sits between them: the node answered, so it is a live port, but a
// cut-short body is no evidence against the refusal another port reported.
func betterAPIOutcome(a, b APIOutcome) bool {
	return apiOutcomeRank(a) > apiOutcomeRank(b)
}

func apiOutcomeRank(o APIOutcome) int {
	switch {
	case o.Reachable && !o.Blocked && o.Verified:
		return apiRankPassed
	case o.Blocked:
		return apiRankBlocked
	case o.Reachable:
		return apiRankUnverified
	default:
		return apiRankUnreachable
	}
}

// fanoutSem returns the semaphore bounding a through-node fan-out. It is
// ALWAYS buffered with capacity >= 1, which is what lets every caller acquire
// in the spawning loop, BEFORE wg.Go (like filterReachable): the first send
// never blocks, and later sends park the spawner only while concurrency
// workers are in flight, so goroutine creation is bounded as well as
// execution. A raw zero-buffer channel would deadlock under that shape — the
// first send would have no receiver until a worker exists, and the spawner
// would never spawn one — which is why the clamp below guards the hand-built
// Concurrency 0 that config validation never lets through.
func fanoutSem(concurrency int) chan struct{} {
	if concurrency < 1 {
		concurrency = 1
	}
	return make(chan struct{}, concurrency)
}

// apiCheck fans a through-node API GET out over proxies (bounded by
// concurrency) and classifies each response with classify, which sees the
// status, the body and whether that body was read to its end: a service can
// refuse an egress with a status (a bare CDN 403) or a marker in the body, and
// a marker check must not read a cut-short body as a clean pass. A status-only
// check passes readBody false: the probe then returns as soon as the headers
// arrive and closes the body unread, which is what keeps a stalled body from
// occupying a worker until the request deadline. Every node logs a debug
// outcome and the progress logger reports each completed 10% decade.
//
// pace is the minimum interval between request STARTS; 0 disables pacing. A
// paced check owns one ticker for the whole fan-out and every worker waits on
// it (or on ctx cancellation) before dialling, so request starts leave at most
// one per interval no matter how many workers are in flight — concurrency
// still caps how many requests are outstanding. The wait must select on
// ctx.Done() too, or a cancelled cycle would sit out the remaining interval.
// One ticker per check, nothing per request.
//
// Outcomes are keyed by entry label, which is what a filter looks them up by.
// Several proxies may share one label (a mieru node's ports), and they finish
// in arbitrary order, so the fold happens under the same mutex that guards the
// map: betterAPIOutcome decides at write time, making the result independent
// of goroutine scheduling.
func (m *MihomoProber) apiCheck(
	ctx context.Context,
	op, msg string,
	proxies []mihomo.Proxy,
	target string,
	header http.Header,
	timeout time.Duration,
	concurrency int,
	pace time.Duration,
	readBody bool,
	classify func(status int, body string, complete bool) apiVerdict,
) map[string]APIOutcome {
	opLog := log.Op(m.logger, op)
	prog := newProgress(opLog, msg+" progress", len(proxies))

	out := make(map[string]APIOutcome, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := fanoutSem(concurrency)
	var ticker *time.Ticker
	if pace > 0 {
		ticker = time.NewTicker(pace)
		defer ticker.Stop()
	}
	for _, px := range proxies {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if ticker != nil {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return
				}
			}

			reachable, status, body, complete := apiProbeOne(ctx, px, target, header, timeout, readBody)
			host := proxyHost(px)
			o := APIOutcome{Server: host, Reachable: reachable}
			if reachable {
				switch classify(status, body, complete) {
				case apiBlocked:
					o.Blocked = true
					o.Verified = true
				case apiAccepted:
					o.Verified = true
				case apiUnverified:
					// Reachable and kept, but no verdict: apiFilter.apply
					// warns about the count and writes no geoblock host.
				}
			}
			n := prog.step()
			opLog.Debug().Str("node", px.Name()).Str("server", host).
				Bool("reachable", o.Reachable).Int("status", status).Bool("blocked", o.Blocked).
				Bool("verified", o.Verified).
				Int64("n", n).Int64("of", prog.total).Msg(msg)
			mu.Lock()
			label := entryLabel(px)
			if prev, ok := out[label]; !ok || betterAPIOutcome(o, prev) {
				out[label] = o
			}
			mu.Unlock()
		})
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

// markerVerdict classifies a response by the service's geo-block marker. An
// incomplete body that does not already carry the marker is unverified, never
// accepted: the marker may sit in the bytes that never arrived. A marker found
// in the partial body is still a refusal.
func markerVerdict(_ int, body, marker string, complete bool) apiVerdict {
	if markerBlocked(body, marker) {
		return apiBlocked
	}
	if !complete {
		return apiUnverified
	}
	return apiAccepted
}

// apiProbeOne dials target through px, issues a GET with header, and returns
// whether a response came back, its status, its (capped) body, and whether the
// body was read to its end. Mirrors mihomo's URLTest transport (a fixed
// pre-dialed conn) but reads the status and body, neither of which a HEAD-only
// URLTest exposes to a classifier. With readBody false it returns at the
// headers and closes the body unread (complete is false -- nothing was read):
// that is the whole contract of a status-only gate, and waiting for bytes it
// will never classify only burns the caller's deadline.
func apiProbeOne(
	ctx context.Context,
	px mihomo.Proxy,
	target string,
	header http.Header,
	timeout time.Duration,
	readBody bool,
) (reachable bool, status int, body string, complete bool) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// One dial-address rule for both probe paths: hostPort defaults the port
	// from the scheme, so an http:// endpoint override is dialled on 80
	// instead of being pinned to 443 and timing every node out. It also
	// refuses a target with no host, which must never reach the node.
	addr, ok := hostPort(target)
	if !ok {
		return false, 0, "", false
	}
	var meta mihomo.Metadata
	if addrErr := meta.SetRemoteAddress(addr); addrErr != nil {
		return false, 0, "", false
	}
	conn, err := px.DialContext(tctx, &meta)
	if err != nil {
		return false, 0, "", false
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
		return false, 0, "", false
	}
	req.Header.Set("User-Agent", browserUserAgent)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, "", false
	}
	defer func() { _ = resp.Body.Close() }()

	if !readBody {
		return true, resp.StatusCode, "", false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody+1))
	if err != nil {
		// The headers arrived, so the node answered; the bytes received are
		// still scanned for a marker, but the response was not read whole.
		return true, resp.StatusCode, string(b), false
	}
	if len(b) > maxAPIBody {
		return true, resp.StatusCode, string(b), false
	}
	return true, resp.StatusCode, string(b), true
}
