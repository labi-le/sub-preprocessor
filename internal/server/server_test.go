package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/server"
	"domains.lst/sub-preprocessor/internal/stable"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

func nopLogger() zerolog.Logger {
	return zerolog.Nop()
}

func newServer(stableHolder *stable.Holder) *server.Server {
	return server.New(nopLogger(), ":8080", stableHolder)
}

func doGet(t *testing.T, srv *server.Server, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// TestServerRecoversHandlerPanic pins the recovery middleware: neither fiber
// nor fasthttp recovers a panic, so without it one would kill the process and
// with it the in-memory stable list. No live route panics today, hence the
// route mounted here — the middleware is registered before any route, so it
// wraps this one too.
func TestServerRecoversHandlerPanic(t *testing.T) {
	t.Parallel()

	srv := newServer(stable.NewHolder())
	srv.TestApp().Get("/boom", func(*fiber.Ctx) error {
		panic("index out of range [7] with length 3")
	})

	status, body := doGet(t, srv, "/boom")
	if status != http.StatusInternalServerError {
		t.Fatalf("panicking handler must answer 500, got %d", status)
	}
	if strings.Contains(body, "index out of range") {
		t.Fatalf("panic value leaked to the client: %q", body)
	}

	status, body = doGet(t, srv, "/healthz")
	if status != http.StatusOK || body != "ok" {
		t.Fatalf("server unusable after a recovered panic: status=%d body=%q", status, body)
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	status, body := doGet(t, newServer(stable.NewHolder()), "/healthz")
	if status != http.StatusOK || body != "ok" {
		t.Fatalf("unexpected healthz answer: status=%d body=%q", status, body)
	}
}

func TestServerReturnsNoContentForFavicon(t *testing.T) {
	t.Parallel()

	status, body := doGet(t, newServer(stable.NewHolder()), "/favicon.ico")
	if status != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", status)
	}
	if body != "" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestStableNotReady(t *testing.T) {
	t.Parallel()

	srv := newServer(stable.NewHolder())
	req := httptest.NewRequest(http.MethodGet, "/stable.txt", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Fatalf("expected Retry-After 30, got %q", ra)
	}
	if !strings.Contains(string(body), "stable list not ready") {
		t.Fatalf("unexpected body: %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("unexpected content type: %q", ct)
	}
}

func TestStableServesPayload(t *testing.T) {
	t.Parallel()

	stableHolder := stable.NewHolder()
	updated := time.Date(2026, 7, 7, 3, 4, 5, 0, time.UTC)
	stableHolder.Store(&stable.Snapshot{
		Payload:   []byte("vless://x#a-001\n"),
		UpdatedAt: updated,
		Stats:     stable.Stats{SourcesOK: 1, SourcesTotal: 2, Merged: 3, Tested: 2, Kept: 1},
	})
	srv := newServer(stableHolder)

	req := httptest.NewRequest(http.MethodGet, "/stable.txt", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if string(body) != "vless://x#a-001\n" {
		t.Fatalf("unexpected body: %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("unexpected content type: %q", ct)
	}
	wantStats := "updated=" + updated.Format(time.RFC3339) + " sources=1/2 merged=3 tested=2 kept=1"
	if got := resp.Header.Get("X-Stable-Stats"); got != wantStats {
		t.Fatalf("stats header:\ngot  %q\nwant %q", got, wantStats)
	}
}

func TestStableRejectsPost(t *testing.T) {
	t.Parallel()

	srv := newServer(stable.NewHolder())
	req := httptest.NewRequest(http.MethodPost, "/stable.txt", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
}

func TestStableStatsHeaderTracksSnapshot(t *testing.T) {
	t.Parallel()

	stableHolder := stable.NewHolder()
	stableHolder.Store(&stable.Snapshot{
		Payload:   []byte("vless://x#a-001\n"),
		UpdatedAt: time.Date(2026, 7, 7, 3, 4, 5, 0, time.UTC),
		Stats:     stable.Stats{SourcesOK: 1, SourcesTotal: 2, Merged: 3, Tested: 2, Kept: 1},
	})
	srv := newServer(stableHolder)

	fetch := func() (hdr, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/stable.txt", nil)
		resp, err := srv.TestApp().Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Header.Get("X-Stable-Stats"), string(b)
	}

	want1 := "updated=" + time.Date(2026, 7, 7, 3, 4, 5, 0, time.UTC).Format(time.RFC3339) + " sources=1/2 merged=3 tested=2 kept=1"
	if hdr, body := fetch(); body != "vless://x#a-001\n" || hdr != want1 {
		t.Fatalf("snapshot A:\nbody %q\nhdr  %q\nwant %q", body, hdr, want1)
	}
	// A second request against the same snapshot must render the identical
	// header (the memoized value, not a fresh format).
	if hdr, _ := fetch(); hdr != want1 {
		t.Fatalf("repeated request header changed: got %q want %q", hdr, want1)
	}

	// A Store of a NEW snapshot must be served with ITS header on the next
	// request: the memo is keyed on the snapshot pointer, never on the first
	// one seen.
	stableHolder.Store(&stable.Snapshot{
		Payload:   []byte("vless://x#b-002\n"),
		UpdatedAt: time.Date(2026, 8, 8, 4, 5, 6, 0, time.UTC),
		Stats:     stable.Stats{SourcesOK: 3, SourcesTotal: 4, Merged: 5, Tested: 4, Kept: 2},
	})
	want2 := "updated=" + time.Date(2026, 8, 8, 4, 5, 6, 0, time.UTC).Format(time.RFC3339) + " sources=3/4 merged=5 tested=4 kept=2"
	if hdr, body := fetch(); body != "vless://x#b-002\n" || hdr != want2 {
		t.Fatalf("snapshot B:\nbody %q\nhdr  %q\nwant %q", body, hdr, want2)
	}
}

// stablePairingOK fails t when one /stable.txt response pairs snapshot A's or
// B's payload with the other snapshot's X-Stable-Stats header.
func stablePairingOK(t *testing.T, srv *server.Server, snapA, snapB *stable.Snapshot) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stable.txt", nil)
	resp, err := srv.TestApp().Test(req)
	if err != nil {
		t.Error(err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Error(err)
		return
	}
	hdr := resp.Header.Get("X-Stable-Stats")
	switch string(body) {
	case "A\n":
		if want := "updated=" + snapA.UpdatedAt.Format(time.RFC3339) + " "; !strings.HasPrefix(hdr, want) {
			t.Errorf("header %q paired with payload %q", hdr, body)
		}
	case "B\n":
		if want := "updated=" + snapB.UpdatedAt.Format(time.RFC3339) + " "; !strings.HasPrefix(hdr, want) {
			t.Errorf("header %q paired with payload %q", hdr, body)
		}
	default:
		t.Errorf("unexpected payload %q", body)
	}
}

func TestStableStatsHeaderNeverMixesSnapshots(t *testing.T) {
	stableHolder := stable.NewHolder()
	srv := newServer(stableHolder)

	snapA := &stable.Snapshot{Payload: []byte("A\n"), UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Stats: stable.Stats{SourcesOK: 1, SourcesTotal: 1, Merged: 1, Tested: 1, Kept: 1}}
	snapB := &stable.Snapshot{Payload: []byte("B\n"), UpdatedAt: time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC), Stats: stable.Stats{SourcesOK: 1, SourcesTotal: 1, Merged: 1, Tested: 1, Kept: 1}}
	stableHolder.Store(snapA)

	// The memo is read and written on every request while the worker swaps
	// snapshots; no response may mix one snapshot's payload with the other's
	// header, and the shared memo state must stay race-free.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					stablePairingOK(t, srv, snapA, snapB)
				}
			}
		})
	}
	for range 20 {
		stableHolder.Store(snapA)
		stableHolder.Store(snapB)
	}
	close(stop)
	wg.Wait()
}
