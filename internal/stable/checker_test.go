package stable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/geofeed"
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

// deadKey mirrors the DeadCache key: server:port plus the resolved address
// (Entry.IP) the checker passed. The filterers below carry one ip for every
// node they yield (fakeFilterer.ip), zero when unset, so assertions spell the
// pair out exactly as the checker keyed it.
type deadKey struct {
	addr string
	ip   netip.Addr
}

// fakeDeadCache remembers what it was told, so like *DeadSet it must clone the
// caller's addr (see the DeadCache contract) rather than pin the arena view.
type fakeDeadCache struct {
	blocked  map[deadKey]bool
	recorded []deadKey
}

func (d *fakeDeadCache) Blocked(addr string, ip netip.Addr) bool {
	return d.blocked[deadKey{addr: addr, ip: ip}]
}
func (d *fakeDeadCache) Block(addr string, ip netip.Addr) error {
	addr = strings.Clone(addr)
	d.recorded = append(d.recorded, deadKey{addr: addr, ip: ip})
	d.blocked[deadKey{addr: addr, ip: ip}] = true
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
	dead := &fakeDeadCache{blocked: map[deadKey]bool{}}
	holder := stable.NewHolder()
	c := stable.NewChecker(
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer }, nil, dead, holder, "", zerolog.Nop(), nil,
	)

	// Cycle 1: both nodes probed; alpha fails -> recorded dead.
	_ = c.RunOnce(context.Background())
	if !dead.blocked[deadKey{addr: "1.1.1.1:443"}] {
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

// TestCheckerDeadCacheRecordsZeroSuccessAndAbsent covers both shapes a failed
// node can arrive in, because the prober's map is moving from successes-only to
// one entry per label. A presence test used to be equivalent to "no successful
// round"; under a populated map it silently blocks nobody, which empties the
// dead cache and quadruples the next cycle's probe set with every counter still
// reading plausible. The absent case must keep working regardless: nothing
// obliges a Prober implementation to name every label.
func TestCheckerDeadCacheRecordsZeroSuccessAndAbsent(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
		"https://gamma.example/sub": "vless://u@3.3.3.3:443#c\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 0, MeanMs: 0}, // every port failed, reported
		"gamma-001": {Successes: 5, MeanMs: 100},
		// beta-001 is named by no entry at all.
	}}
	dead := &fakeDeadCache{blocked: map[deadKey]bool{}}
	spec := testCheckerSpec(prober)
	spec.Sources = append(spec.Sources, config.SubscriptionSource{
		Name: "gamma", URL: "https://gamma.example/sub",
	})
	c := stable.NewChecker(
		spec,
		func() stable.Filterer { return filterer }, nil, dead, stable.NewHolder(), "", zerolog.Nop(), nil,
	)
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !dead.blocked[deadKey{addr: "1.1.1.1:443"}] {
		t.Errorf("a reported zero-success node must be recorded dead, got %v", dead.recorded)
	}
	if !dead.blocked[deadKey{addr: "2.2.2.2:443"}] {
		t.Errorf("a node absent from the results must be recorded dead, got %v", dead.recorded)
	}
	if dead.blocked[deadKey{addr: "3.3.3.3:443"}] {
		t.Errorf("the live node must not be recorded dead, got %v", dead.recorded)
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
	dead := &fakeDeadCache{blocked: map[deadKey]bool{}}
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
	dead := &fakeDeadCache{blocked: map[deadKey]bool{}}
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

// A cycle in which every probed node returned zero successes (our egress down,
// say) must not write the verdict into the dead cache: committing it would
// freeze the published list for deadcache.ttl after the network recovers.
// verdict into the dead cache: committing it would freeze the published list
// for deadcache.ttl after the network recovers.
func TestCheckerDeadCacheNotWrittenWhenWholeProbeSetFails(t *testing.T) {
	t.Parallel()

	const n = 8
	var sb strings.Builder
	for i := range n {
		fmt.Fprintf(&sb, "vless://u@10.%d.%d.%d:443#a%d\n", i/65536, i/256%256, i%256, i)
	}
	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{"https://alpha.example/sub": sb.String()}}
	res := make(map[string]stable.ProbeResult, n)
	for i := range n {
		res[fmt.Sprintf("alpha-%03d", i+1)] = stable.ProbeResult{Successes: 0}
	}
	dead := &fakeDeadCache{blocked: map[deadKey]bool{}}
	holder := stable.NewHolder()
	holder.Store(&stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()})
	c := stable.NewChecker(
		testCheckerSpec(&fakeProber{res: res}),
		func() stable.Filterer { return filterer }, nil, dead, holder, "", zerolog.Nop(), nil,
	)

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(dead.recorded) != 0 {
		t.Errorf("a whole-pool failure must not be written to the dead cache, got %v", dead.recorded)
	}
}

// A mixed verdict is still believed: with most nodes alive, the dead ones are
// cached exactly as before. This pins that the recordDead guard only trips on
// the implausible ~100% failure, not on an ordinary pool's dead share.
func TestCheckerDeadCacheWrittenForPlausibleShare(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
		"https://gamma.example/sub": "vless://u@3.3.3.3:443#c\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 0}, // dead: cached
		"beta-001":  {Successes: 5, MeanMs: 100},
		"gamma-001": {Successes: 5, MeanMs: 100},
	}}
	dead := &fakeDeadCache{blocked: map[deadKey]bool{}}
	spec := testCheckerSpec(prober)
	spec.Sources = append(spec.Sources, config.SubscriptionSource{
		Name: "gamma", URL: "https://gamma.example/sub",
	})
	c := stable.NewChecker(
		spec,
		func() stable.Filterer { return filterer }, nil, dead, stable.NewHolder(), "", zerolog.Nop(), nil,
	)

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !dead.blocked[deadKey{addr: "1.1.1.1:443"}] {
		t.Errorf("a one-in-three failure is a plausible verdict; the dead node must be cached, got %v", dead.recorded)
	}
	if dead.blocked[deadKey{addr: "2.2.2.2:443"}] || dead.blocked[deadKey{addr: "3.3.3.3:443"}] {
		t.Errorf("live nodes must not be cached, got %v", dead.recorded)
	}
}

// cancellingAnnotator cancels the cycle context on its first annotate call, so
// a cancellation lands inside BuildPayload — after every earlier ctx check has
// passed. RunOnce must then refuse to publish the degraded payload.
type cancellingAnnotator struct {
	offline geofeed.CountryCode
	cancel  context.CancelFunc
	once    sync.Once
}

func (a *cancellingAnnotator) Annotate(
	_ context.Context, _, _ *bytes.Buffer, _ preprocess.AnnotateRequest,
) geofeed.CountryCode {
	a.once.Do(a.cancel)
	return a.offline
}

func TestCheckerCancellationDuringPublishKeepsPrevious(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	filterer.ann = &cancellingAnnotator{offline: country(t, "NL"), cancel: cancel}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 100},
		"beta-001":  {Successes: 5, MeanMs: 100},
	}}
	holder := stable.NewHolder()
	previous := &stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()}
	holder.Store(previous)
	rep := &fakeReporter{}
	c := stable.NewChecker(
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer }, nil, nil, holder, "", zerolog.Nop(), rep,
	)

	err := c.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce must surface the publish-phase cancellation, got %v", err)
	}
	if holder.Load() != previous {
		t.Error("the previous snapshot must be kept when the publish phase is cancelled")
	}
	if rep.last != nil {
		t.Error("a cancelled cycle must not observe a published list")
	}
	if rep.errs != 1 {
		t.Errorf("ObserveError calls = %d, want 1", rep.errs)
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

// TestKeptLatenciesReachTheCycleReport pins the hand-off SelectSurvivors' mean
// delay had never made: it gated and sorted the published list, then died
// inside the Survivor slice. Nothing downstream fails when it is dropped --
// the field is simply nil and the histogram renders an empty, plausible zero
// -- so the assertion has to be on what a Reporter receives.
//
// The zero-latency node is load-bearing, not filler. keptSpeeds skips zeros
// because a zero Mbps means the bandwidth filter never ran; every survivor was
// probed by definition, so a zero here is a real sub-millisecond mean and
// dropping it would bias the histogram's low end. Without this entry, giving
// keptLatencies its sibling's zero-skip passes.
func TestKeptLatenciesReachTheCycleReport(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
		"https://gamma.example/sub": "vless://u@3.3.3.3:443#c\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 420},
		"beta-001":  {Successes: 5, MeanMs: 130},
		"gamma-001": {Successes: 5, MeanMs: 0},
	}}
	rep := &fakeReporter{}
	spec := testCheckerSpec(prober)
	// Local, not testSources(): a third source would shift SourcesTotal for
	// every other test that shares the fixture.
	spec.Sources = append(spec.Sources, config.SubscriptionSource{
		Name: "gamma", URL: "https://gamma.example/sub",
	})
	c := stable.NewChecker(
		spec,
		func() stable.Filterer { return filterer }, nil, nil, stable.NewHolder(), "", zerolog.Nop(), rep,
	)
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.last == nil {
		t.Fatal("reporter.Observe must fire on a published cycle")
	}
	// Ascending, because the report carries them in published order.
	if want := []int{0, 130, 420}; !slices.Equal(rep.last.KeptLatenciesMs, want) {
		t.Errorf("report KeptLatenciesMs = %v, want %v", rep.last.KeptLatenciesMs, want)
	}
}

// slowProber burns a known interval inside Probe so the phase breakdown can be
// checked for attribution rather than for merely being non-zero.
type slowProber struct {
	fakeProber
	delay time.Duration
}

func (p *slowProber) Probe(ctx context.Context, payload []byte) (map[string]stable.ProbeResult, error) {
	time.Sleep(p.delay)

	return p.fakeProber.Probe(ctx, payload)
}

// TestCyclePhasesAttributeTheProbe pins the boundaries, not the plumbing: a
// breakdown that charges the probe's time to another phase is worse than no
// breakdown, since it sends the next optimisation at the wrong stage. The
// prober's sleep is the only thing in the cycle that takes measurable time, so
// it must appear in Probe and in nothing else.
func TestCyclePhasesAttributeTheProbe(t *testing.T) {
	t.Parallel()

	const delay = 40 * time.Millisecond

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
	}}
	prober := &slowProber{
		fakeProber: fakeProber{res: map[string]stable.ProbeResult{
			"alpha-001": {Successes: 5, MeanMs: 100},
			"beta-001":  {Successes: 5, MeanMs: 100},
		}},
		delay: delay,
	}
	rep := &fakeReporter{}
	c := stable.NewChecker(
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer }, nil, nil, stable.NewHolder(), "", zerolog.Nop(), rep,
	)
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.last == nil {
		t.Fatal("reporter.Observe must fire on a published cycle")
	}

	p := rep.last.Phases
	if p.Probe < delay {
		t.Errorf("Phases.Probe = %v, want >= %v", p.Probe, delay)
	}
	others := []struct {
		name string
		d    time.Duration
	}{
		{"Fetch", p.Fetch},
		{"Merge", p.Merge},
		{"DeadFilter", p.DeadFilter},
		{"Egress", p.Egress},
		{"Publish", p.Publish},
	}
	for _, o := range others {
		if o.d >= delay {
			t.Errorf("Phases.%s = %v, want < %v: the probe's time leaked into it", o.name, o.d, delay)
		}
	}
	sum := p.Fetch + p.Merge + p.DeadFilter + p.Probe + p.Egress + p.Publish
	if sum > rep.last.Duration {
		t.Errorf("phases sum to %v, past the cycle's own %v", sum, rep.last.Duration)
	}
}

// TestProbeStagesReachTheCycleReport walks the whole seam the metric rides:
// prober -> probeStages -> CycleReport -> Reporter. The absent label is the
// point: it must count as StageUnknown rather than drop out, or the stage
// counts stop summing to Probed and the panel's ratio silently stops closing.
func TestProbeStagesReachTheCycleReport(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
		"https://gamma.example/sub": "vless://u@3.3.3.3:443#c\n",
	}}
	prober := &fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 100, Stage: stable.StagePassed},
		"beta-001":  {Stage: stable.StageCondemned},
		// gamma-001 is named by no entry: unparsable proxies never reach the
		// prober, and a fake need not name every label either.
	}}
	rep := &fakeReporter{}
	spec := testCheckerSpec(prober)
	spec.Sources = append(spec.Sources, config.SubscriptionSource{
		Name: "gamma", URL: "https://gamma.example/sub",
	})
	c := stable.NewChecker(
		spec,
		func() stable.Filterer { return filterer }, nil, nil, stable.NewHolder(), "", zerolog.Nop(), rep,
	)
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.last == nil {
		t.Fatal("reporter.Observe must fire on a published cycle")
	}

	// A pair slice, not a map literal keyed by ProbeStage: the fold must NOT
	// carry a key for a stage nobody reached, and an exhaustive literal cannot
	// express that.
	wants := []struct {
		stage stable.ProbeStage
		n     int
	}{
		{stable.StagePassed, 1},
		{stable.StageCondemned, 1},
		{stable.StageUnknown, 1},
	}
	for _, w := range wants {
		if got := rep.last.ProbeStages[w.stage]; got != w.n {
			t.Errorf("ProbeStages[%v] = %d, want %d", w.stage, got, w.n)
		}
	}
	if len(rep.last.ProbeStages) != len(wants) {
		t.Errorf("report ProbeStages = %v, want exactly %d stages", rep.last.ProbeStages, len(wants))
	}
	total := 0
	for _, n := range rep.last.ProbeStages {
		total += n
	}
	if total != rep.last.Probed {
		t.Errorf("stage counts sum to %d, want Probed = %d: the ratio must stay closed", total, rep.last.Probed)
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
	wantTrace := stable.TraceReport{State: stable.TraceRan, Answered: 1, Unanswered: 1, Moved: 1}
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
	wantTrace := stable.TraceReport{State: stable.TraceRan, Answered: 1, Unanswered: 0, Moved: 0}
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

// A trace that RAN but nobody answered must still report TraceRan: the
// metrics slice renders nothing while State is TraceAbsent, so a skipped stage
// and a ran-but-empty one must not read alike (answered=0/unanswered=0 either
// way).
func TestCheckerTraceRanReportsStateWhenNobodyAnswered(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{
		bodies: map[fetch.SubscriptionURL]string{
			"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
		},
		ip:  addr(t, "9.9.9.9"),
		ann: tagAnnotator{offline: country(t, "NL")},
	}
	prober := &tracingProber{
		fakeProber: fakeProber{res: map[string]stable.ProbeResult{"alpha-001": {Successes: 5, MeanMs: 100}}},
		trace:      map[string]stable.TraceResult{}, // every node unanswered
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
	want := stable.TraceReport{State: stable.TraceRan, Unanswered: 1}
	if rep.last.Trace != want {
		t.Errorf("Trace = %+v, want %+v (the stage ran; only the answers are zero)", rep.last.Trace, want)
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

// hwidFilterer records what each source's fetch was told, on top of the ordinary
// fake source bodies.
type hwidFilterer struct {
	inner fakeFilterer
	mu    sync.Mutex
	seen  map[fetch.SubscriptionURL]string
}

func (f *hwidFilterer) FilterNodes(
	ctx context.Context, req preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	f.mu.Lock()
	f.seen[req.SubscriptionURL] = req.HWID
	f.mu.Unlock()

	return f.inner.FilterNodes(ctx, req)
}

//nolint:ireturn // implements stable.Filterer; handing out the interface is the point
func (f *hwidFilterer) Annotator() preprocess.Annotator { return f.inner.Annotator() }

// The worker is the only caller holding per-source config, so it is the only one
// that can send an hwid. A source that loses its own value still finishes the
// cycle clean — the panel serves a placeholder node under a 200 — so the value
// has to be pinned per source, including the neighbour that must send none.
func TestCheckerCarriesEachSourcesHWID(t *testing.T) {
	t.Parallel()

	const hwid = "abcdef0123456789"
	filterer := &hwidFilterer{
		inner: fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
			"https://alpha.example/sub": "vless://u@1.1.1.1:443#a\n",
			"https://beta.example/sub":  "vless://u@2.2.2.2:443#b\n",
		}},
		seen: map[fetch.SubscriptionURL]string{},
	}
	spec := testCheckerSpec(&fakeProber{res: map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 300},
		"beta-001":  {Successes: 5, MeanMs: 100},
	}})
	spec.Sources = []config.SubscriptionSource{
		{Name: "alpha", URL: "https://alpha.example/sub", HWID: hwid},
		{Name: "beta", URL: "https://beta.example/sub"},
	}

	err := stable.NewChecker(
		spec,
		func() stable.Filterer { return filterer },
		nil, nil, stable.NewHolder(), "", zerolog.Nop(), nil,
	).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	want := map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": hwid,
		"https://beta.example/sub":  "",
	}
	if !maps.Equal(filterer.seen, want) {
		t.Errorf("hwids per source = %q, want %q", filterer.seen, want)
	}
}

// probedUUID is the vless credential the retention fixtures below parse: the
// fake prober turns the probed payload into REAL mihomo adapters, so the lines
// must be ones convert + adapter.ParseProxy accept (plain vless, no query).
const probedUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

func probedVlessLine(host, port, name string) string {
	return "vless://" + probedUUID + "@" + host + ":" + port + "#" + name
}

// countingProxy wraps a real mihomo adapter so a test can observe its Close.
type countingProxy struct {
	mihomo.Proxy
	closes int
}

func (p *countingProxy) Close() error {
	p.closes++
	return p.Proxy.Close()
}

// retainingProber is fakeProber with the probedAdapterSource capability: its
// Probe parses the payload it is handed into REAL mihomo adapters (wrapped to
// count Close) and retains them for TakeProbedAdapters, exactly as MihomoProber
// does, so a full cycle's adapter lifecycle is observable end to end.
type retainingProber struct {
	fakeProber
	closers []*countingProxy
	takes   int
}

func (p *retainingProber) Probe(ctx context.Context, payload []byte) (map[string]stable.ProbeResult, error) {
	res, err := p.fakeProber.Probe(ctx, payload)
	if err != nil {
		return nil, err
	}
	mappings, convErr := convert.ConvertsV2Ray(payload)
	if convErr != nil {
		return nil, convErr
	}
	for _, m := range mappings {
		px, perr := adapter.ParseProxy(m)
		if perr != nil {
			return nil, perr
		}
		p.closers = append(p.closers, &countingProxy{Proxy: px})
	}

	return res, nil
}

func (p *retainingProber) ParseProxies([]byte) ([]mihomo.Proxy, error) {
	return nil, errors.New("ParseProxies must not run when the prober retains its probe adapters")
}

func (p *retainingProber) TakeProbedAdapters() []mihomo.Proxy {
	p.takes++
	out := make([]mihomo.Proxy, len(p.closers))
	for i, w := range p.closers {
		out[i] = w
	}

	return out
}

func assertEachClosedOnce(t *testing.T, closers []*countingProxy) {
	t.Helper()

	for i, w := range closers {
		if w.closes != 1 {
			t.Errorf("adapter %d (%s) was closed %d times, want exactly 1", i, w.Name(), w.closes)
		}
	}
}

// retentionFixtures are two parseable vless nodes, one per source.
func retentionFixtures() fakeFilterer {
	return fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": probedVlessLine("192.0.2.1", "443", "a") + "\n",
		"https://beta.example/sub":  probedVlessLine("192.0.2.2", "443", "b") + "\n",
	}}
}

func aliveTwo() map[string]stable.ProbeResult {
	return map[string]stable.ProbeResult{
		"alpha-001": {Successes: 5, MeanMs: 100},
		"beta-001":  {Successes: 5, MeanMs: 100},
	}
}

// The egress stage is skipped entirely (no filters, no trace), so the adapters
// the probe retained are consumed by nobody: RunOnce's own close is the only
// one, and each must fire exactly once.
func TestProbedAdaptersClosedOnceWhenEgressStageSkipped(t *testing.T) {
	t.Parallel()

	prober := &retainingProber{}
	prober.res = aliveTwo()
	holder := stable.NewHolder()

	if err := newTestChecker(retentionFixtures(), prober, holder).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if holder.Load() == nil {
		t.Fatal("the cycle must publish")
	}
	if prober.takes != 1 {
		t.Errorf("TakeProbedAdapters calls = %d, want exactly 1", prober.takes)
	}
	if len(prober.closers) != 2 {
		t.Fatalf("probe retained %d adapters, want 2", len(prober.closers))
	}
	assertEachClosedOnce(t, prober.closers)
}

// tracingRetainingProber is retainingProber that also answers the egress
// trace, recording the proxies it was actually handed.
type tracingRetainingProber struct {
	*retainingProber
	trace map[string]stable.TraceResult
	seen  []mihomo.Proxy
}

func (p *tracingRetainingProber) TraceCheck(_ context.Context, proxies []mihomo.Proxy) map[string]stable.TraceResult {
	p.seen = append(p.seen, proxies...)
	return p.trace
}

// The egress stage consumes the RETAINED adapters (the trace sees the very
// objects Probe built) and never re-parses: ParseProxies fails loudly on this
// prober, so a fallback would have surfaced as a skipped stage. Each adapter
// is still closed exactly once, by RunOnce.
func TestProbedAdaptersConsumedByEgressAndClosedOnce(t *testing.T) {
	t.Parallel()

	prober := &tracingRetainingProber{
		retainingProber: &retainingProber{},
		trace: map[string]stable.TraceResult{
			"alpha-001": {IP: addr(t, "5.6.7.8"), Country: country(t, "DE")},
			"beta-001":  {IP: addr(t, "5.6.7.9"), Country: country(t, "NL")},
		},
	}
	prober.res = aliveTwo()
	spec := testCheckerSpec(prober)
	spec.Trace = true
	holder := stable.NewHolder()
	c := stable.NewChecker(spec, func() stable.Filterer { return retentionFixtures() },
		nil, nil, holder, "", zerolog.Nop(), nil)

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if holder.Load() == nil {
		t.Fatal("the cycle must publish")
	}
	if len(prober.seen) != 2 {
		t.Fatalf("trace saw %d proxies, want the 2 retained adapters", len(prober.seen))
	}
	seen := make(map[*countingProxy]int, len(prober.seen))
	for _, px := range prober.seen {
		w, ok := px.(*countingProxy)
		if !ok {
			t.Fatalf("trace consumed a %T, not a probe adapter", px)
		}
		seen[w]++
	}
	for _, w := range prober.closers {
		if seen[w] != 1 {
			t.Errorf("adapter %s reached the trace %d times, want exactly 1", w.Name(), seen[w])
		}
	}
	assertEachClosedOnce(t, prober.closers)
}

// A cancellation landing after Probe returns still owes the retained adapters
// their close: the take happens before the ctx gate, and the deferred close
// covers the early return.
func TestProbedAdaptersClosedOnceWhenCycleCancelledAfterProbe(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prober := &retainingProber{}
	prober.res = aliveTwo()
	holder := stable.NewHolder()
	previous := &stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()}
	holder.Store(previous)
	// cancel inside Probe, as cancellingProber does; results still come back.
	c2 := &cancellingRetainingProber{retainingProber: prober, cancel: cancel}
	c := stable.NewChecker(
		testCheckerSpec(c2),
		func() stable.Filterer { return retentionFixtures() },
		nil, nil, holder, "", zerolog.Nop(), nil,
	)

	err := c.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce must surface the cancellation, got %v", err)
	}
	if holder.Load() != previous {
		t.Error("previous snapshot must be kept after cancellation")
	}
	if prober.takes != 1 {
		t.Errorf("TakeProbedAdapters calls = %d, want exactly 1", prober.takes)
	}
	assertEachClosedOnce(t, prober.closers)
}

// cancellingRetainingProber cancels the cycle context inside Probe and returns
// the results anyway, like cancellingProber, so the cancellation lands on the
// checker's post-probe gate.
type cancellingRetainingProber struct {
	*retainingProber
	cancel context.CancelFunc
}

func (p *cancellingRetainingProber) Probe(ctx context.Context, payload []byte) (map[string]stable.ProbeResult, error) {
	p.cancel()
	return p.retainingProber.Probe(ctx, payload)
}

// A probe error means nothing was retained and nothing was taken: the
// prober's own error path owns its adapters (see MihomoProber.releaseProbed).
func TestProbeErrorLeavesNothingToTakeOrClose(t *testing.T) {
	t.Parallel()

	prober := &retainingProber{}
	prober.err = context.Canceled
	holder := stable.NewHolder()
	holder.Store(&stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()})

	c := newTestChecker(retentionFixtures(), prober, holder)
	if err := c.RunOnce(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce must return the probe error, got %v", err)
	}
	if prober.takes != 0 {
		t.Errorf("TakeProbedAdapters calls = %d, want 0 after a failed probe", prober.takes)
	}
	if len(prober.closers) != 0 {
		t.Errorf("a failed probe retained %d adapters, want none", len(prober.closers))
	}
}

// Zero survivors still close the retained adapters exactly once: selection
// happens after the take, and the no-survivor exit is one more path the
// deferred close covers.
func TestProbedAdaptersClosedOnceWhenNothingSurvives(t *testing.T) {
	t.Parallel()

	prober := &retainingProber{}
	prober.res = map[string]stable.ProbeResult{
		"alpha-001": {Successes: 0},
		"beta-001":  {Successes: 0},
	}
	holder := stable.NewHolder()
	holder.Store(&stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()})

	c := newTestChecker(retentionFixtures(), prober, holder)
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if holder.Load() == nil || string(holder.Load().Payload) != "old\n" {
		t.Error("the previous list must be kept when nothing survives")
	}
	assertEachClosedOnce(t, prober.closers)
}

// LogicCycle#2: the dead cache is keyed on the RESOLVED address as well as the
// host:port, so a hostname re-pointed to a live server is re-probed instead of
// skipped for the jittered TTL, while the same hostname at the same dead
// address stays skipped. beta rides along so alpha's failure is a plausible
// 1-in-2 share and the recordDead breaker lets the write through.
func TestCheckerDeadCacheReProbesWhenResolvedAddressChanges(t *testing.T) {
	t.Parallel()

	filterer := fakeFilterer{bodies: map[fetch.SubscriptionURL]string{
		"https://alpha.example/sub": "vless://u@dead.example:443#a\n",
		"https://beta.example/sub":  "vless://u@192.0.2.2:443#b\n",
	}}
	prober := &fakeProber{}
	dead := &fakeDeadCache{blocked: map[deadKey]bool{}}
	holder := stable.NewHolder()
	holder.Store(&stable.Snapshot{Payload: []byte("old\n"), UpdatedAt: time.Now()})
	c := stable.NewChecker(
		testCheckerSpec(prober),
		func() stable.Filterer { return filterer },
		nil, dead, holder, "", zerolog.Nop(), nil,
	)

	// Cycle 1: dead.example resolves to 192.0.2.10 and fails; the entry is
	// booked under that address.
	filterer.ip = addr(t, "192.0.2.10")
	prober.res = map[string]stable.ProbeResult{"beta-001": {Successes: 5, MeanMs: 100}}
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	want := deadKey{addr: "dead.example:443", ip: addr(t, "192.0.2.10")}
	if !dead.blocked[want] {
		t.Fatalf("cycle 1 must book dead.example:443 at 192.0.2.10, recorded %v", dead.recorded)
	}

	// Cycle 2: the host now resolves to a live address; the old verdict must
	// not follow the hostname, so the node is probed again.
	filterer.ip = addr(t, "192.0.2.20")
	prober.res = aliveTwo()
	prober.gotPayload = nil
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if !strings.Contains(string(prober.gotPayload), "dead.example:443") {
		t.Errorf("a re-pointed host must be re-probed; cycle 2 probed %q", prober.gotPayload)
	}

	// Cycle 3: the host is back on the booked dead address, so the standing
	// entry applies again and the node is skipped unprobed.
	filterer.ip = addr(t, "192.0.2.10")
	prober.res = map[string]stable.ProbeResult{"beta-001": {Successes: 5, MeanMs: 100}}
	prober.gotPayload = nil
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if strings.Contains(string(prober.gotPayload), "dead.example:443") {
		t.Errorf("the dead address must stay skipped; cycle 3 probed %q", prober.gotPayload)
	}
}
