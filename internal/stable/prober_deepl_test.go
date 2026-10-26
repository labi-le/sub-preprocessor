package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	h3http "github.com/metacubex/http"
	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/quic-go/http3"
	h3tls "github.com/metacubex/tls"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

func TestDeeplVerdict(t *testing.T) {
	t.Parallel()
	for _, status := range []int{0, 200, 204, 206, 299, 300, 302, 403, 429, 451, 500, 502} {
		want := apiUnverified
		if status == 451 {
			want = apiBlocked
		} else if status >= 200 && status < 300 {
			want = apiAccepted
		}
		if got := deeplVerdict(status); got != want {
			t.Fatalf("status %d: got %v, want %v", status, got, want)
		}
	}
}

// Isolate the root pool: changing process-wide TLS trust would race other tests.
func TestDeeplTransports(t *testing.T) {
	if os.Getenv("DEEPL_TEST_CERT") == "" {
		runDeeplTransportProcess(t)
		return
	}
	cert, err := tls.LoadX509KeyPair(os.Getenv("DEEPL_TEST_CERT"), os.Getenv("DEEPL_TEST_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("statuses", func(t *testing.T) { testDeeplStatuses(t, cert) })
	t.Run("multiport", func(t *testing.T) { testDeeplMultiport(t, cert) })
	t.Run("cancelled", func(t *testing.T) { testDeeplCancelled(t, cert) })
	t.Run("persistence", func(t *testing.T) { testDeeplPersistence(t, cert) })
}

type deeplStatusCase struct {
	name              string
	tcp, udp          int
	noUDP, stall      bool
	blocked, verified bool
}

func testDeeplStatuses(t *testing.T, cert tls.Certificate) {
	t.Helper()
	for _, tc := range []deeplStatusCase{
		{name: "both_pass", tcp: 200, udp: 200, verified: true},
		{name: "split_egress", tcp: 200, udp: 451, blocked: true, verified: true},
		{name: "tcp_block", tcp: 451, udp: 200, blocked: true, verified: true},
		{name: "tcp_rate_limit", tcp: 429, udp: 200},
		{name: "udp_rate_limit", tcp: 200, udp: 429},
		{name: "redirect", tcp: 200, udp: 302},
		{name: "cdn_failure", tcp: 503, udp: 200},
		{name: "no_udp", tcp: 200, udp: 200, noUDP: true},
		{name: "udp_timeout", tcp: 200, udp: 200, stall: true},
	} {
		t.Run(tc.name, func(t *testing.T) { checkDeeplStatus(t, cert, tc) })
	}
}

func checkDeeplStatus(t *testing.T, cert tls.Certificate, tc deeplStatusCase) {
	t.Helper()
	px, tcpHits, udpHits := deeplTestProxy(t, cert, "node", tc.tcp, tc.udp, tc.stall)
	px.noUDP = tc.noUDP
	m := deeplTestProber(500 * time.Millisecond)
	start := time.Now()
	o := m.DeeplCheck(context.Background(), []mihomo.Proxy{px})["node"]
	if !o.Reachable || o.Blocked != tc.blocked || o.Verified != tc.verified {
		t.Fatalf("outcome %+v; want blocked=%v verified=%v", o, tc.blocked, tc.verified)
	}
	wantUDPHits := int32(1)
	if tc.noUDP || tc.tcp == http.StatusUnavailableForLegalReasons {
		wantUDPHits = 0
	}
	if tcpHits.Load() != 1 || udpHits.Load() != wantUDPHits {
		t.Fatalf("translator hits tcp=%d udp=%d, want UDP=%d", tcpHits.Load(), udpHits.Load(), wantUDPHits)
	}
	if tc.tcp == http.StatusUnavailableForLegalReasons && px.opened.Load() != 0 {
		t.Fatal("TCP refusal must not open a packet socket")
	}
	if px.opened.Load() != px.closed.Load() {
		t.Fatalf("packet sockets opened=%d closed=%d", px.opened.Load(), px.closed.Load())
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("request deadline not respected")
	}
	store := &recordingBlocklist{}
	filters := buildNodeFilters([]string{"deepl"}, m, store, zerolog.Nop())
	kept, rep := filters[0].apply(context.Background(), []Survivor{{Entry: Entry{Label: "node"}}}, map[string][]mihomo.Proxy{"node": {px}})
	wantKept := 0
	if tc.verified && !tc.blocked {
		wantKept = 1
	}
	if len(kept) != wantKept || rep.Kept != wantKept {
		t.Fatalf("strict gate kept=%d report=%+v", len(kept), rep)
	}
	if len(store.blocked) != 0 {
		t.Fatalf("whole-batch refusal must not persist: %v", store.blocked)
	}
}

func testDeeplMultiport(t *testing.T, cert tls.Certificate) {
	t.Helper()
	t.Run("multiport_cannot_mix_transports", func(t *testing.T) {
		a, _, _ := deeplTestProxy(t, cert, "node:1001/tcp", 200, 429, false)
		b, _, _ := deeplTestProxy(t, cert, "node:1002/tcp", 429, 200, false)
		a.mieru, b.mieru = true, true
		m := deeplTestProber(time.Second)
		o := m.DeeplCheck(context.Background(), []mihomo.Proxy{a, b})["node"]
		if !o.Reachable || o.Verified || o.Blocked {
			t.Fatalf("mixed-port pass: %+v", o)
		}
	})
	t.Run("multiport_one_complete_pass", func(t *testing.T) {
		a, _, _ := deeplTestProxy(t, cert, "node:1001/tcp", 200, 451, false)
		b, _, _ := deeplTestProxy(t, cert, "node:1002/tcp", 200, 200, false)
		a.mieru, b.mieru = true, true
		o := deeplTestProber(time.Second).DeeplCheck(context.Background(), []mihomo.Proxy{a, b})["node"]
		if !o.Reachable || !o.Verified || o.Blocked {
			t.Fatalf("complete sibling port must pass: %+v", o)
		}
	})
}

func testDeeplCancelled(t *testing.T, cert tls.Certificate) {
	t.Helper()
	t.Run("cancelled_check_does_not_pass", func(t *testing.T) {
		px, _, _ := deeplTestProxy(t, cert, "node", 200, 200, true)
		m := deeplTestProber(10 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		store := &recordingBlocklist{}
		f := buildNodeFilters([]string{"deepl"}, m, store, zerolog.Nop())[0]
		kept, _ := f.apply(ctx, []Survivor{{Entry: Entry{Label: "node"}}}, map[string][]mihomo.Proxy{"node": {px}})
		if len(kept) != 0 || len(store.blocked) != 0 {
			t.Fatalf("cancelled check kept=%v persisted=%v", kept, store.blocked)
		}
		if px.opened.Load() != px.closed.Load() {
			t.Fatalf("cancelled packet sockets opened=%d closed=%d", px.opened.Load(), px.closed.Load())
		}
	})
	t.Run("no_transport_never_passes_batch_breaker", func(t *testing.T) {
		px, _, _ := deeplTestProxy(t, cert, "node", 200, 200, false)
		px.tcpAddress, px.noUDP = "127.0.0.1:0", true
		m := deeplTestProber(time.Second)
		f := buildNodeFilters([]string{"deepl"}, m, nil, zerolog.Nop())[0]
		kept, rep := f.apply(context.Background(), []Survivor{{Entry: Entry{Label: "node"}}}, map[string][]mihomo.Proxy{"node": {px}})
		if len(kept) != 0 || rep.Dropped["unreachable"] != 1 || rep.State != FilterTripped {
			t.Fatalf("total failure kept=%v report=%+v", kept, rep)
		}
	})
}

func testDeeplPersistence(t *testing.T, cert tls.Certificate) {
	t.Helper()
	t.Run("mixed_batch_persists_only_explicit_block", func(t *testing.T) {
		a, _, _ := deeplTestProxy(t, cert, "pass", 200, 200, false)
		b, _, _ := deeplTestProxy(t, cert, "block", 200, 451, false)
		c, _, _ := deeplTestProxy(t, cert, "fault", 200, 503, false)
		m := deeplTestProber(time.Second)
		store := &recordingBlocklist{}
		f := buildNodeFilters([]string{"deepl"}, m, store, zerolog.Nop())[0]
		kept, rep := f.apply(context.Background(), []Survivor{{Entry: Entry{Label: "pass"}}, {Entry: Entry{Label: "block"}}, {Entry: Entry{Label: "fault"}}}, map[string][]mihomo.Proxy{"pass": {a}, "block": {b}, "fault": {c}})
		if len(kept) != 1 || kept[0].Label != "pass" || rep.Dropped["blocked"] != 1 || rep.Dropped["unverified"] != 1 {
			t.Fatalf("kept=%v report=%+v", kept, rep)
		}
		if len(store.blocked) != 1 || store.blocked[0] != "block.example.com" {
			t.Fatalf("persisted %v", store.blocked)
		}
	})
}

func runDeeplTransportProcess(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"example.com"},
		IsCA:     true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeeplTransports$", "-test.v", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "DEEPL_TEST_CERT="+certPath, "DEEPL_TEST_KEY="+keyPath, "SSL_CERT_FILE="+certPath, "SSL_CERT_DIR="+dir)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("isolated transport tests: %v\n%s", runErr, out)
	}
}

func deeplTestProber(timeout time.Duration) *MihomoProber {
	return &MihomoProber{logger: zerolog.Nop(), geo: config.GeoBlockConfig{Deepl: config.DeeplConfig{
		Endpoint: "https://example.com/ru/translator", Timeout: timeout, Concurrency: 2,
	}}}
}

type deeplLocalProxy struct {
	mihomo.Proxy
	name, tcpAddress string
	udpAddress       netip.AddrPort
	noUDP, mieru     bool
	opened, closed   atomic.Int32
}

func (p *deeplLocalProxy) Name() string { return p.name }
func (p *deeplLocalProxy) Addr() string { return p.name + ".example.com:443" }
func (p *deeplLocalProxy) Type() mihomo.AdapterType {
	if p.mieru {
		return mihomo.Mieru
	}
	return mihomo.Direct
}

//nolint:ireturn // implements mihomo.ProxyAdapter
func (p *deeplLocalProxy) DialContext(ctx context.Context, meta *mihomo.Metadata) (mihomo.Conn, error) {
	if meta.Host != "example.com" || meta.DstPort != 443 {
		return nil, errors.New("wrong TCP destination")
	}
	var local mihomo.Metadata
	if err := local.SetRemoteAddress(p.tcpAddress); err != nil {
		return nil, err
	}
	return p.Proxy.DialContext(ctx, &local)
}

//nolint:ireturn // implements mihomo.ProxyAdapter
func (p *deeplLocalProxy) ListenPacketContext(ctx context.Context, meta *mihomo.Metadata) (mihomo.PacketConn, error) {
	if p.noUDP {
		return nil, mihomo.ErrNotSupport
	}
	if meta.Host != "example.com" || meta.DstPort != 443 || meta.NetWork != mihomo.UDP {
		return nil, errors.New("wrong UDP destination")
	}
	meta.DstIP, meta.DstPort = p.udpAddress.Addr(), p.udpAddress.Port()
	pc, err := p.Proxy.ListenPacketContext(ctx, meta)
	if err != nil {
		return nil, err
	}
	p.opened.Add(1)
	return &deeplCountPacket{PacketConn: pc, closed: &p.closed}, nil
}

type deeplCountPacket struct {
	mihomo.PacketConn
	closed *atomic.Int32
}

func (p *deeplCountPacket) Close() error { p.closed.Add(1); return p.PacketConn.Close() }

// Some UDP-native adapters support read deadlines but reject combined deadlines.
func (p *deeplCountPacket) SetDeadline(time.Time) error { return os.ErrInvalid }

func (p *deeplCountPacket) SetWriteDeadline(time.Time) error { return os.ErrInvalid }

func deeplTestProxy(t *testing.T, cert tls.Certificate, name string, tcpStatus, udpStatus int, stall bool) (*deeplLocalProxy, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	tcpHits, udpHits := new(atomic.Int32), new(atomic.Int32)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ru/translator" {
			t.Errorf("TCP path %q", r.URL.Path)
		}
		tcpHits.Add(1)
		w.WriteHeader(tcpStatus)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h3 := &http3.Server{
		TLSConfig: &h3tls.Config{Certificates: []h3tls.Certificate{{Certificate: cert.Certificate, PrivateKey: cert.PrivateKey}}, MinVersion: h3tls.VersionTLS13},
		Handler: h3http.HandlerFunc(func(w h3http.ResponseWriter, r *h3http.Request) {
			if r.URL.Path != "/ru/translator" || r.ProtoMajor != 3 {
				t.Errorf("UDP request %s %s", r.Proto, r.URL.Path)
			}
			udpHits.Add(1)
			if !stall {
				w.WriteHeader(udpStatus)
				w.(h3http.Flusher).Flush()
			}
			<-r.Context().Done()
		}),
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = h3.Serve(pc) }()
	t.Cleanup(func() { _ = h3.Close(); _ = pc.Close(); <-done })
	px := &deeplLocalProxy{Proxy: adapter.NewProxy(outbound.NewDirect()), name: name, tcpAddress: srv.Listener.Addr().String(), udpAddress: pc.LocalAddr().(*net.UDPAddr).AddrPort()}
	t.Cleanup(func() { _ = px.Proxy.Close() })
	return px, tcpHits, udpHits
}
