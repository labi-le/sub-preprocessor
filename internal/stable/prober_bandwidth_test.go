package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"syscall"
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
		{2_500_000, time.Second, 20}, // 2.5MB*8/1s = 20 Mbps
		{1_250_000, time.Second, 10}, // 10 Mbps
		{1_000_000, 100 * time.Millisecond, 80},
		{0, time.Second, 0}, // no bytes
		{2_000_000, 0, 0},   // zero elapsed guarded (no divide/panic)
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

func TestMeasureRedirectIsNotAMeasurement(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A bare Location + status (no body) models a real speed-test URL that
		// 302s to a CDN. The probe pins one conn and never follows redirects,
		// so there is no payload to time behind it.
		w.Header().Set("Location", "https://example.invalid/other")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	reachable, read, _ := measure(context.Background(), client, srv.URL)
	if reachable {
		t.Fatal("an unfollowed 3xx carries no payload; it must not be scored")
	}
	if read != 0 {
		t.Fatalf("rejected response must report no bytes, got %d", read)
	}
}

func TestMeasureRejectsNon2xx(t *testing.T) {
	t.Parallel()

	// A Cloudflare-style block page: a small body served fast. Timed as
	// payload it computes to tens of Mbps and sails past min_mbps.
	const blockPage = 10 << 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(make([]byte, blockPage))
	}))
	defer srv.Close()

	reachable, read, _ := measure(context.Background(), srv.Client(), srv.URL)
	if reachable {
		t.Fatal("a 403 error page must not be reported as a bandwidth sample")
	}
	if read != 0 {
		t.Fatalf("rejected response must report no bytes, got %d", read)
	}
}

func TestMeasureRejectsTruncatedBody(t *testing.T) {
	t.Parallel()

	// Hand-written response: declare a large Content-Length, deliver a
	// fraction of it, then drop the connection — the normal failure mode of
	// an oversubscribed exit. The client sees io.ErrUnexpectedEOF.
	const (
		declared = 2 << 20
		sent     = 8 << 10
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", declared)
		_, _ = conn.Write(make([]byte, sent))
	}))
	defer srv.Close()

	reachable, read, _ := measure(context.Background(), srv.Client(), srv.URL)
	if reachable {
		t.Fatal("a transfer the peer cut short must not be reported as a bandwidth sample")
	}
	if read != 0 {
		t.Fatalf("rejected response must report no bytes, got %d", read)
	}
}

func TestMeasureKeepsDeadlineTruncatedRead(t *testing.T) {
	t.Parallel()

	// The one truncation that IS a valid sample: our own deadline cut a
	// transfer that was still flowing. Those bytes arrived in the measured
	// window, so a slow node stays measurable instead of vanishing.
	const sent = 8 << 10
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 1048576\r\n\r\n")
		_, _ = conn.Write(make([]byte, sent))
		<-done // hold the transfer open until the probe's deadline fires
	}))
	defer srv.Close()
	defer close(done) // LIFO: releases the handler before srv.Close waits

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	reachable, read, _ := measure(ctx, srv.Client(), srv.URL)
	if !reachable {
		t.Fatal("a deadline-truncated transfer is still a sample of a slow node")
	}
	if read != sent {
		t.Fatalf("bytesRead = %d, want %d", read, sent)
	}
}

// expiredCtx is the context state a probe whose own deadline has fired is in:
// measure's tctx is a WithTimeout parent of the cycle, so once it expires any
// read error arrives alongside ctx.Err() != nil. deadlineTruncated must then
// still tell the peer's cut from our own.
func TestDeadlineTruncatedReadsTheErrorClass(t *testing.T) {
	t.Parallel()

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	timeoutErr := &url.Error{Op: "Get", URL: "http://example.com/", Err: context.DeadlineExceeded}
	var timeoutNet net.Error = &net.DNSError{Err: "lookup timed out", IsTimeout: true}
	rst := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	for _, c := range []struct {
		desc string
		ctx  context.Context
		err  error
		want bool
	}{
		// Our own deadline, HTTP/1.1: the transport cancels the conn and the
		// read surfaces as net.ErrClosed once the context is done.
		{"h1 self-cancel: ErrClosed with the context done", expired, net.ErrClosed, true},
		{"ErrClosed with a live context is not our deadline", context.Background(), net.ErrClosed, false},
		// HTTP/2 hands the context error back directly.
		{"h2 self-cancel: DeadlineExceeded", expired, context.DeadlineExceeded, true},
		{"wrapped DeadlineExceeded", expired, fmt.Errorf("read body: %w", context.DeadlineExceeded), true},
		{"os.ErrDeadlineExceeded", expired, os.ErrDeadlineExceeded, true},
		// A Client.Timeout-only wiring rewraps any read error once the client
		// deadline passed; the httpError it builds is a net.Error with
		// Timeout() true and Is(DeadlineExceeded) true (client.go).
		{"client-timeout url.Error wrapping DeadlineExceeded", context.Background(), timeoutErr, true},
		{"a timeout-shaped net.Error", context.Background(), timeoutNet, true},
		// The peer's cuts stay discarded even when our deadline lands in the
		// same instant: their byte count is not a rate.
		{"peer reset at the deadline", expired, rst, false},
		{"short body at the deadline", expired, io.ErrUnexpectedEOF, false},
		{"clean EOF at the deadline", expired, io.EOF, false},
	} {
		if got := deadlineTruncated(c.ctx, c.err); got != c.want {
			t.Errorf("%s: deadlineTruncated = %v, want %v", c.desc, got, c.want)
		}
	}
}

func TestMeasureSendsBrowserUserAgent(t *testing.T) {
	t.Parallel()

	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	measure(context.Background(), srv.Client(), srv.URL)
	if gotUA != browserUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUA, browserUserAgent)
	}
}
