package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// vmessPayload builds a one-node vmess subscription payload targeting
// localhost; nothing listens there, so the test never touches the network.
func vmessPayload(t *testing.T) []byte {
	t.Helper()
	const node = `{"v":"2","ps":"node","add":"127.0.0.1","port":"10086",` +
		`"id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"0","net":"tcp","type":"none","tls":"","scy":"auto"}`
	return []byte("vmess://" + base64.StdEncoding.EncodeToString([]byte(node)) + "\n")
}

func TestProbeCancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	p, err := NewMihomoProber(config.CheckConfig{
		Rounds:         2,
		Concurrency:    1,
		Timeout:        time.Second,
		TestURL:        "http://127.0.0.1:0/",
		ExpectedStatus: "204",
	}, config.BandwidthConfig{}, config.GeoBlockConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, probeErr := p.Probe(ctx, vmessPayload(t))
	if probeErr == nil {
		t.Fatal("a cancelled probe must be an error, not a truncated success")
	}
	if !errors.Is(probeErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", probeErr)
	}
	if res != nil {
		t.Fatalf("result map must be discarded on cancellation, got %v", res)
	}
}

func TestProbeZeroConcurrencyDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	// NewMihomoProber is exported and validates nothing but expected_status, so
	// a hand-built prober can carry Concurrency 0 — the residual case
	// fanoutSem's doc comment names. runRound acquires the semaphore on the
	// producer goroutine before any releasing worker exists, so an unbuffered
	// channel blocks forever. The failure mode is a HANG, not a wrong value,
	// hence the deadline + done channel: without them this wedges the suite.
	p, err := NewMihomoProber(config.CheckConfig{
		Rounds:         1,
		Concurrency:    0,
		Timeout:        50 * time.Millisecond,
		TestURL:        "http://127.0.0.1:0/",
		ExpectedStatus: "204",
	}, config.BandwidthConfig{}, config.GeoBlockConfig{}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	payload := vmessPayload(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Deliberately not the deadline context: the blocked send ignores
		// cancellation, so only a bounded semaphore can let this return.
		_, _ = p.Probe(context.Background(), payload)
	}()

	timeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-done:
	case <-timeout.Done():
		t.Fatal("Probe deadlocked with concurrency 0; the fan-out semaphore must be normalised to >= 1")
	}
}
