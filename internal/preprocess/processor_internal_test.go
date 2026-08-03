package preprocess

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

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
				{Tag: config.TagASN, Providers: []string{config.ProviderASN}},
				{Tag: config.TagGEO, Providers: []string{
					config.ProviderGeofeed, config.ProviderDBIP, config.ProviderRegistry, config.ProviderASN,
				}},
			},
			haveDBIP: true, haveRegistry: true,
			want: []string{config.ProviderGeofeed, config.ProviderDBIP, config.ProviderRegistry},
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
			name:     "no GEO entry falls back to the geofeed",
			annotate: []config.AnnotateSpec{{Tag: config.TagASN, Providers: []string{config.ProviderASN}}},
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
