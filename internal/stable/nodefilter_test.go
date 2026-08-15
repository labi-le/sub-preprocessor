package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"testing"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/preprocess"
)

func TestBandwidthFilterApply(t *testing.T) {
	t.Parallel()

	survivors := []Survivor{
		{Entry: Entry{Label: "s-001", Addr: "h1:443", Raw: "vless://u@h1:443#s-001"}},
		{Entry: Entry{Label: "s-002", Addr: "h2:443", Raw: "vless://u@h2:443#s-002"}},
		{Entry: Entry{Label: "s-003", Addr: "h3:443", Raw: "vless://u@h3:443#s-003"}},
	}
	check := func(context.Context, []mihomo.Proxy) map[string]BandwidthOutcome {
		return map[string]BandwidthOutcome{
			"s-001": {Server: "h1", Reachable: true, Mbps: 50}, // fast -> keep
			"s-002": {Server: "h2", Reachable: true, Mbps: 3},  // slow -> drop
			"s-003": {Server: "h3", Reachable: false},          // unreachable -> drop
		}
	}

	f := &bandwidthFilter{minMbps: 10, check: check, logger: zerolog.Nop()}
	kept, _ := f.apply(context.Background(), survivors, nil)
	if len(kept) != 1 || kept[0].Label != "s-001" {
		t.Fatalf("expected only s-001 kept, got %+v", kept)
	}
	// The measurement is all this filter publishes; the [SPD:] tag it feeds is
	// built once, at publication.
	if kept[0].Mbps != 50 {
		t.Fatalf("Mbps not recorded: %d", kept[0].Mbps)
	}

	// minMbps=0: keep all reachable (no floor).
	f3 := &bandwidthFilter{minMbps: 0, check: check, logger: zerolog.Nop()}
	if kept3, _ := f3.apply(context.Background(), survivors, nil); len(kept3) != 2 {
		t.Fatalf("minMbps=0 keeps all reachable, got %d", len(kept3))
	}

	// cancelled ctx: no-op, survivors unchanged.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, _ := f.apply(ctx, survivors, nil); len(got) != len(survivors) {
		t.Fatalf("cancelled ctx must pass survivors through, got %d", len(got))
	}
}

func TestBuildNodeFilters(t *testing.T) {
	t.Parallel()

	prober, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "KEY", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	if fs := buildNodeFilters(nil, prober, nil, zerolog.Nop()); len(fs) != 0 {
		t.Fatalf("no names -> no filters, got %d", len(fs))
	}

	fs := buildNodeFilters([]string{"gemini", "claude", "chatgpt", "tidal", "bandwidth", "bogus"}, prober, nil, zerolog.Nop())
	if len(fs) != 5 {
		t.Fatalf("gemini + claude + chatgpt + tidal + bandwidth + unknown -> 5 filters, got %d", len(fs))
	}
	// Concrete type, not just the name: only *geminiFilter carries the gate's
	// verification account through to the report, and a plain *apiFilter here
	// would still pass every name and behaviour assertion in this file.
	if _, ok := fs[0].(*geminiFilter); !ok {
		t.Fatalf("gemini filter has type %T, want *geminiFilter", fs[0])
	}
	if got := builtFilterName(fs[2]); got != "chatgpt" {
		t.Fatalf("expected chatgpt filter third, got %q", got)
	}
	if got := builtFilterName(fs[3]); got != "tidal" {
		t.Fatalf("expected tidal filter fourth, got %q", got)
	}
	if got := builtFilterName(fs[4]); got != "bandwidth" {
		t.Fatalf("expected bandwidth filter fifth, got %q", got)
	}
}

// builtFilterName identifies a built filter for the order assertions above.
// NodeFilter carries no name accessor -- nothing in production needs one, the
// per-filter report name is set inside apply -- so the test reads the concrete
// types buildNodeFilters returns.
func builtFilterName(f NodeFilter) string {
	switch v := f.(type) {
	case *geminiFilter:
		return v.filterName
	case *apiFilter:
		return v.filterName
	case *bandwidthFilter:
		return bandwidthFilterName
	default:
		return ""
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
// CDN hiccup would otherwise evict the node from every endpoint. Its refusal
// therefore costs the node exactly this cycle.
func TestTidalFilterKeepsNoStore(t *testing.T) {
	t.Parallel()

	prober, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	fs := buildNodeFilters([]string{"claude", "tidal"}, prober, stubBlocklist{}, zerolog.Nop())
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
	kept, rep := tidal.apply(context.Background(),
		[]Survivor{{Entry: Entry{Label: "s-001", Addr: "h1:443"}}}, nil)
	if len(kept) != 0 || rep.Dropped["blocked"] != 1 {
		t.Fatalf("blocked survivor must drop without a store: kept=%d rep=%+v", len(kept), rep)
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
		config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
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
	proxies := make(map[string][]mihomo.Proxy, len(pxs))
	for _, p := range pxs {
		label := entryLabel(p)
		proxies[label] = append(proxies[label], p)
	}
	if _, ok := proxies["s-001"]; !ok {
		t.Fatal("setup: s-001 did not parse into the proxy map")
	}

	survivors := []Survivor{
		{Entry: Entry{Label: "s-001", Raw: benchVlessLine("1.1.1.1", "443", "s-001")}},
		{Entry: Entry{Label: "s-002", Raw: benchVlessLine("2.2.2.2", "443", "s-002")}},
		{Entry: Entry{Label: "s-003", Raw: benchVlessLine("3.3.3.3", "443", "s-003")}},
	}
	check := func(_ context.Context, given []mihomo.Proxy) map[string]APIOutcome {
		out := make(map[string]APIOutcome, len(given))
		for _, p := range given {
			out[p.Name()] = APIOutcome{Server: p.Name(), Reachable: true}
		}
		return out
	}
	f := &apiFilter{filterName: "test", check: check, logger: zerolog.Nop()}
	kept, _ := f.apply(context.Background(), survivors, proxies)
	if len(kept) != 2 {
		t.Fatalf("expected s-001 and s-002 kept, got %+v", kept)
	}
	for _, s := range kept {
		if s.Label == "s-003" {
			t.Fatal("s-003 (absent from proxy map) must be dropped as unreachable")
		}
	}
}

// fakeGeminiChecker drives the gemini gate with no network: enabled selects
// apiFilter.apply's disabled branch, and rep is the account GeminiCheck hands
// back. calls proves the disabled branch never reaches the check at all.
type fakeGeminiChecker struct {
	outcomes map[string]APIOutcome
	rep      GeminiReport
	enabled  bool
	calls    int
}

func (f *fakeGeminiChecker) GeminiEnabled() bool { return f.enabled }

func (f *fakeGeminiChecker) GeminiCheck(context.Context, []mihomo.Proxy) (map[string]APIOutcome, GeminiReport) {
	f.calls++
	return f.outcomes, f.rep
}

// TestGeminiFilterKeepsUnverifiedOutOfDropped is point 2 of the agreement made
// executable. These nodes are KEPT and published; FilterReport.Dropped renders
// as stable_filter_dropped_nodes{reason=...}, so a count landing there tells
// an operator the gate threw away what it actually let through — the defect
// corrected/unanswered already shipped once (shipped by b545d0a, corrected in
// e554307).
func TestGeminiFilterKeepsUnverifiedOutOfDropped(t *testing.T) {
	t.Parallel()

	const unverified = 22
	gc := &fakeGeminiChecker{
		enabled:  true,
		outcomes: map[string]APIOutcome{"s-001": {Server: "h1", Reachable: true}},
		rep:      GeminiReport{State: GeminiGateRan, Checks: 306, Unverified: unverified},
	}
	f := newGeminiFilter(gc, nil, zerolog.Nop())

	kept, rep := f.apply(context.Background(), []Survivor{{Entry: Entry{Label: "s-001", Addr: "h1:443"}}}, nil)

	if len(kept) != 1 {
		t.Fatalf("an unverified node is KEPT, not dropped: kept %d", len(kept))
	}
	if got := f.verification(); got != gc.rep {
		t.Fatalf("verification() = %+v, want %+v", got, gc.rep)
	}
	for reason, n := range rep.Dropped {
		if n == unverified {
			t.Fatalf("the unverified count reached Dropped[%q]; it renders as a drop reason", reason)
		}
	}
	if len(rep.Dropped) != 2 || rep.Dropped[dropBlocked] != 0 || rep.Dropped[dropUnreachable] != 0 {
		t.Fatalf("gemini must report only its two drop reasons, both zero here: %+v", rep.Dropped)
	}
}

// TestGeminiFilterDisabledIsNotAGateThatRanClean covers the state the metric
// exists for. A keyless gate checks NOTHING and passes every survivor through,
// which from outside looks exactly like a gate that verified them all; the two
// must not reach the report as the same value. The second apply also pins that
// last cycle's numbers are not republished when this cycle never checked.
func TestGeminiFilterDisabledIsNotAGateThatRanClean(t *testing.T) {
	t.Parallel()

	survivors := []Survivor{{Entry: Entry{Label: "s-001", Addr: "h1:443"}}}
	gc := &fakeGeminiChecker{
		enabled:  true,
		outcomes: map[string]APIOutcome{"s-001": {Server: "h1", Reachable: true}},
		rep:      GeminiReport{State: GeminiGateRan, Checks: 306, Unverified: 22},
	}
	f := newGeminiFilter(gc, nil, zerolog.Nop())
	if _, _ = f.apply(context.Background(), survivors, nil); f.verification().State != GeminiGateRan {
		t.Fatalf("setup: an enabled gate must report GeminiGateRan, got %+v", f.verification())
	}

	gc.enabled = false
	kept, _ := f.apply(context.Background(), survivors, nil)

	if len(kept) != 1 {
		t.Fatalf("a skipped gate passes survivors through: kept %d", len(kept))
	}
	if gc.calls != 1 {
		t.Fatalf("the disabled branch must not call the check: calls = %d", gc.calls)
	}
	want := GeminiReport{State: GeminiGateSkipped}
	if got := f.verification(); got != want {
		t.Fatalf("verification() = %+v, want %+v (never last cycle's numbers)", got, want)
	}
}

// oneNodeFilterer and oneNodeProber are the smallest pipeline that reaches
// publication: one node, probed clean, so RunOnce gets as far as Observe.
type oneNodeFilterer struct{}

func (oneNodeFilterer) FilterNodes(
	context.Context, preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	return []preprocess.NodeResult{{Raw: benchVlessLine("1.1.1.1", "443", "n")}}, preprocess.Stats{}, nil
}

//nolint:ireturn // implements Filterer; handing out the interface is the point
func (oneNodeFilterer) Annotator() preprocess.Annotator { return nil }

type oneNodeProber struct{}

func (oneNodeProber) Probe(context.Context, []byte) (map[string]ProbeResult, error) {
	return map[string]ProbeResult{"src-001": {Successes: 5, MeanMs: 100}}, nil
}

func (oneNodeProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

type cycleRecorder struct{ last *CycleReport }

func (r *cycleRecorder) Observe(c CycleReport) { r.last = &c }
func (r *cycleRecorder) ObserveError()         {}

// TestGeminiAccountReachesTheCycleReport walks the whole seam the metric rides:
// filter -> filterAndMeasureEgress -> CycleReport -> Reporter. Each hand-off
// can drop the account without failing anything, because a lost one is the
// zero GeminiReport and that renders as a gate that never ran -- so the
// assertion is on what a Reporter actually receives, not on the filter's own
// verification().
func TestGeminiAccountReachesTheCycleReport(t *testing.T) {
	t.Parallel()

	want := GeminiReport{State: GeminiGateRan, Checks: 306, Unverified: 22}
	gc := &fakeGeminiChecker{
		enabled:  true,
		outcomes: map[string]APIOutcome{"src-001": {Server: "1.1.1.1", Reachable: true}},
		rep:      want,
	}
	rec := &cycleRecorder{}
	c := NewChecker(CheckerSpec{
		Sources:       []config.SubscriptionSource{{Name: "src", Body: benchVlessLine("1.1.1.1", "443", "n")}},
		Interval:      time.Hour,
		Rounds:        5,
		MaxAvgMs:      1000,
		SourceTimeout: time.Minute,
		Prober:        oneNodeProber{},
		Filters:       []NodeFilter{newGeminiFilter(gc, nil, zerolog.Nop())},
	}, func() Filterer { return oneNodeFilterer{} }, nil, nil, NewHolder(), "", zerolog.Nop(), rec)

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rec.last == nil {
		t.Fatal("a published cycle must reach the Reporter")
	}
	if rec.last.Gemini != want {
		t.Fatalf("CycleReport.Gemini = %+v, want %+v", rec.last.Gemini, want)
	}
}

// TestKeylessGateReachesTheReportAsSkipped is the same walk with nothing faked
// between the config name and the report: a real keyless MihomoProber through
// the real buildNodeFilters. This is the production shape of the failure --
// the key resolves to "", the filter is built anyway, and the cycle publishes
// every survivor unverified -- and it must arrive as Skipped, never as the
// zero value a cycle without a gemini gate at all produces.
func TestKeylessGateReachesTheReportAsSkipped(t *testing.T) {
	t.Parallel()

	keyless, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"},
		config.BandwidthConfig{}, config.GeoBlockConfig{}, config.CloudflareConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if keyless.GeminiEnabled() {
		t.Fatal("setup: an empty key must disable the gate")
	}

	rec := &cycleRecorder{}
	c := NewChecker(CheckerSpec{
		Sources:       []config.SubscriptionSource{{Name: "src", Body: benchVlessLine("1.1.1.1", "443", "n")}},
		Interval:      time.Hour,
		Rounds:        5,
		MaxAvgMs:      1000,
		SourceTimeout: time.Minute,
		Prober:        oneNodeProber{},
		Filters:       buildNodeFilters([]string{geminiFilterName}, keyless, nil, zerolog.Nop()),
	}, func() Filterer { return oneNodeFilterer{} }, nil, nil, NewHolder(), "", zerolog.Nop(), rec)

	if err = c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rec.last == nil {
		t.Fatal("a skipped gate still publishes: the survivors passed through")
	}
	if rec.last.Kept != 1 {
		t.Fatalf("a skipped gate drops nobody, Kept = %d", rec.last.Kept)
	}
	want := GeminiReport{State: GeminiGateSkipped}
	if rec.last.Gemini != want {
		t.Fatalf("CycleReport.Gemini = %+v, want %+v", rec.last.Gemini, want)
	}
}

// dropLabelFilter drops exactly the labels named and needs no proxies, so a
// cycle test can tell a filter drop from a probe drop.
type dropLabelFilter map[string]bool

func (f dropLabelFilter) apply(
	_ context.Context, survivors []Survivor, _ map[string][]mihomo.Proxy,
) ([]Survivor, FilterReport) {
	rep := FilterReport{Name: "drop", In: len(survivors), Dropped: map[string]int{}}
	kept := make([]Survivor, 0, len(survivors))
	for _, s := range survivors {
		if f[s.Label] {
			rep.Dropped[dropBlocked]++

			continue
		}
		kept = append(kept, s)
	}
	rep.Kept = len(kept)

	return kept, rep
}

type threeNodeFilterer struct{}

func (threeNodeFilterer) FilterNodes(
	context.Context, preprocess.FilterRequest,
) ([]preprocess.NodeResult, preprocess.Stats, error) {
	return []preprocess.NodeResult{
		{Raw: benchVlessLine("192.0.2.1", "443", "n1")},
		{Raw: benchVlessLine("192.0.2.2", "443", "n2")},
		{Raw: benchVlessLine("192.0.2.3", "443", "n3")},
	}, preprocess.Stats{Total: 3, Kept: 3}, nil
}

//nolint:ireturn // implements Filterer; handing out the interface is the point
func (threeNodeFilterer) Annotator() preprocess.Annotator { return nil }

// twoOfThreeProber fails the third node by omission, which is how a prober
// reports a node that never answered a round (see SelectSurvivors).
type twoOfThreeProber struct{}

func (twoOfThreeProber) Probe(context.Context, []byte) (map[string]ProbeResult, error) {
	return map[string]ProbeResult{
		"src-001": {Successes: 5, MeanMs: 100},
		"src-002": {Successes: 5, MeanMs: 100},
	}, nil
}

func (twoOfThreeProber) ParseProxies([]byte) ([]mihomo.Proxy, error) { return nil, nil }

// TestSourceStageCountsSeparateTheTwoCuts walks the seam the two new columns
// ride: SelectSurvivors -> filterAndMeasureEgress -> CycleReport -> Reporter.
// The three nodes of the one source take the three possible routes, so a count
// taken at the wrong cut point cannot pass: the probed-out node must be in
// neither column, and the filtered-out one in Tested only.
func TestSourceStageCountsSeparateTheTwoCuts(t *testing.T) {
	t.Parallel()

	rec := &cycleRecorder{}
	c := NewChecker(CheckerSpec{
		Sources:       []config.SubscriptionSource{{Name: "src", Body: benchVlessLine("192.0.2.1", "443", "n")}},
		Interval:      time.Hour,
		Rounds:        5,
		MaxAvgMs:      1000,
		SourceTimeout: time.Minute,
		Prober:        twoOfThreeProber{},
		Filters:       []NodeFilter{dropLabelFilter{"src-002": true}},
	}, func() Filterer { return threeNodeFilterer{} }, nil, nil, NewHolder(), "", zerolog.Nop(), rec)

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rec.last == nil {
		t.Fatal("a published cycle must reach the Reporter")
	}
	if len(rec.last.Sources) != 1 {
		t.Fatalf("CycleReport.Sources = %+v, want the one configured source", rec.last.Sources)
	}
	got := rec.last.Sources[0]
	if got.Valid != 3 || got.Tested != 2 || got.Filtered != 1 {
		t.Fatalf("source stage counts = valid %d tested %d filtered %d, want 3/2/1",
			got.Valid, got.Tested, got.Filtered)
	}
}
