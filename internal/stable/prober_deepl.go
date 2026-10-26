package stable

import (
	"context"
	"net/http"
	"strings"
	"sync"

	h3http "github.com/metacubex/http"
	mihomo "github.com/metacubex/mihomo/constant"
	quic "github.com/metacubex/quic-go"
	"github.com/metacubex/quic-go/http3"
	h3tls "github.com/metacubex/tls"

	"domains.lst/sub-preprocessor/internal/log"
)

func (m *MihomoProber) deeplURL() string {
	return strings.TrimRight(m.geo.Deepl.Endpoint, "/")
}

// DeeplCheck requires both transports on one adapter: TCP and UDP can have different egresses.
func (m *MihomoProber) DeeplCheck(ctx context.Context, proxies []mihomo.Proxy) map[string]APIOutcome {
	c := m.geo.Deepl
	target := m.deeplURL()
	opLog := log.Op(m.logger, "stable.DeeplCheck")
	prog := newProgress(opLog, "deepl check progress", len(proxies))
	out := make(map[string]APIOutcome, len(proxies))
	sem := fanoutSem(c.Concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, px := range proxies {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return out
		}
		wg.Go(func() {
			defer func() { <-sem }()
			tctx, cancel := context.WithTimeout(ctx, c.Timeout)
			defer cancel()
			tcpReachable, tcpStatus, _, _ := apiProbeOne(tctx, px, target, nil, c.Timeout, false)
			tcpVerdict := deeplVerdict(tcpStatus)
			var udpReachable bool
			var udpStatus int
			if tcpVerdict != apiBlocked {
				udpReachable, udpStatus = deeplHTTP3Probe(tctx, px, target)
			}
			udpVerdict := deeplVerdict(udpStatus)
			o := APIOutcome{Server: proxyHost(px), Reachable: tcpReachable || udpReachable}
			o.Blocked = tcpVerdict == apiBlocked || udpVerdict == apiBlocked
			o.Verified = o.Blocked || (tcpVerdict == apiAccepted && udpVerdict == apiAccepted)
			n := prog.step()
			opLog.Debug().Str("node", px.Name()).Str("server", o.Server).
				Int("tcp_status", tcpStatus).Int("http3_status", udpStatus).
				Bool("reachable", o.Reachable).Bool("blocked", o.Blocked).Bool("verified", o.Verified).
				Int64("n", n).Int64("of", prog.total).Msg("deepl check")
			mu.Lock()
			label := entryLabel(px)
			if prev, ok := out[label]; !ok || betterAPIOutcome(o, prev) {
				out[label] = o
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

func deeplVerdict(status int) apiVerdict {
	if status == http.StatusUnavailableForLegalReasons {
		return apiBlocked
	}
	if status >= 200 && status < 300 {
		return apiAccepted
	}
	return apiUnverified
}

func deeplHTTP3Probe(ctx context.Context, px mihomo.Proxy, target string) (bool, int) {
	req, err := h3http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil || req.URL.Scheme != "https" {
		return false, 0
	}
	addr, ok := hostPort(target)
	if !ok {
		return false, 0
	}
	meta := mihomo.Metadata{NetWork: mihomo.UDP}
	if err = meta.SetRemoteAddress(addr); err != nil {
		return false, 0
	}
	pc, err := px.ListenPacketContext(ctx, &meta)
	if err != nil {
		return false, 0
	}
	var closeOnce sync.Once
	closePacket := func() { closeOnce.Do(func() { _ = pc.Close() }) }
	stopClose := context.AfterFunc(ctx, closePacket)
	defer func() {
		stopClose()
		closePacket()
	}()
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		if err = pc.SetReadDeadline(deadline); err != nil {
			return false, 0
		}
	}
	if err = pc.ResolveUDP(ctx, &meta); err != nil {
		return false, 0
	}
	remote := meta.UDPAddr()
	if remote == nil {
		return false, 0
	}
	qt := &quic.Transport{Conn: pc}
	defer func() { _ = qt.Close() }()
	transport := &http3.Transport{
		Dial: func(_ context.Context, _ string, tlsConfig *h3tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			// HTTP/3's internal dial context can outlive the request; bind the handshake to our budget.
			return qt.Dial(ctx, remote, tlsConfig, cfg)
		},
	}
	defer func() { _ = transport.Close() }()
	req.Header.Set("User-Agent", browserUserAgent)
	// RoundTrip does not follow redirects or fall back to a direct UDP socket.
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return false, 0
	}
	_ = resp.Body.Close()
	return true, resp.StatusCode
}
