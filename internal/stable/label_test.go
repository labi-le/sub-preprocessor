package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// mieruLine renders a mierus:// share link whose fragment is name. Each
// port/protocol pair mihomo finds here becomes one proxy named
// "<name>:<port>/<protocol>".
func mieruLine(server, name string, portProto ...[2]string) string {
	var b strings.Builder
	b.WriteString("mierus://user:pass@")
	b.WriteString(server)
	b.WriteByte('?')
	for i, pp := range portProto {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString("port=")
		b.WriteString(pp[0])
		b.WriteString("&protocol=")
		b.WriteString(pp[1])
	}
	b.WriteByte('#')
	b.WriteString(name)

	return b.String()
}

// labelProxy is a mihomo.Proxy stub exposing only Name and Type, the two
// methods entryLabel reads. The embedded nil interface panics on anything
// else, which is the point: it pins the read set. It exists because the
// degenerate names entryLabel guards against (no colon, leading colon) are
// shapes mihomo's converter cannot be made to emit, so they are unreachable
// through a real payload.
type labelProxy struct {
	mihomo.Proxy
	name string
	typ  mihomo.AdapterType
}

func (p labelProxy) Name() string             { return p.name }
func (p labelProxy) Type() mihomo.AdapterType { return p.typ }

func TestEntryLabel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		desc string
		name string
		typ  mihomo.AdapterType
		want string
	}{
		{"non-mieru names are the label verbatim", "src-001", mihomo.Vless, "src-001"},
		{
			"a colon in a non-mieru name is part of the label, not a suffix",
			"weird:src-001", mihomo.Vless, "weird:src-001",
		},
		{"mieru sheds its :<port>/<protocol> suffix", "src-001:2999/TCP", mihomo.Mieru, "src-001"},
		{"a port range is still one suffix", "src-001:9998-9999/UDP", mihomo.Mieru, "src-001"},
		{
			"only the LAST colon starts the suffix",
			"weird:src-001:9998-9999/UDP", mihomo.Mieru, "weird:src-001",
		},
		// Guards: folding these would produce "", a label that collides with
		// every entry whose own label is missing.
		{"a colonless mieru name is not folded", "src-001", mihomo.Mieru, "src-001"},
		{"a leading colon is not folded to empty", ":2999/TCP", mihomo.Mieru, ":2999/TCP"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			px := labelProxy{name: c.name, typ: c.typ}
			if got := entryLabel(px); got != c.want {
				t.Errorf("entryLabel(%q, %v) = %q, want %q", c.name, c.typ, got, c.want)
			}
		})
	}
}

// TestEntryLabelFoldsRealMieruProxies pins the upstream shape entryLabel is
// built on: mihomo expands ONE mierus:// link into one Mieru-typed proxy per
// configured port, named "<fragment>:<port>/<protocol>". If a mihomo bump
// changes that naming, this fails here instead of silently un-selecting every
// mieru node in production.
func TestEntryLabelFoldsRealMieruProxies(t *testing.T) {
	t.Parallel()

	pxs := parseTestProxies(t,
		mieruLine("1.2.3.4", "src-001", [2]string{"2999", "TCP"}, [2]string{"9998-9999", "UDP"})+"\n"+
			mieruLine("5.6.7.8", "weird:src-002", [2]string{"3000", "TCP"})+"\n"+
			benchVlessLine("9.9.9.9", "443", "weird:src-003"))

	want := map[string]string{
		"src-001:2999/TCP":       "src-001",
		"src-001:9998-9999/UDP":  "src-001",
		"weird:src-002:3000/TCP": "weird:src-002",
		"weird:src-003":          "weird:src-003",
	}
	if len(pxs) != len(want) {
		t.Fatalf("parsed %d proxies, want %d: %v", len(pxs), len(want), proxyNames(pxs))
	}
	for _, px := range pxs {
		label, known := want[px.Name()]
		if !known {
			t.Fatalf("unexpected proxy name %q; mihomo's mieru naming changed", px.Name())
		}
		if got := entryLabel(px); got != label {
			t.Errorf("entryLabel(%q) = %q, want %q", px.Name(), got, label)
		}
	}
}

func TestFoldProbeResults(t *testing.T) {
	t.Parallel()

	const rounds = 3
	pxs := parseTestProxies(t,
		mieruLine("1.2.3.4", "src-001", [2]string{"2999", "TCP"}, [2]string{"3000", "UDP"},
			[2]string{"3001", "TCP"})+"\n"+
			benchVlessLine("9.9.9.9", "443", "src-002")+"\n"+
			benchVlessLine("8.8.8.8", "443", "src-003"))

	accs := accsByName(t, pxs, map[string]delayAcc{
		// The middle port is the best node: it answered every round.
		"src-001:2999/TCP": {succ: 1, sum: 100},
		"src-001:3000/UDP": {succ: rounds, sum: 900},
		"src-001:3001/TCP": {succ: 2, sum: 200},
		"src-002":          {succ: 2, sum: 500},
		// src-003 never answered: folded as a zero, because absence is no
		// longer what marks a node dead — recordDead reads Successes.
		"src-003": {succ: 0, sum: 0, stage: StageConnect},
	})
	res := foldProbeResults(pxs, accs)

	if len(res) != 3 {
		t.Fatalf("folded to %d results, want src-001, src-002 and src-003: %+v", len(res), res)
	}
	if res["src-003"] != (ProbeResult{Stage: StageConnect}) {
		t.Errorf("a node that never answered must fold to a zero-success entry, got %+v", res["src-003"])
	}
	got, ok := res["src-001"]
	if !ok {
		t.Fatalf("mieru node absent under its bare label; keys = %+v", res)
	}
	if got.Successes != rounds || got.MeanMs != 300 {
		// Summing instead of picking is the failure mode: three ports would
		// give 6 successes over 3 rounds, so SelectSurvivors computes
		// rounds-Successes = -3 and every mieru node walks through maxFail
		// however badly it probed.
		t.Errorf("src-001 = %+v, want the best port {Successes:%d MeanMs:300}", got, rounds)
	}
	if res["src-002"] != (ProbeResult{Successes: 2, MeanMs: 250, Stage: StageUnknown}) {
		t.Errorf("non-mieru result changed: %+v", res["src-002"])
	}
}

// TestFoldProbeResultsTieBreaksOnLatency locks the tiebreak, and locks it
// order-independently: with equal successes the faster port must win whether
// it is first or last in the payload.
func TestFoldProbeResultsTieBreaksOnLatency(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		desc      string
		fastFirst bool
	}{
		{"fast port first", true},
		{"fast port last", false},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			pxs := parseTestProxies(t, mieruLine("1.2.3.4", "src-001",
				[2]string{"2999", "TCP"}, [2]string{"3000", "UDP"}))
			fast, slow := "src-001:2999/TCP", "src-001:3000/UDP"
			if !c.fastFirst {
				fast, slow = slow, fast
			}
			accs := accsByName(t, pxs, map[string]delayAcc{
				fast: {succ: 2, sum: 100},
				slow: {succ: 2, sum: 800},
			})
			if got := foldProbeResults(pxs, accs); got["src-001"].MeanMs != 50 {
				t.Errorf("src-001 = %+v, want the 50ms port", got["src-001"])
			}
		})
	}
}

// TestFoldProbeResultsTieBreaksOnStage locks the LAST tiebreak, the only one
// that can separate two ports that both failed: successes and mean are 0 for
// both, so without the stage key a label reports whichever port the payload
// happened to list first. A port that got a tunnel up is not a connect failure.
func TestFoldProbeResultsTieBreaksOnStage(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		desc       string
		fetchFirst bool
	}{
		{"furthest port first", true},
		{"furthest port last", false},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			pxs := parseTestProxies(t, mieruLine("1.2.3.4", "src-001",
				[2]string{"2999", "TCP"}, [2]string{"3000", "UDP"}))
			fetch, connect := "src-001:2999/TCP", "src-001:3000/UDP"
			if !c.fetchFirst {
				fetch, connect = connect, fetch
			}
			accs := accsByName(t, pxs, map[string]delayAcc{
				fetch:   {stage: StageFetch},
				connect: {stage: StageConnect},
			})
			if got := foldProbeResults(pxs, accs); got["src-001"] != (ProbeResult{Stage: StageFetch}) {
				t.Errorf("src-001 = %+v, want the furthest stage (fetch)", got["src-001"])
			}
		})
	}
}

// mieruPort is one port of a mierus:// link as mihomo expands it: a
// Mieru-typed proxy named "<label>:<port>/<protocol>", so entryLabel folds
// several of them onto one key. A live port delegates to an embedded DIRECT
// outbound, which dials the address the probe already put in the metadata; a
// dead one refuses. delay holds the dial open long enough to make this port
// the LAST of the fan-out to reach the fold, so a test can pin either
// completion order instead of hoping for one.
type mieruPort struct {
	mihomo.Proxy // adapter.NewProxy(outbound.NewDirect()) for a live port, nil for a dead one
	name, addr   string
	delay        time.Duration
}

func (p *mieruPort) Name() string             { return p.name }
func (p *mieruPort) Type() mihomo.AdapterType { return mihomo.Mieru }
func (p *mieruPort) Addr() string             { return p.addr }

func (p *mieruPort) DialContext(ctx context.Context, meta *mihomo.Metadata) (mihomo.Conn, error) { //nolint:ireturn // implements mihomo.Proxy; the signature is not ours to choose
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.Proxy == nil {
		return nil, errors.New("port refused by test")
	}

	return p.Proxy.DialContext(ctx, meta)
}

// TestThroughNodeChecksFoldToTheLivePort enters the real write-time folds in
// apiCheck and BandwidthCheck with two ports of one label whose outcomes
// DIFFER, which is the only way to observe a fold at all: strip both
// conditions back to last-write-wins and every other test in this package
// still passes, because their ports all fail identically.
//
// Each case pins one completion order outright — the delayed port is the one
// last-write-wins would keep — and both must fold to the LIVE port: reachable
// for the API check, a measured rate for the bandwidth check.
func TestThroughNodeChecksFoldToTheLivePort(t *testing.T) {
	t.Parallel()

	const (
		label = "src-001"
		// Far beyond a loopback request, so "which port finished last" is a
		// decision this test makes rather than one the scheduler makes.
		lateBy = 150 * time.Millisecond
		// Enough bytes that the transfer is timed as payload, not as noise.
		bodyLen = 64 << 10
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			// measure() starts its clock once the headers land, so flushing
			// them before the body keeps the measured interval above zero and
			// the computed Mbps off the floor.
			flusher.Flush()
			time.Sleep(2 * time.Millisecond)
		}
		_, _ = w.Write(make([]byte, bodyLen))
	}))
	t.Cleanup(srv.Close)

	for _, c := range []struct {
		desc      string
		liveDelay time.Duration
		deadDelay time.Duration
	}{
		{"dead port folds in last", 0, lateBy},
		{"live port folds in last", lateBy, 0},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			pxs := []mihomo.Proxy{
				&mieruPort{
					Proxy: adapter.NewProxy(outbound.NewDirect()),
					name:  label + ":2999/TCP", addr: "1.2.3.4:2999", delay: c.liveDelay,
				},
				&mieruPort{name: label + ":9998-9999/UDP", addr: "1.2.3.4:9998", delay: c.deadDelay},
			}
			const timeout = 5 * time.Second
			m := testProberWith(t, config.BandwidthConfig{
				TestURL: srv.URL, Timeout: timeout, Concurrency: 2,
			})
			ctx := context.Background()

			api := m.apiCheck(ctx, "test.api", "api", pxs, srv.URL, nil,
				timeout, 2, func(int, string) bool { return false })
			if len(api) != 1 {
				t.Fatalf("apiCheck outcomes keyed %v, want the single label %s", mapKeys(api), label)
			}
			if got := api[label]; !got.Reachable || got.Blocked {
				t.Errorf("apiCheck folded %s to %+v, want the live port's reachable outcome", label, got)
			}

			bw := m.BandwidthCheck(ctx, pxs)
			if len(bw) != 1 {
				t.Fatalf("BandwidthCheck outcomes keyed %v, want the single label %s", mapKeys(bw), label)
			}
			if got := bw[label]; !got.Reachable || got.Mbps <= 0 {
				t.Errorf("BandwidthCheck folded %s to %+v, want the live port's measurement", label, got)
			}
		})
	}
}

// TestFilterMieruSurvivorEntersSubset is the end-to-end regression: with
// the shared proxy map keyed by proxy name, the filter stage's subset selection
// (proxies[s.Label]) never finds a mieru survivor, so it is never checked and
// the zero-value outcome drops it as unreachable. The vless survivor beside it
// pins that the exact-match path is untouched.
func TestFilterMieruSurvivorEntersSubset(t *testing.T) {
	t.Parallel()

	prober := testProber(t)
	mieru := mieruLine("1.2.3.4", "src-001", [2]string{"2999", "TCP"})
	vless := benchVlessLine("9.9.9.9", "443", "src-002")
	survivors := []Survivor{
		{Entry: Entry{Label: "src-001", Raw: mieru}},
		{Entry: Entry{Label: "src-002", Raw: vless}},
	}

	var checked []string
	check := func(_ context.Context, given []mihomo.Proxy) map[string]APIOutcome {
		out := make(map[string]APIOutcome, len(given))
		for _, px := range given {
			checked = append(checked, px.Name())
			out[entryLabel(px)] = APIOutcome{Server: px.Addr(), Reachable: true}
		}

		return out
	}
	c := NewChecker(CheckerSpec{
		Prober:  prober,
		Filters: []NodeFilter{&apiFilter{filterName: "test", check: check, logger: zerolog.Nop()}},
	}, nil, nil, nil, nil, "", zerolog.Nop(), nil)

	kept, reports, _, _ := c.filterAndMeasureEgress(context.Background(), c.spec.Load(), survivors)

	if len(checked) != 2 {
		t.Fatalf("check saw %v, want one proxy per survivor (the mieru node must reach the subset)", checked)
	}
	if checked[0] != "src-001:2999/TCP" {
		t.Errorf("mieru proxy entered the subset as %q, want its mihomo name", checked[0])
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d survivors, want both: %+v", len(kept), kept)
	}
	if reports[0].Dropped["unreachable"] != 0 {
		t.Errorf("a checked, reachable node was reported unreachable: %+v", reports[0].Dropped)
	}
}

// TestFilterMieruDeadPortDoesNotVetoLivePort is the fold regression.
// Collapsing the shared proxy map on the PROXY side hands the filter chain
// whichever port mihomo emitted last, so a node the latency probe selected on
// a live port gets measured on a dead one, dropped, and mis-booked as
// unreachable. Every port must reach the subset; the outcome fold then keeps
// the verdict that keeps the node.
func TestFilterMieruDeadPortDoesNotVetoLivePort(t *testing.T) {
	t.Parallel()

	// deadPort is configured LAST, i.e. it is exactly the proxy a proxy-side
	// collapse would have kept.
	const deadPort = "9998-9999"
	mieru := mieruLine("1.2.3.4", "src-001",
		[2]string{"2999", "TCP"}, [2]string{deadPort, "UDP"})
	survivors := []Survivor{{Entry: Entry{Label: "src-001", Raw: mieru}}}

	var subset []string
	api := func(_ context.Context, given []mihomo.Proxy) map[string]APIOutcome {
		subset = proxyNames(given)
		out := make(map[string]APIOutcome, len(given))
		for _, px := range given {
			o := APIOutcome{Server: px.Addr(), Reachable: !strings.Contains(px.Name(), deadPort)}
			label := entryLabel(px)
			if prev, ok := out[label]; !ok || betterAPIOutcome(o, prev) {
				out[label] = o
			}
		}

		return out
	}
	bw := func(_ context.Context, given []mihomo.Proxy) map[string]BandwidthOutcome {
		out := make(map[string]BandwidthOutcome, len(given))
		for _, px := range given {
			o := BandwidthOutcome{Server: px.Addr()}
			if !strings.Contains(px.Name(), deadPort) {
				o.Reachable, o.Mbps = true, 50
			}
			label := entryLabel(px)
			if prev, ok := out[label]; !ok || betterBandwidthOutcome(o, prev) {
				out[label] = o
			}
		}

		return out
	}
	c := NewChecker(CheckerSpec{
		Prober: testProber(t),
		Filters: []NodeFilter{
			&apiFilter{filterName: "test", check: api, logger: zerolog.Nop()},
			&bandwidthFilter{minMbps: 10, check: bw, logger: zerolog.Nop()},
		},
	}, nil, nil, nil, nil, "", zerolog.Nop(), nil)

	kept, reports, _, _ := c.filterAndMeasureEgress(context.Background(), c.spec.Load(), survivors)

	if len(subset) != 2 || !strings.Contains(subset[len(subset)-1], deadPort) {
		t.Fatalf("filter subset was %v, want both ports with the dead one last", subset)
	}
	if len(kept) != 1 {
		t.Fatalf("kept %d survivors, want the node its live port serves: %+v", len(kept), reports)
	}
	if kept[0].Mbps != 50 {
		t.Errorf("kept Mbps %d, want the live port's 50", kept[0].Mbps)
	}
}

// TestBetterAPIOutcomePriority pins the order the ports of one mierus:// link
// are folded in. It compares every ordered pair, so no result can come from
// two cases happening to meet in a convenient order.
func TestBetterAPIOutcomePriority(t *testing.T) {
	t.Parallel()

	// Weakest first: an unreachable port says nothing, a block is a real
	// verdict about the shared host, and a clean response keeps the node.
	ranked := []struct {
		desc string
		o    APIOutcome
	}{
		{"no response", APIOutcome{Server: "h"}},
		{"geo-blocked", APIOutcome{Server: "h", Reachable: true, Blocked: true}},
		{"clean response", APIOutcome{Server: "h", Reachable: true}},
	}
	for i, a := range ranked {
		for j, b := range ranked {
			if got := betterAPIOutcome(a.o, b.o); got != (i > j) {
				t.Errorf("betterAPIOutcome(%s, %s) = %v, want %v", a.desc, b.desc, got, i > j)
			}
		}
	}
}

// TestBetterBandwidthOutcomePriority is TestBetterAPIOutcomePriority for the
// speed fold: reachability first, then throughput.
func TestBetterBandwidthOutcomePriority(t *testing.T) {
	t.Parallel()

	ranked := []struct {
		desc string
		o    BandwidthOutcome
	}{
		// A fabricated rate on an unreachable port must not outrank a real one.
		{"no transfer", BandwidthOutcome{Server: "h", Mbps: 999}},
		{"reachable, slow", BandwidthOutcome{Server: "h", Reachable: true, Mbps: 1}},
		{"reachable, fast", BandwidthOutcome{Server: "h", Reachable: true, Mbps: 50}},
	}
	for i, a := range ranked {
		for j, b := range ranked {
			if got := betterBandwidthOutcome(a.o, b.o); got != (i > j) {
				t.Errorf("betterBandwidthOutcome(%s, %s) = %v, want %v", a.desc, b.desc, got, i > j)
			}
		}
	}
}

func testProber(t *testing.T) *MihomoProber {
	t.Helper()

	return testProberWith(t, config.BandwidthConfig{})
}

// testProberWith builds the prober through its real constructor, so a check
// under test reads the same validated fields production does.
func testProberWith(t *testing.T, bandwidth config.BandwidthConfig) *MihomoProber {
	t.Helper()

	p, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"},
		bandwidth, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// parseTestProxies parses payload through the real mihomo converter+adapter so
// the tests assert against genuine proxy names and adapter types. The proxies
// are never dialled, so they need no Close.
func parseTestProxies(t *testing.T, payload string) []mihomo.Proxy {
	t.Helper()

	pxs, err := testProber(t).ParseProxies([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}

	return pxs
}

func proxyNames(pxs []mihomo.Proxy) []string {
	names := make([]string, len(pxs))
	for i, px := range pxs {
		names[i] = px.Name()
	}

	return names
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}
