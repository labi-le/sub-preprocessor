package crawl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"rsc.io/qr"
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
//	GET  /qr      HTML page (auto-refreshing) with the MTProto login QR while a
//	              login is pending; GET /qr.png is the raw QR PNG (404 when none).
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
	mux.HandleFunc("/qr.png", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := c.tgLoginURL.Load()
		if p == nil {
			http.Error(w, "no pending Telegram login\n", http.StatusNotFound)
			return
		}
		code, err := qr.Encode(*p, qr.M)
		if err != nil {
			http.Error(w, "qr encode failed\n", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(code.PNG())
	})
	mux.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		page := qrPendingPage
		if c.tgLoginURL.Load() == nil {
			page = qrDonePage
		}
		_, _ = w.Write([]byte(page))
	})
	return mux
}

// qrPendingPage renders the pending MTProto login QR; it reloads every 15s so
// the browser always shows a currently-valid (auto-refreshing) login token, and
// no-store keeps the embedded /qr.png fresh.
const qrPendingPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="15"><title>Telegram login</title></head>
<body style="font-family:sans-serif;text-align:center;padding:2rem">
<h2>Link this crawler to your Telegram</h2>
<p>In Telegram: <b>Settings -&gt; Devices -&gt; Link Desktop Device</b>, then scan:</p>
<img alt="login QR" src="/qr.png" style="width:320px;height:320px;image-rendering:pixelated">
<p style="color:#888">The code refreshes automatically; this page reloads every 15s.</p>
</body></html>
`

// qrDonePage is shown when no login is pending (authorized, or MTProto off).
const qrDonePage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Telegram login</title></head>
<body style="font-family:sans-serif;text-align:center;padding:2rem">
<h2>No pending Telegram login</h2>
<p>The crawler is authorized (or MTProto is disabled). You can close this page.</p>
</body></html>
`
