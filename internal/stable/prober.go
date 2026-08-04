package stable

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
	succ int
	sum  int
}

// betterProbe reports whether a is the result to keep when two proxies fold
// onto one label: more successful rounds first, lower mean latency as the
// tiebreak.
func betterProbe(a, b ProbeResult) bool {
	if a.Successes != b.Successes {
		return a.Successes > b.Successes
	}
	return a.MeanMs < b.MeanMs
}

// Probe parses the payload once and URL-tests every node for the configured
// number of rounds. The result map contains only nodes that succeeded at
// least once, keyed by entry label (see entryLabel).
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
	prog := newProgress(opLog, "url-test progress", m.cfg.Rounds*len(proxies))

	accs := make(map[string]*delayAcc, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	// One semaphore shared by every round so the effective number of in-flight
	// URL tests honors check.concurrency instead of rounds*concurrency.
	// fanoutSem, not a raw channel: runRound acquires on the producer
	// goroutine before any releaser exists, so a zero bound would deadlock.
	sem := fanoutSem(m.cfg.Concurrency)
	for range m.cfg.Rounds {
		wg.Go(func() {
			m.runRound(ctx, opLog, prog, proxies, sem, &mu, accs)
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
		if a == nil || a.succ == 0 {
			continue
		}
		r := ProbeResult{Successes: a.succ, MeanMs: a.sum / a.succ}
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
			if testErr != nil {
				ev.Err(testErr).Msg("url-test")
				return
			}
			ev.Uint16("delay_ms", delay).Msg("url-test")
			mu.Lock()
			defer mu.Unlock()
			a := accs[px.Name()]
			if a == nil {
				a = &delayAcc{}
				accs[px.Name()] = a
			}
			a.succ++
			a.sum += int(delay)
		}()
	}
	wg.Wait()
}
