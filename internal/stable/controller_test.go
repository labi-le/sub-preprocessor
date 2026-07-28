package stable //nolint:testpackage // asserts the Controller's internal worker lifecycle

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/preprocess"
)

// emptyFilterer yields no nodes, so every cycle ends at the "no entries merged"
// branch: instant, no network, no probing.
type emptyFilterer struct{}

func (emptyFilterer) Filter(context.Context, *bytes.Buffer, preprocess.FilterRequest) (preprocess.Stats, error) {
	return preprocess.Stats{}, nil
}

func testControllerConfig(interval time.Duration, sources ...string) config.Config {
	srcs := make([]config.SubscriptionSource, 0, len(sources))
	for _, s := range sources {
		srcs = append(srcs, config.SubscriptionSource{Name: s, URL: "https://" + s + ".example/sub"})
	}

	return config.Config{
		Subscriptions: config.SubscriptionsConfig{
			Interval: interval,
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

	if err := ctl.Apply(testControllerConfig(time.Hour, "alpha")); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first := ctl.checker
	if first == nil {
		t.Fatal("first Apply must start a worker")
	}

	if err := ctl.Apply(testControllerConfig(time.Hour, "alpha", "beta")); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if ctl.checker != first {
		t.Fatal("Apply must reconfigure the running worker, not replace it")
	}
	if got := len(ctl.checker.spec.Load().Sources); got != 2 {
		t.Fatalf("reconfigured worker sees %d sources, want 2", got)
	}
}

// TestApplyStopsWorkerWhenSourcesGone: an empty source list still has to tear
// the worker down, otherwise it keeps polling a config the user removed.
func TestApplyStopsWorkerWhenSourcesGone(t *testing.T) {
	t.Parallel()

	holder := NewHolder()
	ctl := NewController(t.Context(), holder,
		func() Filterer { return emptyFilterer{} },
		nil, nil, zerolog.Nop(), nil)
	defer ctl.Stop()

	if err := ctl.Apply(testControllerConfig(time.Hour, "alpha")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ctl.checker == nil {
		t.Fatal("worker should be running")
	}

	if err := ctl.Apply(config.Config{}); err != nil {
		t.Fatalf("Apply with no sources: %v", err)
	}
	if ctl.checker != nil {
		t.Fatal("worker must be stopped when the source list empties")
	}
}
