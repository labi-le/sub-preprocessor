package preprocess

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/subscription"
)

func TestShouldReloadGeofeedLocked(t *testing.T) {
	t.Parallel()

	svc := &Processor{refreshInterval: time.Hour, loadedAt: time.Now().Add(-2 * time.Hour)}
	if !svc.shouldReloadGeofeedLocked(time.Now()) {
		t.Fatal("expected geofeed reload")
	}

	svc = &Processor{refreshInterval: time.Hour, loadedAt: time.Now().Add(-30 * time.Minute)}
	if svc.shouldReloadGeofeedLocked(time.Now()) {
		t.Fatal("did not expect geofeed reload")
	}

	svc = &Processor{refreshInterval: 0, loadedAt: time.Now().Add(-24 * time.Hour)}
	if svc.shouldReloadGeofeedLocked(time.Now()) {
		t.Fatal("did not expect geofeed reload when refresh interval disabled")
	}
}

// recordingCompactFilter mimics ASNFilter: it records the IPs it was handed
// and compacts them in place, dropping the first IP.
type recordingCompactFilter struct {
	seen [][]netip.Addr
}

func (f *recordingCompactFilter) Process(_ context.Context, ips []netip.Addr, _ *PipelineContext) []netip.Addr {
	f.seen = append(f.seen, append([]netip.Addr(nil), ips...))
	n := 0
	for _, ip := range ips[1:] {
		ips[n] = ip
		n++
	}
	return ips[:n]
}

func TestProcessNodeKeepsCachedResolvedSlicePristine(t *testing.T) {
	t.Parallel()

	ipA := netip.MustParseAddr("192.0.2.1")
	ipB := netip.MustParseAddr("192.0.2.2")
	full := []netip.Addr{ipA, ipB}

	f := &recordingCompactFilter{}
	p := &Processor{filters: []Filter{f}}

	pctx := &PipelineContext{
		Buffer:      &bytes.Buffer{},
		Resolved:    map[string][]netip.Addr{"example.com": {ipA, ipB}},
		Stats:       &Stats{},
		IsFirstNode: true,
	}
	node := subscription.Node{Raw: "vless://u@example.com:443#X", Server: "example.com", Port: "443"}

	p.processNode(context.Background(), node, pctx)
	p.processNode(context.Background(), node, pctx)

	if len(f.seen) != 2 {
		t.Fatalf("expected filter to run twice, ran %d times", len(f.seen))
	}
	if !slices.Equal(f.seen[0], full) {
		t.Fatalf("first node saw %v, want %v", f.seen[0], full)
	}
	if !slices.Equal(f.seen[1], full) {
		t.Fatalf("second node saw dirty cached slice %v, want %v", f.seen[1], full)
	}
	if !slices.Equal(pctx.Resolved["example.com"], full) {
		t.Fatalf("cached resolved slice mutated to %v, want %v", pctx.Resolved["example.com"], full)
	}
}

func TestProcessBodyCancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := &Processor{}
	pctx := &PipelineContext{
		Buffer:      &bytes.Buffer{},
		Resolved:    map[string][]netip.Addr{},
		Stats:       &Stats{},
		IsFirstNode: true,
	}
	body := []byte("vless://u@example.com:443#A\nvless://u@example.org:443#B")

	err := p.processBody(ctx, body, pctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request must not succeed with a truncated list, got err=%v", err)
	}
	if pctx.Stats.Kept != 0 {
		t.Fatalf("expected no nodes kept after pre-cancelled ctx, got %d", pctx.Stats.Kept)
	}
}
