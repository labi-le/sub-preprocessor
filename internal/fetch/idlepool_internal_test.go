package fetch

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// idlePoolLimit pins the shipped ceiling. The behavioural tests below fetch
// more distinct origins than this, because with MaxIdleConns=0 every one of
// them would stay retained.
const idlePoolLimit = 100

// clientConstructors are the two exported entry points that must share the
// transport policy.
var clientConstructors = map[string]func() *http.Client{
	"safe":         NewSafeHTTPClient,
	"unrestricted": NewUnrestrictedHTTPClient,
}

// countingServer reports how many TCP connections it currently holds open.
// Tests redirect the client's dial to its listener while keeping a distinct
// hostname per request, so the listening socket serves many transport pool
// keys and every key's retained socket shows up in the count.
type countingServer struct {
	*httptest.Server
	open atomic.Int64
}

func newCountingServer(t *testing.T, handler http.HandlerFunc) *countingServer {
	t.Helper()

	srv := &countingServer{}
	srv.Server = httptest.NewUnstartedServer(handler)
	srv.Server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			srv.open.Add(1)
		case http.StateClosed:
			srv.open.Add(-1)
		case http.StateActive, http.StateIdle, http.StateHijacked:
			// Only new and closed change how many sockets the server holds.
		}
	}
	srv.Server.Start()
	t.Cleanup(srv.Server.Close)
	return srv
}

// redirectDial points client at srv while leaving the request hostname alone,
// which is what makes each test hostname a distinct pool key.
func (s *countingServer) redirectDial(client *http.Client) {
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, s.Listener.Addr().String())
	}
}

// waitOpen blocks until exactly want connections are open. StateClosed is
// delivered asynchronously, so the eviction and expiry paths need a poll
// instead of a fixed sleep.
func (s *countingServer) waitOpen(t *testing.T, want int64) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := s.open.Load(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open connections = %d, want %d", s.open.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "1")
	_, _ = io.WriteString(w, "x")
}

func getDrained(t *testing.T, client *http.Client, rawURL string) {
	t.Helper()

	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("fetch %s: %v", rawURL, err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read %s: %v", rawURL, err)
	}
}

// Both constructors must carry the same pool policy: churn across more
// distinct origins than the ceiling leaves exactly the ceiling retained, not
// one socket per origin held until the peer closes it.
func TestNewHTTPClientBoundsRetainedIdleConnections(t *testing.T) {
	t.Parallel()

	for name, newClient := range clientConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := newCountingServer(t, okHandler)
			client := newClient()
			srv.redirectDial(client)
			t.Cleanup(client.CloseIdleConnections)

			for i := range idlePoolLimit + 30 {
				getDrained(t, client, fmt.Sprintf("http://origin-%03d.example/", i))
			}
			srv.waitOpen(t, idlePoolLimit)
		})
	}
}

// The 90s production expiry cannot be observed in a test, and the seam lets a
// test pass any timeout, so pin that the exported constructors ship the agreed
// policy instead of the transport zero values (unlimited pool, sockets that
// never expire). The 100 ceiling is pinned behaviourally above.
func TestNewHTTPClientSetsProductionIdlePolicy(t *testing.T) {
	t.Parallel()

	for name, newClient := range clientConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := newClient()
			tr, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
			}
			if tr.MaxIdleConns != idlePoolLimit {
				t.Fatalf("MaxIdleConns = %d, want %d", tr.MaxIdleConns, idlePoolLimit)
			}
			if tr.IdleConnTimeout != 90*time.Second {
				t.Fatalf("IdleConnTimeout = %s, want 90s", tr.IdleConnTimeout)
			}
		})
	}
}

// Normal reuse must survive the ceiling: sequential requests to one origin
// keep one socket.
func TestNewHTTPClientReusesOneConnectionPerOrigin(t *testing.T) {
	t.Parallel()

	srv := newCountingServer(t, okHandler)
	client := NewUnrestrictedHTTPClient()
	srv.redirectDial(client)
	t.Cleanup(client.CloseIdleConnections)

	for range 3 {
		getDrained(t, client, "http://origin.example/")
	}
	srv.waitOpen(t, 1)
}

// The production expiry is 90s, so this test uses the seam to pass a short
// timeout and prove an idle socket is closed by the transport itself.
func TestNewHTTPClientExpiresIdleConnections(t *testing.T) {
	t.Parallel()

	const idle = 250 * time.Millisecond

	srv := newCountingServer(t, okHandler)
	client := newHTTPClientWithIdleTimeout(false, idle)
	srv.redirectDial(client)
	t.Cleanup(client.CloseIdleConnections)

	getDrained(t, client, "http://origin.example/")
	srv.waitOpen(t, 0)
}

// IdleConnTimeout applies to idle sockets only. A request that outlives the
// timeout must still complete on its original connection.
func TestIdleTimeoutDoesNotCloseActiveRequest(t *testing.T) {
	t.Parallel()

	const idle = 30 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	srv := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "ok")
	})
	t.Cleanup(releaseOnce)
	client := newHTTPClientWithIdleTimeout(false, idle)
	srv.redirectDial(client)
	t.Cleanup(client.CloseIdleConnections)

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Get("http://slow.example/")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		done <- result{body: string(b), err: err}
	}()

	select {
	case <-started:
	case got := <-done:
		t.Fatalf("request ended before the handler started: %v", got.err)
	}
	time.Sleep(5 * idle)
	if got := srv.open.Load(); got != 1 {
		t.Fatalf("active connection closed after idle timeout: open = %d, want 1", got)
	}
	releaseOnce()

	got := <-done
	if got.err != nil {
		t.Fatalf("active request failed after idle timeout: %v", got.err)
	}
	if got.body != "ok" {
		t.Fatalf("body = %q, want %q", got.body, "ok")
	}
}
