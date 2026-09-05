package crawl //nolint:testpackage // drives the Crawler with unexported stubs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// blockingFetcher is a network-free fetchClient whose page blocks until release
// is closed, so a triggered cycle can be held in-flight while the test probes
// the concurrent-trigger path. It counts calls so the test can confirm a cycle
// actually ran.
type blockingFetcher struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (f *blockingFetcher) page(_ context.Context, _ string) (string, error) {
	if f.calls.Add(1) == 1 {
		close(f.started) // signal the first cycle has entered the fetch
	}
	<-f.release // hold the cycle (and thus the runGuarded lock) in-flight
	return "", nil
}

// newTestCrawler builds a Crawler that never touches the network: one seed
// channel, one page, state persistence disabled, and a valid empty private.yaml.
func newTestCrawler(t *testing.T, f *blockingFetcher) *Crawler {
	t.Helper()
	priv := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}
	return &Crawler{
		opts:   Options{Channels: []string{"testchannel"}, Pages: 1, PrivatePath: priv},
		client: f,
		logger: zerolog.Nop(),
	}
}

// TestTriggerSharesTheScheduleBudget: a triggered cycle must run under the
// schedule loop's per-cycle bound, not the daily cap: at the shipped 30m
// interval a POST /crawl on the daily 2h budget would skip up to four
// scheduled ticks — the overrun cycleBudget exists to prevent. When no
// schedule loop has published a budget the trigger falls back to the daily cap.
func TestTriggerSharesTheScheduleBudget(t *testing.T) {
	t.Parallel()

	c := &Crawler{logger: zerolog.Nop()}
	if got := c.triggerBudget(); got != cycleBudget(oneDay) {
		t.Errorf("trigger-only budget = %v, want the daily cap %v", got, cycleBudget(oneDay))
	}
	interval := 30 * time.Minute
	c.scheduleBudget.Store(int64(cycleBudget(interval)))
	if got := c.triggerBudget(); got != cycleBudget(interval) {
		t.Errorf("trigger budget under a 30m schedule = %v, want %v", got, cycleBudget(interval))
	}
	if got := c.triggerBudget(); got == cycleBudget(oneDay) {
		t.Errorf("trigger budget = the daily cap %v; the schedule's bound must win", got)
	}
	c.scheduleBudget.Store(0)
	if got := c.triggerBudget(); got != cycleBudget(oneDay) {
		t.Errorf("after the schedule stops, budget = %v, want the daily cap", got)
	}
}

func TestServeHandlers(t *testing.T) {
	t.Parallel()

	f := &blockingFetcher{started: make(chan struct{}), release: make(chan struct{})}
	c := newTestCrawler(t, f)

	// Serve builds the mux; drive it via httptest without opening a socket.
	// A cancelled ctx makes Serve return immediately, but the mux/handlers are
	// exercised directly through the server we build here instead.
	srv := httptest.NewServer(serveMux(context.Background(), c))
	defer srv.Close()

	// GET /healthz -> 200 ok
	if resp := do(t, srv, http.MethodGet, "/healthz"); resp != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", resp)
	}

	// GET /crawl -> 405
	if resp := do(t, srv, http.MethodGet, "/crawl"); resp != http.StatusMethodNotAllowed {
		t.Fatalf("GET /crawl = %d, want 405", resp)
	}

	// POST /crawl -> 202 and the cycle runs (page is entered).
	if resp := do(t, srv, http.MethodPost, "/crawl"); resp != http.StatusAccepted {
		t.Fatalf("POST /crawl = %d, want 202", resp)
	}
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatal("triggered cycle never entered the fetcher")
	}

	// Second POST /crawl while the first cycle is still in-flight -> 409.
	if resp := do(t, srv, http.MethodPost, "/crawl"); resp != http.StatusConflict {
		t.Fatalf("concurrent POST /crawl = %d, want 409", resp)
	}

	// Release the cycle and confirm exactly one cycle ran.
	close(f.release)
	waitUnlock(t, c)
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("fetcher called %d times, want 1 (concurrent trigger must be skipped)", got)
	}
}

// do issues one request and returns the status code.
func do(t *testing.T, srv *httptest.Server, method, path string) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// waitUnlock blocks until the crawl cycle has released the running lock.
func waitUnlock(t *testing.T, c *Crawler) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if c.running.TryLock() {
			c.running.Unlock()
			return
		}
		select {
		case <-deadline:
			t.Fatal("cycle never released the running lock")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestRunDailyGuardedWaitsOutARunningCycle pins LogicSources#5: a cycle that
// holds the running lock at the daily instant (an HTTP trigger that straddles
// it) must not consume the day's scheduled run. runDailyGuarded retries until
// the lock frees; without the retry the collided tick is dropped and the next
// attempt is a full 24h away. Mutates the retry pacing var, so not parallel.
func TestRunDailyGuardedWaitsOutARunningCycle(t *testing.T) {
	old := dailyRetryAfter
	dailyRetryAfter = 10 * time.Millisecond
	t.Cleanup(func() { dailyRetryAfter = old })

	f := &blockingFetcher{started: make(chan struct{}), release: make(chan struct{})}
	c := newTestCrawler(t, f)

	// The in-flight cycle: an HTTP trigger holding the running lock while its
	// page fetch blocks.
	triggerDone := make(chan struct{})
	go func() {
		c.runGuarded(context.Background(), 0)
		close(triggerDone)
	}()
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the running cycle never entered its fetch")
	}

	// The daily instant fires while that cycle holds the lock.
	dailyDone := make(chan struct{})
	go func() {
		c.runDailyGuarded(context.Background(), 0)
		close(dailyDone)
	}()
	// Give the retry loop time to attempt (and be refused) at least once while
	// the trigger still holds the lock.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-dailyDone:
		t.Fatal("the scheduled run started before the running cycle released the lock")
	default:
	}

	close(f.release)
	select {
	case <-triggerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the running cycle never finished after its fetch was released")
	}
	select {
	case <-dailyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduled run never started after the lock freed; the collided tick was dropped")
	}
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("fetcher called %d times, want 2: the trigger's cycle and the retried scheduled one", got)
	}
}
