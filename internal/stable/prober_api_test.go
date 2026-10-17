package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// dialRecorder is a mihomo.Proxy that captures the address the probe asked it
// to dial and then refuses. Only DialContext is reachable from apiProbeOne, so
// the embedded nil interface is deliberate: any other call would panic and
// expose a widened dependency surface.
type dialRecorder struct {
	mihomo.Proxy
	addr   string
	dialed bool
}

func (d *dialRecorder) DialContext(_ context.Context, meta *mihomo.Metadata) (mihomo.Conn, error) { //nolint:ireturn // implements mihomo.Proxy; the signature is not ours to choose
	d.addr = meta.RemoteAddress()
	d.dialed = true
	return nil, errors.New("dial refused by test")
}

// TestAPIProbeOneDialsSchemeDefaultPort pins the dial address to the scheme.
// The API path used to hardcode 443, so an http:// endpoint override was
// dialled on 443 while the client spoke cleartext on that pinned conn: every
// node timed out and the filter emptied the survivor set with reason
// "unreachable".
func TestAPIProbeOneDialsSchemeDefaultPort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{"http defaults to 80", "http://mirror.internal/v1/models", "mirror.internal:80"},
		{"https defaults to 443", "https://api.anthropic.com/v1/models", "api.anthropic.com:443"},
		{"explicit port wins", "https://api.anthropic.com:8443/v1/models", "api.anthropic.com:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			px := &dialRecorder{}
			reachable, status, body, complete := apiProbeOne(context.Background(), px, tc.target, nil, time.Second, true)
			if reachable || status != 0 || body != "" || complete {
				t.Fatalf("a refused dial must yield no outcome, got (%v, %d, %q, %v)", reachable, status, body, complete)
			}
			if px.addr != tc.want {
				t.Fatalf("dialled %q, want %q", px.addr, tc.want)
			}
		})
	}
}

// oneShotHTTP starts a TCP server that reads one HTTP request and hands the
// connection to answer, which owns what the client sees on the wire. That is
// how these tests can produce responses net/http's server will not: a body cut
// short of its declared Content-Length, or headers followed by a stall.
func oneShotHTTP(t *testing.T, answer func(net.Conn)) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		br := bufio.NewReader(conn)
		for {
			line, readErr := br.ReadString('\n')
			if readErr != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		answer(conn)
	}()

	return "http://" + ln.Addr().String() + "/v1/models"
}

// TestAPIProbeOneMarksShortBodyIncomplete covers the two ways a body stops
// arriving after the headers did. The old code discarded io.ReadAll's error and
// reported an intact response, which let a marker check read the missing
// remainder as "not blocked".
func TestAPIProbeOneMarksShortBodyIncomplete(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		answer func(net.Conn)
	}{
		{"unexpected EOF", func(conn net.Conn) {
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n{\"error\":{")
		}},
		{"timeout after headers", func(conn net.Conn) {
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 1000\r\n\r\n{\"error\":{")
			// Hold the connection open until the client's own deadline closes
			// it; the headers and the partial body have already been written.
			_, _ = io.Copy(io.Discard, conn)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			px := adapter.NewProxy(outbound.NewDirect())
			target := oneShotHTTP(t, tc.answer)
			reachable, status, body, complete := apiProbeOne(context.Background(), px, target, nil, 500*time.Millisecond, true)
			if !reachable || status != http.StatusBadRequest {
				t.Fatalf("the headers arrived, so the node answered: (reachable=%v, status=%d)", reachable, status)
			}
			if complete {
				t.Fatalf("a body stopped before its Content-Length must not read as complete (body=%q)", body)
			}
			if !strings.HasPrefix(body, `{"error":{`) {
				t.Fatalf("the bytes that did arrive must be preserved for the marker scan, got %q", body)
			}
		})
	}
}

// TestAPICheckMarksMarkerEvidenceByCompleteness drives the classifier contract
// through apiCheck: an incomplete body without the marker is unverified (kept
// by the filter, never certified), while a complete refusal stays blocked, a
// complete clean response stays verified, and a body that ran past maxAPIBody
// is unverified unless the marker was already in the bytes read.
func TestAPICheckMarksMarkerEvidenceByCompleteness(t *testing.T) {
	t.Parallel()

	const marker = "Request not allowed"
	refusalBody := `{"error":"Request not allowed here"}`
	cleanBody := `{"ok":true}`
	oversizedMarkerPrefix := marker + strings.Repeat("x", maxAPIBody)

	for _, tc := range []struct {
		name         string
		answer       func(net.Conn)
		timeout      time.Duration
		wantBlocked  bool
		wantVerified bool
	}{
		{
			name: "truncated without marker",
			answer: func(conn net.Conn) {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n{\"error\":{")
			},
			timeout: 2 * time.Second,
		},
		{
			name: "timeout without marker",
			answer: func(conn net.Conn) {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 1000\r\n\r\n{\"error\":{")
				_, _ = io.Copy(io.Discard, conn)
			},
			timeout: 500 * time.Millisecond,
		},
		{
			name: "complete refusal",
			answer: func(conn net.Conn) {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(refusalBody), refusalBody)
			},
			timeout:      2 * time.Second,
			wantBlocked:  true,
			wantVerified: true,
		},
		{
			name: "complete clean response",
			answer: func(conn net.Conn) {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(cleanBody), cleanBody)
			},
			timeout:      2 * time.Second,
			wantVerified: true,
		},
		{
			name: "body past the cap without marker",
			answer: func(conn net.Conn) {
				body := strings.Repeat("x", maxAPIBody+1)
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			},
			timeout: 2 * time.Second,
		},
		{
			name: "marker before the cap",
			answer: func(conn net.Conn) {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(oversizedMarkerPrefix), oversizedMarkerPrefix)
			},
			timeout:      2 * time.Second,
			wantBlocked:  true,
			wantVerified: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			px := adapter.NewProxy(outbound.NewDirect())
			target := oneShotHTTP(t, tc.answer)
			m := &MihomoProber{logger: zerolog.Nop()}
			o, ok := m.apiCheck(context.Background(), "test.api", "api", []mihomo.Proxy{px},
				target, nil, tc.timeout, 1, 0, true, markerVerdictFor(marker))[px.Name()]
			if !ok {
				t.Fatalf("no outcome for %q", px.Name())
			}
			if !o.Reachable {
				t.Fatalf("the node answered: %+v", o)
			}
			if o.Blocked != tc.wantBlocked || o.Verified != tc.wantVerified {
				t.Fatalf("outcome = %+v, want blocked=%v verified=%v", o, tc.wantBlocked, tc.wantVerified)
			}
		})
	}
}

func markerVerdictFor(marker string) func(int, string, bool) apiVerdict {
	return func(status int, body string, complete bool) apiVerdict {
		return markerVerdict(status, body, marker, complete)
	}
}

// TestTidalCheckKeepsStatusVerdictOnIncompleteBody pins the deliberate
// exception: tidal reads the status and nothing else, so a body that stops
// arriving cannot turn its verdict unverified.
func TestTidalCheckKeepsStatusVerdictOnIncompleteBody(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		status      int
		wantBlocked bool
	}{
		{"truncated 2xx still passes", http.StatusOK, false},
		{"truncated 403 still blocks", http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := oneShotHTTP(t, func(conn net.Conn) {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n{\"countryCode\":",
					tc.status, http.StatusText(tc.status))
			})
			px := adapter.NewProxy(outbound.NewDirect())
			m := &MihomoProber{logger: zerolog.Nop(), geo: config.GeoBlockConfig{
				Tidal: config.TidalConfig{Endpoint: target, Timeout: 2 * time.Second, Concurrency: 1},
			}}
			o, ok := m.TidalCheck(context.Background(), []mihomo.Proxy{px})[px.Name()]
			if !ok {
				t.Fatalf("no outcome for %q", px.Name())
			}
			if !o.Reachable || o.Blocked != tc.wantBlocked {
				t.Fatalf("outcome = %+v, want blocked=%v", o, tc.wantBlocked)
			}
			if !o.Verified {
				t.Fatalf("a status-only check is verified by the status alone: %+v", o)
			}
		})
	}
}

// TestAPIProbeOneRejectsUnparsableTarget: hostPort returns an unparsable target
// verbatim rather than erroring, so the early return now depends on
// SetRemoteAddress rejecting it. Nothing must reach the node.
func TestAPIProbeOneRejectsUnparsableTarget(t *testing.T) {
	t.Parallel()

	px := &dialRecorder{}
	if reachable, _, _, _ := apiProbeOne(context.Background(), px, "://nonsense", nil, time.Second, true); reachable {
		t.Fatal("an unparsable endpoint must not be reported reachable")
	}
	if px.dialed {
		t.Fatal("an unparsable endpoint must not be dialled at all")
	}
}
