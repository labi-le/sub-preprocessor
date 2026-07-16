package crawl //nolint:testpackage // drives the Crawler with unexported stubs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestQREndpoint(t *testing.T) {
	t.Parallel()

	f := &blockingFetcher{started: make(chan struct{}), release: make(chan struct{})}
	c := newTestCrawler(t, f)
	srv := httptest.NewServer(serveMux(context.Background(), c))
	defer srv.Close()

	// No login pending: /qr.png is 404 and /qr shows the done page.
	if code := do(t, srv, http.MethodGet, "/qr.png"); code != http.StatusNotFound {
		t.Fatalf("GET /qr.png (no login) = %d, want 404", code)
	}
	if body, ct, code := get(t, srv, "/qr"); code != http.StatusOK || !strings.Contains(ct, "text/html") || !strings.Contains(body, "No pending") {
		t.Fatalf("GET /qr (no login) = %d %q body=%q", code, ct, body)
	}

	// Publish a login token: /qr.png serves a PNG and /qr embeds it.
	u := "tg://login?token=QUJDMTIz"
	c.tgLoginURL.Store(&u)

	body, ct, code := get(t, srv, "/qr.png")
	if code != http.StatusOK || ct != "image/png" {
		t.Fatalf("GET /qr.png (pending) = %d %q, want 200 image/png", code, ct)
	}
	if len(body) < 8 || body[:8] != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("GET /qr.png did not return a PNG")
	}
	if b, _, _ := get(t, srv, "/qr"); !strings.Contains(b, `src="/qr.png"`) {
		t.Fatalf("GET /qr (pending) missing QR image: %q", b)
	}

	// Method guard.
	if got := do(t, srv, http.MethodPost, "/qr.png"); got != http.StatusMethodNotAllowed {
		t.Fatalf("POST /qr.png = %d, want 405", got)
	}
}

// get issues a GET and returns the body, Content-Type, and status code.
func get(t *testing.T, srv *httptest.Server, path string) (string, string, int) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.Header.Get("Content-Type"), resp.StatusCode
}
