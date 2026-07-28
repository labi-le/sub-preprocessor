package app

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/metrics"
)

const metricsProbeTimeout = 2 * time.Second

// TestStartMetricsReportsBindFailure pins the contract that a metrics bind
// failure reaches the caller. It used to be swallowed by a detached goroutine
// while startup logged "metrics listener started" anyway, leaving the service
// running with no observability.
func TestStartMetricsReportsBindFailure(t *testing.T) {
	t.Parallel()

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer func() { _ = held.Close() }()

	srv, err := startMetrics(t.Context(), held.Addr().String(), metrics.New(), zerolog.Nop())
	if err == nil {
		_ = srv.Close()
		t.Fatal("binding an occupied port must be reported as an error")
	}
	if srv != nil {
		t.Fatal("no server may be handed back when the bind failed")
	}
}

// TestStartMetricsServesAfterBind covers the happy path end to end: the
// listener is bound before the function returns, so the resolved address is
// scrapeable immediately.
func TestStartMetricsServesAfterBind(t *testing.T) {
	t.Parallel()

	srv, err := startMetrics(t.Context(), "127.0.0.1:0", metrics.New(), zerolog.Nop())
	if err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	defer func() { _ = srv.Close() }()

	client := &http.Client{Timeout: metricsProbeTimeout}
	resp, err := client.Get("http://" + srv.Addr + "/metrics")
	if err != nil {
		t.Fatalf("scrape %s: %v", srv.Addr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read scrape: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "stable_cycles_total") {
		t.Fatalf("scrape did not render the metric set: %q", body)
	}
}
