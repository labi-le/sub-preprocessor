package stable_test

import (
	"context"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/ioutil"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/stable"
)

// poolFilterer fabricates one source's nodes and registers a cleanup on the
// Nodes array it hands back. That array is reachable only through the
// []SourceBody a cycle holds, so the cleanup running is a direct observation
// that the pre-dedupe pool became unreachable.
type poolFilterer struct {
	names     map[fetch.SubscriptionURL]string
	perSource int
	padBytes  int
	freed     *atomic.Int64
}

func (f poolFilterer) FilterNodes(
	_ context.Context, req preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	name := f.names[req.SubscriptionURL]
	ip := netip.MustParseAddr("1.2.3.4")
	pad := strings.Repeat("p", f.padBytes)
	nodes := make([]preprocess.NodeResult, f.perSource)
	for i := range nodes {
		// Each Raw owns its bytes: nodes carved out of one shared fixture
		// string would be kept alive by the fixture and prove nothing.
		line := []byte("vless://u@" + name + strconv.Itoa(i) + ".example:443?pad=" + pad + "#n")
		nodes[i] = preprocess.NodeResult{Raw: ioutil.UnsafeString(line), IP: ip}
	}
	runtime.AddCleanup(&nodes[0], func(c *atomic.Int64) { c.Add(1) }, f.freed)

	return nodes, preprocess.Stats{}, nil
}

//nolint:ireturn // implements stable.Filterer
func (f poolFilterer) Annotator() preprocess.Annotator { return nil }

// gcProber stands in for the URL test that owns 20 of a cycle's 20 minutes, and
// reports what the cycle still holds while that probe is on the stack.
type gcProber struct {
	res      map[string]stable.ProbeResult
	freed    *atomic.Int64
	want     int64
	sawFreed int64
	heapKiB  uint64
}

func (p *gcProber) Probe(_ context.Context, _ []byte) (map[string]stable.ProbeResult, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		p.sawFreed = p.freed.Load()
		if p.sawFreed >= p.want || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	p.heapKiB = m.HeapAlloc / 1024

	return p.res, nil
}

func (p *gcProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

// keepOnly mimics the production dead cache, which rules out 74% of the merged
// pool before the probe; keeping two nodes leaves the rest collectable. The
// keys are addr-only because every fixture node carries the SAME resolved
// address — poolFilterer assigns netip.MustParseAddr("1.2.3.4") to each
// NodeResult.IP, which Merge carries into Entry.IP — so filterDead consults
// Blocked with one (addr, 1.2.3.4) pair across the pool and an addr-only key
// matches them all. What the fixture cannot exercise is the ip half of the
// dead key: a hostname re-pointed to a different address is re-probed
// (LogicCycle#2), which the DeadSet unit test pins instead.
type keepOnly struct{ keep map[string]bool }

func (d keepOnly) Blocked(addr string, _ netip.Addr) bool { return !d.keep[addr] }
func (d keepOnly) Block(string, netip.Addr) error         { return nil }
func (d keepOnly) Prune() error                           { return nil }

func retentionCycle(
	t *testing.T, perSource, padBytes int, dead stable.DeadCache,
) (*gcProber, *stable.Snapshot, uint64) {
	t.Helper()

	sources := []config.SubscriptionSource{
		{Name: "alpha", URL: "https://alpha.example/sub"},
		{Name: "beta", URL: "https://beta.example/sub"},
	}
	filterer := poolFilterer{
		names: map[fetch.SubscriptionURL]string{
			"https://alpha.example/sub": "a",
			"https://beta.example/sub":  "b",
		},
		perSource: perSource,
		padBytes:  padBytes,
		freed:     &atomic.Int64{},
	}
	prober := &gcProber{
		res: map[string]stable.ProbeResult{
			"alpha-001": {Successes: 5, MeanMs: 10},
			"beta-001":  {Successes: 5, MeanMs: 20},
		},
		freed: filterer.freed,
		want:  int64(len(sources)),
	}
	holder := stable.NewHolder()
	c := stable.NewChecker(
		stable.CheckerSpec{
			Sources:       sources,
			Interval:      time.Hour,
			Rounds:        5,
			MaxAvgMs:      1000,
			SourceTimeout: time.Minute,
			Prober:        prober,
		},
		func() stable.Filterer { return filterer },
		nil, dead, holder, "", zerolog.Nop(), nil,
	)

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	snap := holder.Load()
	if snap == nil {
		t.Fatal("no snapshot published: the fixture produced nothing to probe")
	}

	return prober, snap, before.HeapAlloc / 1024
}

// TestRunOnceReleasesPoolBeforeProbeReturns pins the lifetime of the
// pre-dedupe pool. bodies is read below the probe only through len(), which
// needs the length word alone; a full-slice read there — one added for a new
// per-source metric, say — holds every source's nodes for the whole probe.
func TestRunOnceReleasesPoolBeforeProbeReturns(t *testing.T) {
	prober, snap, _ := retentionCycle(t, 200, 0, nil)

	if snap.Stats.Merged != 400 {
		t.Fatalf("fixture merged %d nodes, want 400", snap.Stats.Merged)
	}
	if prober.sawFreed != 2 {
		t.Errorf("source pools released during the probe: got %d of 2", prober.sawFreed)
	}
}

// TestRunOnceHeapDuringProbeExcludesMergedPool covers what no cleanup can
// reach: Merge's relabeled entries, which only the merged slice heads. 10000
// nodes of ~2KB make a ~20MB pool and ~20MB of entries, and the probe needs
// neither — it needs the two nodes the dead cache left.
//
// Its observable is the one thing in this package that is not local to its own
// test: runtime.MemStats is process-global. That is acceptable here because
// the test is sequential, its baseline is taken immediately before RunOnce so
// anything already live cancels out, and t.Parallel tests batch after the
// sequential ones. What it cannot cancel is a large allocation going live from
// another goroutine mid-cycle, which is why the failure names that reading too.
func TestRunOnceHeapDuringProbeExcludesMergedPool(t *testing.T) {
	keep := map[string]bool{"a0.example:443": true, "b0.example:443": true}
	prober, snap, baseKiB := retentionCycle(t, 5000, 2000, keepOnly{keep: keep})

	if snap.Stats.Merged != 10000 || snap.Stats.Tested != 2 {
		t.Fatalf("fixture merged %d tested %d, want 10000 and 2", snap.Stats.Merged, snap.Stats.Tested)
	}
	// Measured 829-834 KiB when the pools are released and 42456 KiB when a
	// KeepAlive holds them, so the bound sits 14.7x above the passing figure
	// and 3.5x below the failing one.
	const maxGrowthKiB = 12 * 1024
	if grew := int64(prober.heapKiB) - int64(baseKiB); grew > maxGrowthKiB {
		t.Errorf("heap grew %d KiB across the probe, bound %d KiB (releasing the pools measures ~830 KiB, "+
			"holding them ~42400 KiB).\nEither RunOnce now keeps the merged pool alive across the probe, "+
			"or — since this bound is runtime.MemStats-derived and so process-global — another sequential "+
			"test in this package left tens of MB live during the cycle. The size tells them apart.",
			grew, maxGrowthKiB)
	}
}
