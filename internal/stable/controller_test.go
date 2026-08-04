package stable //nolint:testpackage // asserts the Controller's internal worker lifecycle

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/preprocess"
)

// syncBuf collects log output from the worker's fetchSources fan-out, which
// writes from several goroutines, while the test reads it back with the worker
// still running. A bare bytes.Buffer races on both counts.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// emptyFilterer yields no nodes, so every cycle ends at the "no entries merged"
// branch: instant, no network, no probing.
type emptyFilterer struct{}

func (emptyFilterer) FilterNodes(
	context.Context, preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	return nil, preprocess.Stats{}, nil
}

//nolint:ireturn // implements stable.Filterer; handing out the interface is the point
func (emptyFilterer) Annotator() preprocess.Annotator { return nil }

// The interval is fixed: every caller wants a cycle that never fires a second
// time on its own, so the test drives Apply/Reconfigure rather than the clock.
func testControllerConfig(sources ...string) config.Config {
	srcs := make([]config.SubscriptionSource, 0, len(sources))
	for _, s := range sources {
		srcs = append(srcs, config.SubscriptionSource{Name: s, URL: "https://" + s + ".example/sub"})
	}

	return config.Config{
		Subscriptions: config.SubscriptionsConfig{
			Interval: time.Hour,
			Check: config.CheckConfig{
				Rounds: 1, MaxFail: 0, MaxAvgMs: 1000,
				SourceTimeout: time.Minute, ExpectedStatus: "204",
			},
			Sources: srcs,
		},
	}
}

// TestApplyReconfiguresRunningWorker locks the reload contract: a config change
// must be handed to the worker already running, not used to replace it. The old
// Apply called Stop(), which cancelled the in-flight cycle's context and threw
// away a whole probe pass -- in production every recorded cycle failure was the
// crawler rewriting private.yaml while a cycle was running.
func TestApplyReconfiguresRunningWorker(t *testing.T) {
	t.Parallel()

	holder := NewHolder()
	ctl := NewController(t.Context(), holder,
		func() Filterer { return emptyFilterer{} },
		nil, nil, zerolog.Nop(), nil)
	defer ctl.Stop()

	if err := ctl.Apply(testControllerConfig("alpha")); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first := ctl.checker
	if first == nil {
		t.Fatal("first Apply must start a worker")
	}

	if err := ctl.Apply(testControllerConfig("alpha", "beta")); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if ctl.checker != first {
		t.Fatal("Apply must reconfigure the running worker, not replace it")
	}
	if got := len(ctl.checker.spec.Load().Sources); got != 2 {
		t.Fatalf("reconfigured worker sees %d sources, want 2", got)
	}
}

// TestApplyCarriesCloudflareTimeoutToRunningWorker pins the half of the
// geo.cloudflare move that a reload-gate test cannot see. reload's coverage
// table proves a geo.cloudflare.timeout edit reaches Controller.Apply at all;
// this proves the value then lands in the spec the RUNNING worker reads, on the
// prober its post-probe trace stage calls. Its geo.* siblings are carried by
// OptionsFromConfig into a freshly built processor instead, and a worker that
// kept its old prober would look identical from outside.
func TestApplyCarriesCloudflareTimeoutToRunningWorker(t *testing.T) {
	t.Parallel()

	ctl := NewController(t.Context(), NewHolder(),
		func() Filterer { return emptyFilterer{} },
		nil, nil, zerolog.Nop(), nil)
	defer ctl.Stop()

	cfg := testControllerConfig("alpha")
	cfg.Geo.Cloudflare = config.CloudflareConfig{Timeout: 15 * time.Second, Concurrency: 8}
	if err := ctl.Apply(cfg); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	running := ctl.checker

	cfg.Geo.Cloudflare.Timeout = 30 * time.Second
	if err := ctl.Apply(cfg); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if ctl.checker != running {
		t.Fatal("Apply must reconfigure the running worker, not replace it")
	}
	prober, ok := ctl.checker.spec.Load().Prober.(*MihomoProber)
	if !ok {
		t.Fatalf("worker spec holds a %T, not the MihomoProber that runs the trace", ctl.checker.spec.Load().Prober)
	}
	if got := prober.cloudflare.Timeout; got != 30*time.Second {
		t.Fatalf("running worker traces with a %v timeout, want 30s: the edit never reached its spec", got)
	}
}

// TestApplyKeepsWorkerWhenSourcesGone: an empty merged source list is refused,
// not obeyed. Every source of this deployment arrives from an overlay, so an
// empty list is nearly always a missing or truncated sources.yaml/private.yaml;
// stopping the worker on it cancels the cycle in flight and leaves /stable.txt
// frozen on its last publication with nothing behind it. The previous spec stays
// live and the refusal is logged. With no worker running it stays a no-op, so a
// genuine zero-source deployment still never starts one.
func TestApplyKeepsWorkerWhenSourcesGone(t *testing.T) {
	t.Parallel()

	var logBuf syncBuf
	holder := NewHolder()
	ctl := NewController(t.Context(), holder,
		func() Filterer { return emptyFilterer{} },
		nil, nil, zerolog.New(&logBuf), nil)
	defer ctl.Stop()

	if err := ctl.Apply(testControllerConfig("alpha")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	running := ctl.checker
	if running == nil {
		t.Fatal("worker should be running")
	}

	if err := ctl.Apply(config.Config{}); err != nil {
		t.Fatalf("Apply with no sources: %v", err)
	}
	if ctl.checker != running {
		t.Fatal("an empty source list must leave the running worker in place")
	}
	if got := len(ctl.checker.spec.Load().Sources); got != 1 {
		t.Fatalf("worker sees %d sources, want the previous 1", got)
	}
	if !strings.Contains(logBuf.String(), "no subscription sources") {
		t.Errorf("the refusal must be logged, got %q", logBuf.String())
	}

	idle := NewController(t.Context(), holder,
		func() Filterer { return emptyFilterer{} },
		nil, nil, zerolog.Nop(), nil)
	if err := idle.Apply(config.Config{}); err != nil {
		t.Fatalf("Apply with no sources and no worker: %v", err)
	}
	if idle.checker != nil {
		t.Fatal("a zero-source config must not start a worker")
	}
}
