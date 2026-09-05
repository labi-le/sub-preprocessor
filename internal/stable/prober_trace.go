package stable

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"sync"

	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/log"
)

const (
	countryCodeLen = 2
	// locNoCountry is Cloudflare's reserved loc for a client it has no country
	// data for; see validCountry.
	locNoCountry = "XX"

	// cloudflareTraceURL is deliberately NOT a config key. parseTrace and
	// validCountry below encode this one vendor's answer down to the reserved
	// loc values Cloudflare documents for CF-IPCountry (XX, T1) and its
	// uppercase convention, so no other VENDOR's endpoint can satisfy this
	// parser without emulating Cloudflare. An `endpoint:` key would therefore
	// have been a false affordance: what it invites is another vendor's
	// trace-like endpoint, which parses to no answer -- and silently, since a
	// rejected body is reported as "unanswered", which drops nothing and looks
	// exactly like an unreachable node. The exception is intra-vendor, and is
	// stated here rather than left as "every other value": Cloudflare serves
	// /cdn-cgi/trace on every domain it proxies, so any Cloudflare-fronted host
	// parses identically. If cloudflare.com is ever unreachable or intercepted
	// from an egress, that substitution is the one that works -- and it is made
	// here, in this constant.
	cloudflareTraceURL = "https://cloudflare.com/cdn-cgi/trace"
)

// traceURL is the endpoint TraceCheck probes. m.traceEndpoint is the test
// seam and is empty in every production build; see MihomoProber.
func (m *MihomoProber) traceURL() string {
	if m.traceEndpoint != "" {
		return m.traceEndpoint
	}

	return cloudflareTraceURL
}

// TraceResult is one node's measured egress. IP is a fact: the address the
// endpoint saw the request arrive from. Country is Cloudflare's geo-IP lookup
// OF that address — still a database opinion, but formed about the right
// address, which is the whole difference from the offline chain.
//
// Both are VALUES, never views into the response body: a result travels on to
// Survivor.Egress, and its country ends up as a Prometheus label inside the
// metrics snapshot, so a sub-slice would pin a whole 64 KiB body well past the
// cycle. parseTrace guarantees them rather than leaving it to each consumer —
// a TraceResult that exists at all carries an address netip accepts and a
// country of exactly two UPPERCASE ASCII letters that is none of Cloudflare's
// reserved non-country codes. Anything else is reported as no answer.
type TraceResult struct {
	IP      netip.Addr
	Country geofeed.CountryCode
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
	c := m.cloudflare
	opLog := log.Op(m.logger, "stable.TraceCheck")
	prog := newProgress(opLog, "cloudflare trace progress", len(proxies))

	// winner folds a label's proxies down to ONE outcome by a total order over
	// them, so the stored result does not depend on which goroutine finished
	// first.
	winner := make(map[string]traceOutcome, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := fanoutSem(c.Concurrency)
	for _, px := range proxies {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()

			reachable, status, body := apiProbeOne(ctx, px, m.traceURL(), nil, c.Timeout)
			res, ok := parseTrace(body)
			host := proxyHost(px)
			n := prog.step()
			ev := opLog.Debug().Str("node", px.Name()).Str("server", host).
				Bool("reachable", reachable).Int("status", status).
				Bool("answered", ok).
				Int64("n", n).Int64("of", prog.total)
			// loc and egress exist only for a parsed answer: the zero Country
			// would log as two NUL bytes and a zero Addr as "invalid IP",
			// which reads like a real location where none was measured.
			if ok {
				ev = ev.Stringer("loc", res.Country).Stringer("egress", res.IP)
			}
			ev.Msg("cloudflare trace")
			if !reachable || status < http.StatusOK || status >= http.StatusMultipleChoices || !ok {
				return
			}
			mu.Lock()
			cand := traceOutcome{addr: px.Addr(), name: px.Name(), res: res}
			label := entryLabel(px)
			if best, seen := winner[label]; !seen || betterTraceOutcome(cand, best) {
				winner[label] = cand
			}
			mu.Unlock()
		})
	}
	wg.Wait()

	out := make(map[string]TraceResult, len(winner))
	for label, w := range winner {
		out[label] = w.res
	}

	return out
}

// traceOutcome is one proxy's trace answer plus the identity the fold orders
// by. A mieru label holds one proxy per port and they can exit differently, so
// exactly one of them has to win the label.
type traceOutcome struct {
	addr string
	name string
	res  TraceResult
}

// betterTraceOutcome reports whether a should replace b as their label's
// stored result. The order has to be TOTAL: on a tie the map is not
// overwritten and the survivor is whichever goroutine reached the mutex first,
// which is exactly what folding by an order is meant to rule out.
//
// Address alone is not total. mihomo builds a mieru proxy's address from
// server and port only (adapter/outbound/mieru.go NewMieru: net.JoinHostPort,
// and a port RANGE contributes its begin port), while the NAME also carries
// the transport (common/convert/converter.go: "<label>:<port>/<protocol>").
// Two proxies of one mierus:// link therefore share an address whenever the
// link repeats a port under two protocols, or writes a plain port equal to a
// range's begin port. The name cannot collide: uniqueName suffixes "-%02d".
func betterTraceOutcome(a, b traceOutcome) bool {
	if a.addr != b.addr {
		return a.addr < b.addr
	}

	return a.name < b.name
}

// parseTrace reads Cloudflare's /cdn-cgi/trace body: "key=value" lines, of no
// fixed length — the endpoint echoes the request User-Agent back in uag=, so
// the only bound is apiProbeOne's maxAPIBody. Only a result carrying BOTH
// fields counts — a tag is only worth replacing when the replacement is
// complete, and a partial answer means the response came from something that
// is not the trace endpoint.
//
// Both fields are validated and CONVERTED here because that is what the rest
// of the package is promised: a country of two UPPERCASE ASCII letters, never
// one of Cloudflare's reserved non-country codes, and an address netip
// accepts. The conversion is also what keeps the body out of the result — see
// TraceResult. A rejected body is reported as NO answer, so the caller keeps
// whatever the offline chain resolves; this never invents a second spelling of
// countryUnknown.
func parseTrace(body string) (TraceResult, bool) {
	var ip, loc string
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
			ip = value
		case "loc":
			loc = value
		}
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil || !validCountry(loc) {
		return TraceResult{}, false
	}

	return TraceResult{IP: addr, Country: geofeed.CountryCode{loc[0], loc[1]}}, true
}

// validCountry accepts only what a [GEO:] tag may carry. Besides ISO-3166-1
// alpha-2, Cloudflare documents two reserved loc values that are NOT
// countries: XX for a client it has no country data for, and T1 for the Tor
// network (loc is the CF-IPCountry value —
// https://developers.cloudflare.com/fundamentals/reference/http-headers/#cf-ipcountry).
// T1 is already caught by the letters test; XX is not, and XX is the one that
// costs information: overwriting an offline [GEO:DE] with [GEO:XX] replaces a
// possibly-correct guess with none at all.
//
// The letters test is case-EXACT, and the reserved-code guard leans on that: a
// case-folding test would admit "xx", the very code the guard exists to reject.
// It would also admit any lowercase loc, while every geo database upper-folds
// its input through the shared parseCountry (geofeed/dbip.go), so one country
// would reach stable_kept_country_nodes under two label values. Cloudflare
// documents loc uppercase, and a rejection here reads as no answer, so being
// wrong about that costs the node its correction, never a wrong tag.
func validCountry(c string) bool {
	return len(c) == countryCodeLen && upperLetter(c[0]) && upperLetter(c[1]) && c != locNoCountry
}

func upperLetter(b byte) bool { return b >= 'A' && b <= 'Z' }
