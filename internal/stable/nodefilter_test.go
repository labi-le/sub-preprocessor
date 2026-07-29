package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/subscription"
)

func vmessLine(payload string) string {
	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(payload))
}

func TestBandwidthFilterApply(t *testing.T) {
	t.Parallel()

	survivors := []Survivor{
		{Entry: Entry{Label: "s-001", Addr: "h1:443", Tagged: "vless://u@h1:443#[GEO:FI][IP:1.1.1.1] s-001"}},
		{Entry: Entry{Label: "s-002", Addr: "h2:443", Tagged: "vless://u@h2:443#[GEO:SE][IP:2.2.2.2] s-002"}},
		{Entry: Entry{Label: "s-003", Addr: "h3:443", Tagged: "vless://u@h3:443#[GEO:DE][IP:3.3.3.3] s-003"}},
	}
	check := func(context.Context, []mihomo.Proxy) map[string]BandwidthOutcome {
		return map[string]BandwidthOutcome{
			"s-001": {Server: "h1", Reachable: true, Mbps: 50}, // fast -> keep
			"s-002": {Server: "h2", Reachable: true, Mbps: 3},  // slow -> drop
			"s-003": {Server: "h3", Reachable: false},          // unreachable -> drop
		}
	}

	f := &bandwidthFilter{minMbps: 10, annotate: true, check: check, logger: zerolog.Nop()}
	kept, rejected, _ := f.apply(context.Background(), survivors, nil)
	if len(kept) != 1 || kept[0].Label != "s-001" {
		t.Fatalf("expected only s-001 kept, got %+v", kept)
	}
	if kept[0].Mbps != 50 {
		t.Fatalf("Mbps not recorded: %d", kept[0].Mbps)
	}
	if !strings.Contains(kept[0].Tagged, "[SPD:50M]") {
		t.Fatalf("missing speed tag: %q", kept[0].Tagged)
	}
	// Nothing is cached. Both drop reasons here describe our side, not the
	// node: s-003 answered nothing, and s-002's sub-floor speed is measured
	// over a host uplink shared by the concurrent downloads. Caching either
	// would hide the node from /stable.txt for the whole cache TTL -- and
	// because filterDead consults that cache before the probe, a bad minute on
	// our uplink would take the whole batch out for hours.
	if len(rejected) != 0 {
		t.Fatalf("a bandwidth verdict must never be cached, got %v", rejected)
	}

	// annotate=false: kept but no tag injected.
	f2 := &bandwidthFilter{minMbps: 10, annotate: false, check: check, logger: zerolog.Nop()}
	kept2, _, _ := f2.apply(context.Background(), survivors, nil)
	if len(kept2) != 1 || strings.Contains(kept2[0].Tagged, "[SPD:") {
		t.Fatalf("annotate=false must not inject SPD: %q", kept2[0].Tagged)
	}

	// minMbps=0: keep all reachable (no floor).
	f3 := &bandwidthFilter{minMbps: 0, annotate: false, check: check, logger: zerolog.Nop()}
	if kept3, _, _ := f3.apply(context.Background(), survivors, nil); len(kept3) != 2 {
		t.Fatalf("minMbps=0 keeps all reachable, got %d", len(kept3))
	}

	// cancelled ctx: no-op, survivors unchanged.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, _, _ := f.apply(ctx, survivors, nil); len(got) != len(survivors) {
		t.Fatalf("cancelled ctx must pass survivors through, got %d", len(got))
	}
}

func TestBandwidthFilterAnnotatesVmess(t *testing.T) {
	t.Parallel()

	// vmess name lives in base64 JSON ps; annotation must go through the
	// vmess-aware relabel path, not fragment surgery.
	vmess := vmessLine(`{"v":"2","ps":"s-001","add":"1.2.3.4","port":"443","id":"uuid","net":"ws"}`)
	survivors := []Survivor{{Entry: Entry{Label: "s-001", Tagged: vmess}}}
	check := func(context.Context, []mihomo.Proxy) map[string]BandwidthOutcome {
		return map[string]BandwidthOutcome{"s-001": {Server: "1.2.3.4", Reachable: true, Mbps: 42}}
	}
	f := &bandwidthFilter{minMbps: 1, annotate: true, check: check, logger: zerolog.Nop()}
	kept, _, _ := f.apply(context.Background(), survivors, nil)
	if len(kept) != 1 {
		t.Fatalf("expected 1 kept, got %d", len(kept))
	}
	// Re-parse the annotated vmess and confirm the ps carries the tag.
	var name string
	subscription.Parse([]byte(kept[0].Tagged), func(n subscription.Node) bool {
		name = n.Name
		return false
	})
	if !strings.Contains(name, "[SPD:42M]") {
		t.Fatalf("vmess ps missing speed tag: %q", name)
	}
}

func TestBuildNodeFilters(t *testing.T) {
	t.Parallel()

	prober, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.BandwidthConfig{}, config.GeoBlockConfig{}, "KEY", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	if fs := buildNodeFilters(nil, prober, nil, true, zerolog.Nop()); len(fs) != 0 {
		t.Fatalf("no names -> no filters, got %d", len(fs))
	}

	fs := buildNodeFilters([]string{"gemini", "claude", "chatgpt", "tidal", "bandwidth", "bogus"}, prober, nil, true, zerolog.Nop())
	if len(fs) != 5 {
		t.Fatalf("gemini + claude + chatgpt + tidal + bandwidth + unknown -> 5 filters, got %d", len(fs))
	}
	if fs[2].name() != "chatgpt" {
		t.Fatalf("expected chatgpt filter third, got %q", fs[2].name())
	}
	if fs[3].name() != "tidal" {
		t.Fatalf("expected tidal filter fourth, got %q", fs[3].name())
	}
	if fs[4].name() != "bandwidth" {
		t.Fatalf("expected bandwidth filter fifth, got %q", fs[4].name())
	}
}

// stubBlocklist is a non-nil Blocklist; the assertions below only care whether
// a filter got one at all.
type stubBlocklist struct{}

func (stubBlocklist) Block(string) error { return nil }
func (stubBlocklist) Prune() error       { return nil }

// TestTidalFilterKeepsNoStore locks a deliberate asymmetry: the tidal gate never
// persists a drop. Its verdict is a bare status code, weaker than the AI checks'
// explicit refusal markers, and the store is host-keyed for its whole TTL — one
// CDN hiccup would otherwise evict the node from every endpoint. It still
// reports the refusal as a short-lived reject, which is where a fail-closed
// verdict belongs.
func TestTidalFilterKeepsNoStore(t *testing.T) {
	t.Parallel()

	prober, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.BandwidthConfig{}, config.GeoBlockConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	fs := buildNodeFilters([]string{"claude", "tidal"}, prober, stubBlocklist{}, false, zerolog.Nop())
	if len(fs) != 2 {
		t.Fatalf("claude + tidal -> 2 filters, got %d", len(fs))
	}
	claude, ok := fs[0].(*apiFilter)
	if !ok {
		t.Fatalf("claude filter has type %T, want *apiFilter", fs[0])
	}
	if claude.store == nil {
		t.Fatal("claude must feed the geoblock store")
	}
	tidal, ok := fs[1].(*apiFilter)
	if !ok {
		t.Fatalf("tidal filter has type %T, want *apiFilter", fs[1])
	}
	if tidal.store != nil {
		t.Fatal("tidal must not feed the shared geoblock store")
	}

	// Drive the blocked branch on the filter as built: the `store != nil` guard
	// in apply is all that stands between this path and a nil interface call,
	// and no other test reaches it with a nil store.
	tidal.check = func(context.Context, []mihomo.Proxy) map[string]APIOutcome {
		return map[string]APIOutcome{"s-001": {Server: "h1", Reachable: true, Blocked: true}}
	}
	kept, rejected, rep := tidal.apply(context.Background(),
		[]Survivor{{Entry: Entry{Label: "s-001", Addr: "h1:443"}}}, nil)
	if len(kept) != 0 || rep.Dropped["blocked"] != 1 {
		t.Fatalf("blocked survivor must drop without a store: kept=%d rep=%+v", len(kept), rep)
	}
	// The nil store now governs BOTH persistence paths. tidalBlocked is
	// fail-closed on any non-2xx, so a single 429 from api.tidal.com marks
	// every node in the batch Blocked; filterDead reads the reject cache
	// before the probe, so remembering that would drop the whole batch from
	// /stable.txt for hours over one rate-limit. It drops for this cycle only.
	if len(rejected) != 0 {
		t.Fatalf("a bare-status verdict must not be cached, got %v", rejected)
	}
}

// TestApiFilterDropsSurvivorAbsentFromProxyMap locks the parse-once behavior:
// a survivor whose label is absent from the shared proxy map (e.g. its node
// failed to parse) is excluded from the check subset and dropped as
// unreachable — identical to the old per-filter "unparsable node" drop. The
// fake check is proxy-aware (an outcome exists only for a proxy actually
// passed in), so this asserts the subset selection, not just a missing outcome.
func TestApiFilterDropsSurvivorAbsentFromProxyMap(t *testing.T) {
	t.Parallel()

	prober, err := NewMihomoProber(
		config.CheckConfig{ExpectedStatus: "204"},
		config.BandwidthConfig{}, config.GeoBlockConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	// Real proxies for s-001 and s-002 only; s-003 is intentionally absent.
	payload := benchVlessLine("1.1.1.1", "443", "s-001") + "\n" +
		benchVlessLine("2.2.2.2", "443", "s-002")
	pxs, err := prober.ParseProxies([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	proxies := make(map[string]mihomo.Proxy, len(pxs))
	for _, p := range pxs {
		proxies[p.Name()] = p
	}
	if _, ok := proxies["s-001"]; !ok {
		t.Fatal("setup: s-001 did not parse into the proxy map")
	}

	survivors := []Survivor{
		{Entry: Entry{Label: "s-001", Tagged: benchVlessLine("1.1.1.1", "443", "s-001")}},
		{Entry: Entry{Label: "s-002", Tagged: benchVlessLine("2.2.2.2", "443", "s-002")}},
		{Entry: Entry{Label: "s-003", Tagged: benchVlessLine("3.3.3.3", "443", "s-003")}},
	}
	check := func(_ context.Context, given []mihomo.Proxy) map[string]APIOutcome {
		out := make(map[string]APIOutcome, len(given))
		for _, p := range given {
			out[p.Name()] = APIOutcome{Server: p.Name(), Reachable: true}
		}
		return out
	}
	f := &apiFilter{filterName: "test", check: check, logger: zerolog.Nop()}
	kept, _, _ := f.apply(context.Background(), survivors, proxies)
	if len(kept) != 2 {
		t.Fatalf("expected s-001 and s-002 kept, got %+v", kept)
	}
	for _, s := range kept {
		if s.Label == "s-003" {
			t.Fatal("s-003 (absent from proxy map) must be dropped as unreachable")
		}
	}
}

// staticFilterer returns the same preprocessed body for every source, so a
// cycle runs end to end with no network and a stable merge result.
type staticFilterer struct{ body string }

func (f staticFilterer) Filter(_ context.Context, b *bytes.Buffer, _ preprocess.FilterRequest) (preprocess.Stats, error) {
	b.WriteString(f.body)
	return preprocess.Stats{}, nil
}

// replayProber records every payload it is asked to probe and reports every
// node in it as healthy, so the only thing that can shrink the probe set
// between cycles is the checker's own bookkeeping.
type replayProber struct{ payloads []string }

func (p *replayProber) Probe(_ context.Context, payload []byte) (map[string]ProbeResult, error) {
	p.payloads = append(p.payloads, string(payload))
	res := make(map[string]ProbeResult)
	subscription.Parse(payload, func(n subscription.Node) bool {
		res[n.Name] = ProbeResult{Successes: 5, MeanMs: 10}
		return true
	})
	return res, nil
}

func (p *replayProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

// Addresses carried by the fixture body below; the API check refuses the second.
const (
	rejectFastAddr = "1.1.1.1:443"
	rejectSlowAddr = "2.2.2.2:443"
)

// refuseSlowAddr is the through-node verdict every reject-cache test starts from.
// Merge relabels to <source>-NNN in body order, so the outcomes key on those
// labels rather than the original names.
func refuseSlowAddr(context.Context, []mihomo.Proxy) map[string]APIOutcome {
	return map[string]APIOutcome{
		"alpha-001": {Server: "1.1.1.1", Reachable: true},
		"alpha-002": {Server: "2.2.2.2", Reachable: true, Blocked: true},
	}
}

// storeBackedFilter builds the only kind of filter allowed to seed the reject
// cache: a non-nil store is exactly what "trusted enough to persist a verdict"
// means. enabled is the filter's own precondition (nil = always active).
func storeBackedFilter(enabled func() bool) *apiFilter {
	return &apiFilter{
		filterName: "claude",
		enabled:    enabled,
		check:      refuseSlowAddr,
		store:      &stubBlocklist{},
		logger:     zerolog.Nop(),
	}
}

// rejectCacheChecker wires a checker over one source carrying both addresses.
// The returned spec is a copy, ready to be edited and handed to Reconfigure.
func rejectCacheChecker(filter NodeFilter, params NodeFilterParams) (*Checker, *replayProber, CheckerSpec) {
	prober := &replayProber{}
	spec := CheckerSpec{
		Sources:       []config.SubscriptionSource{{Name: "alpha", URL: "https://alpha.example/sub"}},
		Interval:      time.Hour,
		Rounds:        5,
		MaxAvgMs:      1000,
		SourceTimeout: time.Minute,
		Prober:        prober,
		Filters:       []NodeFilter{filter},
		FilterParams:  params,
	}
	filterer := staticFilterer{
		body: "vless://u@" + rejectFastAddr + "#fast\nvless://u@" + rejectSlowAddr + "#slow\n",
	}

	return NewChecker(spec, func() Filterer { return filterer }, nil, nil, NewHolder(), zerolog.Nop(), nil), prober, spec
}

func runCycle(t *testing.T, c *Checker, label string) {
	t.Helper()
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

// TestCheckerCachesFilterRejects: a through-node rejection used to be
// unrepeatable knowledge. recordDead can only mark nodes ABSENT from the probe
// results, and a filter reject is by construction present (it passed the
// latency probe), so the API checks re-ran their full test on the same
// known-bad nodes every hour forever. Here the refused node must be probed
// once and then skipped, while the accepted one keeps being probed.
//
// The filter under test is a store-backed API check on purpose: only a check
// trusted enough to persist a verdict may seed the reject cache. bandwidth and
// tidal are excluded by construction, which their own tests pin.
func TestCheckerCachesFilterRejects(t *testing.T) {
	t.Parallel()

	c, prober, spec := rejectCacheChecker(storeBackedFilter(nil), NodeFilterParams{})
	runCycle(t, c, "cycle 1")
	runCycle(t, c, "cycle 2")

	if len(prober.payloads) != 2 {
		t.Fatalf("want one probe per cycle, got %d", len(prober.payloads))
	}
	if !strings.Contains(prober.payloads[0], rejectSlowAddr) {
		t.Fatalf("cycle 1 must probe the refused node to learn it is refused: %q", prober.payloads[0])
	}
	if strings.Contains(prober.payloads[1], rejectSlowAddr) {
		t.Errorf("cycle 2 must skip the node the API filter already refused: %q", prober.payloads[1])
	}
	if !strings.Contains(prober.payloads[1], rejectFastAddr) {
		t.Errorf("cycle 2 must still probe the node that passed: %q", prober.payloads[1])
	}

	// The cache is scoped to the filter that produced the verdict: drop that
	// filter and its rejects stop suppressing anything, immediately.
	spec.Filters = nil
	c.Reconfigure(spec)
	runCycle(t, c, "cycle 3")
	if !strings.Contains(prober.payloads[2], rejectSlowAddr) {
		t.Errorf("removing the API filter must un-suppress its rejects: %q", prober.payloads[2])
	}
}

// TestReconfigureDropsRejectsOnParamsChange: a name scopes a verdict to a filter,
// not to the parameters it was reached under. Lowering the bandwidth floor is the
// documented remedy when capacity drops, so verdicts surviving that edit would
// make the operator's corrective action look inert: every node rejected under the
// OLD floor stays out of the probe -- and therefore out of /stable.txt -- for the
// rest of the 6h TTL, never re-measured against the new one.
func TestReconfigureDropsRejectsOnParamsChange(t *testing.T) {
	t.Parallel()

	params := NodeFilterParams{
		Names:     []string{"claude", "bandwidth"},
		Bandwidth: config.BandwidthConfig{MinMbps: new(50)},
	}
	c, prober, spec := rejectCacheChecker(storeBackedFilter(nil), params)
	runCycle(t, c, "cycle 1")
	runCycle(t, c, "cycle 2")
	if strings.Contains(prober.payloads[1], rejectSlowAddr) {
		t.Fatalf("cycle 2 must skip the refused node, or the rest proves nothing: %q", prober.payloads[1])
	}

	// A fresh pointer, as a reloaded config would produce: the comparison has to
	// look through it, not at it.
	spec.FilterParams.Bandwidth.MinMbps = new(20)
	c.Reconfigure(spec)
	runCycle(t, c, "cycle 3")
	if !strings.Contains(prober.payloads[2], rejectSlowAddr) {
		t.Errorf("a min_mbps edit must not inherit verdicts measured under the old floor: %q", prober.payloads[2])
	}
}

// TestFilterDeadIgnoresDisabledFilter: a gemini whose key file becomes unreadable
// is rebuilt with enabled() == false, and apply then keeps every survivor. Its
// remembered rejects must stop suppressing as well, or a check nobody performs
// any more keeps nodes out of /stable.txt for the rest of the TTL. Nothing about
// the config changed here, so the params reset cannot cover this case.
func TestFilterDeadIgnoresDisabledFilter(t *testing.T) {
	t.Parallel()

	live := true
	c, prober, _ := rejectCacheChecker(storeBackedFilter(func() bool { return live }), NodeFilterParams{})
	runCycle(t, c, "cycle 1")
	runCycle(t, c, "cycle 2")
	if strings.Contains(prober.payloads[1], rejectSlowAddr) {
		t.Fatalf("cycle 2 must skip the refused node, or the rest proves nothing: %q", prober.payloads[1])
	}

	live = false
	runCycle(t, c, "cycle 3")
	if !strings.Contains(prober.payloads[2], rejectSlowAddr) {
		t.Errorf("a disabled filter drops nobody, so its rejects must not suppress either: %q", prober.payloads[2])
	}
}
