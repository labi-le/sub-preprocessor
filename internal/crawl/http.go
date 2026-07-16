package crawl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// serveShutdownTimeout bounds the graceful drain when ctx is cancelled
	// before a hard stop.
	serveShutdownTimeout = 5 * time.Second
	// serveReadHeaderTimeout caps how long a client may take to send request
	// headers, guarding the trigger endpoint against slowloris-style stalls.
	serveReadHeaderTimeout = 10 * time.Second
)

// Serve runs an HTTP control surface for the crawler on addr until ctx is
// cancelled, then shuts the server down gracefully. It exposes:
//
//	POST /crawl   trigger one guarded cycle; 202 when started, 409 when a cycle
//	              is already running. The cycle runs in a background goroutine,
//	              so the request never blocks on a full crawl.
//	GET  /healthz liveness probe; always 200 "ok".
//
// Other methods on /crawl return 405; unknown paths return 404. Only the
// stdlib net/http is used.
func (c *Crawler) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           serveMux(ctx, c),
		ReadHeaderTimeout: serveReadHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("crawl http shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("crawl http serve: %w", err)
	}
}

// serveMux builds the crawler's HTTP routes. ctx is the cycle context handed to
// runGuarded when a trigger fires. Split out from Serve so tests can drive the
// handlers via httptest without binding a socket.
func serveMux(ctx context.Context, c *Crawler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/crawl", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Acquire the cycle lock in the handler so the 202/409 response is
		// accurate, then release it inside the goroutine when the cycle ends
		// (a sync.Mutex may be unlocked from a different goroutine). Holding it
		// across the handoff closes the check-then-act window a re-acquire leaves.
		if !c.running.TryLock() {
			http.Error(w, "cycle already running\n", http.StatusConflict)
			return
		}
		go func() {
			defer c.running.Unlock()
			c.RunOnce(ctx)
		}()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("crawl started\n"))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}
