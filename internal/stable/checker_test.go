package stable_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/stable"
)

type fakeFilterer struct {
	bodies map[fetch.SubscriptionURL]string
}

func (f fakeFilterer) Filter(_ context.Context, b *bytes.Buffer, req preprocess.FilterRequest) (preprocess.Stats, error) {
	body, ok := f.bodies[req.SubscriptionURL]
	if !ok {
		return preprocess.Stats{}, errors.New("source unavailable")
	}
	b.WriteString(body)

	return preprocess.Stats{}, nil
}

type fakeProber struct {
	res        map[string]stable.ProbeResult
	err        error
	gotPayload []byte
}

func (p *fakeProber) Probe(_ context.Context, payload []byte) (map[string]stable.ProbeResult, error) {
	p.gotPayload = append([]byte(nil), payload...)
	if p.err != nil {
		return nil, p.err
	}

	return p.res, nil
}

func (p *fakeProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

func testSources() []config.SubscriptionSource {
	return []config.SubscriptionSource{
		{Name: "alpha", URL: "https://alpha.example/sub"},
		{Name: "beta", URL: "https://beta.example/sub"},
	}
}

func newTestChecker(filterer stable.Filterer, prober stable.Prober, holder *stable.Holder) *stable.Checker {
	return stable.NewChecker(
		testSources(),
		filter.All(),
		time.Hour,
		5, 0, 1000,
		time.Minute,
		func() stable.Filterer { return filterer },
		prober,
		nil,
		nil,
		holder,
		zerolog.Nop(),
	)
}

func TestCheckerStoresSnapshot(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443?x=1#orig\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#z\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 300},
		"beta-001":  {Successes: 5, MeanMs: 100},
	}}
	holder := stable.NewHolder()

	_ = newTestChecker(filterer, prober, holder).RunOnce(context.Background())

	snap := holder.Load()
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	wantPayload := "vless://u@2.2.2.2:443#beta-001\nvless://u@1.1.1.1:443?x=1#alpha-001\n"
	if got := string(snap.Payload); got != wantPayload {
		t.Errorf("payload:\ngot  %q\nwant %q", got, wantPayload)
	}
	wantStats := stable.Stats{SourcesOK: 2, SourcesTotal: 2, Merged: 2, Tested: 2, Kept: 2}
	if snap.Stats != wantStats {
		t.Errorf("stats: got %+v want %+v", snap.Stats, wantStats)
	}
	if snap.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
	wantProbed := "vless://u@1.1.1.1:443?x=1#alpha-001\nvless://u@2.2.2.2:443#beta-001\n"
	wantProbedAlt := "vless://u@2.2.2.2:443#beta-001\nvless://u@1.1.1.1:443?x=1#alpha-001\n"
	if got := string(prober.gotPayload); got != wantProbed && got != wantProbedAlt {
		t.Errorf("probed payload:\ngot  %q\nwant %q or %q", string(prober.gotPayload), wantProbed, wantProbedAlt)
	}
}

func TestCheckerPartialSourceFailure(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://beta.example/sub": "vless://u@2.2.2.2:443#z\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"beta-001": {Successes: 5, MeanMs: 100},
	}}
	holder := stable.NewHolder()

	_ = newTestChecker(filterer, prober, holder).RunOnce(context.Background())

	snap := holder.Load()
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	wantStats := stable.Stats{SourcesOK: 1, SourcesTotal: 2, Merged: 1, Tested: 1, Kept: 1}
	if snap.Stats != wantStats {
		t.Errorf("stats: got %+v want %+v", snap.Stats, wantStats)
	}
}

func TestCheckerAllSourcesFailKeepsHolder(t *testing.T) {
	t.Parallel()

	prober := &fakeProber{}
	holder := stable.NewHolder()

	_ = newTestChecker(fakeFilterer{}, prober, holder).RunOnce(context.Background())

	if holder.Load() != nil {
		t.Error("expected nil snapshot after all sources failed")
	}
	if prober.gotPayload != nil {
		t.Error("prober must not run when no entries merged")
	}
}

func TestCheckerZeroSurvivorsKeepsPrevious(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{}}
	holder := stable.NewHolder()
	previous := &stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()}
	holder.Store(previous)

	_ = newTestChecker(filterer, prober, holder).RunOnce(context.Background())

	if holder.Load() != previous {
		t.Error("expected previous snapshot to be kept")
	}
}

func TestCheckerProberErrorKeepsHolder(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	prober := &fakeProber{err: errors.New("probe blew up")}
	holder := stable.NewHolder()

	_ = newTestChecker(filterer, prober, holder).RunOnce(context.Background())

	if holder.Load() != nil {
		t.Error("expected nil snapshot after prober error")
	}
}

func TestCheckerRunStopsOnCancel(t *testing.T) {
	t.Parallel()

	holder := stable.NewHolder()
	checker := newTestChecker(fakeFilterer{}, &fakeProber{}, holder)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		checker.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}

func TestControllerApplyAndStop(t *testing.T) {
	t.Parallel()

	holder := stable.NewHolder()
	ctl := stable.NewController(
		context.Background(),
		holder,
		func() stable.Filterer { return fakeFilterer{} },
		nil,
		nil,
		zerolog.Nop(),
	)

	disabled := config.Config{}
	if err := ctl.Apply(disabled); err != nil {
		t.Fatalf("Apply(disabled): %v", err)
	}
	ctl.Stop()

	enabled := config.Config{
		Groups: config.Groups{"geo_blocked": {"RU", "CN"}},
		Filters: []config.FilterConfig{{
			Type:             config.FilterCountry,
			Provider:         config.ProviderGeofeed,
			ExcludeCountries: []string{"IR"},
			ExcludeGroups:    []string{"geo_blocked"},
		}},
		Subscriptions: config.SubscriptionsConfig{
			Interval: time.Hour,
			Check: config.CheckConfig{
				Rounds:         1,
				Timeout:        time.Second,
				TestURL:        "https://www.gstatic.com/generate_204",
				ExpectedStatus: "204",
				MaxFail:        0,
				MaxAvgMs:       1000,
				Concurrency:    1,
			},
			Sources: []config.SubscriptionSource{{Name: "alpha", URL: "https://alpha.example/sub"}},
		},
	}
	if err := ctl.Apply(enabled); err != nil {
		t.Fatalf("Apply(enabled): %v", err)
	}
	ctl.Stop()
	ctl.Stop() // idempotent
}

func TestControllerApplyRejectsBadExpectedStatus(t *testing.T) {
	t.Parallel()

	ctl := stable.NewController(
		context.Background(),
		stable.NewHolder(),
		func() stable.Filterer { return fakeFilterer{} },
		nil,
		nil,
		zerolog.Nop(),
	)

	cfg := config.Config{
		Subscriptions: config.SubscriptionsConfig{
			Interval: time.Hour,
			Check: config.CheckConfig{
				Rounds:         1,
				Timeout:        time.Second,
				TestURL:        "https://www.gstatic.com/generate_204",
				ExpectedStatus: "not-a-range",
				MaxAvgMs:       1000,
				Concurrency:    1,
			},
			Sources: []config.SubscriptionSource{{Name: "alpha", URL: "https://alpha.example/sub"}},
		},
	}
	if err := ctl.Apply(cfg); err == nil {
		ctl.Stop()
		t.Fatal("expected error for bad expected_status")
	}
}

type fakeDeadCache struct {
	blocked  map[string]bool
	recorded []string
}

func (d *fakeDeadCache) Blocked(key string) bool { return d.blocked[key] }
func (d *fakeDeadCache) Block(key string) error {
	d.recorded = append(d.recorded, key)
	d.blocked[key] = true
	return nil
}
func (d *fakeDeadCache) Prune() error { return nil }

func TestCheckerDeadCacheSkipsAndRecords(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	// alpha-001 is dead (absent from probe results); beta-001 is alive.
	prober := &fakeProber{res: map[string]stable.ProbeResult{"beta-001": {Successes: 5, MeanMs: 100}}}
	dead := &fakeDeadCache{blocked: map[string]bool{}}
	holder := stable.NewHolder()
	c := stable.NewChecker(
		testSources(), filter.All(), time.Hour, 5, 0, 1000, time.Minute,
		func() stable.Filterer { return filterer }, prober, nil, dead, holder, zerolog.Nop(),
	)

	// Cycle 1: both nodes probed; alpha fails -> recorded dead.
	_ = c.RunOnce(context.Background())
	if !dead.blocked["1.1.1.1:443"] {
		t.Fatalf("dead alpha should be recorded, got %v", dead.recorded)
	}
	if !strings.Contains(string(prober.gotPayload), "1.1.1.1:443") {
		t.Fatal("cycle 1 should have probed alpha")
	}

	// Cycle 2: alpha is now known-dead -> skipped before probing; beta still probed.
	prober.gotPayload = nil
	_ = c.RunOnce(context.Background())
	if strings.Contains(string(prober.gotPayload), "1.1.1.1:443") {
		t.Errorf("cycle 2 must skip known-dead alpha, probed %q", prober.gotPayload)
	}
	if !strings.Contains(string(prober.gotPayload), "2.2.2.2:443") {
		t.Errorf("cycle 2 must still probe beta, probed %q", prober.gotPayload)
	}
}

// cancellingProber returns results but cancels the cycle context first,
// simulating a shutdown racing the end of a probe.
type cancellingProber struct {
	cancel context.CancelFunc
	res    map[string]stable.ProbeResult
}

func (p *cancellingProber) Probe(context.Context, []byte) (map[string]stable.ProbeResult, error) {
	p.cancel()
	return p.res, nil
}

func (p *cancellingProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

func TestCheckerProbeErrorKeepsSnapshotAndDeadCache(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	prober := &fakeProber{err: context.Canceled}
	dead := &fakeDeadCache{blocked: map[string]bool{}}
	holder := stable.NewHolder()
	previous := &stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()}
	holder.Store(previous)

	c := stable.NewChecker(
		testSources(), filter.All(), time.Hour, 5, 0, 1000, time.Minute,
		func() stable.Filterer { return filterer }, prober, nil, dead, holder, zerolog.Nop(),
	)
	err := c.RunOnce(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce must return the probe error, got %v", err)
	}
	if holder.Load() != previous {
		t.Error("previous snapshot must be kept after a probe error")
	}
	if len(dead.recorded) != 0 {
		t.Errorf("dead cache must not be written after a probe error, recorded %v", dead.recorded)
	}
}

func TestCheckerCancelAfterProbeSkipsWrites(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// beta absent from results: without the ctx gate it would be recorded dead.
	prober := &cancellingProber{cancel: cancel, res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 100},
	}}
	dead := &fakeDeadCache{blocked: map[string]bool{}}
	holder := stable.NewHolder()
	previous := &stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()}
	holder.Store(previous)

	c := stable.NewChecker(
		testSources(), filter.All(), time.Hour, 5, 0, 1000, time.Minute,
		func() stable.Filterer { return filterer }, prober, nil, dead, holder, zerolog.Nop(),
	)
	err := c.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce must surface the cancellation, got %v", err)
	}
	if holder.Load() != previous {
		t.Error("previous snapshot must be kept after cancellation")
	}
	if len(dead.recorded) != 0 {
		t.Errorf("dead cache must not be written after cancellation, recorded %v", dead.recorded)
	}
}

// slowFilterer delays configured sources to force out-of-config-order
// completion.
type slowFilterer struct {
	fakeFilterer
	delays map[fetch.SubscriptionURL]time.Duration
}

func (f slowFilterer) Filter(ctx context.Context, b *bytes.Buffer, req preprocess.FilterRequest) (preprocess.Stats, error) {
	time.Sleep(f.delays[req.SubscriptionURL])
	return f.fakeFilterer.Filter(ctx, b, req)
}

func TestCheckerMergeOrderIgnoresFetchCompletion(t *testing.T) {
	t.Parallel()

	// Both sources carry the same host. First-source-wins must follow config
	// order (alpha), even though alpha finishes last.
	filterer := slowFilterer{
		fakeFilterer: fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
			"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
			"https://beta.example/sub":  "vless://u@1.1.1.1:443#b\n",
		}},
		delays: map[fetch.SubscriptionURL]time.Duration{
			"https://alpha.example/sub": 150 * time.Millisecond,
		},
	}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 100},
	}}
	holder := stable.NewHolder()

	if err := newTestChecker(filterer, prober, holder).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	snap := holder.Load()
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if got, want := string(snap.Payload), "vless://u@1.1.1.1:443#alpha-001\n"; got != want {
		t.Errorf("payload:\ngot  %q\nwant %q (first configured source must win)", got, want)
	}
}
