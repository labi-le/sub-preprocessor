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
)

// Prober measures reachability of the proxy nodes in a subscription payload.
type Prober interface {
	Probe(ctx context.Context, payload []byte) (map[string]ProbeResult, error)
}

// MihomoProber runs repeated URL tests through mihomo's adapter stack.
type MihomoProber struct {
	cfg      config.CheckConfig
	expected utils.IntRanges[uint16]
	logger   zerolog.Logger
}

func NewMihomoProber(cfg config.CheckConfig, logger zerolog.Logger) (*MihomoProber, error) {
	expected, err := utils.NewUnsignedRanges[uint16](cfg.ExpectedStatus)
	if err != nil {
		return nil, fmt.Errorf("parse expected_status %q: %w", cfg.ExpectedStatus, err)
	}

	return &MihomoProber{cfg: cfg, expected: expected, logger: logger}, nil
}

type delayAcc struct {
	succ int
	sum  int
}

// Probe parses the payload once and URL-tests every node for the configured
// number of rounds. The result map contains only nodes that succeeded at
// least once, keyed by node name.
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

	accs := make(map[string]*delayAcc, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range m.cfg.Rounds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.runRound(ctx, proxies, &mu, accs)
		}()
	}
	wg.Wait()

	res := make(map[string]ProbeResult, len(accs))
	for name, a := range accs {
		if a.succ == 0 {
			continue
		}
		res[name] = ProbeResult{Successes: a.succ, MeanMs: a.sum / a.succ}
	}

	return res, nil
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
	proxies []mihomo.Proxy,
	mu *sync.Mutex,
	accs map[string]*delayAcc,
) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, m.cfg.Concurrency)
	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			tctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
			defer cancel()

			delay, testErr := px.URLTest(tctx, m.cfg.TestURL, m.expected)
			if testErr != nil {
				return
			}
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
