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
		Buffer:      &bytes.Buffer{},
		Resolved:    map[string][]netip.Addr{"example.com": {ipA, ipB}},
		Stats:       &Stats{},
		IsFirstNode: true,
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
		Buffer:      &bytes.Buffer{},
		Resolved:    map[string][]netip.Addr{},
		Stats:       &Stats{},
		IsFirstNode: true,
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
			Buffer:      &bytes.Buffer{},
			Resolved:    map[string][]netip.Addr{},
			Stats:       &Stats{},
			IsFirstNode: true,
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
