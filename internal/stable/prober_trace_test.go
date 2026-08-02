package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	mihomo "github.com/metacubex/mihomo/constant"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// traceAnswer renders a /cdn-cgi/trace body around the two lines parseTrace
// reads, keeping the neighbours the live endpoint writes between them.
func traceAnswer(ip, loc string) string {
	return "fl=1169f9\nh=cloudflare.com\nip=" + ip + "\nts=1785677902.76\n" +
		"visit_scheme=https\ncolo=HEL\nhttp=http/2\nloc=" + loc + "\ntls=TLSv1.3\n"
}

// TestParseTraceRejectsWhatTheTagCannotCarry pins the validation the rest of
// the package is promised: Country is two UPPERCASE ASCII letters and never
// one of Cloudflare's reserved non-country codes, IP parses as an address.
//
// XX is the case this exists for. It is two bytes, so a length check passes
// it, and a node Cloudflare cannot place then overwrites a possibly-correct
// offline [GEO:DE] with a code that names no country. Every rejection has to
// read as NO answer — same as an unreachable node — so the caller keeps the
// offline tag instead of publishing a second spelling of countryUnknown.
//
// The lowercase cases are why the letters test is case-EXACT. A case-folding
// one lets xx slip past the != XX guard that exists to reject it, and lets any
// lowercase loc through, splitting stable_kept_country_nodes between two
// spellings of one country — the split both geo databases avoid by
// upper-folding, and that merge.go's tagCountry does not.
func TestParseTraceRejectsWhatTheTagCannotCarry(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"loc XX, Cloudflare's no-country-data code": "ip=1.2.3.4\nloc=XX\n",
		"loc xx, the same reserved code lowercased": "ip=1.2.3.4\nloc=xx\n",
		"loc T1, Cloudflare's Tor code":             "ip=1.2.3.4\nloc=T1\n",
		"loc digits":                                "ip=1.2.3.4\nloc=12\n",
		"loc is the annotator's unknown marker":     "ip=1.2.3.4\nloc=??\n",
		"loc lowercase country":                     "ip=1.2.3.4\nloc=de\n",
		"loc mixed case country":                    "ip=1.2.3.4\nloc=De\n",
		// Two runes, four bytes: the code is counted in bytes because that is
		// what an alpha-2 code is.
		"loc non-ascii":     "ip=1.2.3.4\nloc=ДЕ\n",
		"ip out of range":   "ip=1.2.3.400\nloc=DE\n",
		"ip carries a port": "ip=1.2.3.4:443\nloc=DE\n",
		"ip is a hostname":  "ip=example.com\nloc=DE\n",
	} {
		if res, ok := parseTrace(body); ok {
			t.Errorf("%s: must not parse, got %+v", name, res)
		}
	}
}

func TestParseTraceAcceptsARealAnswer(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ body, ip, country string }{
		"full answer": {traceAnswer("93.100.194.40", "RU"), "93.100.194.40", "RU"},
		"ipv6 egress": {traceAnswer("2a00:1450:4001:80f::200e", "DE"), "2a00:1450:4001:80f::200e", "DE"},
		// The endpoint terminates its last line, but nothing in a key=value
		// format promises that, and the scan must not lose an unterminated
		// tail.
		"no trailing newline": {"colo=HEL\nip=1.2.3.4\nloc=DE", "1.2.3.4", "DE"},
	} {
		res, ok := parseTrace(tc.body)
		if !ok {
			t.Errorf("%s: must parse", name)

			continue
		}
		if res.IP != tc.ip || res.Country != tc.country {
			t.Errorf("%s: got %+v, want ip %s loc %s", name, res, tc.ip, tc.country)
		}
	}
}

// TestBetterTraceOutcomePriority pins the order the ports of one mierus:// link
// are folded in. It compares every ordered pair, so no result can come from
// two cases happening to meet in a convenient order.
func TestBetterTraceOutcomePriority(t *testing.T) {
	t.Parallel()

	// Weakest first. Address decides where it can, but it is not a total order
	// over a label's proxies — mihomo builds a mieru address from server+port
	// alone while the name also carries the transport — so the name has to
	// finish the job.
	ranked := []struct {
		desc string
		o    traceOutcome
	}{
		{"higher address", traceOutcome{addr: "1.2.3.4:9998", name: "src-001:9998-9999/UDP"}},
		{"same address, higher name", traceOutcome{addr: "1.2.3.4:2999", name: "src-001:2999/UDP"}},
		{"same address, lower name", traceOutcome{addr: "1.2.3.4:2999", name: "src-001:2999/TCP"}},
		// The last two make the ADDRESS key load-bearing: their keys disagree,
		// so ranking by name alone would order this pair backwards. Every case
		// above sorts the same way under either key, which is why dropping the
		// address branch entirely leaves them all green.
		//
		// The shape is rare, not impossible, so the pair is worth pinning.
		// mihomo builds a mieru proxy's NAME from the raw port token but its
		// ADDRESS from the parsed int (converter.go "%s:%s/%s" vs NewMieru's
		// strconv.Itoa), so a leading zero desynchronizes the two keys — and
		// portNumber accepts "0999", mieruPort republishes it verbatim. For
		// "?port=0999&protocol=TCP&port=1000&protocol=UDP#src-001" mihomo
		// emits addr h:999 / name "src-001:0999/TCP" and addr h:1000 / name
		// "src-001:1000/UDP": byte-wise the first address is the GREATER one
		// while its name is the smaller. A range begin port carries the zero
		// the same way ("0999-9999" Sscanf'd to 999). The fixture values below
		// are stand-ins for that shape, chosen to read clearly.
		{"higher address, lower name", traceOutcome{addr: "1.2.3.4:1500", name: "src-001:1000/TCP"}},
		{"lower address, higher name", traceOutcome{addr: "1.2.3.4:1000", name: "src-001:9000/UDP"}},
	}
	for i, a := range ranked {
		for j, b := range ranked {
			if got := betterTraceOutcome(a.o, b.o); got != (i > j) {
				t.Errorf("betterTraceOutcome(%s, %s) = %v, want %v", a.desc, b.desc, got, i > j)
			}
		}
	}
}

// tracePort is one port of a mierus:// link as mihomo expands it: a
// Mieru-typed proxy named "<label>:<port>/<protocol>", so entryLabel folds
// several of them onto one key.
//
// Unlike its bandwidth/API twin it dials a server of ITS OWN instead of the
// address the probe put in the metadata. Two ports of one label have to answer
// DIFFERENT traces for a fold to be observable at all, and one shared endpoint
// cannot tell them apart. A nil embedded proxy is a dead port; delay holds the
// dial open long enough to make this port the LAST to reach the fold, so a
// test pins the completion order instead of hoping for one.
type tracePort struct {
	mihomo.Proxy
	name, addr, dial string
	delay            time.Duration
}

func (p *tracePort) Name() string             { return p.name }
func (p *tracePort) Type() mihomo.AdapterType { return mihomo.Mieru }
func (p *tracePort) Addr() string             { return p.addr }

func (p *tracePort) DialContext(ctx context.Context, _ *mihomo.Metadata) (mihomo.Conn, error) { //nolint:ireturn // implements mihomo.Proxy; the signature is not ours to choose
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.Proxy == nil {
		return nil, errors.New("port refused by test")
	}
	var meta mihomo.Metadata
	if err := meta.SetRemoteAddress(p.dial); err != nil {
		return nil, err
	}

	return p.Proxy.DialContext(ctx, &meta)
}

func traceServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func traceProber(endpoint string, logger zerolog.Logger) *MihomoProber {
	return &MihomoProber{logger: logger, geo: config.GeoBlockConfig{
		GeoTrace: config.GeoTraceConfig{
			Endpoint:    endpoint,
			Timeout:     5 * time.Second,
			Concurrency: 2,
		},
	}}
}

// TestTraceCheckKeepsOnlyAnsweredNodes drives the whole through-node path —
// apiCheck-style fan-out, apiProbeOne dial, status gate, parse — against a
// local server via mihomo's DIRECT outbound.
//
// Absence from the map is the contract: it is how the filter books a node
// unanswered and leaves the offline tag alone. The 403 case is the one worth
// pinning, because a CDN refusal or a captive portal can serve a body that
// parses perfectly while describing the wrong egress entirely.
func TestTraceCheckKeepsOnlyAnsweredNodes(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		desc     string
		status   int
		body     string
		answered bool
	}{
		{"200 with a full trace", http.StatusOK, traceAnswer("5.6.7.8", "DE"), true},
		{"403 carrying a valid trace", http.StatusForbidden, traceAnswer("5.6.7.8", "DE"), false},
		{"302 interstitial", http.StatusFound, traceAnswer("5.6.7.8", "DE"), false},
		{"204 with no body", http.StatusNoContent, "", false},
		{"200 with an unplaceable egress", http.StatusOK, traceAnswer("5.6.7.8", "XX"), false},
		{"200 that is not the trace endpoint", http.StatusOK, "<html><body>403 Forbidden</body></html>", false},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			srv := traceServer(t, c.status, c.body)
			px := adapter.NewProxy(outbound.NewDirect())
			got := traceProber(srv.URL, zerolog.Nop()).TraceCheck(context.Background(), []mihomo.Proxy{px})

			res, ok := got[px.Name()]
			if ok != c.answered {
				t.Fatalf("answered = %v (%+v), want %v", ok, res, c.answered)
			}
			if c.answered && res != (TraceResult{Country: "DE", IP: "5.6.7.8"}) {
				t.Fatalf("got %+v", res)
			}
		})
	}
}

// TestTraceCheckFoldsOnePortPerLabelDeterministically enters the real fold with
// two ports of one label answering DIFFERENT traces, which is the only way to
// observe a fold at all.
//
// Both ports carry the SAME address on purpose: that is what mihomo produces
// for "?port=2999&protocol=TCP&port=2999&protocol=UDP", since a mieru address
// is server+port while the name also carries the transport. Each case pins one
// completion order outright — the delayed port is the one a first-writer-wins
// fold would keep — and both must fold to the same port, or the published tag
// depends on the scheduler.
func TestTraceCheckFoldsOnePortPerLabelDeterministically(t *testing.T) {
	t.Parallel()

	const (
		label = "src-001"
		// Far beyond a loopback request, so "which port folded in last" is a
		// decision this test makes rather than one the scheduler makes.
		lateBy = 150 * time.Millisecond
	)
	tcpSrv := traceServer(t, http.StatusOK, traceAnswer("5.6.7.8", "DE"))
	udpSrv := traceServer(t, http.StatusOK, traceAnswer("9.9.9.9", "FR"))

	for _, c := range []struct {
		desc               string
		tcpDelay, udpDelay time.Duration
	}{
		{"UDP port folds in last", 0, lateBy},
		{"TCP port folds in last", lateBy, 0},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()

			pxs := []mihomo.Proxy{
				&tracePort{
					Proxy: adapter.NewProxy(outbound.NewDirect()),
					name:  label + ":2999/UDP", addr: "1.2.3.4:2999",
					dial: udpSrv.Listener.Addr().String(), delay: c.udpDelay,
				},
				&tracePort{
					Proxy: adapter.NewProxy(outbound.NewDirect()),
					name:  label + ":2999/TCP", addr: "1.2.3.4:2999",
					dial: tcpSrv.Listener.Addr().String(), delay: c.tcpDelay,
				},
			}
			got := traceProber(tcpSrv.URL, zerolog.Nop()).TraceCheck(context.Background(), pxs)

			if len(got) != 1 {
				t.Fatalf("outcomes keyed %v, want the single label %s", mapKeys(got), label)
			}
			// TCP sorts below UDP, and the name is the only thing that
			// separates these two.
			if want := (TraceResult{Country: "DE", IP: "5.6.7.8"}); got[label] != want {
				t.Errorf("folded %s to %+v, want the lower-named port's trace %+v", label, got[label], want)
			}
		})
	}
}

// TestTraceCheckAccountsForEveryNode pins the progress ordinals an operator
// watches during a cycle: step() runs BEFORE the status gate returns, so a node
// that answers nothing is still counted and "n of total" reaches the total.
func TestTraceCheckAccountsForEveryNode(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(zerolog.SyncWriter(&buf)).Level(zerolog.DebugLevel)
	pxs := []mihomo.Proxy{
		&tracePort{name: "src-001:2999/TCP", addr: "1.2.3.4:2999"},
		&tracePort{name: "src-001:9998-9999/UDP", addr: "1.2.3.4:9998"},
	}
	// A dead port answers nothing, so nothing may be published for the label.
	if got := traceProber("http://127.0.0.1:1/", logger).TraceCheck(context.Background(), pxs); len(got) != 0 {
		t.Fatalf("dead ports produced %v", got)
	}

	seen := map[int64]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var ev struct {
			Message string `json:"message"`
			N       int64  `json:"n"`
			Of      int64  `json:"of"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if ev.Message != "geotrace" {
			continue
		}
		if ev.Of != int64(len(pxs)) {
			t.Errorf("event %+v reports of=%d, want %d", ev, ev.Of, len(pxs))
		}
		seen[ev.N] = true
	}
	if len(seen) != len(pxs) {
		t.Fatalf("ordinals logged = %v, want one per node", seen)
	}
}
