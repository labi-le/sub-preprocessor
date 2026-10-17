package asn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"domains.lst/sub-preprocessor/internal/geofeed"
)

func TestResolve_NegativeCachesFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("cymru unreachable")
	calls := 0

	r := New(time.Second, time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		calls++
		return Result{}, wantErr
	}

	ip := netip.MustParseAddr("192.0.2.1")
	for i := range 3 {
		res, err := r.Resolve(context.Background(), ip)
		if !errors.Is(err, wantErr) {
			t.Fatalf("resolve #%d: err = %v, want %v", i, err, wantErr)
		}
		if res != (Result{}) {
			t.Fatalf("resolve #%d: result = %+v, want zero", i, res)
		}
	}

	if calls != 1 {
		t.Fatalf("lookups = %d, want 1 (failure negative-cached)", calls)
	}
}

func TestResolve_CachesSuccess(t *testing.T) {
	t.Parallel()

	want := Result{Country: geofeed.CountryCode{'F', 'I'}, Name: "Example AS"}
	calls := 0

	r := New(time.Second, time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		calls++
		return want, nil
	}

	ip := netip.MustParseAddr("192.0.2.2")
	for i := range 3 {
		res, err := r.Resolve(context.Background(), ip)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		if res != want {
			t.Fatalf("resolve #%d: result = %+v, want %+v", i, res, want)
		}
	}

	if calls != 1 {
		t.Fatalf("lookups = %d, want 1 (success cached)", calls)
	}
}

func TestResolve_CanceledContextNotNegativeCached(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("cymru unreachable")
	calls := 0

	r := New(time.Second, time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		calls++
		return Result{}, wantErr
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ip := netip.MustParseAddr("192.0.2.3")
	if _, err := r.Resolve(ctx, ip); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolve: err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("lookups = %d, want 0 (a canceled caller must not start work)", calls)
	}

	// The cancellation must not have poisoned the cache: the next callers do
	// one real lookup, and its genuine failure is negative-cached.
	for i := range 2 {
		if _, err := r.Resolve(context.Background(), ip); !errors.Is(err, wantErr) {
			t.Fatalf("resolve #%d: err = %v, want %v", i, err, wantErr)
		}
	}
	if calls != 1 {
		t.Fatalf("lookups = %d, want 1 (failure cached once)", calls)
	}
}

func TestResolve_CacheBounded(t *testing.T) {
	t.Parallel()

	r := New(time.Second, time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		return Result{Country: geofeed.CountryCode{'D', 'E'}}, nil
	}

	n := maxCacheEntries + maxCacheEntries/2
	for i := range n {
		ip := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		if _, err := r.Resolve(context.Background(), ip); err != nil {
			t.Fatalf("resolve %s: %v", ip, err)
		}
		if got := r.CacheLen(); got > maxCacheEntries {
			t.Fatalf("cache grew to %d entries, cap is %d", got, maxCacheEntries)
		}
	}
}

func TestResolve_UsesConfiguredCacheTTL(t *testing.T) {
	t.Parallel()

	r := New(time.Second, 12*time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		return Result{Country: geofeed.CountryCode{'F', 'I'}, Name: "AS"}, nil
	}
	ip := netip.MustParseAddr("192.0.2.10")
	before := time.Now()
	if _, err := r.Resolve(context.Background(), ip); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	exp := r.cache[ip].expiresAt
	r.mu.Unlock()
	if d := exp.Sub(before); d < 11*time.Hour || d > 13*time.Hour {
		t.Fatalf("positive TTL = %v, want ~12h", d)
	}

	// A zero/negative configured TTL falls back to the package default.
	if rd := New(time.Second, 0); rd.cacheTTL != defaultCacheTTL {
		t.Fatalf("zero ttl should fall back to %v, got %v", defaultCacheTTL, rd.cacheTTL)
	}
}

func TestResolve_CoalescesConcurrentSameIP(t *testing.T) {
	t.Parallel()

	const workers = 16
	want := Result{Country: geofeed.CountryCode{'U', 'S'}, Name: "EXAMPLE-AS"}
	r := New(time.Second, time.Hour)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	var once sync.Once
	r.lookupFn = func(ctx context.Context, _ netip.Addr) (Result, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-release:
			return want, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}

	ip := netip.MustParseAddr("198.18.0.1")
	start := make(chan struct{})
	results := make(chan Result, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			res, err := r.Resolve(context.Background(), ip)
			results <- res
			errs <- err
		})
	}
	close(start)
	<-started
	// Wait until every caller has joined the same flight. Without coalescing
	// the waiters never accumulate, so the test fails instead of racing the
	// first lookup's completion.
	waitFlightWaiters(t, r, ip, workers)
	close(release)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent resolve: %v", err)
		}
	}
	for res := range results {
		if res != want {
			t.Fatalf("result = %+v, want %+v", res, want)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("lookup calls = %d, want 1 (concurrent misses coalesced)", n)
	}

	if _, err := r.Resolve(context.Background(), ip); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("warm resolve changed lookup calls to %d", n)
	}
}

func TestResolve_CanceledLeaderDoesNotFailWaiters(t *testing.T) {
	t.Parallel()

	want := Result{Country: geofeed.CountryCode{'D', 'E'}, Name: "EXAMPLE-AS"}
	r := New(time.Second, time.Hour)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	var once sync.Once
	r.lookupFn = func(ctx context.Context, _ netip.Addr) (Result, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-release:
			return want, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}

	ip := netip.MustParseAddr("198.18.0.2")
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := r.Resolve(leaderCtx, ip)
		leaderErr <- err
	}()
	<-started

	waiterErr := make(chan error, 1)
	go func() {
		_, err := r.Resolve(context.Background(), ip)
		waiterErr <- err
	}()
	waitFlightWaiters(t, r, ip, 2)

	cancelLeader()
	close(release)

	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want context.Canceled", err)
	}
	if err := <-waiterErr; err != nil {
		t.Fatalf("waiter err = %v, want nil", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("lookup calls = %d, want 1", n)
	}
	// The canceled leader must not have negative-cached the shared result.
	if _, err := r.Resolve(context.Background(), ip); err != nil {
		t.Fatalf("resolve after canceled leader: %v", err)
	}
}

func TestResolve_CanceledWaiterLeavesFlightRunning(t *testing.T) {
	t.Parallel()

	want := Result{Country: geofeed.CountryCode{'F', 'R'}, Name: "EXAMPLE-AS"}
	r := New(time.Second, time.Hour)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	var once sync.Once
	r.lookupFn = func(ctx context.Context, _ netip.Addr) (Result, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-release:
			return want, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}

	ip := netip.MustParseAddr("198.18.0.3")
	leaderErr := make(chan error, 1)
	go func() {
		_, err := r.Resolve(context.Background(), ip)
		leaderErr <- err
	}()
	<-started

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		_, err := r.Resolve(waiterCtx, ip)
		waiterErr <- err
	}()
	waitFlightWaiters(t, r, ip, 2)
	cancelWaiter()

	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}

	close(release)
	if err := <-leaderErr; err != nil {
		t.Fatalf("leader err = %v, want nil", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("lookup calls = %d, want 1", n)
	}
}

func TestResolve_LastWaiterCancellationStopsFlight(t *testing.T) {
	t.Parallel()

	r := New(time.Second, time.Hour)
	started := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	r.lookupFn = func(ctx context.Context, _ netip.Addr) (Result, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		close(stopped)
		return Result{}, ctx.Err()
	}

	ip := netip.MustParseAddr("198.18.0.4")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := r.Resolve(ctx, ip)
		errCh <- err
	}()
	<-started
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve err = %v, want context.Canceled", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("flight kept running after its last waiter left")
	}

	r.flightMu.Lock()
	flights := len(r.flights)
	r.flightMu.Unlock()
	if flights != 0 {
		t.Fatalf("flight map has %d entries, want 0", flights)
	}
	r.mu.RLock()
	_, cached := r.cache[ip]
	r.mu.RUnlock()
	if cached {
		t.Fatal("abandoned lookup was cached")
	}
}

func TestResolve_TotalBudgetBoundsFlight(t *testing.T) {
	t.Parallel()

	r := New(100*time.Millisecond, time.Hour)
	r.lookupFn = func(ctx context.Context, _ netip.Addr) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}

	ip := netip.MustParseAddr("198.18.0.5")
	start := time.Now()
	_, err := r.Resolve(context.Background(), ip)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("resolve took %v, want the resolver-owned 100ms budget", elapsed)
	}
	r.mu.RLock()
	_, cached := r.cache[ip]
	r.mu.RUnlock()
	if !cached {
		t.Fatal("resolver-owned timeout should be negative-cached")
	}
}

// TestResolve_LocalDNSEndToEnd runs the real query path against a loopback
// Cymru stub: distinct cold IPs in one AS share a single AS-name query, the
// second pass adds nothing, and every IP keeps its own origin country.
func TestResolve_LocalDNSEndToEnd(t *testing.T) {
	t.Parallel()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var origins, names atomic.Int64
	started := make(chan struct{})
	server := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }}
	server.Handler = dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		msg := new(dns.Msg)
		msg.SetReply(req)
		msg.RecursionAvailable = true
		for _, q := range req.Question {
			var value string
			switch {
			case strings.HasSuffix(q.Name, cymruOriginDomain+"."):
				origins.Add(1)
				value = "64500 | 198.18.0.0/24 | US | arin | 2020-01-01"
			case strings.EqualFold(q.Name, "AS64500."+cymruASDomain+"."):
				names.Add(1)
				value = "64500 | US | arin | 2020-01-01 | EXAMPLE-AS"
			default:
				msg.Rcode = dns.RcodeNameError
				continue
			}
			msg.Answer = append(msg.Answer, &dns.TXT{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 3600},
				Txt: []string{value},
			})
		}
		_ = w.WriteMsg(msg)
	})
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	<-started
	t.Cleanup(func() {
		_ = server.Shutdown()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	r := New(5*time.Second, time.Hour)
	r.lookupResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, pc.LocalAddr().String())
	}}

	const count = 64
	want := Result{Country: geofeed.CountryCode{'U', 'S'}, Name: "EXAMPLE-AS"}
	for pass := range 2 {
		for i := range count {
			ip := netip.AddrFrom4([4]byte{198, 18, 0, byte(i)})
			got, err := r.Resolve(context.Background(), ip)
			if err != nil {
				t.Fatalf("pass=%d ip=%s: %v", pass, ip, err)
			}
			if got != want {
				t.Fatalf("pass=%d ip=%s result = %+v, want %+v", pass, ip, got, want)
			}
		}
	}

	if got := origins.Load(); got != count {
		t.Fatalf("origin queries = %d, want %d", got, count)
	}
	if got := names.Load(); got != 1 {
		t.Fatalf("name queries = %d, want 1 (shared by ASN)", got)
	}
}

func TestResolve_NameCacheReusedAcrossIPs(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	originQueries, nameQueries := 0, 0
	r := New(time.Second, 12*time.Hour)
	r.lookupTXT = func(_ context.Context, name string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(name, cymruOriginDomain):
			originQueries++
			country := "US"
			if strings.HasPrefix(name, "2.") {
				country = "DE"
			}
			return []string{"64500 | 198.18.0.0/24 | " + country + " | arin | 2020-01-01"}, nil
		case name == "AS64500."+cymruASDomain:
			nameQueries++
			return []string{"64500 | US | arin | 2020-01-01 | EXAMPLE-AS"}, nil
		default:
			return nil, fmt.Errorf("unexpected TXT query %q", name)
		}
	}

	ipUS := netip.MustParseAddr("198.18.0.1")
	ipDE := netip.MustParseAddr("198.18.0.2")
	us := geofeed.CountryCode{'U', 'S'}
	de := geofeed.CountryCode{'D', 'E'}
	for pass := range 2 {
		gotUS, err := r.Resolve(context.Background(), ipUS)
		if err != nil {
			t.Fatalf("pass %d US: %v", pass, err)
		}
		gotDE, err := r.Resolve(context.Background(), ipDE)
		if err != nil {
			t.Fatalf("pass %d DE: %v", pass, err)
		}
		if gotUS != (Result{Country: us, Name: "EXAMPLE-AS"}) {
			t.Fatalf("pass %d US result = %+v", pass, gotUS)
		}
		if gotDE != (Result{Country: de, Name: "EXAMPLE-AS"}) {
			t.Fatalf("pass %d DE result = %+v (country must stay per prefix)", pass, gotDE)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if originQueries != 2 {
		t.Fatalf("origin queries = %d, want 2 (one per distinct IP)", originQueries)
	}
	if nameQueries != 1 {
		t.Fatalf("name queries = %d, want 1 (shared by ASN)", nameQueries)
	}

	r.mu.RLock()
	entry, ok := r.nameCache[64500]
	r.mu.RUnlock()
	if !ok {
		t.Fatal("AS name not cached")
	}
	if d := time.Until(entry.expiresAt); d < 11*time.Hour || d > 13*time.Hour {
		t.Fatalf("name cache TTL = %v, want ~12h", d)
	}
}

func TestResolve_NameLookupFailureStillFailsResolve(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("AS name lookup down")
	var mu sync.Mutex
	originQueries, nameQueries := 0, 0
	r := New(time.Second, time.Hour)
	r.lookupTXT = func(_ context.Context, name string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(name, cymruOriginDomain) {
			originQueries++
			return []string{"64500 | 198.18.0.0/24 | US | arin | 2020-01-01"}, nil
		}
		nameQueries++
		return nil, wantErr
	}

	for _, raw := range []string{"198.18.0.1", "198.18.0.2"} {
		res, err := r.Resolve(context.Background(), netip.MustParseAddr(raw))
		if !errors.Is(err, wantErr) {
			t.Fatalf("resolve %s: err = %v, want %v", raw, err, wantErr)
		}
		if res != (Result{}) {
			t.Fatalf("resolve %s: result = %+v, want zero (no country-only degradation)", raw, res)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if originQueries != 2 {
		t.Fatalf("origin queries = %d, want 2", originQueries)
	}
	if nameQueries != 1 {
		t.Fatalf("name queries = %d, want 1 (ASN-wide negative cache)", nameQueries)
	}
}

func TestResolve_EmptyNameIsCachedAsSuccess(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	nameQueries := 0
	r := New(time.Second, 12*time.Hour)
	r.lookupTXT = func(_ context.Context, name string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(name, cymruOriginDomain) {
			return []string{"64500 | 198.18.0.0/24 | US | arin | 2020-01-01"}, nil
		}
		nameQueries++
		return []string{"no pipe-delimited record here"}, nil
	}

	for _, raw := range []string{"198.18.0.1", "198.18.0.2"} {
		res, err := r.Resolve(context.Background(), netip.MustParseAddr(raw))
		if err != nil {
			t.Fatalf("resolve %s: %v", raw, err)
		}
		want := Result{Country: geofeed.CountryCode{'U', 'S'}}
		if res != want {
			t.Fatalf("resolve %s: result = %+v, want %+v", raw, res, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if nameQueries != 1 {
		t.Fatalf("name queries = %d, want 1 (empty name is a cached success)", nameQueries)
	}
}

func TestResolve_CoalescesConcurrentNameLookups(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	originQueries, nameQueries := 0, 0
	nameStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r := New(5*time.Second, time.Hour)
	r.lookupTXT = func(ctx context.Context, name string) ([]string, error) {
		if strings.HasSuffix(name, cymruOriginDomain) {
			mu.Lock()
			originQueries++
			mu.Unlock()
			country := "US"
			if strings.HasPrefix(name, "2.") {
				country = "DE"
			}
			return []string{"64500 | 198.18.0.0/24 | " + country + " | arin | 2020-01-01"}, nil
		}
		mu.Lock()
		nameQueries++
		mu.Unlock()
		once.Do(func() { close(nameStarted) })
		select {
		case <-release:
			return []string{"64500 | US | arin | 2020-01-01 | EXAMPLE-AS"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	ipUS := netip.MustParseAddr("198.18.0.1")
	ipDE := netip.MustParseAddr("198.18.0.2")
	type outcome struct {
		ip  netip.Addr
		res Result
		err error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, ip := range []netip.Addr{ipUS, ipDE} {
		wg.Go(func() {
			res, err := r.Resolve(context.Background(), ip)
			results <- outcome{ip: ip, res: res, err: err}
		})
	}

	<-nameStarted
	close(release)
	wg.Wait()
	close(results)

	us := geofeed.CountryCode{'U', 'S'}
	de := geofeed.CountryCode{'D', 'E'}
	for out := range results {
		if out.err != nil {
			t.Fatalf("resolve %s: %v", out.ip, out.err)
		}
		want := us
		if out.ip == ipDE {
			want = de
		}
		if out.res != (Result{Country: want, Name: "EXAMPLE-AS"}) {
			t.Fatalf("resolve %s: result = %+v, want country %v", out.ip, out.res, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if nameQueries != 1 {
		t.Fatalf("name queries = %d, want 1 (concurrent name lookups coalesced)", nameQueries)
	}
	if originQueries != 2 {
		t.Fatalf("origin queries = %d, want 2", originQueries)
	}
}

func TestResolve_CapsIPExpiryByReusedName(t *testing.T) {
	t.Parallel()

	// A successful name entry that is about to expire must not be copied into
	// a fresh full-lived IP entry.
	ipSuccess := netip.MustParseAddr("198.18.0.1")
	nameExpiry := time.Now().Add(time.Minute)
	r := New(time.Second, time.Hour)
	r.storeName(64500, cachedName{name: "EXAMPLE-AS", expiresAt: nameExpiry})
	r.lookupTXT = func(_ context.Context, name string) ([]string, error) {
		if !strings.HasSuffix(name, cymruOriginDomain) {
			return nil, fmt.Errorf("name must come from the cache, got %q", name)
		}
		return []string{"64500 | 198.18.0.0/24 | US | arin | 2020-01-01"}, nil
	}
	if _, err := r.Resolve(context.Background(), ipSuccess); err != nil {
		t.Fatal(err)
	}
	r.mu.RLock()
	ipExpiry := r.cache[ipSuccess].expiresAt
	r.mu.RUnlock()
	if !ipExpiry.Equal(nameExpiry) {
		t.Fatalf("IP expiry = %v, want the name expiry %v", ipExpiry, nameExpiry)
	}

	// The same cap applies to a reused negative name entry, so failure
	// recovery is not postponed past the name entry's own expiry.
	ipFailure := netip.MustParseAddr("198.18.0.2")
	nameErr := errors.New("AS name down")
	r2 := New(time.Second, time.Hour)
	r2.storeName(64500, cachedName{err: nameErr, expiresAt: nameExpiry})
	r2.lookupTXT = r.lookupTXT
	if _, err := r2.Resolve(context.Background(), ipFailure); !errors.Is(err, nameErr) {
		t.Fatalf("err = %v, want %v", err, nameErr)
	}
	r2.mu.RLock()
	ipExpiry = r2.cache[ipFailure].expiresAt
	r2.mu.RUnlock()
	if !ipExpiry.Equal(nameExpiry) {
		t.Fatalf("negative IP expiry = %v, want the name expiry %v", ipExpiry, nameExpiry)
	}
}

func TestResolve_WarmPathsDoNotAllocate(t *testing.T) {
	want := Result{Country: geofeed.CountryCode{'F', 'I'}, Name: "EXAMPLE-AS"}
	ip := netip.MustParseAddr("198.18.0.9")

	r := New(time.Second, time.Hour)
	r.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		return want, nil
	}
	if _, err := r.Resolve(context.Background(), ip); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(100, func() { _, _ = r.Resolve(context.Background(), ip) }); allocs != 0 {
		t.Fatalf("warm success allocs = %v, want 0", allocs)
	}

	wantErr := errors.New("cymru unreachable")
	rn := New(time.Second, time.Hour)
	rn.lookupFn = func(_ context.Context, _ netip.Addr) (Result, error) {
		return Result{}, wantErr
	}
	if _, err := rn.Resolve(context.Background(), ip); !errors.Is(err, wantErr) {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(100, func() { _, _ = rn.Resolve(context.Background(), ip) }); allocs != 0 {
		t.Fatalf("warm negative allocs = %v, want 0", allocs)
	}
}

// waitFlightWaiters blocks until the in-flight lookup for ip has at least want
// waiters, so a cancellation test cannot race the second caller's join.
func waitFlightWaiters(t *testing.T, r *Resolver, ip netip.Addr, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.flightMu.Lock()
		n := 0
		if f := r.flights[ip]; f != nil {
			n = f.waiters
		}
		r.flightMu.Unlock()
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("flight waiters = %d, want %d", n, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestParseOriginRecord_MultiOrigin pins the country of a prefix announced by
// several ASes. Field 0 is an AS LIST, and parsing it whole used to fail the
// record and throw away the country in field 2 -- so the annotate chain missed
// and the ASN filter fell open on every multi-origin prefix. Records below are
// verbatim Cymru origin answers for multi-origin prefixes.
func TestParseOriginRecord_MultiOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		txt         string
		wantASN     uint32
		wantCountry geofeed.CountryCode
	}{
		{
			name:        "single origin",
			txt:         "216071 | 146.103.121.0/24 | AE | ripencc | 1992-10-23",
			wantASN:     216071,
			wantCountry: geofeed.CountryCode{'A', 'E'},
		},
		{
			name:        "two origins arin",
			txt:         "15169 43515 | 35.212.128.0/17 | US | arin | 2017-09-29",
			wantASN:     15169,
			wantCountry: geofeed.CountryCode{'U', 'S'},
		},
		{
			name:        "two origins ripencc",
			txt:         "13238 208398 | 87.250.224.0/19 | RU | ripencc | 2006-01-30",
			wantASN:     13238,
			wantCountry: geofeed.CountryCode{'R', 'U'},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotASN, gotCountry, err := parseOriginRecord(tc.txt)
			if err != nil {
				t.Fatalf("parseOriginRecord(%q) = %v", tc.txt, err)
			}
			if gotCountry != tc.wantCountry {
				t.Errorf("country = %q, want %q", gotCountry.String(), tc.wantCountry.String())
			}
			// One number, because the caller builds AS<n>.asn.cymru.com from it.
			if gotASN != tc.wantASN {
				t.Errorf("asn = %d, want %d (first listed origin)", gotASN, tc.wantASN)
			}
		})
	}

	// A field 0 with no number at all is still an error: there is no AS to name
	// and no reason to invent a Resolve success shape for it.
	if _, _, err := parseOriginRecord("nonsense | 10.0.0.0/8 | US | arin | 2020-01-01"); err == nil {
		t.Error("an unparseable AS field must still error")
	}
}

// TestParseOriginRecords_EmptyAnswerIsNoRecord pins the guard that keeps an
// empty origin answer from reading as a parsed record: the caller builds
// AS<n>.asn.cymru.com from the result, and continuing with asn 0 would query a
// name Cymru does not serve. An answer with no parseable record must error.
func TestParseOriginRecords_EmptyAnswerIsNoRecord(t *testing.T) {
	t.Parallel()

	if _, _, err := parseOriginRecords(nil); !errors.Is(err, errEmptyOriginAnswer) {
		t.Fatalf("nil answer err = %v, want errEmptyOriginAnswer", err)
	}
	if _, _, err := parseOriginRecords([]string{}); !errors.Is(err, errEmptyOriginAnswer) {
		t.Fatalf("empty answer err = %v, want errEmptyOriginAnswer", err)
	}
	if _, _, err := parseOriginRecords([]string{"not a record"}); err == nil {
		t.Fatal("an answer with no parseable record must error")
	}

	// The first parseable record wins, as it did when the loop lived in
	// lookup: a leading malformed record must not lose the good one after it.
	asn, country, err := parseOriginRecords([]string{
		"garbage | x | ZZ | arin | 2020-01-01",
		"64500 | 192.0.2.0/24 | US | arin | 2020-01-01",
	})
	if err != nil {
		t.Fatalf("a later parseable record must win: %v", err)
	}
	if asn != 64500 || country != (geofeed.CountryCode{'U', 'S'}) {
		t.Fatalf("got asn %d country %q, want 64500 US", asn, country)
	}
}
