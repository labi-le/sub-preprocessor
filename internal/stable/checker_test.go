package stable_test

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/stable"
)

type fakeFilterer struct {
	bodies map[fetch.SubscriptionURL]string
	// ip is the address the IP stage judged, carried onto every node it yields
	// — the worker never resolves one itself.
	ip  netip.Addr
	ann preprocess.Annotator
}

func (f fakeFilterer) FilterNodes(
	_ context.Context, req preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	body, ok := f.bodies[req.SubscriptionURL]
	if !ok {
		return nil, preprocess.Stats{}, errors.New("source unavailable")
	}

	nodes := sourceBody("", body).Nodes
	for i := range nodes {
		nodes[i].IP = f.ip
	}

	return nodes, preprocess.Stats{}, nil
}

//nolint:ireturn // implements stable.Filterer; handing out the interface is the point
func (f fakeFilterer) Annotator() preprocess.Annotator { return f.ann }

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

func testCheckerSpec(prober stable.Prober) stable.CheckerSpec {
	return stable.CheckerSpec{
		Sources:       testSources(),
		Interval:      time.Hour,
		Rounds:        5,
		MaxFail:       0,
		MaxAvgMs:      1000,
		SourceTimeout: time.Minute,
		Prober:        prober,
	}
}

func newTestChecker(filterer stable.Filterer, prober stable.Prober, holder *stable.Holder) *stable.Checker {
	return stable.NewChecker(
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer },
		nil,
		nil,
		holder,
		"",
		zerolog.Nop(),
		nil,
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
		"",
		zerolog.Nop(),
		nil,
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
		"",
		zerolog.Nop(),
		nil,
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
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer }, nil, dead, holder, "", zerolog.Nop(), nil,
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

// fakeBlocklist mirrors fakeDeadCache for the persistent side: it counts the
// calls the Checker makes so the prune cadence is observable.
type fakeBlocklist struct {
	blocked []string
	prunes  int
}

func (b *fakeBlocklist) Block(host string) error {
	b.blocked = append(b.blocked, host)
	return nil
}
func (b *fakeBlocklist) Prune() error { b.prunes++; return nil }

// TestCheckerPrunesBlocklistEveryCycle: the geoblock store is the only TTL
// store the worker writes to that outlives the process, and it used to be
// swept exactly once — inside geoblock.Open. On a container that restarts
// monthly at best, every host ever refused stayed in the map and the table
// long after its expiry. The dead cache has always been pruned per cycle; this
// pins the same cadence for the store, which is why Blocklist now has Prune.
func TestCheckerPrunesBlocklistEveryCycle(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 100},
		"beta-001":  {Successes: 5, MeanMs: 100},
	}}
	store := &fakeBlocklist{}
	c := stable.NewChecker(
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer }, store, nil, stable.NewHolder(), "", zerolog.Nop(), nil,
	)

	for cycle := 1; cycle <= 2; cycle++ {
		if err := c.RunOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if store.prunes != cycle {
			t.Fatalf("after cycle %d the store was pruned %d time(s), want %d", cycle, store.prunes, cycle)
		}
	}
	if len(store.blocked) != 0 {
		t.Fatalf("the checker must never write to the store itself, got %v", store.blocked)
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
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer }, nil, dead, holder, "", zerolog.Nop(), nil,
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
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer }, nil, dead, holder, "", zerolog.Nop(), nil,
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

func (f slowFilterer) FilterNodes(
	ctx context.Context, req preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	time.Sleep(f.delays[req.SubscriptionURL])

	return f.fakeFilterer.FilterNodes(ctx, req)
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

type fakeReporter struct {
	last *stable.CycleReport
	errs int
}

func (r *fakeReporter) Observe(c stable.CycleReport) { r.last = &c }
func (r *fakeReporter) ObserveError()                { r.errs++ }

func TestCheckerReportsPublishedCycle(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 100},
		"beta-001":  {Successes: 5, MeanMs: 100},
	}}
	holder := stable.NewHolder()
	rep := &fakeReporter{}
	c := stable.NewChecker(
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer }, nil, nil, holder, "", zerolog.Nop(), rep,
	)
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.last == nil {
		t.Fatal("reporter.Observe must fire on a published cycle")
	}
	if rep.last.Kept != 2 {
		t.Errorf("report Kept = %d, want 2", rep.last.Kept)
	}
	if rep.last.SourcesTotal != len(testSources()) {
		t.Errorf("report SourcesTotal = %d, want %d", rep.last.SourcesTotal, len(testSources()))
	}
}

// blockingProber parks inside Probe until released, so a Reconfigure can be
// timed to land while a cycle is in flight.
type blockingProber struct {
	entered chan struct{}
	release chan struct{}
	res     map[string]stable.ProbeResult
}

func (p *blockingProber) Probe(ctx context.Context, _ []byte) (map[string]stable.ProbeResult, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return p.res, nil
}

func (p *blockingProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

// TestReconfigureLeavesCycleInFlightIntact: RunOnce reads its spec once at the
// start, so a reload landing mid-cycle configures the NEXT cycle instead of
// half-rewriting this one. Without the snapshot a cycle could publish stats
// counted against one source list while its nodes came from another.
func TestReconfigureLeavesCycleInFlightIntact(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#orig\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#z\n",
	}}
	prober := &blockingProber{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		res: map[string]stable.ProbeResult{
			"alpha-001": {Successes: 5, MeanMs: 100},
			"beta-001":  {Successes: 5, MeanMs: 100},
		},
	}
	holder := stable.NewHolder()
	c := newTestChecker(filterer, prober, holder)

	done := make(chan error, 1)
	go func() { done <- c.RunOnce(context.Background()) }()

	<-prober.entered
	// Reload arrives mid-probe, dropping a source.
	spec := testCheckerSpec(prober)
	spec.Sources = spec.Sources[:1]
	c.Reconfigure(spec)
	close(prober.release)

	if err := <-done; err != nil {
		t.Fatalf("cycle must complete despite the reload: %v", err)
	}
	snap := holder.Load()
	if snap == nil {
		t.Fatal("cycle must still publish after a mid-flight reload")
	}
	if snap.Stats.SourcesTotal != 2 {
		t.Fatalf("SourcesTotal = %d, want 2: the cycle must report the spec it started with", snap.Stats.SourcesTotal)
	}
}

// countingProber signals every Probe call so a test can wait for the Nth cycle.
type countingProber struct {
	calls chan struct{}
	res   map[string]stable.ProbeResult
}

func (p *countingProber) Probe(context.Context, []byte) (map[string]stable.ProbeResult, error) {
	select {
	case p.calls <- struct{}{}:
	default:
	}

	return p.res, nil
}

func (p *countingProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

// TestReconfigureAppliesIntervalWhileIdle: an interval change landing between
// cycles must take effect immediately. Run re-reads the interval only after a
// wakeup, so without Reconfigure poking the loop a shortened interval would sit
// unapplied until the OLD period elapsed and one more cycle finished.
func TestReconfigureAppliesIntervalWhileIdle(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#orig\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#z\n",
	}}
	prober := &countingProber{
		calls: make(chan struct{}, 8),
		res: map[string]stable.ProbeResult{
			"alpha-001": {Successes: 5, MeanMs: 100},
			"beta-001":  {Successes: 5, MeanMs: 100},
		},
	}
	spec := testCheckerSpec(prober)
	spec.Interval = time.Hour
	c := stable.NewChecker(spec, func() stable.Filterer { return filterer },
		nil, nil, stable.NewHolder(), "", zerolog.Nop(), nil)

	go c.Run(t.Context()) // cancelled on test cleanup, which stops the loop

	select {
	case <-prober.calls: // immediate first cycle
	case <-time.After(5 * time.Second):
		t.Fatal("first cycle never ran")
	}

	shortened := testCheckerSpec(prober)
	shortened.Interval = 10 * time.Millisecond
	c.Reconfigure(shortened)

	select {
	case <-prober.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("shortened interval was not applied while the worker sat idle")
	}
}

// tracingProber is fakeProber that also answers the egress trace, which is
// what turns the trace stage on: the flag alone is not enough, the prober has
// to be able to run it.
type tracingProber struct {
	fakeProber
	trace map[string]stable.TraceResult
}

func (p *tracingProber) TraceCheck(context.Context, []mihomo.Proxy) map[string]stable.TraceResult {
	return p.trace
}

// TestCheckerAnnotatesBeforeCountingCountries locks the ordering the cycle
// depends on: Survivor.Country exists only once the publication has run the
// GEO chain, and the kept-country and geo-unknown gauges read that field. Build
// the payload after observing and every cycle reports zero countries while
// publishing tagged nodes.
//
// It also pins the address split end to end: the traced node is described by
// the egress it reported, its untraced neighbour by the address the IP stage
// judged, and the trace report counts the one country that actually moved.
func TestCheckerAnnotatesBeforeCountingCountries(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{
		bodies: map[fetch.SubscriptionURL]string{
			"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
			"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
		},
		ip:  addr(t, "9.9.9.9"),
		ann: tagAnnotator{offline: country(t, "NL")},
	}
	prober := &tracingProber{
		fakeProber: fakeProber{res: map[string]stable.ProbeResult{
			"alpha-001": {Successes: 5, MeanMs: 100},
			"beta-001":  {Successes: 5, MeanMs: 200},
		}},
		trace: map[string]stable.TraceResult{
			"alpha-001": {IP: addr(t, "5.6.7.8"), Country: country(t, "DE")},
		},
	}
	spec := testCheckerSpec(prober)
	spec.Trace = true
	holder := stable.NewHolder()
	rep := &fakeReporter{}
	c := stable.NewChecker(spec, func() stable.Filterer { return filterer }, nil, nil, holder, "", zerolog.Nop(), rep)

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	snap := holder.Load()
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	want := "vless://u@1.1.1.1:443#[GEO:DE][IP:5.6.7.8] alpha-001\n" +
		"vless://u@2.2.2.2:443#[GEO:NL][IP:9.9.9.9] beta-001\n"
	if got := string(snap.Payload); got != want {
		t.Errorf("payload:\ngot  %q\nwant %q", got, want)
	}
	if rep.last == nil {
		t.Fatal("reporter.Observe must fire on a published cycle")
	}
	if got := rep.last.KeptCountries; got["DE"] != 1 || got["NL"] != 1 || len(got) != 2 {
		t.Errorf("KeptCountries = %v, want one DE and one NL", got)
	}
	if rep.last.GeoUnknown != 0 {
		t.Errorf("GeoUnknown = %d, want 0", rep.last.GeoUnknown)
	}
	wantTrace := stable.TraceReport{Answered: 1, Unanswered: 1, Moved: 1}
	if rep.last.Trace != wantTrace {
		t.Errorf("Trace = %+v, want %+v", rep.last.Trace, wantTrace)
	}
}

// A trace that agrees with the offline chain is answered but not MOVED: the
// number the trace exists to justify counts corrections, not round trips.
func TestCheckerTraceAgreeingWithOfflineChainIsNotMoved(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{
		bodies: map[fetch.SubscriptionURL]string{
			"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		},
		ip:  addr(t, "9.9.9.9"),
		ann: tagAnnotator{offline: country(t, "DE")},
	}
	prober := &tracingProber{
		fakeProber: fakeProber{res: map[string]stable.ProbeResult{"alpha-001": {Successes: 5, MeanMs: 100}}},
		trace: map[string]stable.TraceResult{
			"alpha-001": {IP: addr(t, "5.6.7.8"), Country: country(t, "DE")},
		},
	}
	spec := testCheckerSpec(prober)
	spec.Trace = true
	rep := &fakeReporter{}
	c := stable.NewChecker(spec, func() stable.Filterer { return filterer }, nil, nil, stable.NewHolder(), "", zerolog.Nop(), rep)

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.last == nil {
		t.Fatal("reporter.Observe must fire on a published cycle")
	}
	wantTrace := stable.TraceReport{Answered: 1, Unanswered: 0, Moved: 0}
	if rep.last.Trace != wantTrace {
		t.Errorf("Trace = %+v, want %+v", rep.last.Trace, wantTrace)
	}
}

// A prober that cannot trace leaves the stage off entirely rather than
// reporting every survivor unanswered — the config asked for an annotation the
// build could not provide, and the cycle says so with a warning, not a metric.
func TestCheckerTraceSkippedWithoutProberSupport(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{
		bodies: map[fetch.SubscriptionURL]string{
			"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		},
		ip:  addr(t, "9.9.9.9"),
		ann: tagAnnotator{offline: country(t, "NL")},
	}
	spec := testCheckerSpec(&fakeProber{res: map[string]stable.ProbeResult{"alpha-001": {Successes: 5, MeanMs: 100}}})
	spec.Trace = true
	rep := &fakeReporter{}
	c := stable.NewChecker(spec, func() stable.Filterer { return filterer }, nil, nil, stable.NewHolder(), "", zerolog.Nop(), rep)

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.last == nil {
		t.Fatal("reporter.Observe must fire on a published cycle")
	}
	if rep.last.Trace != (stable.TraceReport{}) {
		t.Errorf("Trace = %+v, want the zero report", rep.last.Trace)
	}
}

// lockedBuffer collects log output from the cycle's concurrent per-source
// goroutines; bytes.Buffer alone is not safe for that and -race says so.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// publishingChecker is the two-source cycle every test below publishes from,
// wired to snapshotPath. Its outcome is fixed so the assertions can be about
// persistence alone.
func publishingChecker(snapshotPath string, logger zerolog.Logger, holder *stable.Holder) *stable.Checker {
	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#orig\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#z\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 300},
		"beta-001":  {Successes: 5, MeanMs: 100},
	}}

	return stable.NewChecker(
		testCheckerSpec(prober), func() stable.Filterer { return filterer },
		nil, nil, holder, snapshotPath, logger, nil,
	)
}

// TestCheckerPersistsPublishedSnapshot: the cycle that publishes must also
// leave the list on disk, or the next restart serves 503 for a whole cycle.
func TestCheckerPersistsPublishedSnapshot(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stable.json")
	holder := stable.NewHolder()

	if err := publishingChecker(path, zerolog.Nop(), holder).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	published := holder.Load()
	if published == nil {
		t.Fatal("the cycle published nothing, so this test proves nothing about persistence")
	}
	restored := stable.LoadSnapshot(path, zerolog.Nop())
	if restored == nil {
		t.Fatal("the published cycle wrote no snapshot; a restart would answer 503 until the next cycle")
	}
	if string(restored.Payload) != string(published.Payload) {
		t.Errorf("persisted payload:\ngot  %q\nwant %q", restored.Payload, published.Payload)
	}
	if restored.Stats != published.Stats {
		t.Errorf("persisted stats: got %+v want %+v", restored.Stats, published.Stats)
	}
	if !restored.UpdatedAt.Equal(published.UpdatedAt) {
		t.Errorf("persisted updated_at: got %v want %v", restored.UpdatedAt, published.UpdatedAt)
	}
}

// TestCheckerSurvivesSnapshotWriteFailure: persistence is a side effect, never
// a gate. An unwritable path costs the next restart its head start and must
// leave the cycle successful and the in-memory publication untouched.
func TestCheckerSurvivesSnapshotWriteFailure(t *testing.T) {
	t.Parallel()

	// A directory component that is not a directory: creating the sibling temp
	// file fails with ENOTDIR on every attempt.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logBuf lockedBuffer
	holder := stable.NewHolder()

	err := publishingChecker(filepath.Join(blocker, "stable.json"), zerolog.New(&logBuf), holder).
		RunOnce(context.Background())

	if err != nil {
		t.Fatalf("a snapshot that cannot be written must not fail the cycle: %v", err)
	}
	snap := holder.Load()
	if snap == nil {
		t.Fatal("the in-memory publication must survive a failed snapshot write")
	}
	want := "vless://u@2.2.2.2:443#beta-001\nvless://u@1.1.1.1:443#alpha-001\n"
	if got := string(snap.Payload); got != want {
		t.Errorf("published payload:\ngot  %q\nwant %q", got, want)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `"level":"warn"`) || !strings.Contains(logs, "persisting the stable snapshot failed") {
		t.Errorf("a failed snapshot write must warn, got:\n%s", logs)
	}
	if !strings.Contains(logs, "stable list updated") {
		t.Errorf("the cycle must still report a published list, got:\n%s", logs)
	}
}
