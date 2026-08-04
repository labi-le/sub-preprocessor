package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"testing"

	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
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
