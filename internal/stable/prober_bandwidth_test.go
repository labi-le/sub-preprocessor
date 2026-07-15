package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestComputeMbps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		bytes   int64
		elapsed time.Duration
		want    int
	}{
		{2_500_000, time.Second, 20},       // 2.5MB*8/1s = 20 Mbps
		{1_250_000, time.Second, 10},       // 10 Mbps
		{1_000_000, 100 * time.Millisecond, 80},
		{0, time.Second, 0},                // no bytes
		{2_000_000, 0, 0},                  // zero elapsed guarded (no divide/panic)
	}
	for _, c := range cases {
		if got := computeMbps(c.bytes, c.elapsed); got != c.want {
			t.Errorf("computeMbps(%d, %v) = %d, want %d", c.bytes, c.elapsed, got, c.want)
		}
	}
}

func TestMeasureSendsIdentityAndCountsBytes(t *testing.T) {
	t.Parallel()

	const n = 200_000
	var gotEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		_, _ = w.Write(make([]byte, n))
	}))
	defer srv.Close()

	reachable, read, elapsed := measure(context.Background(), srv.Client(), srv.URL)
	if !reachable {
		t.Fatal("expected reachable")
	}
	if read != n {
		t.Fatalf("bytesRead = %d, want %d", read, n)
	}
	if gotEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotEncoding)
	}
	if elapsed <= 0 {
		t.Fatalf("elapsed must be positive, got %v", elapsed)
	}
}

func TestMeasureRedirectYieldsNoBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A bare Location + status (no body) models a real speed-test URL that
		// 302s to a CDN; http.Redirect would inject a ~52-byte HTML anchor body
		// that measure would legitimately count, so set the redirect by hand.
		w.Header().Set("Location", "https://example.invalid/other")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	reachable, read, _ := measure(context.Background(), client, srv.URL)
	if !reachable {
		t.Fatal("a 3xx is still a response (reachable)")
	}
	if read != 0 {
		t.Fatalf("redirect body should be ~0, got %d", read)
	}
}
