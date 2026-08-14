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
	succ  int
	sum   int
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
	proxies, err := m.parseProxies(payload)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	opLog := log.Op(m.logger, "stable.Probe")
	live, condemned := m.filterReachable(ctx, opLog, proxies)
	prog := newProgress(opLog, "url-test progress", m.cfg.Rounds*len(live))

	accs := make(map[string]*delayAcc, len(proxies))
	// Seeded before any round starts, which is the only time accs is safe to
	// touch without mu.
	for _, px := range condemned {
		accs[px.Name()] = &delayAcc{stage: StageCondemned}
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
			m.runRound(ctx, opLog, prog, live, sem, &mu, accs)
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
// Iterating proxies rather than accs is what makes a tie resolve in payload
// order instead of by map-iteration chance.
func foldProbeResults(proxies []mihomo.Proxy, accs map[string]*delayAcc) map[string]ProbeResult {
	res := make(map[string]ProbeResult, len(accs))
	for _, px := range proxies {
		a := accs[px.Name()]
		if a == nil {
			continue
		}
		r := ProbeResult{Successes: a.succ, Stage: a.stage}
		if a.succ > 0 {
			r.MeanMs = a.sum / a.succ
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
	return m.parseProxies(payload)
}

func (m *MihomoProber) parseProxies(payload []byte) ([]mihomo.Proxy, error) {
	mappings, err := convert.ConvertsV2Ray(payload)
	if err != nil {
		return nil, fmt.Errorf("convert payload: %w", err)
	}

	proxies := make([]mihomo.Proxy, 0, len(mappings))
	parseFailures := 0
	for _, mapping := range mappings {
		px, parseErr := adapter.ParseProxy(mapping)
		if parseErr != nil {
			parseFailures++

			continue
		}
		proxies = append(proxies, px)
	}
	if parseFailures > 0 {
		m.logger.Warn().Int("count", parseFailures).Msg("skipped unparsable proxies")
	}
	if len(proxies) == 0 {
		return nil, errors.New("no parsable proxies in payload")
	}

	return proxies, nil
}

// A node that cannot answer still burns check.timeout on every round, and the
// overwhelming majority of probed nodes fail, so one TCP connect ahead of the
// URL test is the cheapest way to stop paying that twice.
const (
	precheckTimeout = 500 * time.Millisecond
	// Retried once, because a single dropped SYN must not cost a live node its
	// dead-cache TTL.
	precheckAttempts = 2
	// Deliberately not check.concurrency: that 16 is pinned because URLTest
	// reports the wall-clock latency max_avg_ms gates on, and this yields one
	// bit no gate reads.
	precheckConcurrency = 128
)

// dialsServerOverTCP reports whether mihomo reaches this adapter's server with
// a plain TCP connect to Addr() — verified against mihomo v1.19.27, where each
// listed adapter dials dialer.DialContext(ctx, "tcp", Base.addr) and Addr()
// returns that same addr (vless.go:305 and :452 for the pair).
//
// The default is fail-open, not a guess: hysteria2, tuic and mieru reach their
// server with ListenPacket over UDP, so a TCP verdict would delete those
// protocols wholesale. An unknown type must lose the speedup, never the node.
func dialsServerOverTCP(t mihomo.AdapterType) bool {
	switch t { //nolint:exhaustive // the default is the point: every unlisted type, including one a future mihomo adds, keeps the URL test
	case mihomo.Vless, mihomo.Vmess, mihomo.Trojan, mihomo.Shadowsocks,
		mihomo.ShadowsocksR, mihomo.Socks5, mihomo.Http, mihomo.Snell:
		return true
	default:
		return false
	}
}

// filterReachable splits the proxies on whether their server endpoint accepts
// a TCP connection. It is its own phase so that none of its dials overlaps a
// measured URL test, which is also what frees it from check.concurrency.
func (m *MihomoProber) filterReachable(
	ctx context.Context,
	opLog zerolog.Logger,
	proxies []mihomo.Proxy,
) (live, condemned []mihomo.Proxy) {
	// One dial per distinct endpoint: a multi-port mierus:// link is several
	// proxies, but two proxies on one address ask one question.
	addrs := make([]string, 0, len(proxies))
	index := make(map[string]int, len(proxies))
	for _, px := range proxies {
		if !dialsServerOverTCP(px.Type()) {
			continue
		}
		if _, ok := index[px.Addr()]; !ok {
			index[px.Addr()] = len(addrs)
			addrs = append(addrs, px.Addr())
		}
	}
	if len(addrs) == 0 {
		return proxies, nil
	}

	reachable := make([]bool, len(addrs))
	sem := fanoutSem(precheckConcurrency)
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reachable[i] = reachableTCP(ctx, addr)
		}()
	}
	wg.Wait()

	live = make([]mihomo.Proxy, 0, len(proxies))
	for _, px := range proxies {
		// Re-tested rather than looked up by address alone: a UDP-dialling
		// proxy may share server:port with a TCP one, and Merge's dedupe key
		// does not rule that out for a mierus:// port list.
		if !dialsServerOverTCP(px.Type()) || reachable[index[px.Addr()]] {
			live = append(live, px)

			continue
		}
		condemned = append(condemned, px)
	}
	if len(condemned) > 0 {
		opLog.Info().Int("condemned", len(condemned)).Int("dialled", len(addrs)).
			Int("probing", len(live)).Msg("tcp pre-check ruled out unreachable endpoints")
	}

	return live, condemned
}

func reachableTCP(ctx context.Context, addr string) bool {
	var d net.Dialer
	for range precheckAttempts {
		dctx, cancel := context.WithTimeout(ctx, precheckTimeout)
		conn, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()

			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}

	return false
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
	sem chan struct{},
	mu *sync.Mutex,
	accs map[string]*delayAcc,
) {
	var wg sync.WaitGroup
	for _, px := range proxies {
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
			a := accs[px.Name()]
			if a == nil {
				a = &delayAcc{}
				accs[px.Name()] = a
			}
			if stage > a.stage {
				a.stage = stage
			}
			if testErr != nil {
				return
			}
			a.succ++
			a.sum += int(delay)
		}()
	}
	wg.Wait()
}
