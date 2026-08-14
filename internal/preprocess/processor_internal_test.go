package preprocess

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/asn"
	"domains.lst/sub-preprocessor/internal/cidrset"
	"domains.lst/sub-preprocessor/internal/config"
	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
	"domains.lst/sub-preprocessor/internal/resolver"
	"domains.lst/sub-preprocessor/internal/subscription"
)

func TestShouldReloadGeofeedLocked(t *testing.T) {
	t.Parallel()

	svc := &Processor{refreshInterval: time.Hour, loadedAt: time.Now().Add(-2 * time.Hour)}
	if !svc.shouldReloadGeofeedLocked(time.Now()) {
		t.Fatal("expected geofeed reload")
	}

	svc = &Processor{refreshInterval: time.Hour, loadedAt: time.Now().Add(-30 * time.Minute)}
	if svc.shouldReloadGeofeedLocked(time.Now()) {
		t.Fatal("did not expect geofeed reload")
	}

	svc = &Processor{refreshInterval: 0, loadedAt: time.Now().Add(-24 * time.Hour)}
	if svc.shouldReloadGeofeedLocked(time.Now()) {
		t.Fatal("did not expect geofeed reload when refresh interval disabled")
	}

	// A pending retry gates the next attempt in both directions: the data is
	// long past its interval, but the attempt that would refresh it just failed.
	now := time.Now()
	svc = &Processor{
		refreshInterval: 24 * time.Hour,
		loadedAt:        now.Add(-25 * time.Hour),
		retryAt:         now.Add(reloadRetryInterval),
	}
	if svc.shouldReloadGeofeedLocked(now) {
		t.Fatal("did not expect a reload while the retry is pending")
	}
	if !svc.shouldReloadGeofeedLocked(now.Add(reloadRetryInterval)) {
		t.Fatal("expected a reload once the retry is due")
	}
}

// recordingCompactFilter mimics ASNFilter: it records the IPs it was handed
// and compacts them in place, dropping the first IP.
type recordingCompactFilter struct {
	seen [][]netip.Addr
}

func (f *recordingCompactFilter) Process(_ context.Context, ips []netip.Addr, _ *PipelineContext) []netip.Addr {
	f.seen = append(f.seen, append([]netip.Addr(nil), ips...))
	n := 0
	for _, ip := range ips[1:] {
		ips[n] = ip
		n++
	}
	return ips[:n]
}

func TestProcessNodeKeepsCachedResolvedSlicePristine(t *testing.T) {
	t.Parallel()

	ipA := netip.MustParseAddr("192.0.2.1")
	ipB := netip.MustParseAddr("192.0.2.2")
	full := []netip.Addr{ipA, ipB}

	f := &recordingCompactFilter{}
	p := &Processor{filters: []Filter{f}}

	pctx := &PipelineContext{
		sink:     &bufferSink{buf: &bytes.Buffer{}},
		Resolved: map[string][]netip.Addr{"example.com": {ipA, ipB}},
		Stats:    &Stats{},
	}
	node := subscription.Node{Raw: "vless://u@example.com:443#X", Server: "example.com", Port: "443"}

	p.processNode(context.Background(), node, pctx)
	p.processNode(context.Background(), node, pctx)

	if len(f.seen) != 2 {
		t.Fatalf("expected filter to run twice, ran %d times", len(f.seen))
	}
	if !slices.Equal(f.seen[0], full) {
		t.Fatalf("first node saw %v, want %v", f.seen[0], full)
	}
	if !slices.Equal(f.seen[1], full) {
		t.Fatalf("second node saw dirty cached slice %v, want %v", f.seen[1], full)
	}
	if !slices.Equal(pctx.Resolved["example.com"], full) {
		t.Fatalf("cached resolved slice mutated to %v, want %v", pctx.Resolved["example.com"], full)
	}
}

func TestProcessBodyCancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := &Processor{}
	pctx := &PipelineContext{
		sink:     &bufferSink{buf: &bytes.Buffer{}},
		Resolved: map[string][]netip.Addr{},
		Stats:    &Stats{},
	}
	body := []byte("vless://u@example.com:443#A\nvless://u@example.org:443#B")

	err := p.processBody(ctx, body, pctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request must not succeed with a truncated list, got err=%v", err)
	}
	if pctx.Stats.Kept != 0 {
		t.Fatalf("expected no nodes kept after pre-cancelled ctx, got %d", pctx.Stats.Kept)
	}
}

// TestFilterInlineBodyNoFetch drives Filter with an inline Body request: the
// nodes use bare IP servers so resolveNode handles them without DNS, proving the
// Body path filters directly with no subscription.Load / HTTP fetch. The payload
// is base64-encoded so the subscription.Normalize step in the inline path is
// actually exercised. Filters are empty, so every syntactically valid node with
// a resolvable (bare-IP) server is kept and emitted into the buffer.
func TestFilterInlineBodyNoFetch(t *testing.T) {
	t.Parallel()

	plain := "vless://a@1.1.1.1:443#n1\nvless://b@2.2.2.2:443#n2\nvless://c@3.3.3.3:443#n3\n"
	body := []byte(base64.StdEncoding.EncodeToString([]byte(plain)))
	p := &Processor{
		logger:   zerolog.Nop(),
		resolver: resolver.New(time.Second, "", 0, 0),
	}

	var buf bytes.Buffer
	stats, err := p.Filter(context.Background(), &buf, FilterRequest{
		Body:             body,
		AllowedCountries: filter.All(),
	})
	if err != nil {
		t.Fatalf("inline Filter failed: %v", err)
	}
	if stats.Total != 3 || stats.Kept != 3 {
		t.Fatalf("inline Filter stats = %+v, want total=3 kept=3", stats)
	}
	out := buf.String()
	for _, want := range []string{
		"vless://a@1.1.1.1:443#n1",
		"vless://b@2.2.2.2:443#n2",
		"vless://c@3.3.3.3:443#n3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing node %q; got:\n%s", want, out)
		}
	}
}

func newInlineProcessor() *Processor {
	return &Processor{logger: zerolog.Nop(), resolver: resolver.New(time.Second, "", 0, 0)}
}

// TestFilterNodesMatchesFilter pins both entry points to one pipeline: the
// structural sink must yield exactly the nodes Filter prints, in the same
// order and with the same stats, each carrying the address the filters judged
// it by — the worker's later tags describe THAT address, and a mismatch would
// publish a node under a country nothing checked.
func TestFilterNodesMatchesFilter(t *testing.T) {
	t.Parallel()

	const plain = "vless://a@1.1.1.1:443#n1\nvless://b@2.2.2.2:443#n2\nvless://c@3.3.3.3:443#n3"
	req := func() FilterRequest {
		return FilterRequest{Body: []byte(plain), AllowedCountries: filter.All()}
	}

	var buf bytes.Buffer
	wantStats, err := newInlineProcessor().Filter(context.Background(), &buf, req())
	if err != nil {
		t.Fatalf("Filter failed: %v", err)
	}

	nodes, stats, err := newInlineProcessor().FilterNodes(context.Background(), req())
	if err != nil {
		t.Fatalf("FilterNodes failed: %v", err)
	}
	if stats != wantStats {
		t.Fatalf("FilterNodes stats = %+v, want %+v", stats, wantStats)
	}

	// Bare-IP servers keep the expected addresses readable and the test
	// resolver-free.
	wantIPs := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	lines := strings.Split(buf.String(), "\n")
	if len(nodes) != len(lines) || len(nodes) != len(wantIPs) {
		t.Fatalf("FilterNodes returned %d nodes, Filter printed %d lines", len(nodes), len(lines))
	}
	for i, line := range lines {
		if nodes[i].Raw != line {
			t.Errorf("node %d = %q, Filter printed %q", i, nodes[i].Raw, line)
		}
		if want := netip.MustParseAddr(wantIPs[i]); nodes[i].IP != want {
			t.Errorf("node %d carries IP %v, want %v", i, nodes[i].IP, want)
		}
	}
}

// TestFilterNodesClonesRaw: subscription.Parse hands out views into the source
// body, which the caller is free to reuse or release the moment FilterNodes
// returns. Comparing against a compile-time constant is the point — comparing
// against a string read out beforehand would alias the same bytes and pass.
func TestFilterNodesClonesRaw(t *testing.T) {
	t.Parallel()

	const want = "vless://a@1.1.1.1:443#n1"
	body := []byte(want)

	nodes, _, err := newInlineProcessor().FilterNodes(context.Background(), FilterRequest{
		Body:             body,
		AllowedCountries: filter.All(),
	})
	if err != nil {
		t.Fatalf("FilterNodes failed: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	for i := range body {
		body[i] = 'x'
	}
	if nodes[0].Raw != want {
		t.Fatalf("NodeResult.Raw aliases the source body: %q", nodes[0].Raw)
	}
}

// TestFilterNodesSharesFilterRejections: the two entry points must fail alike,
// or the worker would publish a junk body Filter refuses.
func TestFilterNodesSharesFilterRejections(t *testing.T) {
	t.Parallel()

	if _, _, err := newInlineProcessor().FilterNodes(context.Background(), FilterRequest{
		Body:             []byte("not a node\nstill://"),
		AllowedCountries: filter.All(),
	}); err == nil || err.Error() != "no supported URI nodes found" {
		t.Fatalf("a bodyful of junk must be refused, got err=%v", err)
	}

	if _, _, err := newInlineProcessor().FilterNodes(context.Background(), FilterRequest{
		Body: []byte("vless://a@1.1.1.1:443#n1"),
	}); err == nil {
		t.Fatal("an empty allow set must be refused")
	}

	var big strings.Builder
	for range maxSubscriptionNodes + 1 {
		big.WriteString("vless://u@192.0.2.1:443#n\n")
	}
	if _, _, err := newInlineProcessor().FilterNodes(context.Background(), FilterRequest{
		Body:             []byte(big.String()),
		AllowedCountries: filter.All(),
	}); !errors.Is(err, ErrTooManyNodes) {
		t.Fatalf("the node ceiling must bite on FilterNodes too, got err=%v", err)
	}
}

// geoEntries builds n distinct host entries so the reload guards have a real,
// measurable database size to compare against.
func geoEntries(n int) []geofeed.Entry {
	entries := make([]geofeed.Entry, n)
	for i := range entries {
		entries[i] = geofeed.Entry{
			Prefix:  netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}), 32),
			Country: geofeed.CountryCode{'D', 'E'},
		}
	}
	return entries
}

// geoRanges is geoEntries for the range-based (dbip/registry) databases.
func geoRanges(n int) []geofeed.Range {
	ranges := make([]geofeed.Range, n)
	for i := range ranges {
		addr := netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1})
		ranges[i] = geofeed.Range{Start: addr, End: addr, Country: geofeed.CountryCode{'D', 'E'}}
	}
	return ranges
}

// staleProcessor is a processor holding a full geofeed database that is already
// past its 24h refresh interval, i.e. the state every background reload starts
// from in production.
func staleProcessor(t *testing.T, load func(context.Context) ([]geofeed.Entry, int, error)) *Processor {
	t.Helper()
	return &Processor{
		logger:          zerolog.Nop(),
		countryLookup:   geofeed.NewLookup(geoEntries(1000)),
		loadedAt:        time.Now().Add(-25 * time.Hour),
		refreshInterval: 24 * time.Hour,
		loadEntries:     load,
	}
}

// assertKeptAndRetriesSoon checks the invariant every refused or failed reload
// must hold: the live lookup and its load time are untouched, and the next
// attempt lands within minutes instead of a full refresh interval.
func assertKeptAndRetriesSoon(t *testing.T, p *Processor, want geofeed.CountryLookup, wantAt time.Time) {
	t.Helper()
	if p.countryLookup != want {
		t.Fatal("the existing lookup must be kept")
	}
	if !p.loadedAt.Equal(wantAt) {
		t.Fatalf("loadedAt must still mark the last good data, got %v want %v", p.loadedAt, wantAt)
	}
	now := time.Now()
	if p.shouldReloadGeofeedLocked(now) {
		t.Fatal("the retry must be throttled immediately after the failure")
	}
	if !p.shouldReloadGeofeedLocked(now.Add(reloadRetryInterval + time.Minute)) {
		t.Fatal("the retry must be due within minutes, not a full refresh interval")
	}
}

// TestDoReloadPartialLoadKeepsExistingLookup covers the partial-swap defect: the
// dominant source fails, LoadAll still returns the handful of entries the small
// source yielded with a nil error, and the background swap must refuse it rather
// than replace a full country database with a fraction of one.
func TestDoReloadPartialLoadKeepsExistingLookup(t *testing.T) {
	t.Parallel()

	p := staleProcessor(t, func(context.Context) ([]geofeed.Entry, int, error) {
		return geoEntries(3), 1, nil
	})
	current, loadedAt := p.countryLookup, p.loadedAt

	p.doReload(context.Background())

	assertKeptAndRetriesSoon(t, p, current, loadedAt)
}

// TestDoReloadTotalFailureRetriesShortly covers the second half: a reload where
// every source failed must not push the next attempt out by the whole refresh
// interval (24h in production).
func TestDoReloadTotalFailureRetriesShortly(t *testing.T) {
	t.Parallel()

	p := staleProcessor(t, func(context.Context) ([]geofeed.Entry, int, error) {
		return nil, 2, errors.New("no geofeed entries loaded")
	})
	current, loadedAt := p.countryLookup, p.loadedAt

	p.doReload(context.Background())

	assertKeptAndRetriesSoon(t, p, current, loadedAt)

	// A second consecutive failure backs off instead of re-downloading the
	// feeds every five minutes for as long as the source stays down.
	firstRetry := p.retryAt
	p.doReload(context.Background())
	if !p.retryAt.After(firstRetry.Add(reloadRetryInterval - time.Second)) {
		t.Fatalf("consecutive failures must back off, got %v after %v", p.retryAt, firstRetry)
	}
}

// TestRetryDelayBacksOffAndClamps pins the backoff curve: minutes for the first
// failure, doubling after that, and never worse than the refresh cadence the
// database would have used anyway.
func TestRetryDelayBacksOffAndClamps(t *testing.T) {
	t.Parallel()

	const interval = 24 * time.Hour

	if got := retryDelay(0, interval); got != reloadRetryInterval {
		t.Fatalf("first retry = %v, want %v", got, reloadRetryInterval)
	}
	if got := retryDelay(3, interval); got != 8*reloadRetryInterval {
		t.Fatalf("fourth retry = %v, want %v", got, 8*reloadRetryInterval)
	}
	if got := retryDelay(100, interval); got != interval {
		t.Fatalf("a permanently dead source must clamp to %v, got %v", interval, got)
	}
}

// TestDoReloadCatastrophicShrinkRefused covers the truncated-but-successful
// body: no source reported an error, yet the feed came back a fraction of its
// former size, which only the relative-size guard can catch.
func TestDoReloadCatastrophicShrinkRefused(t *testing.T) {
	t.Parallel()

	p := staleProcessor(t, func(context.Context) ([]geofeed.Entry, int, error) {
		return geoEntries(100), 0, nil
	})
	current, loadedAt := p.countryLookup, p.loadedAt

	p.doReload(context.Background())

	assertKeptAndRetriesSoon(t, p, current, loadedAt)
}

// TestDoReloadCleanLoadSwaps proves the guards do not block the normal path: a
// complete reload replaces the lookup, restamps loadedAt and clears the retry.
func TestDoReloadCleanLoadSwaps(t *testing.T) {
	t.Parallel()

	p := staleProcessor(t, func(context.Context) ([]geofeed.Entry, int, error) {
		return geoEntries(1200), 0, nil
	})
	p.retryAt = time.Now().Add(reloadRetryInterval)
	current, loadedAt := p.countryLookup, p.loadedAt

	p.doReload(context.Background())

	if p.countryLookup == current {
		t.Fatal("a clean reload must swap in the new lookup")
	}
	if !p.loadedAt.After(loadedAt) {
		t.Fatal("a clean reload must restamp loadedAt")
	}
	if !p.retryAt.IsZero() {
		t.Fatal("a clean reload must clear the pending retry")
	}
	if p.shouldReloadGeofeedLocked(time.Now().Add(time.Hour)) {
		t.Fatal("fresh data must not be stale an hour into a 24h interval")
	}
}

// TestGeoDBDoReloadPartialLoadKeepsExistingLookup mirrors the geofeed guard on
// the registry/dbip path, where a single dead RIR would otherwise shrink the
// live database for a whole refresh interval.
func TestGeoDBDoReloadPartialLoadKeepsExistingLookup(t *testing.T) {
	t.Parallel()

	current := geofeed.NewRangeLookup(geoRanges(1000))
	loadedAt := time.Now().Add(-25 * time.Hour)
	db := &geoDB{
		name:     "registry",
		lookup:   current,
		loadedAt: loadedAt,
		interval: 24 * time.Hour,
		load: func(context.Context) ([]geofeed.Range, int, error) {
			return geoRanges(3), 1, nil
		},
	}

	db.doReload(context.Background(), zerolog.Nop())

	if db.lookup != current {
		t.Fatal("partial reload must keep the existing database")
	}
	if !db.loadedAt.Equal(loadedAt) {
		t.Fatalf("loadedAt must still mark the last good data, got %v", db.loadedAt)
	}
	now := time.Now()
	if db.staleLocked(now) {
		t.Fatal("the retry must be throttled immediately after the refused swap")
	}
	if !db.staleLocked(now.Add(reloadRetryInterval + time.Minute)) {
		t.Fatal("the retry must be due within minutes, not a full refresh interval")
	}
}

const (
	refusalPartial = "partial load"
	refusalShrink  = "catastrophic shrink"
)

// TestSwapRefusalSize pins the guard both the geo databases and the allow-list
// swap through. Every caller reaches it after its own successful load, so
// neutering a condition here is invisible to the reload tests: measured,
// turning `failed > 0` into `failed > 99999` left the whole package green.
func TestSwapRefusalSize(t *testing.T) {
	t.Parallel()

	const live = 1000
	// wholeSpace is what 0.0.0.0/0 covers — the size an int cannot hold on a
	// 32-bit build, and the reason the parameters are uint64.
	const wholeSpace uint64 = 1 << 32

	for _, tc := range []struct {
		name    string
		current uint64
		loaded  uint64
		failed  int
		want    string
	}{
		{"a partial load is refused however healthy the sizes look", live, 10 * live, 1, refusalPartial},
		{"a load under the ratio is refused", live, live/2 - 1, 0, refusalShrink},
		{"an empty load is refused", live, 0, 0, refusalShrink},
		{"growth is allowed", live, 10 * live, 0, ""},
		{"a shrink above the ratio is allowed", live, live * 3 / 4, 0, ""},
		{"an unknown current size allows anything", 0, 0, 1, ""},
		{"the whole address space still refuses a collapse", wholeSpace, wholeSpace / 4, 0, refusalShrink},
		{"the whole address space still allows a healthy load", wholeSpace, wholeSpace, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := swapRefusalSize(tc.current, tc.loaded, tc.failed); got != tc.want {
				t.Fatalf("swapRefusalSize(%d, %d, %d) = %q, want %q",
					tc.current, tc.loaded, tc.failed, got, tc.want)
			}
		})
	}
}

// unsizedLookup is a CountryLookup the type system permits and no production
// path builds: the shape lookupLen answers -1 for.
type unsizedLookup struct{}

func (unsizedLookup) LookupCountry(netip.Addr) geofeed.CountryCode { return geofeed.CountryCode{} }

// TestSwapRefusalUnsizedLookups pins both -1 conversions, which TestSwapRefusalSize
// cannot reach because it calls the size-only half directly. Dropping either
// one wraps -1 to 1.8e19 unsigned, which refuses EVERY later swap and freezes
// the database for the process lifetime while the retry keeps rearming.
func TestSwapRefusalUnsizedLookups(t *testing.T) {
	t.Parallel()

	sized := geofeed.NewRangeLookup(geoRanges(1000))
	if got := swapRefusal(unsizedLookup{}, sized, 0); got != "" {
		t.Fatalf("an unsized current lookup is no baseline to protect, got refusal %q", got)
	}
	if got := swapRefusal(sized, unsizedLookup{}, 0); got != refusalShrink {
		t.Fatalf("a replacement that cannot report a size must be refused, got %q", got)
	}
}

// TestProcessBodyEnforcesNodeCeiling pins the bound on how much work one body
// can buy: nodes are resolved serially with a per-hostname DNS timeout, so an
// unbounded node list is what turns a single request into hours of lookups. The
// ceiling has to be enforced before the first node is touched, hence the
// Stats.Total assertion. Bare-IP servers keep the accepted case resolver-free.
func TestProcessBodyEnforcesNodeCeiling(t *testing.T) {
	t.Parallel()

	nodeBody := func(nodes int) []byte {
		var sb strings.Builder
		for range nodes {
			sb.WriteString("vless://u@192.0.2.1:443#n\n")
		}
		return []byte(sb.String())
	}
	newPctx := func() *PipelineContext {
		return &PipelineContext{
			sink:     &bufferSink{buf: &bytes.Buffer{}},
			Resolved: map[string][]netip.Addr{},
			Stats:    &Stats{},
		}
	}

	p := &Processor{}

	over := newPctx()
	err := p.processBody(context.Background(), nodeBody(maxSubscriptionNodes+1), over)
	if !errors.Is(err, ErrTooManyNodes) {
		t.Fatalf("a body of %d nodes must be rejected, got err=%v", maxSubscriptionNodes+1, err)
	}
	if over.Stats.Total != 0 {
		t.Fatalf("ceiling must bite before the first lookup, %d nodes were processed", over.Stats.Total)
	}

	atLimit := newPctx()
	if err = p.processBody(context.Background(), nodeBody(maxSubscriptionNodes), atLimit); err != nil {
		t.Fatalf("a body exactly at the ceiling must be accepted, got err=%v", err)
	}
	if atLimit.Stats.Total != maxSubscriptionNodes {
		t.Fatalf("processed %d nodes, want %d", atLimit.Stats.Total, maxSubscriptionNodes)
	}
}

// TestProcessBodyReservesSurvivorSliceFromLineCount pins the sizing half of the
// same newline count the ceiling above uses. The collecting sink is the
// /stable.txt worker's, and it is handed one source body per source: growing its
// slice instead of reserving it cost a measured 7.6 MB per cycle across the
// 157-source corpus. The clamp is the other half of the contract — the line
// count is an upper bound, so a body of junk lines must not buy a reservation
// sized for nodes it does not carry.
func TestProcessBodyReservesSurvivorSliceFromLineCount(t *testing.T) {
	t.Parallel()

	body := func(lines int, line string) []byte {
		var sb strings.Builder
		for range lines {
			sb.WriteString(line)
		}
		return []byte(sb.String())
	}

	p := &Processor{}
	for _, tc := range []struct {
		name    string
		body    []byte
		wantCap int
	}{
		{"nodes", body(300, "vless://u@192.0.2.1:443#n\n"), 301},
		{"junk lines clamp", body(maxNodeHint+5000, "not a node at all\n"), maxNodeHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sink := &sliceSink{}
			pctx := &PipelineContext{
				sink:     sink,
				Resolved: map[string][]netip.Addr{},
				Stats:    &Stats{},
			}
			if err := p.processBody(context.Background(), tc.body, pctx); err != nil {
				t.Fatalf("processBody: %v", err)
			}
			if cap(sink.nodes) != tc.wantCap {
				t.Fatalf("survivor slice cap = %d, want %d", cap(sink.nodes), tc.wantCap)
			}
		})
	}
}

// TestFilterIPv6LiteralIsNotADNSFailure pins the accepted half of PP-04. The
// pipeline is IPv4-only, so an IPv6-literal node is refused before any lookup
// happens — but it used to be booked as Stats.DNSDrop, telling the operator
// that name resolution failed for a node that was never resolved at all.
func TestFilterIPv6LiteralIsNotADNSFailure(t *testing.T) {
	t.Parallel()

	p := &Processor{
		logger:   zerolog.Nop(),
		resolver: resolver.New(time.Second, "", 0, 0),
	}

	var buf bytes.Buffer
	stats, err := p.Filter(context.Background(), &buf, FilterRequest{
		Body:             []byte("vless://u@[2001:db8::1]:8443#v6\nvless://u@192.0.2.7:443#v4\n"),
		AllowedCountries: filter.All(),
	})
	if err != nil {
		t.Fatalf("Filter failed: %v", err)
	}
	if stats.DNSDrop != 0 {
		t.Errorf("dns_drop = %d, want 0: no name was ever looked up", stats.DNSDrop)
	}
	if stats.IPv6Drop != 1 {
		t.Errorf("ipv6_drop = %d, want 1", stats.IPv6Drop)
	}
	if stats.Kept != 1 {
		t.Errorf("kept = %d, want 1 (the v4 node)", stats.Kept)
	}
	if stats.Total != stats.Kept+stats.IPv6Drop {
		t.Errorf("total %d must equal kept+drops %d", stats.Total, stats.Kept+stats.IPv6Drop)
	}
}

// TestFilterCountsUnparseableLines pins PP-05. Stats.Unsupported used to guard
// a structurally unreachable condition and was therefore always zero, while the
// lines subscription.Parse actually refused were counted nowhere at all.
func TestFilterCountsUnparseableLines(t *testing.T) {
	t.Parallel()

	p := &Processor{
		logger:   zerolog.Nop(),
		resolver: resolver.New(time.Second, "", 0, 0),
	}

	var buf bytes.Buffer
	stats, err := p.Filter(context.Background(), &buf, FilterRequest{
		Body: []byte("vless://u@192.0.2.7:443#ok\n" +
			`<a href="https://panel.example/renew">renew</a>` + "\n" +
			"vmess://!!!not-base64!!!\n"),
		AllowedCountries: filter.All(),
	})
	if err != nil {
		t.Fatalf("Filter failed: %v", err)
	}
	if stats.Unsupported != 2 {
		t.Errorf("unsupported = %d, want 2", stats.Unsupported)
	}
	if stats.Total != 1 || stats.Kept != 1 {
		t.Errorf("stats = %+v, want total=1 kept=1: refused lines are not nodes", stats)
	}
}

// TestFilterHTMLErrorPageFails is PP-07 seen from the pipeline: a source that
// starts answering with an error page must fail the whole call, not publish
// markup as nodes.
func TestFilterHTMLErrorPageFails(t *testing.T) {
	t.Parallel()

	p := &Processor{
		logger:   zerolog.Nop(),
		resolver: resolver.New(time.Second, "", 0, 0),
	}

	var buf bytes.Buffer
	_, err := p.Filter(context.Background(), &buf, FilterRequest{
		Body: []byte("<!DOCTYPE html>\n<html><body>\n" +
			`<p>Token expired. <a href="https://panel.example/renew">Renew</a></p>` + "\n" +
			"</body></html>\n"),
		AllowedCountries: filter.All(),
	})
	if err == nil {
		t.Fatalf("an HTML page must not filter as a healthy subscription; buffer:\n%s", buf.String())
	}
	if buf.Len() != 0 {
		t.Errorf("nothing may be published from an HTML page, got:\n%s", buf.String())
	}
}

// TestCountryChainConsultsEveryLoadedDatabase pins PP-02. The country filter
// used to judge nodes against the geofeed alone while the GEO tag resolved
// through the whole provider chain, so a node DB-IP places in DE was dropped as
// unplaceable by an exclusion the tag said did not apply to it. countryOrder is
// the shipped GEO chain, geofeed ahead of dbip.
func TestCountryChainConsultsEveryLoadedDatabase(t *testing.T) {
	t.Parallel()

	// 203.0.113.9 is in the dbip database only; the geofeed cannot place it.
	ip := netip.MustParseAddr("203.0.113.9")
	dbipOnly := []geofeed.Range{{Start: ip, End: ip, Country: geofeed.CountryCode{'D', 'E'}}}

	newProcessor := func() *Processor {
		return &Processor{
			logger:          zerolog.Nop(),
			resolver:        resolver.New(time.Second, "", 0, 0),
			countryLookup:   geofeed.NewLookup(geoEntries(10)),
			loadedAt:        time.Now(),
			refreshInterval: 24 * time.Hour,
			filters:         []Filter{NewGeofeedFilter()},
			countryOrder:    []string{config.ProviderGeofeed, config.ProviderDBIP},
			dbip: &geoDB{
				name:     "dbip",
				lookup:   geofeed.NewRangeLookup(dbipOnly),
				loadedAt: time.Now(),
				interval: 24 * time.Hour,
			},
		}
	}
	body := []byte("vless://u@203.0.113.9:443#n\n")

	// An allow-list the dbip answer satisfies: the node must survive.
	var kept bytes.Buffer
	stats, err := newProcessor().Filter(context.Background(), &kept, FilterRequest{
		Body:             body,
		AllowedCountries: filter.ParseAllowed("DE"),
	})
	if err != nil {
		t.Fatalf("Filter failed: %v", err)
	}
	if stats.Kept != 1 {
		t.Fatalf("kept = %d, want 1: dbip places this IP in DE", stats.Kept)
	}

	// A deny-list naming the same country must now reach it too.
	var dropped bytes.Buffer
	stats, err = newProcessor().Filter(context.Background(), &dropped, FilterRequest{
		Body:             body,
		AllowedCountries: filter.All(),
		DeniedCountries:  filter.ParseAllowed("DE"),
	})
	if err != nil {
		t.Fatalf("Filter failed: %v", err)
	}
	if stats.Kept != 0 || stats.GeoDrop != 1 {
		t.Fatalf("stats = %+v, want kept=0 geo_drop=1: exclude DE must reach a dbip-placed node", stats)
	}
}

// TestReloadCarryKeepsGeofeedBackoff pins VP-02. loadedAt marks the last GOOD
// data, so after a failed reload it reads as permanently stale and only retryAt
// throttles the next attempt. A config reload builds a fresh Processor from the
// carried state; if the retry schedule does not travel with the data, the very
// next request re-downloads every geofeed source. The crawler rewrites
// private.yaml hourly, so that is once an hour, forever, against a source that
// is already failing — the round-2 backoff never gets to grow.
func TestReloadCarryKeepsGeofeedBackoff(t *testing.T) {
	t.Parallel()

	p := staleProcessor(t, func(context.Context) ([]geofeed.Entry, int, error) {
		return nil, 2, errors.New("no geofeed entries loaded")
	})
	p.doReload(t.Context())

	failed := p.GeofeedState()
	if failed.RetryAt.IsZero() || failed.Failures == 0 {
		t.Fatalf("precondition: a failed reload must arm the backoff, got %+v", failed)
	}

	next, err := NewProcessor(t.Context(), zerolog.Nop(), Options{
		PreloadedGeofeed: failed,
		RefreshInterval:  24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewProcessor from carried state: %v", err)
	}

	if next.shouldReloadGeofeedLocked(time.Now()) {
		t.Fatal("a config reload must not reset the geofeed backoff into an immediate re-download")
	}
	if !next.shouldReloadGeofeedLocked(failed.RetryAt) {
		t.Fatal("the carried retry must still come due at its own deadline")
	}
	if got := next.GeofeedState().Failures; got != failed.Failures {
		t.Fatalf("carried failure count = %d, want %d: the backoff must keep growing across reloads", got, failed.Failures)
	}
}

// TestReloadCarryKeepsGeoDBBackoff is VP-02 on the geoDB half, where dropping
// the retry schedule is a regression rather than a missed improvement: a failed
// load used to stamp loadedAt=now and that stamp was carried, so a broken mirror
// was retried once per interval. With loadedAt reserved for good data, a carry
// without retryAt means a full DB-IP or RIR download attempt on the first
// request after every reload.
func TestReloadCarryKeepsGeoDBBackoff(t *testing.T) {
	t.Parallel()

	failing := func(context.Context) ([]geofeed.Range, int, error) {
		return nil, 0, errors.New("mirror down")
	}
	db := newGeoDB(t.Context(), zerolog.Nop(), "dbip", 24*time.Hour, GeoState{}, failing)

	failed := db.state()
	if failed.RetryAt.IsZero() || failed.Failures == 0 {
		t.Fatalf("precondition: a failed initial load must arm the backoff, got %+v", failed)
	}

	next := newGeoDB(t.Context(), zerolog.Nop(), "dbip", 24*time.Hour, failed, failing)
	if next.staleLocked(time.Now()) {
		t.Fatal("a config reload must not reset the geo database backoff into an immediate re-download")
	}
	if !next.staleLocked(failed.RetryAt) {
		t.Fatal("the carried retry must still come due at its own deadline")
	}
	if next.reloadFailures != failed.Failures {
		t.Fatalf("carried failure count = %d, want %d", next.reloadFailures, failed.Failures)
	}
}

// TestCountryChainFollowsConfiguredProviderOrder pins VP-03. countryChain used
// to hardcode geofeed -> dbip -> registry while the annotator walks whatever
// order the operator wrote, so a config preferring dbip ahead of geofeed got a
// filter verdict and a [GEO:xx] tag that disagreed — the divergence PP-02
// closed, reintroduced in the opposite direction.
func TestCountryChainFollowsConfiguredProviderOrder(t *testing.T) {
	t.Parallel()

	// One IP, two answers: the published geofeed places it in NL, DB-IP in DE.
	ip := netip.MustParseAddr("203.0.113.9")
	geofeedNL := geofeed.NewLookup([]geofeed.Entry{
		{Prefix: netip.PrefixFrom(ip, 32), Country: geofeed.CountryCode{'N', 'L'}},
	})
	dbipDE := geofeed.NewRangeLookup([]geofeed.Range{
		{Start: ip, End: ip, Country: geofeed.CountryCode{'D', 'E'}},
	})
	body := []byte("vless://u@203.0.113.9:443#n\n")

	cases := []struct {
		name     string
		order    []string
		wantKept int
	}{
		{"geofeed first: NL wins, exclude DE misses", []string{config.ProviderGeofeed, config.ProviderDBIP}, 1},
		{"dbip first: DE wins, exclude DE drops", []string{config.ProviderDBIP, config.ProviderGeofeed}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, err := NewProcessor(t.Context(), zerolog.Nop(), Options{
				RefreshInterval:  24 * time.Hour,
				DNSTimeout:       time.Second,
				PreloadedGeofeed: GeoState{Lookup: geofeedNL, LoadedAt: time.Now()},
				PreloadedDBIP:    GeoState{Lookup: dbipDE, LoadedAt: time.Now()},
				// SSRF-unreachable: the preload proves no download is attempted.
				DBIP:      config.DBIPConfig{URL: "https://127.0.0.1:1/db-{yyyy-mm}.csv.gz", RefreshInterval: new(time.Hour)},
				IPFilters: []config.IPFilterSpec{{Type: config.FilterCountry, Provider: config.ProviderGeofeed}},
				Annotate:  []config.AnnotateSpec{{Tag: config.TagGEO, Providers: tc.order}},
			})
			if err != nil {
				t.Fatalf("NewProcessor: %v", err)
			}

			var buf bytes.Buffer
			stats, errFilter := p.Filter(t.Context(), &buf, FilterRequest{
				Body:             body,
				AllowedCountries: filter.All(),
				DeniedCountries:  filter.ParseAllowed("DE"),
			})
			if errFilter != nil {
				t.Fatalf("Filter failed: %v", errFilter)
			}
			if stats.Kept != tc.wantKept {
				t.Fatalf("kept = %d, want %d: the filter must judge with the configured GEO order %v",
					stats.Kept, tc.wantKept, tc.order)
			}
		})
	}
}

// TestEveryGEOEntryFeedsTheCountryFilter pins PP-02/VP-03 for the multi-entry
// shape. countryChainOrder used to stop at the FIRST GEO entry while Annotate
// resolves across all of them, so splitting one chain over two entries
// inverted BOTH verdicts on the same node: with the geofeed unable to place
// 203.0.113.9 and DB-IP placing it in DE, `countries=DE` dropped it as
// unplaceable and `exclude_countries=DE` KEPT it — a deny-list not working —
// and published `[GEO:??][GEO:DE]`, naming the very country it was excluded
// for. Both directions are pinned because only the allow-list one was ever
// covered, and the deny-list one is the half that fails silently: nothing
// warns, no counter moves, the node is simply published.
//
// The merged single-entry config is the control: the split config must now
// agree with it verdict for verdict.
func TestEveryGEOEntryFeedsTheCountryFilter(t *testing.T) {
	t.Parallel()

	ip := netip.MustParseAddr("203.0.113.9")
	// A populated geofeed that does not cover the node: "unplaceable by the
	// geofeed", not "no geofeed loaded".
	geofeedElsewhere := geofeed.NewLookup([]geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: geofeed.CountryCode{'N', 'L'}},
	})
	dbipDE := geofeed.NewRangeLookup([]geofeed.Range{
		{Start: ip, End: ip, Country: geofeed.CountryCode{'D', 'E'}},
	})
	body := []byte("vless://u@203.0.113.9:443#n\n")

	split := []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
		{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP}},
	}
	merged := []config.AnnotateSpec{
		{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderDBIP}},
	}

	newProcessor := func(t *testing.T, annotate []config.AnnotateSpec) *Processor {
		t.Helper()
		p, err := NewProcessor(t.Context(), zerolog.Nop(), Options{
			RefreshInterval:  24 * time.Hour,
			DNSTimeout:       time.Second,
			PreloadedGeofeed: GeoState{Lookup: geofeedElsewhere, LoadedAt: time.Now()},
			PreloadedDBIP:    GeoState{Lookup: dbipDE, LoadedAt: time.Now()},
			// SSRF-unreachable: the preload proves no download is attempted.
			DBIP:      config.DBIPConfig{URL: "https://127.0.0.1:1/db-{yyyy-mm}.csv.gz", RefreshInterval: new(time.Hour)},
			IPFilters: []config.IPFilterSpec{{Type: config.FilterCountry, Provider: config.ProviderGeofeed}},
			Annotate:  annotate,
		})
		if err != nil {
			t.Fatalf("NewProcessor: %v", err)
		}
		return p
	}

	cases := []struct {
		name     string
		annotate []config.AnnotateSpec
	}{
		{"one merged entry", merged},
		{"the same chain split over two entries", split},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The two directions are separate subtests on purpose: a shared
			// body would t.Fatal on the allow-list half first and never reach
			// the deny-list assertion, so a regression in the half that fails
			// SILENTLY would hide behind the half that is already covered.
			t.Run("countries=DE keeps it", func(t *testing.T) {
				t.Parallel()
				assertKeptUnderAllowList(t, newProcessor(t, tc.annotate), body)
			})

			// The half that used to publish `[GEO:??][GEO:DE]` for a node the
			// operator excluded DE for: nothing warned, no counter moved.
			t.Run("exclude_countries=DE drops it", func(t *testing.T) {
				t.Parallel()
				assertDroppedUnderDenyList(t, newProcessor(t, tc.annotate), body)
			})
		})
	}
}

// assertKeptUnderAllowList runs `countries=DE` over body and requires the node
// to survive carrying a DE tag: DB-IP places it, so every GEO entry's chain
// reaching the filter is what makes the allow-list admit it.
func assertKeptUnderAllowList(t *testing.T, p *Processor, body []byte) {
	t.Helper()

	var kept bytes.Buffer
	stats, err := p.Filter(t.Context(), &kept, FilterRequest{
		Body:             body,
		AllowedCountries: filter.ParseAllowed("DE"),
	})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if stats.Kept != 1 || stats.GeoDrop != 0 {
		t.Fatalf("stats = %+v, want kept=1 geo_drop=0: DB-IP places this node in DE and every GEO entry's chain must reach the filter", stats)
	}
	if !strings.Contains(kept.String(), "GEO:DE") {
		t.Fatalf("published %q, want a DE tag", kept.String())
	}
}

// assertDroppedUnderDenyList runs `exclude_countries=DE` over the same body and
// requires the node to be dropped and nothing published. This is the direction
// that failed silently: a deny-list that stops reaching a database publishes
// the node instead, with no warning and no counter moving.
func assertDroppedUnderDenyList(t *testing.T, p *Processor, body []byte) {
	t.Helper()

	var dropped bytes.Buffer
	stats, err := p.Filter(t.Context(), &dropped, FilterRequest{
		Body:             body,
		AllowedCountries: filter.All(),
		DeniedCountries:  filter.ParseAllowed("DE"),
	})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if stats.Kept != 0 || stats.GeoDrop != 1 {
		t.Fatalf("stats = %+v, want kept=0 geo_drop=1: a deny-list must reach a node only a later GEO entry places", stats)
	}
	if dropped.Len() != 0 {
		t.Fatalf("published %q, want nothing", dropped.String())
	}
}

// TestGeofeedFilterNeverDropsATagThatPlacesTheNode pins the routes.md
// GeofeedFilter contract directly: a node is never dropped as unplaceable
// while carrying a tag a LOCAL database placed it with. The local scope is
// part of the contract, not a fixture limitation — this walks geofeed and
// registry because asn and cloudflare are outside countryChainOrder entirely,
// so no test here can widen the promise to them. The published name is the
// evidence — a drop must leave no name behind, and a name carrying a real
// country must belong to a node the filter kept.
func TestGeofeedFilterNeverDropsATagThatPlacesTheNode(t *testing.T) {
	t.Parallel()

	ip := netip.MustParseAddr("203.0.113.9")
	geofeedElsewhere := geofeed.NewLookup([]geofeed.Entry{
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Country: geofeed.CountryCode{'N', 'L'}},
	})
	registryDE := geofeed.NewRangeLookup([]geofeed.Range{
		{Start: ip, End: ip, Country: geofeed.CountryCode{'D', 'E'}},
	})

	// Three entries, the placing database written last: nothing before it can
	// answer, so only a filter that walks every entry sees DE at all.
	p, err := NewProcessor(t.Context(), zerolog.Nop(), Options{
		RefreshInterval:   24 * time.Hour,
		DNSTimeout:        time.Second,
		PreloadedGeofeed:  GeoState{Lookup: geofeedElsewhere, LoadedAt: time.Now()},
		PreloadedRegistry: GeoState{Lookup: registryDE, LoadedAt: time.Now()},
		Registry:          config.RegistryConfig{URLs: []string{"https://127.0.0.1:1/delegated"}, RefreshInterval: new(time.Hour)},
		IPFilters:         []config.IPFilterSpec{{Type: config.FilterCountry, Provider: config.ProviderGeofeed}},
		Annotate: []config.AnnotateSpec{
			{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
			{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
			{Tag: config.TagGEO, Providers: []string{config.ProviderRegistry}},
		},
	})
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	var buf bytes.Buffer
	stats, err := p.Filter(t.Context(), &buf, FilterRequest{
		Body:             []byte("vless://u@203.0.113.9:443#n\n"),
		AllowedCountries: filter.ParseAllowed("DE"),
	})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if stats.Kept != 1 {
		t.Fatalf("stats = %+v, want kept=1: the registry entry places this node, so it must not be dropped as unplaceable", stats)
	}
	if got := buf.String(); !strings.Contains(got, "GEO:DE") {
		t.Fatalf("published %q, want the placing tag on the kept node", got)
	}
}

// TestCountryChainOrderDerivation covers the rest of VP-03: asn is never a
// filter source (it is a per-IP Cymru round trip, not a local table), a
// provider the process did not build is dropped before it can be dereferenced,
// and every order equivalent to "the geofeed alone" collapses to nil so
// countryChain hands the filter the lookup itself instead of a one-element
// chain.
func TestCountryChainOrderDerivation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		annotate     []config.AnnotateSpec
		haveDBIP     bool
		haveRegistry bool
		want         []string
	}{
		{
			name: "shipped chain keeps its order and drops asn",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{
					config.ProviderGeofeed, config.ProviderDBIP, config.ProviderRegistry, config.ProviderASN,
				}},
			},
			haveDBIP: true, haveRegistry: true,
			want: []string{config.ProviderGeofeed, config.ProviderDBIP, config.ProviderRegistry},
		},
		{
			// Every GEO entry feeds the filter: the second entry's chain is
			// appended in written order, and the geofeed it repeats keeps the
			// position the first entry gave it.
			name: "a later GEO entry extends the first, de-duplicated",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP, config.ProviderGeofeed}},
				{Tag: config.TagGEO, Providers: []string{config.ProviderRegistry, config.ProviderGeofeed}},
			},
			haveDBIP: true, haveRegistry: true,
			want: []string{config.ProviderDBIP, config.ProviderGeofeed, config.ProviderRegistry},
		},
		{
			// The defect shape: split across entries this used to yield the
			// geofeed alone, so the filter never saw dbip.
			name: "a chain split across two entries merges back into one",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
				{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP}},
			},
			haveDBIP: true,
			want:     []string{config.ProviderGeofeed, config.ProviderDBIP},
		},
		{
			// The address cloudflare is asked about does not exist at the IP
			// stage: it is the one the node's traffic LEFT from, and the stage
			// runs before any proxy exists to ask. It is dropped wherever it
			// is written, so a chain led by it still filters on the local
			// databases behind it — the first of the three tag/filter
			// asymmetries README documents.
			name: "cloudflare is dropped from every entry",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderCloudflare, config.ProviderGeofeed}},
				{Tag: config.TagGEO, Providers: []string{config.ProviderCloudflare, config.ProviderDBIP}},
			},
			haveDBIP: true, haveRegistry: true,
			want: []string{config.ProviderGeofeed, config.ProviderDBIP},
		},
		{
			name: "entries naming nothing but the geofeed still collapse",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
				{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderASN}},
			},
			haveDBIP: true, haveRegistry: true,
		},
		{
			// A provider the process did not build is dropped wherever it is
			// written, so a second entry cannot smuggle a nil geoDB in.
			name: "an unbuilt database is skipped in a later entry too",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderDBIP}},
				{Tag: config.TagGEO, Providers: []string{config.ProviderRegistry}},
			},
			haveDBIP: true,
			want:     []string{config.ProviderGeofeed, config.ProviderDBIP},
		},
		{
			name: "operator order is preserved verbatim",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderDBIP, config.ProviderGeofeed}},
			},
			haveDBIP: true,
			want:     []string{config.ProviderDBIP, config.ProviderGeofeed},
		},
		{
			name: "a GEO chain of asn alone falls back to the geofeed",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderASN}},
			},
		},
		{
			name: "a geofeed-only chain collapses",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}},
			},
		},
		{
			name: "a database the process did not build is skipped",
			annotate: []config.AnnotateSpec{
				{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed, config.ProviderRegistry}},
			},
		},
		{
			name:     "an empty annotate list falls back to the geofeed",
			annotate: []config.AnnotateSpec{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := countryChainOrder(tc.annotate, tc.haveDBIP, tc.haveRegistry)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("countryChainOrder = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProviderNeedsASNHasTwoIndependentSources pins the seam the retired ASN
// annotate TAG must not have moved: whether the Cymru resolver is built is
// decided by opts.IPFilters and by the PROVIDER chains in opts.Annotate, and
// never by a tag name. Both sources stand alone — an operator who filters by
// AS name annotates nothing, and the shipped config annotates through asn with
// no asn filter configured — so a rule that read the tag would have silently
// dropped one of them.
func TestProviderNeedsASNHasTwoIndependentSources(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		filters  []config.IPFilterSpec
		annotate []config.AnnotateSpec
		want     bool
	}{
		{
			name:     "an asn filter alone, nothing annotated",
			filters:  []config.IPFilterSpec{{Type: config.FilterASN, DenyPatterns: []string{"spammy"}}},
			annotate: []config.AnnotateSpec{{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}}},
			want:     true,
		},
		{
			name:     "a country filter through the asn provider",
			filters:  []config.IPFilterSpec{{Type: config.FilterCountry, Provider: config.ProviderASN}},
			annotate: nil,
			want:     true,
		},
		{
			name:    "an annotate chain ending in asn, no asn filter",
			filters: []config.IPFilterSpec{{Type: config.FilterCountry, Provider: config.ProviderGeofeed}},
			annotate: []config.AnnotateSpec{{Tag: config.TagGEO, Providers: []string{
				config.ProviderCloudflare, config.ProviderGeofeed, config.ProviderASN,
			}}},
			want: true,
		},
		{
			name:     "neither source names asn",
			filters:  []config.IPFilterSpec{{Type: config.FilterCountry, Provider: config.ProviderGeofeed}},
			annotate: []config.AnnotateSpec{{Tag: config.TagGEO, Providers: []string{config.ProviderGeofeed}}},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := Options{IPFilters: tc.filters, Annotate: tc.annotate}
			if needsASN, _, _ := providerNeeds(opts); needsASN != tc.want {
				t.Fatalf("providerNeeds needsASN = %v, want %v", needsASN, tc.want)
			}

			// The decision only matters through what NewProcessor builds.
			opts.PreloadedGeofeed = GeoState{Lookup: benchGeofeed(), LoadedAt: time.Now()}
			p, err := NewProcessor(t.Context(), zerolog.Nop(), opts)
			if err != nil {
				t.Fatalf("NewProcessor: %v", err)
			}
			if got := p.ASNState() != nil; got != tc.want {
				t.Fatalf("processor built an ASN resolver = %v, want %v", got, tc.want)
			}
		})
	}
}

// shippedConfigOffline loads the repository's own config/config.yaml (copied
// into a temp dir, because config.Load merges the sibling overlays and these
// tests are about config.yaml alone) and returns Options wired for an offline
// run. The shipped chain names dbip and registry, so both geoDBs are built;
// preloading them keeps the callers off the network. Nothing preloads the ASN
// resolver -- whether it is constructed at all is what the callers ask.
func shippedConfigOffline(t *testing.T) (shipped []byte, dir string, cfg config.Config, opts Options) {
	t.Helper()

	shipped, err := os.ReadFile(filepath.Join("..", "..", "config", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if writeErr := os.WriteFile(path, shipped, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	offline := GeoState{Lookup: benchGeofeed(), LoadedAt: time.Now()}
	return shipped, dir, cfg, Options{
		IPFilters:         cfg.IPFilterSpecs(),
		Annotate:          cfg.Annotate,
		DBIP:              cfg.Geo.DBIP,
		Registry:          cfg.Geo.Registry,
		PreloadedGeofeed:  offline,
		PreloadedDBIP:     offline,
		PreloadedRegistry: offline,
	}
}

// TestShippedConfigDropsASNButKeepsTheCapability reads the repository's own
// config/config.yaml rather than a fixture. Two halves, and the second is the
// point: the shipped GEO chain no longer names asn, so the running service
// builds no Cymru resolver -- measured, the provider contributed zero hits
// behind dbip+registry -- but the provider is NOT retired, and a config that
// asks for it must still load and still build it. What a config that asks for
// it gets BUILT is TestASNFilterFormsStillBuild's half.
func TestShippedConfigDropsASNButKeepsTheCapability(t *testing.T) {
	t.Parallel()

	shipped, dir, cfg, opts := shippedConfigOffline(t)

	for _, a := range cfg.Annotate {
		if slices.Contains(a.Providers, config.ProviderASN) {
			t.Fatalf("shipped GEO chain names asn again (%v) without a new measurement", a.Providers)
		}
	}
	for _, f := range cfg.IPFilterSpecs() {
		if f.Provider == config.ProviderASN || f.Type == config.FilterASN {
			t.Fatalf("shipped filters reach asn (%+v); the resolver assertions below are then vacuous", f)
		}
	}

	if needsASN, _, _ := providerNeeds(opts); needsASN {
		t.Fatal("nothing in the shipped config reaches asn, yet the resolver is wanted")
	}
	p, err := NewProcessor(t.Context(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	if p.ASNState() != nil {
		t.Fatal("the shipped config must not build the Cymru resolver")
	}

	// Capability retained: the SAME yaml with asn appended to the chain must
	// still pass config.Load's provider validation and still build the
	// resolver. Going through the loader is the point -- a config naming asn is
	// not a retired spelling, and nothing may start rejecting it.
	const shippedChain = "providers: [cloudflare, geofeed, dbip, registry]"
	if !bytes.Contains(shipped, []byte(shippedChain)) {
		t.Fatalf("shipped config no longer contains %q; re-point this test", shippedChain)
	}
	withASN := bytes.Replace(shipped, []byte(shippedChain),
		[]byte("providers: [cloudflare, geofeed, dbip, registry, asn]"), 1)
	pathASN := filepath.Join(dir, "with-asn.yaml")
	if writeErr := os.WriteFile(pathASN, withASN, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	cfgASN, err := config.Load(pathASN)
	if err != nil {
		t.Fatalf("a chain naming asn must still load: %v", err)
	}

	optsASN := opts
	optsASN.Annotate = cfgASN.Annotate
	if needsASN, _, _ := providerNeeds(optsASN); !needsASN {
		t.Fatal("a chain naming asn must want the resolver")
	}
	pASN, err := NewProcessor(t.Context(), zerolog.Nop(), optsASN)
	if err != nil {
		t.Fatalf("NewProcessor with asn re-added: %v", err)
	}
	if pASN.ASNState() == nil {
		t.Fatal("a chain naming asn must build the resolver")
	}
}

// TestASNFilterFormsStillBuild covers what the ANNOTATE half above does not.
// The provider was retained for its two FILTER forms -- `{type: asn}` deny
// patterns over the AS NAME no local database carries, and `{type: country,
// provider: asn}` -- and buildFilters is where a config that asks for either
// turns into something that RUNS. Each is one `case` arm: delete either and
// every assertion in the sibling test still passes while the configured filter
// silently never executes (an asn entry vanishing from the chain, or a
// country/asn entry quietly downgrading to a geofeed one). So this pins the
// built chain itself -- how many filters, of which concrete types, in config
// order, each with the live resolver behind it.
func TestASNFilterFormsStillBuild(t *testing.T) {
	t.Parallel()

	shipped, dir, _, opts := shippedConfigOffline(t)

	const shippedFilters = "filters:\n  - type: country\n    provider: geofeed\n"
	if !bytes.Contains(shipped, []byte(shippedFilters)) {
		t.Fatalf("shipped config no longer opens filters with %q; re-point this test", shippedFilters)
	}
	withFilters := bytes.Replace(shipped, []byte(shippedFilters),
		[]byte("filters:\n  - type: asn\n    deny_patterns: [\"(?i)servers\\\\.com\"]\n"+
			"  - type: country\n    provider: asn\n  - type: country\n    provider: geofeed\n"), 1)
	path := filepath.Join(dir, "with-asn-filters.yaml")
	if writeErr := os.WriteFile(path, withFilters, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("both asn filter forms must still load: %v", err)
	}

	opts.IPFilters = cfg.IPFilterSpecs()
	if len(opts.IPFilters) != 3 {
		t.Fatalf("IPFilterSpecs() = %+v, want the 3 spliced entries", opts.IPFilters)
	}
	// The GEO chain is still the shipped one, so a resolver here can only have
	// been asked for by a FILTER.
	if needsASN, _, _ := providerNeeds(opts); !needsASN {
		t.Fatal("an asn FILTER must want the resolver even with asn out of the annotate chain")
	}
	p, err := NewProcessor(t.Context(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor with both asn filter forms: %v", err)
	}
	if len(p.filters) != 3 {
		t.Fatalf("built %d filters, want 3 (asn deny, country/asn, country/geofeed)", len(p.filters))
	}

	// Entry 0: `{type: asn}` -- an ASNFilter CARRYING the compiled patterns. A
	// pattern-less ASNFilter here would mean the deny list was dropped.
	denyFilter, ok := p.filters[0].(*ASNFilter)
	if !ok {
		t.Fatalf("filters[0] = %T, want *ASNFilter for `{type: asn}`", p.filters[0])
	}
	if len(denyFilter.patterns) != 1 {
		t.Fatalf("the asn deny filter compiled %d patterns, want 1", len(denyFilter.patterns))
	}
	// Entry 1: `{type: country, provider: asn}` -- also an ASNFilter, and NOT
	// the GeofeedFilter the same case builds for every other provider.
	countryASN, ok := p.filters[1].(*ASNFilter)
	if !ok {
		t.Fatalf("filters[1] = %T, want *ASNFilter for `{type: country, provider: asn}`", p.filters[1])
	}
	if len(countryASN.patterns) != 0 {
		t.Fatalf("a country/asn filter carries %d deny patterns, want 0", len(countryASN.patterns))
	}
	// Entry 2 proves the switch still discriminates rather than answering
	// *ASNFilter to everything.
	if _, isGeofeed := p.filters[2].(*GeofeedFilter); !isGeofeed {
		t.Fatalf("filters[2] = %T, want *GeofeedFilter for `{type: country, provider: geofeed}`", p.filters[2])
	}
	// Both must be able to actually resolve: buildFilters hands them the one
	// resolver NewProcessor constructed, and a typed-nil behind the interface
	// would make Process fall open on every IP.
	for i, f := range []*ASNFilter{denyFilter, countryASN} {
		r, isReal := f.resolver.(*asn.Resolver)
		if !isReal || r == nil {
			t.Fatalf("filters[%d] holds resolver %#v, want the live *asn.Resolver", i, f.resolver)
		}
		if r != p.ASNState() {
			t.Fatalf("filters[%d] holds a different resolver than ASNState()", i)
		}
	}
}

// cidrRanges builds n non-adjacent /32 entries, one covered address each:
// consecutive addresses would merge and the swap guard compares sizes.
func cidrRanges(t *testing.T, n int) cidrset.Set {
	t.Helper()
	var sb strings.Builder
	for i := range n {
		fmt.Fprintf(&sb, "10.%d.%d.1/32\n", i>>8, i&0xff)
	}
	set, skipped := cidrset.Parse([]byte(sb.String()))
	if skipped != 0 || set.Len() != n {
		t.Fatalf("fixture parsed to %d ranges, %d lines skipped, want %d ranges", set.Len(), skipped, n)
	}
	return set
}

// offlineCIDROptions is the minimum an allow-list test needs from NewProcessor:
// a geofeed that never downloads and a single configured cidr filter.
func offlineCIDROptions() Options {
	return Options{
		PreloadedGeofeed: GeoState{Lookup: benchGeofeed(), LoadedAt: time.Now()},
		IPFilters:        []config.IPFilterSpec{{Type: config.FilterCIDR, RefreshInterval: 24 * time.Hour}},
	}
}

// TestNewProcessorRefusesAnEmptyCIDRAllowList: an allow-list that came back
// empty is not a milder filter but the opposite one — it admits nobody — so the
// build fails where an empty geo database only warns.
func TestNewProcessorRefusesAnEmptyCIDRAllowList(t *testing.T) {
	t.Parallel()

	opts := offlineCIDROptions()
	opts.cidrLoad = func(context.Context) (cidrset.Set, int, error) { return cidrset.Set{}, 0, nil }

	_, err := NewProcessor(t.Context(), zerolog.Nop(), opts)
	if err == nil {
		t.Fatal("a cidr source that loaded empty must fail the build, not publish nothing")
	}
	if !errors.Is(err, errEmptyCIDRSet) || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("the error must say the allow-list came back empty, got %v", err)
	}
}

// TestNewProcessorPreloadedCIDRSkipsTheDownload: the crawler rewrites
// private.yaml hourly, so a carried list that re-downloads is an hourly fetch
// of a 30k-line file nothing asked for.
func TestNewProcessorPreloadedCIDRSkipsTheDownload(t *testing.T) {
	t.Parallel()

	carried := CIDRState{Set: mustCIDRSet(t, "198.51.100.0/24"), LoadedAt: time.Now()}
	downloaded := false
	opts := offlineCIDROptions()
	opts.PreloadedCIDR = carried
	opts.cidrLoad = func(context.Context) (cidrset.Set, int, error) {
		downloaded = true
		return cidrset.Set{}, 0, nil
	}

	p, err := NewProcessor(t.Context(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor from carried state: %v", err)
	}
	if downloaded {
		t.Fatal("a carried allow-list must not be re-downloaded on every config reload")
	}
	if got := p.CIDRState().Set.Covered(); got != carried.Set.Covered() {
		t.Fatalf("carried coverage = %d, want %d", got, carried.Set.Covered())
	}
	if len(p.filters) != 1 {
		t.Fatalf("built %d filters, want the one configured cidr filter", len(p.filters))
	}
	if _, ok := p.filters[0].(*CIDRFilter); !ok {
		t.Fatalf("filters[0] = %T, want *CIDRFilter: the configured filter must run", p.filters[0])
	}
}

// TestNewProcessorWiresTheStoreIntoTheFilter pins the only path by which the
// live list reaches CIDRFilter. A getter handing back an empty set drops every
// node, so the instance publishes an EMPTY subscription while cidr_drop and
// every gate read healthy. The type assertion above cannot see that; only
// running a body through the processor NewProcessor built can.
func TestNewProcessorWiresTheStoreIntoTheFilter(t *testing.T) {
	t.Parallel()

	opts := offlineCIDROptions()
	listed := mustCIDRSet(t, "198.51.100.0/24")
	opts.cidrLoad = func(context.Context) (cidrset.Set, int, error) { return listed, 0, nil }

	p, err := NewProcessor(t.Context(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	var buf bytes.Buffer
	stats, err := p.Filter(t.Context(), &buf, FilterRequest{
		Body: []byte("vless://a@198.51.100.10:443#listed\n" +
			"vless://b@203.0.113.5:443#unlisted\n"),
		AllowedCountries: filter.All(),
	})
	if err != nil {
		t.Fatalf("Filter failed: %v", err)
	}
	if stats.Kept != 1 || stats.CIDRDrop != 1 {
		t.Fatalf("stats = %+v, want kept=1 cidr_drop=1: the filter must judge against the store's own set", stats)
	}
	if body := buf.String(); !strings.Contains(body, "198.51.100.10") || strings.Contains(body, "203.0.113.5") {
		t.Fatalf("published %q, want the listed node alone", body)
	}
}

// TestReloadCarryKeepsCIDRBackoff is VP-02 on the allow-list: half a schedule
// is no schedule. Carrying the set without RetryAt/Failures makes the first
// request after every reload re-download a source that is already failing.
func TestReloadCarryKeepsCIDRBackoff(t *testing.T) {
	t.Parallel()

	partial := func(context.Context) (cidrset.Set, int, error) {
		return mustCIDRSet(t, "198.51.100.0/24"), 1, nil
	}
	store, err := newCIDRStore(t.Context(), zerolog.Nop(), 24*time.Hour, CIDRState{}, partial)
	if err != nil {
		t.Fatalf("a partial load that still yielded ranges must build: %v", err)
	}
	carried := store.state()
	if carried.RetryAt.IsZero() || carried.Failures == 0 {
		t.Fatalf("precondition: a partial initial load must arm the backoff, got %+v", carried)
	}

	opts := offlineCIDROptions()
	opts.PreloadedCIDR = carried
	p, err := NewProcessor(t.Context(), zerolog.Nop(), opts)
	if err != nil {
		t.Fatalf("NewProcessor from carried state: %v", err)
	}

	got := p.CIDRState()
	if got.Set.Covered() != carried.Set.Covered() || !got.LoadedAt.Equal(carried.LoadedAt) ||
		!got.RetryAt.Equal(carried.RetryAt) || got.Failures != carried.Failures {
		t.Fatalf("CIDRState() = %+v, want the carried %+v", got, carried)
	}
	if p.cidr.staleLocked(time.Now()) {
		t.Fatal("a config reload must not reset the backoff into an immediate re-download")
	}
	if !p.cidr.staleLocked(carried.RetryAt) {
		t.Fatal("the carried retry must still come due at its own deadline")
	}
}

// TestCIDRStoreRefusesACoverageCollapse is the swap guard on the shape the
// upstream data actually takes: the prefix file and the singleton-/32 file
// cover the same space with 15649 and 141664 ranges. Losing the prefixes GROWS
// the range count while the addresses admitted collapse, so a guard reading
// Len() would swap in a list that drops nearly every node.
func TestCIDRStoreRefusesACoverageCollapse(t *testing.T) {
	t.Parallel()

	live := mustCIDRSet(t, "10.0.0.0/16")
	singletons := cidrRanges(t, 100)
	if singletons.Len() <= live.Len() {
		t.Fatalf("precondition: the shrunk list must have MORE ranges (%d) than the live one (%d)",
			singletons.Len(), live.Len())
	}
	s := &cidrStore{
		set:      live,
		loadedAt: time.Now().Add(-25 * time.Hour),
		interval: 24 * time.Hour,
		load:     func(context.Context) (cidrset.Set, int, error) { return singletons, 0, nil },
	}

	s.doReload(t.Context(), zerolog.Nop())

	if s.set.Covered() != live.Covered() {
		t.Fatalf("the live list must survive a refused swap, covers %d want %d", s.set.Covered(), live.Covered())
	}
	now := time.Now()
	if s.staleLocked(now) {
		t.Fatal("the retry must be throttled immediately after the refused swap")
	}
	if !s.staleLocked(now.Add(reloadRetryInterval + time.Minute)) {
		t.Fatal("the retry must be due within minutes, not a full refresh interval")
	}
}

// TestCIDRStoreCleanReloadSwaps proves the guard above does not block the
// normal path, without which refusing everything would pass it.
func TestCIDRStoreCleanReloadSwaps(t *testing.T) {
	t.Parallel()

	loaded := mustCIDRSet(t, "10.0.0.0/15")
	s := &cidrStore{
		set:      mustCIDRSet(t, "10.0.0.0/16"),
		loadedAt: time.Now().Add(-25 * time.Hour),
		retryAt:  time.Now().Add(reloadRetryInterval),
		interval: 24 * time.Hour,
		load:     func(context.Context) (cidrset.Set, int, error) { return loaded, 0, nil },
	}
	loadedAt := s.loadedAt

	s.doReload(t.Context(), zerolog.Nop())

	if s.set.Covered() != loaded.Covered() {
		t.Fatalf("a clean reload must swap in the new list, covers %d want %d", s.set.Covered(), loaded.Covered())
	}
	if !s.loadedAt.After(loadedAt) {
		t.Fatal("a clean reload must restamp loadedAt")
	}
	if !s.retryAt.IsZero() {
		t.Fatal("a clean reload must clear the pending retry")
	}
}

// TestFilterStatsIdentityHoldsWithCIDRDrop: Kept plus every drop reason must
// still sum to Total, or a node the allow-list dropped is invisible in the
// X-Preprocessor-Stats header and on the dashboard.
func TestFilterStatsIdentityHoldsWithCIDRDrop(t *testing.T) {
	t.Parallel()

	p := newInlineProcessor()
	p.filters = []Filter{NewCIDRFilter(staticCIDR(mustCIDRSet(t, "198.51.100.0/24")))}

	var buf bytes.Buffer
	stats, err := p.Filter(t.Context(), &buf, FilterRequest{
		Body: []byte("vless://a@198.51.100.10:443#in\n" +
			"vless://b@203.0.113.5:443#out\n" +
			"vless://c@[2001:db8::1]:8443#v6\n" +
			"vless://d@198.51.100.11:443#in2\n" +
			"vmess://!!!not-base64!!!\n"),
		AllowedCountries: filter.All(),
	})
	if err != nil {
		t.Fatalf("Filter failed: %v", err)
	}
	if stats.Kept != 2 || stats.CIDRDrop != 1 || stats.IPv6Drop != 1 {
		t.Fatalf("stats = %+v, want kept=2 cidr_drop=1 ipv6_drop=1", stats)
	}
	drops := stats.DNSDrop + stats.GeoDrop + stats.ASNDrop + stats.CIDRDrop + stats.GeoBlockDrop + stats.IPv6Drop
	if stats.Total != stats.Kept+drops {
		t.Fatalf("total %d must equal kept+drops %d (%+v)", stats.Total, stats.Kept+drops, stats)
	}
	if stats.Unsupported != 1 {
		t.Fatalf("unsupported = %d, want 1: a refused line never became a node", stats.Unsupported)
	}
}

// TestMaybeRefreshDatabasesKicksStaleTablesOnly pins the three call sites that
// are the ENTIRE mechanism by which a refresh_interval ever fires: each store's
// maybeRefresh is reachable from nowhere else, so dropping one ships a table
// frozen for the container's uptime with every gate green — and for the
// allow-list, cidr_drop reading healthy while it admits nodes the operator
// dropped a month ago. The negative half matters as much: this runs once per
// source per cycle, so a gate that always fires is dozens of background
// re-downloads an hour.
func TestMaybeRefreshDatabasesKicksStaleTablesOnly(t *testing.T) {
	t.Parallel()

	const interval = 24 * time.Hour
	stale := time.Now().Add(-25 * time.Hour)
	loaded := mustCIDRSet(t, "10.0.0.0/16")
	dbip, registry, cidr := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)
	// The signal is sent from the reload goroutine, so the loads must not touch
	// t: they outlive the test body.
	geoLoad := func(signal chan<- struct{}) func(context.Context) ([]geofeed.Range, int, error) {
		return func(context.Context) ([]geofeed.Range, int, error) {
			signal <- struct{}{}
			return geoRanges(10), 0, nil
		}
	}
	p := &Processor{
		logger:   zerolog.Nop(),
		dbip:     &geoDB{name: "dbip", loadedAt: stale, interval: interval, load: geoLoad(dbip)},
		registry: &geoDB{name: "registry", loadedAt: stale, interval: interval, load: geoLoad(registry)},
		cidr: &cidrStore{
			set:      loaded,
			loadedAt: stale,
			interval: interval,
			load: func(context.Context) (cidrset.Set, int, error) {
				cidr <- struct{}{}
				return loaded, 0, nil
			},
		},
	}

	p.maybeRefreshDatabases(t.Context())

	for _, table := range []struct {
		name     string
		reloaded <-chan struct{}
	}{{"dbip", dbip}, {"registry", registry}, {"cidr", cidr}} {
		select {
		case <-table.reloaded:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never reloaded: a stale table nothing refreshes is frozen for the process lifetime",
				table.name)
		}
	}

	// A skipped reload spawns no goroutine, and absence has no signal to wait
	// on, so the fresh store is given a bounded window to misbehave in.
	refetched := make(chan struct{}, 1)
	freshStore := &Processor{
		logger: zerolog.Nop(),
		cidr: &cidrStore{
			set:      loaded,
			loadedAt: time.Now(),
			interval: interval,
			load: func(context.Context) (cidrset.Set, int, error) {
				refetched <- struct{}{}
				return loaded, 0, nil
			},
		},
	}

	freshStore.maybeRefreshDatabases(t.Context())

	select {
	case <-refetched:
		t.Fatal("a list loaded seconds ago must not re-download: the staleness gate is all that stands between one fetch and one per source per cycle")
	case <-time.After(500 * time.Millisecond):
	}
}
