package stable

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/log"
)

// maxBandwidthBody caps the body read per node against a hostile/misconfigured
// endpoint that streams forever. Normal test payloads (a few MB) are far below.
const maxBandwidthBody = 256 << 20

// bitsPerMegabit converts a bits-per-second rate to megabits per second.
const bitsPerMegabit = 1e6

// BandwidthOutcome is the per-node result of a through-node download-speed test.
// Reachable is false when the dial/GET failed; a reachable-but-slow node has
// Reachable=true and a low Mbps.
type BandwidthOutcome struct {
	Server    string
	Reachable bool
	Mbps      int
}

// computeMbps converts a byte count and transfer duration to integer Mbps.
// Guards elapsed<=0 and bytesRead<=0 so a sub-second or empty transfer never
// divides by zero or yields NaN.
func computeMbps(bytesRead int64, elapsed time.Duration) int {
	if bytesRead <= 0 || elapsed <= 0 {
		return 0
	}
	return int(float64(bytesRead) * 8 / elapsed.Seconds() / bitsPerMegabit)
}

// measure issues a GET to target through the supplied client, forcing
// Accept-Encoding: identity so bytesRead equals wire bytes (Go otherwise adds
// gzip and transparently decompresses, inflating the rate). Timing starts after
// the response headers arrive (connect/TLS/TTFB excluded) and covers only the
// body transfer. A partial read at the deadline still returns its byte count.
func measure(ctx context.Context, client *http.Client, target string) (bool, int64, time.Duration) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, 0, 0
	}
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return false, 0, 0
	}
	defer func() { _ = resp.Body.Close() }()

	start := time.Now()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBandwidthBody))
	elapsed := time.Since(start)
	return true, n, elapsed
}

// bandwidthProbeOne dials target through px, downloads it over a fixed-conn
// transport (mirroring apiProbeOne), and returns the measured Mbps. Compression
// is disabled and redirects are not followed (the conn is pinned to one host).
func bandwidthProbeOne(ctx context.Context, px mihomo.Proxy, target string, timeout time.Duration) (bool, int) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var meta mihomo.Metadata
	if addrErr := meta.SetRemoteAddress(hostPort(target)); addrErr != nil {
		return false, 0
	}
	conn, err := px.DialContext(tctx, &meta)
	if err != nil {
		return false, 0
	}
	defer func() { _ = conn.Close() }()

	transport := &http.Transport{
		DialContext:         func(context.Context, string, string) (net.Conn, error) { return conn, nil },
		TLSHandshakeTimeout: apiTLSHandshakeTimeout,
		DisableCompression:  true,
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	reachable, n, elapsed := measure(tctx, client, target)
	if !reachable {
		return false, 0
	}
	return true, computeMbps(n, elapsed)
}

// BandwidthCheck downloads the configured test_url through every node in payload
// (bounded by check.bandwidth.concurrency) and returns each node's measured
// speed. Mirrors apiCheck's fan-out: one shared semaphore, per-node debug log,
// progress reporter.
func (m *MihomoProber) BandwidthCheck(ctx context.Context, payload []byte) map[string]BandwidthOutcome {
	proxies, err := m.parseProxies(payload)
	if err != nil {
		m.logger.Warn().Err(err).Msg("bandwidth check: no proxies")
		return nil
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	target := m.cfg.Bandwidth.TestURL
	timeout := m.cfg.Bandwidth.Timeout
	concurrency := m.cfg.Bandwidth.Concurrency

	opLog := log.Op(m.logger, "stable.BandwidthCheck")
	prog := newProgress(opLog, "bandwidth check progress", len(proxies))

	out := make(map[string]BandwidthOutcome, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reachable, mbps := bandwidthProbeOne(ctx, px, target, timeout)
			host, _, splitErr := net.SplitHostPort(px.Addr())
			if splitErr != nil {
				host = px.Addr()
			}
			o := BandwidthOutcome{Server: host, Reachable: reachable, Mbps: mbps}
			n := prog.step()
			opLog.Debug().Str("node", px.Name()).Str("server", host).
				Bool("reachable", o.Reachable).Int("mbps", o.Mbps).
				Int64("n", n).Int64("of", prog.total).Msg("bandwidth check")
			mu.Lock()
			out[px.Name()] = o
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// BandwidthMinMbps resolves the configured floor; nil (unset) means 0 = no floor.
func (m *MihomoProber) BandwidthMinMbps() int {
	if m.cfg.Bandwidth.MinMbps == nil {
		return 0
	}
	return *m.cfg.Bandwidth.MinMbps
}

// hostPort extracts host:port from a URL, defaulting the port by scheme.
func hostPort(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}
