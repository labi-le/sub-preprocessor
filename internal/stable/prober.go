package stable

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"
	"github.com/metacubex/mihomo/common/utils"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/log"
)

// browserUserAgent is sent on every through-node GET probe so traffic egressing
// each node looks like an ordinary browser: the default Go-http-client/1.1 UA is
// a common WAF / geo-panel block trigger that would skew reachability results.
const browserUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

// Prober measures reachability of the proxy nodes in a subscription payload.
type Prober interface {
	Probe(ctx context.Context, payload []byte) (map[string]ProbeResult, error)
	// ParseProxies parses a subscription payload into live mihomo proxies. The
	// caller owns their lifecycle and MUST Close each proxy exactly once.
	ParseProxies(payload []byte) ([]mihomo.Proxy, error)
}

// MihomoProber runs repeated URL tests through mihomo's adapter stack.
type MihomoProber struct {
	cfg        config.CheckConfig
	bandwidth  config.BandwidthConfig
	expected   utils.IntRanges[uint16]
	geo        config.GeoBlockConfig
	cloudflare config.CloudflareConfig
	geminiKey  string
	logger     zerolog.Logger
	// precheck is filled by filterReachable and read back through
	// PrecheckReport after Probe returns, both on the cycle goroutine. Cycles
	// never overlap (Controller.Apply swaps a spec, it does not run one).
	precheck PrecheckReport
	// traceEndpoint overrides the cdn-cgi/trace URL for tests, which point it
	// at an httptest server. It is a field rather than a config key because
	// the answer is only parseable from Cloudflare (see cloudflareTraceURL);
	// NewMihomoProber leaves it empty, so nothing an operator writes can move
	// the probe.
	traceEndpoint string
}

// NewMihomoProber takes the whole geoblock block rather than one parameter per
// through-node API check: a new check then adds a GeoBlockConfig field and a
// Controller.Apply switch case, both required anyway, instead of widening this
// signature again. cf is separate because the cdn-cgi/trace probe is no gate
// and so lives under geo.cloudflare, not geoblock. geminiKey stays separate
// because only Gemini needs a credential and resolving it can fail, which is
// the caller's decision to log.
func NewMihomoProber(
	cfg config.CheckConfig,
	bandwidth config.BandwidthConfig,
	geo config.GeoBlockConfig,
	cf config.CloudflareConfig,
	geminiKey string,
	logger zerolog.Logger,
) (*MihomoProber, error) {
	expected, err := utils.NewUnsignedRanges[uint16](cfg.ExpectedStatus)
	if err != nil {
		return nil, fmt.Errorf("parse expected_status %q: %w", cfg.ExpectedStatus, err)
	}

	return &MihomoProber{
		cfg: cfg, bandwidth: bandwidth, expected: expected,
		geo: geo, cloudflare: cf, geminiKey: geminiKey, logger: logger,
	}, nil
}

type delayAcc struct {
	// int32, not int: one of these per parsed proxy lives in a single slice for
	// the whole probe. succ is bounded by check.rounds and sum by rounds x a
	// uint16 delay, and the element is 12 bytes where two ints beside a
	// ProbeStage pad to 24 in every field order.
	succ  int32
	sum   int32
	stage ProbeStage
}

// betterProbe reports whether a is the result to keep when two proxies fold
// onto one label: more successful rounds first, lower mean latency next. Stage
// only breaks a remaining tie, which in practice means two ports that both
// failed — keeping the furthest either got.
func betterProbe(a, b ProbeResult) bool {
	if a.Successes != b.Successes {
		return a.Successes > b.Successes
	}
	if a.MeanMs != b.MeanMs {
		return a.MeanMs < b.MeanMs
	}

	return a.Stage > b.Stage
}

// Probe parses the payload once, drops nodes whose server accepts no TCP
// connection, and URL-tests the rest for the configured number of rounds. The
// result map holds one entry per entry label (see entryLabel) for every node
// that reached the prober, failures included: Successes == 0 is what marks a
// node dead, never absence.
func (m *MihomoProber) Probe(ctx context.Context, payload []byte) (map[string]ProbeResult, error) {
	// Never last cycle's verdict: a Probe that fails before the pre-check must
	// report PrecheckAbsent, not the previous pool's numbers.
	m.precheck = PrecheckReport{}
	proxies, tcpServer, err := m.parseProxies(payload)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	opLog := log.Op(m.logger, "stable.Probe")
	live, condemned := m.filterReachable(ctx, opLog, proxies, tcpServer)
	prog := newProgress(opLog, "url-test progress", m.cfg.Rounds*len(live))

	// Indexed by proxy position, so a node's state is one slice element rather
	// than a map entry plus its own heap allocation.
	accs := make([]delayAcc, len(proxies))
	// Seeded before any round starts, which is the only time accs is safe to
	// touch without mu.
	for _, i := range condemned {
		accs[i].stage = StageCondemned
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	// One semaphore shared by every round so the effective number of in-flight
	// URL tests honors check.concurrency instead of rounds*concurrency.
	// fanoutSem, not a raw channel: runRound acquires on the producer
	// goroutine before any releaser exists, so a zero bound would deadlock.
	sem := fanoutSem(m.cfg.Concurrency)
	for range m.cfg.Rounds {
		wg.Go(func() {
			m.runRound(ctx, opLog, prog, proxies, live, sem, &mu, accs)
		})
	}
	wg.Wait()

	if ctxErr := ctx.Err(); ctxErr != nil {
		// Partial results from a cancelled probe would masquerade as a
		// truncated-but-successful cycle; report the cancellation instead.
		return nil, fmt.Errorf("probe interrupted: %w", ctxErr)
	}

	return foldProbeResults(proxies, accs), nil
}

// foldProbeResults collapses the per-proxy accumulators onto entry labels.
//
// A mierus:// entry arrives here as one proxy per configured port, all folding
// onto the same label. Best-of, never a sum: mieru dials one of its ports, so
// a single working port makes the node usable, whereas adding N ports' rounds
// together would let Successes exceed check.rounds and walk straight through
// SelectSurvivors' maxFail gate.
//
// Payload order is load-bearing: it is what resolves a tie between two ports
// deterministically.
func foldProbeResults(proxies []mihomo.Proxy, accs []delayAcc) map[string]ProbeResult {
	res := make(map[string]ProbeResult, len(proxies))
	for i, px := range proxies {
		a := &accs[i]
		r := ProbeResult{Successes: int(a.succ), Stage: a.stage}
		if a.succ > 0 {
			r.MeanMs = int(a.sum / a.succ)
		}
		label := entryLabel(px)
		if prev, ok := res[label]; !ok || betterProbe(r, prev) {
			res[label] = r
		}
	}

	return res
}

// ParseProxies is the exported wrapper over parseProxies so the checker can
// parse the survivor set once and share the proxies across the node-filter
// chain. The caller owns closing every returned proxy exactly once.
func (m *MihomoProber) ParseProxies(payload []byte) ([]mihomo.Proxy, error) {
	proxies, _, err := m.parseProxies(payload)

	return proxies, err
}

// parseProxies returns the parsed proxies and, index-aligned with them, whether
// each one's server is reachable by a plain TCP connect to Addr(). That verdict
// has to be taken here: adapter.ParseProxy consumes the raw mapping and the
// mihomo.Proxy it hands back exposes no transport, so below this loop the
// QUIC-only vless shape is indistinguishable from the TCP one.
func (m *MihomoProber) parseProxies(payload []byte) ([]mihomo.Proxy, []bool, error) {
	mappings, err := convert.ConvertsV2Ray(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("convert payload: %w", err)
	}

	proxies := make([]mihomo.Proxy, 0, len(mappings))
	tcpServer := make([]bool, 0, len(mappings))
	parseFailures := 0
	for _, mapping := range mappings {
		px, parseErr := adapter.ParseProxy(mapping)
		if parseErr != nil {
			parseFailures++

			continue
		}
		proxies = append(proxies, px)
		tcpServer = append(tcpServer, dialsServerOverTCP(px.Type(), mapping))
	}
	if parseFailures > 0 {
		m.logger.Warn().Int("count", parseFailures).Msg("skipped unparsable proxies")
	}
	if len(proxies) == 0 {
		return nil, nil, errors.New("no parsable proxies in payload")
	}

	return proxies, tcpServer, nil
}

// A node that cannot answer still burns check.timeout on every round, and the
// overwhelming majority of probed nodes fail, so pre-checking the TCP connect
// is the cheapest way to stop paying that twice.
const (
	// Retried once so a single dropped SYN cannot cost a live node its
	// dead-cache TTL. A resolver hiccup no longer rides on this: an
	// unresolvable name is failed open outright (see reachableTCP).
	precheckAttempts = 2
	// Bounded by the ROUTER's conntrack table, which is the binding cost here.
	// Measurement independence is real but secondary: this dial yields no bit
	// any gate reads, so check.concurrency — 16 in config against max_avg_ms
	// 800, 32 in config-vassago against 4000, each pinned by its own latency
	// measurement — has no claim on it.
	//
	// Measured 2.8 tracked flows per endpoint (one TCP flow when the first
	// attempt connects, two when it is black-holed, plus the A/AAAA pair for a
	// hostname). A production pool's ~8800 distinct endpoints is ~24700 flows,
	// created in the ~35s this phase takes at 128 and each held for
	// nf_conntrack's 120s SYN_SENT/TIME_WAIT default — 13x the ~1900 the
	// URL-test path alone creates. The phase is SHORTER than that timeout, so
	// peak occupancy is essentially everything created: ~24700 entries, which
	// assumes nf_conntrack_max 32768 and does NOT fit the 8192 an OpenWrt box
	// ships. This service runs beside one, and a full table drops the whole
	// LAN's traffic, not just this worker's.
	//
	// Kept at 128 because lowering it is not the lever: occupancy is
	// min(flows created, rate * 120s) and the rate is proportional to this
	// constant, so every value above ~37 peaks at the same ~24700, while the
	// ~12 that would fit 8192 stretches the phase to ~6min against a gross
	// saving of ~465s — below a break-even near 10 the pre-check costs more
	// than it saves.
	precheckConcurrency = 128
	// Above this share of the endpoints it JUDGED — refused plus reachable, the
	// fail-open ones excluded — the pre-check is judged unreliable rather than
	// believed. A healthy egress measured 58.9% condemned on the raw merged
	// pool, while the faults this guards — a local egress outage, a degenerate
	// check.timeout — refuse essentially EVERY endpoint they touch, so their
	// signature is ~100% and the 36-point margin costs no real verdict. DNS
	// down is no longer one of them: it leaves nothing judged, and an empty
	// judged set condemns nobody anyway.
	precheckBreakerPercent = 95
	// A share over a handful of judged endpoints carries no signal; production
	// cycles dial thousands.
	precheckBreakerMin = 100
)

// precheckDialBudget is the per-attempt dial deadline. Derived, never a
// constant: 500ms was 8x tighter than config-vassago's max_avg_ms of 4000, an
// instance deliberately tuned to admit slow nodes, so it deleted what that
// instance exists to keep.
//
// All precheckAttempts attempts share exactly ONE url-test round's budget,
// which is what makes the verdict safe by construction: a node that cannot
// finish a bare TCP handshake inside check.timeout cannot finish TCP, TLS,
// tunnel and GET inside it either. The budget binds on a black-holed endpoint
// and on a lookup that does not answer inside it — a refused SYN returns in
// microseconds — and only the first of those two condemns.
func (m *MihomoProber) precheckDialBudget() time.Duration {
	return m.cfg.Timeout / precheckAttempts
}

// PrecheckReport is the optional half of the Prober contract the checker reads
// after Probe (see stable.precheckReporter). A tripped breaker condemns nobody,
// so without this the metrics cannot tell it from a pre-check that ran clean.
func (m *MihomoProber) PrecheckReport() PrecheckReport {
	return m.precheck
}

// dialsServerOverTCP reports whether mihomo reaches this node's server with a
// plain TCP connect to Addr() — verified against mihomo v1.19.27, where each
// listed adapter dials dialer.DialContext(ctx, "tcp", Base.addr) and Addr()
// returns that same addr (vless.go:305 and :452 for the pair; anytls goes
// through transport/anytls/client.go:67, whose server anytls.go:108 builds from
// the same host and port as the Addr() of anytls.go:87).
//
// The list holds only types convert.ConvertsV2Ray can emit, so every entry
// checks against a scheme case in common/convert/converter.go. Ssh (ssh.go:63)
// and Snell (snell.go:97) TCP-dial Addr() too but have no scheme there, so an
// entry for either could never fire.
//
// Fail open, and not a guess: hysteria2, tuic and mieru reach their server with
// ListenPacket over UDP, and an unlisted type must lose the speedup rather than
// the node.
func dialsServerOverTCP(t mihomo.AdapterType, mapping map[string]any) bool {
	switch t { //nolint:exhaustive // the default is the point: every unlisted type, including one a future mihomo adds, keeps the URL test
	case mihomo.Vless, mihomo.Vmess, mihomo.Trojan, mihomo.Shadowsocks,
		mihomo.ShadowsocksR, mihomo.Socks5, mihomo.Http, mihomo.AnyTLS:
		return !dialsServerOverQUIC(mapping)
	default:
		return false
	}
}

// dialsServerOverQUIC reports whether the raw share-link mapping selects
// mihomo's xhttp-over-QUIC mode, whose only dial is ListenPacket over UDP:
// xhttp.NewTransport answers an *http3.Transport when len(alpn) == 1 &&
// alpn[0] == "h3" (transport/xhttp/client.go:160-168), and the TCP closure
// vless.go:585 hands it as dialRaw is then never invoked. Both keys come off
// the link itself (common/convert/v.go:71 sets network from ?type=, :36 sets
// alpn from ?alpn=).
//
// Of 14384 sampled vless nodes 1112 carry type=xhttp, 238 an alpn containing
// h3, and 5 an alpn of exactly [h3] — rare, not impossible, and the price of
// missing those 5 is not a lost speedup but a live node condemned unprobed and
// dead-cached for deadcache.ttl. So an alpn of an unexpected type answers yes:
// a mapping we cannot read is one we cannot prove dials TCP.
func dialsServerOverQUIC(mapping map[string]any) bool {
	if network, _ := mapping["network"].(string); network != "xhttp" {
		return false
	}
	alpn, present := mapping["alpn"]
	if !present {
		// An absent alpn is len 0, which fails mihomo's h3 test.
		return false
	}
	list, ok := alpn.([]string)
	if !ok {
		return true
	}

	return len(list) == 1 && list[0] == "h3"
}

// filterReachable splits the proxies on whether their server endpoint accepts a
// TCP connection, returning positions into proxies. It is its own phase so that
// none of its dials overlaps a measured URL test, which is also what frees it
// from check.concurrency.
//
// The bare net.Dialer is deliberate — warming mihomo's own DNS cache would
// shave the delay max_avg_ms gates on — and it costs a THIRD resolver in one
// cycle beside internal/resolver (TTL-cached, resolver.address-configurable)
// and mihomo's SystemResolver, neither of which this path honours. That
// asymmetry is why a failed LOOKUP fails open and only a refused or black-holed
// SYN condemns: mihomo's URL test dials through a stale-serving LRU
// (dns/system.go:69 builds SystemResolver over the cache dns/resolver.go:460
// creates WithStale(true)), at most one blocking lookup per host per PROCESS,
// while every attempt here pays the uncached net.DefaultResolver inside its own
// budget. The same name also resolved through internal/resolver earlier in this
// cycle — preprocess drops a node whose server resolves to nothing as DNSDrop
// before the merge — so a failure here, 5.2% of endpoints in the measured mix,
// is evidence about this path's resolver rather than about the node, and
// condemning on it would dead-cache a live node for a jittered [3h, 4.5h)
// against a 1h interval.
func (m *MihomoProber) filterReachable(
	ctx context.Context,
	opLog zerolog.Logger,
	proxies []mihomo.Proxy,
	tcpServer []bool,
) (live, condemned []int) {
	// One dial per distinct endpoint: a multi-port mierus:// link is several
	// proxies, but two proxies on one address ask one question.
	addrs := make([]string, 0, len(proxies))
	index := make(map[string]int, len(proxies))
	for i, px := range proxies {
		if !tcpServer[i] {
			continue
		}
		if _, ok := index[px.Addr()]; !ok {
			index[px.Addr()] = len(addrs)
			addrs = append(addrs, px.Addr())
		}
	}
	if len(addrs) == 0 {
		// Ran with nothing to dial, which Dialled 0 says; PrecheckAbsent would
		// claim there is no pre-check at all.
		m.precheck = PrecheckReport{State: PrecheckRan}

		return allPositions(len(proxies)), nil
	}

	verdicts := make([]precheckVerdict, len(addrs))
	sem := fanoutSem(precheckConcurrency)
	budget := m.precheckDialBudget()
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			verdicts[i] = reachableTCP(ctx, addr, budget)
		}()
	}
	wg.Wait()

	refused, unresolved := 0, 0
	for _, v := range verdicts {
		if v == verdictRefused {
			refused++
		}
		if v == verdictUnresolved {
			unresolved++
		}
	}
	// Condemning the pool writes the whole probed set into the dead cache for
	// deadcache.ttl — three cycles at the shipped 3h against a 1h interval —
	// and the pre-check now reaches that verdict in ~75s where a full probe
	// pass took ~20min, so an implausible verdict must fail open.
	//
	// The share is taken over the endpoints the pre-check actually JUDGED: a
	// resolver outage must not dilute the denominator until the breaker stops
	// firing.
	decided := len(addrs) - unresolved
	if decided >= precheckBreakerMin && refused*100 >= decided*precheckBreakerPercent {
		m.precheck = PrecheckReport{
			State: PrecheckTripped, Dialled: len(addrs), Refused: refused, Unresolved: unresolved,
		}
		opLog.Warn().Int("refused", refused).Int("decided", decided).
			Int("dialled", len(addrs)).Int("threshold_pct", precheckBreakerPercent).
			Msg("tcp pre-check refused nearly every endpoint it judged; treating it as unreliable and probing everything")

		return allPositions(len(proxies)), nil
	}
	m.precheck = PrecheckReport{
		State: PrecheckRan, Dialled: len(addrs), Refused: refused, Unresolved: unresolved,
	}

	live = make([]int, 0, len(proxies))
	for i, px := range proxies {
		// Re-tested rather than looked up by address alone: a proxy the
		// pre-check excludes may share server:port with one it dialled, and
		// Merge's dedupe key does not rule that out for a mierus:// port list.
		if !tcpServer[i] || verdicts[index[px.Addr()]] != verdictRefused {
			live = append(live, i)

			continue
		}
		condemned = append(condemned, i)
	}
	if len(condemned) > 0 {
		opLog.Info().Int("condemned", len(condemned)).Int("dialled", len(addrs)).
			Int("unresolved", unresolved).Int("probing", len(live)).
			Msg("tcp pre-check ruled out unreachable endpoints")
	}

	return live, condemned
}

func allPositions(n int) []int {
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}

	return all
}

// precheckVerdict is what one endpoint's dials proved. The zero value proves
// nothing, so a slot no dial reached fails open.
type precheckVerdict uint8

const (
	verdictUnresolved precheckVerdict = iota
	verdictRefused
	verdictReachable
)

// reachableTCP dials addr up to precheckAttempts times, the last attempt
// deciding. A *net.DNSError — which net.Dialer wraps in *net.OpError, and which
// net/lookup.go:358 also returns when the budget kills the lookup itself —
// means the name never became an address, so nothing was proved about the
// endpoint; every other dial error is a refused or black-holed SYN.
func reachableTCP(ctx context.Context, addr string, budget time.Duration) precheckVerdict {
	var d net.Dialer
	verdict := verdictRefused
	for range precheckAttempts {
		dctx, cancel := context.WithTimeout(ctx, budget)
		conn, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()

			return verdictReachable
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			verdict = verdictUnresolved
		} else {
			verdict = verdictRefused
		}
		if ctx.Err() != nil {
			// The cycle ran out, not the endpoint's patience.
			return verdictUnresolved
		}
	}

	return verdict
}

// probeStage tells a failure that never got a tunnel from one that failed the
// GET through it. Only those two, because mihomo cannot support a third: the
// vless adapter renders its dial error with %s and err.Error()
// (adapter/outbound/vless.go:312), destroying the chain errors.As would need to
// see whether the transport or the crypto failed, and vless dominates every
// pool this worker reads. trojan wraps with %w instead, so honouring that
// difference would classify the same failure per protocol. The pre-check
// separates the transport class structurally rather than by parsing errors.
func probeStage(err error) ProbeStage {
	var fetchErr *url.Error
	if errors.As(err, &fetchErr) {
		return StageFetch
	}

	return StageConnect
}

func (m *MihomoProber) runRound(
	ctx context.Context,
	opLog zerolog.Logger,
	prog *progress,
	proxies []mihomo.Proxy,
	live []int,
	sem chan struct{},
	mu *sync.Mutex,
	accs []delayAcc,
) {
	var wg sync.WaitGroup
	for _, i := range live {
		px := proxies[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			tctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
			defer cancel()

			delay, testErr := px.URLTest(tctx, m.cfg.TestURL, m.expected)
			n := prog.step()
			ev := opLog.Debug().Str("node", px.Name()).Int64("n", n).Int64("of", prog.total)
			stage := StagePassed
			if testErr != nil {
				stage = probeStage(testErr)
				ev.Err(testErr).Str("stage", stage.String()).Msg("url-test")
			} else {
				ev.Uint16("delay_ms", delay).Msg("url-test")
			}

			mu.Lock()
			defer mu.Unlock()
			a := &accs[i]
			if stage > a.stage {
				a.stage = stage
			}
			if testErr != nil {
				return
			}
			a.succ++
			a.sum += int32(delay)
		}()
	}
	wg.Wait()
}
