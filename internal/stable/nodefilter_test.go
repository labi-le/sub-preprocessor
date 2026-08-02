package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
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
	kept, _ := f.apply(context.Background(), survivors, nil)
	if len(kept) != 1 || kept[0].Label != "s-001" {
		t.Fatalf("expected only s-001 kept, got %+v", kept)
	}
	if kept[0].Mbps != 50 {
		t.Fatalf("Mbps not recorded: %d", kept[0].Mbps)
	}
	if !strings.Contains(kept[0].Tagged, "[SPD:50M]") {
		t.Fatalf("missing speed tag: %q", kept[0].Tagged)
	}

	// annotate=false: kept but no tag injected.
	f2 := &bandwidthFilter{minMbps: 10, annotate: false, check: check, logger: zerolog.Nop()}
	kept2, _ := f2.apply(context.Background(), survivors, nil)
	if len(kept2) != 1 || strings.Contains(kept2[0].Tagged, "[SPD:") {
		t.Fatalf("annotate=false must not inject SPD: %q", kept2[0].Tagged)
	}

	// minMbps=0: keep all reachable (no floor).
	f3 := &bandwidthFilter{minMbps: 0, annotate: false, check: check, logger: zerolog.Nop()}
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
	kept, _ := f.apply(context.Background(), survivors, nil)
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

	fs := buildNodeFilters([]string{"gemini", "claude", "chatgpt", "tidal", "geotrace", "bandwidth", "bogus"}, prober, nil, true, zerolog.Nop())
	if len(fs) != 6 {
		t.Fatalf("gemini + claude + chatgpt + tidal + geotrace + bandwidth + unknown -> 6 filters, got %d", len(fs))
	}
	if got := builtFilterName(fs[2]); got != "chatgpt" {
		t.Fatalf("expected chatgpt filter third, got %q", got)
	}
	if got := builtFilterName(fs[3]); got != "tidal" {
		t.Fatalf("expected tidal filter fourth, got %q", got)
	}
	if got := builtFilterName(fs[4]); got != "geotrace" {
		t.Fatalf("expected geotrace filter fifth, got %q", got)
	}
	if got := builtFilterName(fs[5]); got != "bandwidth" {
		t.Fatalf("expected bandwidth filter sixth, got %q", got)
	}
}

// builtFilterName identifies a built filter for the order assertions above.
// NodeFilter carries no name accessor -- nothing in production needs one, the
// per-filter report name is set inside apply -- so the test reads the concrete
// types buildNodeFilters returns.
func builtFilterName(f NodeFilter) string {
	switch v := f.(type) {
	case *apiFilter:
		return v.filterName
	case *geotraceFilter:
		return geotraceFilterName
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
	proxies := make(map[string][]mihomo.Proxy, len(pxs))
	for _, p := range pxs {
		label := entryLabel(p)
		proxies[label] = append(proxies[label], p)
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
