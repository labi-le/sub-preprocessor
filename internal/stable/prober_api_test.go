package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"errors"
	"testing"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"
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
			reachable, status, body := apiProbeOne(context.Background(), px, tc.target, nil, time.Second)
			if reachable || status != 0 || body != "" {
				t.Fatalf("a refused dial must yield no outcome, got (%v, %d, %q)", reachable, status, body)
			}
			if px.addr != tc.want {
				t.Fatalf("dialled %q, want %q", px.addr, tc.want)
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
	if reachable, _, _ := apiProbeOne(context.Background(), px, "://nonsense", nil, time.Second); reachable {
		t.Fatal("an unparsable endpoint must not be reported reachable")
	}
	if px.dialed {
		t.Fatal("an unparsable endpoint must not be dialled at all")
	}
}
