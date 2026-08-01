package stable

import (
	"context"
	"net"
	"strings"
	"sync"

	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/log"
)

// TraceResult is one node's measured egress. IP is a fact: the address the
// endpoint saw the request arrive from. Country is Cloudflare's geo-IP lookup
// OF that address — still a database opinion, but formed about the right
// address, which is the whole difference from the offline chain.
type TraceResult struct {
	Country string
	IP      string
}

// TraceCheck asks each node where its traffic actually comes out.
//
// The offline annotate chain cannot answer this. It places the address our
// resolver got for the node's hostname, and 41% of the named hosts measured in
// the pool resolve into Cloudflare's shared anycast ranges (104.21/16,
// 172.67/16). Those prefixes terminate in dozens of countries at once, so no
// registration database can be right about them: the tag ends up describing
// Cloudflare's registration while the traffic leaves from the origin server —
// a node tagged CA whose egress is in Germany.
//
// This runs its own fan-out rather than apiCheck's, because apiCheck folds each
// response down to a blocked/reachable verdict and discards the body, and four
// production gates depend on that signature.
func (m *MihomoProber) TraceCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]TraceResult {
	c := m.geo.GeoTrace
	opLog := log.Op(m.logger, "stable.TraceCheck")
	prog := newProgress(opLog, "geotrace progress", len(proxies))

	out := make(map[string]TraceResult, len(proxies))
	// winner records which proxy address produced the stored result, so a label
	// covering several proxies folds by a total order instead of by whoever
	// finished first.
	winner := make(map[string]string, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := fanoutSem(c.Concurrency)
	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reachable, status, body := apiProbeOne(ctx, px, c.Endpoint, nil, c.Timeout)
			res, ok := parseTrace(body)
			host, _, splitErr := net.SplitHostPort(px.Addr())
			if splitErr != nil {
				host = px.Addr()
			}
			n := prog.step()
			opLog.Debug().Str("node", px.Name()).Str("server", host).
				Bool("reachable", reachable).Int("status", status).
				Str("loc", res.Country).Str("egress", res.IP).
				Int64("n", n).Int64("of", prog.total).Msg("geotrace")
			if !reachable || status < 200 || status >= 300 || !ok {
				return
			}
			mu.Lock()
			// A mieru label holds one proxy per port and they can exit
			// differently. Completion order is scheduler-dependent, so the
			// lowest address wins rather than the fastest goroutine.
			label := entryLabel(px)
			if best, seen := winner[label]; !seen || px.Addr() < best {
				out[label] = res
				winner[label] = px.Addr()
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	return out
}

// parseTrace reads Cloudflare's /cdn-cgi/trace body: 211 bytes of "key=value"
// lines. Only a result carrying BOTH fields counts — a tag is only worth
// replacing when the replacement is complete, and a partial answer means the
// response came from something that is not the trace endpoint.
func parseTrace(body string) (TraceResult, bool) {
	var res TraceResult
	for len(body) > 0 {
		line := body
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			line, body = body[:i], body[i+1:]
		} else {
			body = ""
		}
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || value == "" {
			continue
		}
		switch key {
		case "ip":
			res.IP = value
		case "loc":
			res.Country = value
		}
	}
	if res.IP == "" || len(res.Country) != countryCodeLen {
		return TraceResult{}, false
	}

	return res, true
}

const countryCodeLen = 2
