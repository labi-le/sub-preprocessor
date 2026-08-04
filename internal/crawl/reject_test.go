package crawl //nolint:testpackage // exercises unexported crawl helpers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/fetch"
)

// The fixture covers two of candidate's three pre-fetch gates (noise host, and a
// non-public literal IP for the validate gate) and every post-fetch verdict
// (live, nodeless 2xx, expired, gone status, transient status, transport
// failure). The parse gate is the one path no fixture takes: every URL here
// parses, so record's `unparseable` field is not exercised from a cycle. The
// payload queries stand in for the real subscription credential — these URLs
// carry `?payload=<base64>` in production, and nothing that logs a candidate may
// reproduce it.
const (
	fixLive      = "https://live.example/sub?payload=SECRETPAYLOAD1234"
	fixNodeless  = "https://nodeless.example/sub"
	fixExpired   = "https://expired.example/sub"
	fixGone      = "https://gone.example/sub"
	fixTransient = "https://transient.example/sub"
	fixDead      = "https://dead.example/sub?payload=DEADSECRET9999"
	fixRaw       = "https://raw.example/sub?payload=RAWSECRET7777"
	fixRedirect  = "https://redirect.example/sub?payload=REDIRSECRET5555"
	fixElsewhere = "https://elsewhere.example/x?payload=OTHERSECRET3333"
	// fixPanel is the Marzban/3x-ui shape: the credential is the last PATH
	// segment and there is no query at all, so a line that logged host+path
	// would publish it whole.
	fixPanel     = "https://panel.example/sub/eyJhbGciOiJIUzI1NiJ9.PATHTOKEN4444"
	fixPrivateIP = "https://10.0.0.1/sub"
	fixNoise     = "https://t.me/somechannel"
	fixNoiseCDN  = "https://cdn4.telesco.pe/file/a.jpg"
	fixChannel   = "chan"
)

// fixturePage is one scraped t.me page carrying every fixture URL, several of
// them twice, so per-candidate accounting is proven to dedupe rather than
// counting a link once per occurrence.
const fixturePage = `<a href="` + fixNoise + `">x</a>` +
	`<img src="` + fixNoiseCDN + `"/>` +
	`<pre>` + fixPrivateIP + ` ` + fixLive + `</pre>` +
	`<pre>` + fixNodeless + ` ` + fixExpired + `</pre>` +
	`<pre>` + fixGone + ` ` + fixTransient + ` ` + fixDead + ` ` + fixRaw + ` ` + fixRedirect + `</pre>` +
	`<pre>` + fixPanel + `</pre>` +
	`<pre>` + fixLive + ` ` + fixNoise + ` ` + fixGone + `</pre>`

// fixtureClassify answers each fixture URL with the outcome its name promises.
// The three transport failures cover every way a URL reaches a log: fixDead is
// shaped like a real one (net/http wraps it in *url.Error, which embeds the
// request URL), fixRaw formats the URL straight into the message, and
// fixRedirect carries a SECOND URL the candidate's own substitution cannot
// touch — the shape a refused redirect produces.
func fixtureClassify(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
	switch string(u) {
	case fixLive:
		return classify.Result{Nodes: 3}, nil
	case fixNodeless, fixPanel:
		return classify.Result{}, nil
	case fixExpired:
		return classify.Result{Nodes: 2, Expired: true}, nil
	case fixGone:
		return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
	case fixTransient:
		return classify.Result{}, &classify.StatusError{Code: http.StatusServiceUnavailable, Status: "503 Service Unavailable"}
	case fixDead:
		return classify.Result{}, fmt.Errorf("do request: %w",
			&url.Error{Op: "Get", URL: fixDead, Err: errors.New("dial tcp: i/o timeout")})
	case fixRaw:
		return classify.Result{}, fmt.Errorf("origin refused %s", fixRaw) //nolint:err113 // fixture
	case fixRedirect:
		return classify.Result{}, fmt.Errorf("do request: %w", &url.Error{Op: "Get", URL: fixRedirect,
			Err: fmt.Errorf("invalid url: %w", &url.Error{
				Op: "parse", URL: fixElsewhere, Err: errors.New("net/url: invalid control character in URL"),
			})})
	default:
		return classify.Result{}, errors.New("unexpected fixture url: " + string(u))
	}
}

// cycleResult is everything one fixture cycle produced.
type cycleResult struct {
	lines []map[string]any
	raw   string
	priv  string
	// classified is every URL handed to the classifier, sorted, duplicates
	// kept. It IS the accept/reject decision: which links passed the pre-fetch
	// gates, and which managed sources the recheck re-examined.
	classified []string
}

// fixtureCrawler wires a page and classifier into a network-free crawler whose
// private.yaml starts with one hand-added source, two managed sources the cycle
// proves dead (one by status, one by advertised expiry) and one it cannot judge.
func fixtureCrawler(t *testing.T, logger zerolog.Logger, page string, seen *[]string, mu *sync.Mutex) (*Crawler, string) {
	t.Helper()
	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	const seed = "subscriptions:\n" +
		"  sources:\n" +
		"    - name: hand-added\n" +
		"      url: https://hand.example/sub\n" +
		"    - name: tg-0123456789\n" +
		"      url: " + fixGone + "\n" +
		"    - name: tg-keep-aaaaaa\n" +
		"      url: " + fixTransient + "\n" +
		"    - name: tg-exp-bbbbbb\n" +
		"      url: " + fixExpired + "\n"
	if err := os.WriteFile(priv, []byte(seed), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}
	return &Crawler{
		opts: Options{
			Channels:    []string{fixChannel},
			PrivatePath: priv,
			StatePath:   filepath.Join(dir, "state.json"),
			Pages:       1,
			Prune:       true,
			MaxDepth:    0,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/" + fixChannel: page}},
		classifyFn: func(ctx context.Context, hc *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			mu.Lock()
			*seen = append(*seen, string(u))
			mu.Unlock()
			return fixtureClassify(ctx, hc, u)
		},
		logger: logger,
	}, priv
}

// runCycle runs one cycle against page and returns its log stream and the exact
// set of URLs it decided were worth classifying.
func runCycle(t *testing.T, page string) cycleResult {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	var seen []string
	c, priv := fixtureCrawler(t, zerolog.New(&buf), page, &seen, &mu)
	c.RunOnce(context.Background())

	out := cycleResult{raw: buf.String(), priv: priv, classified: seen}
	slices.Sort(out.classified)
	for l := range strings.SplitSeq(strings.TrimSpace(out.raw), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, l)
		}
		out.lines = append(out.lines, m)
	}
	return out
}

// withMsg returns the log lines whose message is msg.
func withMsg(lines []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["message"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// TestRunOnceCandidateOutcomesAreByteStable pins the accept/reject decision of a
// full cycle twice over: the exact set of URLs the cycle chose to fetch, and the
// exact private.yaml it produced. Per-candidate reject logging is observability
// only — it may add log lines and must move neither. The classified set is the
// stricter of the two, because a candidate the gates newly let through does not
// necessarily reach the file: it is a decision change all the same.
//
// Regenerating either literal to make this pass defeats its only purpose. A diff
// here means the decision path changed, not that a golden went stale.
func TestRunOnceCandidateOutcomesAreByteStable(t *testing.T) {
	t.Parallel()

	res := runCycle(t, fixturePage)
	wantClassified := []string{
		fixDead,
		fixExpired, fixExpired, // discovered, and rechecked as tg-exp-bbbbbb
		fixGone, fixGone, // discovered, and rechecked as tg-0123456789
		fixLive,
		fixNodeless,
		fixPanel,
		fixRaw,
		fixRedirect,
		fixTransient, fixTransient, // discovered, and rechecked as tg-keep-aaaaaa
	}
	slices.Sort(wantClassified)
	if !slices.Equal(res.classified, wantClassified) {
		t.Fatalf("classified set changed:\n got: %q\nwant: %q", res.classified, wantClassified)
	}

	got, err := os.ReadFile(res.priv)
	if err != nil {
		t.Fatalf("read private.yaml: %v", err)
	}
	const want = `subscriptions:
    sources:
        - name: hand-added
          url: https://hand.example/sub
        - name: tg-chan-3d9dfd
          url: https://live.example/sub?payload=SECRETPAYLOAD1234
        - name: tg-keep-aaaaaa
          url: https://transient.example/sub
`
	if string(got) != want {
		t.Fatalf("private.yaml changed:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRejectLinePerReason: every rejection reason is reachable from one cycle
// and each names the candidate it rejected. Without this the enum could grow a
// member no code path can produce, which reads as a permanently-zero column in
// the summary.
func TestRejectLinePerReason(t *testing.T) {
	t.Parallel()

	lines := runCycle(t, fixturePage).lines
	got := map[string]map[string]any{}
	for _, l := range withMsg(lines, "candidate rejected") {
		host, _ := l["host"].(string)
		got[host] = l
	}

	for _, tc := range []struct {
		host   string
		reason rejectReason
		status float64
	}{
		{"t.me", rejectNoiseHost, 0},
		{"cdn4.telesco.pe", rejectNoiseHost, 0},
		{"10.0.0.1", rejectInvalidURL, 0},
		{"nodeless.example", rejectNodeless, 0},
		{"panel.example", rejectNodeless, 0},
		{"expired.example", rejectExpired, 0},
		{"gone.example", rejectStatus, http.StatusNotFound},
		{"transient.example", rejectStatus, http.StatusServiceUnavailable},
		{"dead.example", rejectFetch, 0},
		{"raw.example", rejectFetch, 0},
		{"redirect.example", rejectFetch, 0},
	} {
		l, ok := got[tc.host]
		if !ok {
			t.Errorf("no reject line for %q; got %v", tc.host, got)
			continue
		}
		if l["reason"] != string(tc.reason) {
			t.Errorf("%s: reason = %v, want %q", tc.host, l["reason"], tc.reason)
		}
		if l["channel"] != fixChannel {
			t.Errorf("%s: channel = %v, want %q", tc.host, l["channel"], fixChannel)
		}
		if tc.status != 0 && l["status"] != tc.status {
			t.Errorf("%s: status = %v, want %v", tc.host, l["status"], tc.status)
		}
	}
	if l, ok := got["live.example"]; ok {
		t.Errorf("the live candidate was logged as rejected: %v", l)
	}
	// Every fixture reason is covered above, so nothing else may appear.
	if len(got) != len(withMsg(lines, "candidate rejected")) {
		t.Errorf("two reject lines share a host: %d distinct of %d", len(got), len(withMsg(lines, "candidate rejected")))
	}
}

// TestRejectLineCarriesTheMergeSourceName: the identifier on a reject line is
// what correlates it to a private.yaml entry, since the line carries no path
// and no query. It is checked twice — against an independently computed
// tg-<slug>-<sha6>, and against sourceName itself — so neither a drifting log
// format nor a drifting naming rule can pass unnoticed.
//
// The slug is this channel's, not necessarily the discoverer's: a URL first
// seen in another channel is named for that one by the merge. The sha6 suffix
// is channel-independent and is what actually correlates.
func TestRejectLineCarriesTheMergeSourceName(t *testing.T) {
	t.Parallel()

	lines := runCycle(t, fixturePage).lines
	sum := sha256.Sum256([]byte(fixNodeless))
	want := managedPrefix + fixChannel + "-" + hex.EncodeToString(sum[:])[:6]

	var found string
	for _, l := range withMsg(lines, "candidate rejected") {
		if l["host"] == "nodeless.example" {
			found, _ = l["source"].(string)
		}
	}
	if found != want {
		t.Fatalf("source = %q, want %q", found, want)
	}
	if got := sourceName(fixNodeless, "", fixChannel, nil); got != want {
		t.Fatalf("sourceName = %q, want %q: the log no longer reuses the merge's naming", got, want)
	}
}

// TestRejectLinesCarryNoCredential greps the whole captured stream. The
// credential is the query for one panel shape and the PATH for another —
// Marzban's /sub/<USER_TOKEN> and 3x-ui's /<subPath>/<subId> have no query at
// all — so neither may appear in any field or any error text. The three
// transport failures are the trap, and they need both halves of the redaction:
// substituting the candidate's own URL, and refusing to emit error text that
// still names any part of one.
func TestRejectLinesCarryNoCredential(t *testing.T) {
	t.Parallel()

	res := runCycle(t, fixturePage)
	lines, raw := res.lines, res.raw
	if raw == "" {
		t.Fatal("no log output captured")
	}
	secrets := make([]string, 0, 20)
	secrets = append(secrets,
		"payload=", "SECRETPAYLOAD1234", "DEADSECRET9999", "RAWSECRET7777",
		"REDIRSECRET5555", "OTHERSECRET3333",
		// The path-borne credential: the token, and the whole path holding it.
		"PATHTOKEN4444", "/sub/eyJhbGciOiJIUzI1NiJ9.PATHTOKEN4444",
		mustQuery(t, fixLive), mustQuery(t, fixDead), mustQuery(t, fixRaw),
		mustQuery(t, fixRedirect), mustQuery(t, fixElsewhere),
	)
	for _, u := range []string{fixLive, fixDead, fixRaw, fixRedirect, fixPanel} {
		secrets = append(secrets, mustPath(t, u))
	}
	for _, secret := range secrets {
		if strings.Contains(raw, secret) {
			t.Errorf("log stream leaks %q:\n%s", secret, raw)
		}
	}
	// A URL in free text is how a credential escapes even after the fields are
	// clean: `host` is the only place a candidate's location may appear.
	for _, l := range withMsg(lines, "candidate rejected") {
		if msg, _ := l["error"].(string); strings.Contains(msg, "://") {
			t.Errorf("reject line embeds a URL in its error: %q", msg)
		}
	}
	// Redaction must cost the diagnosis only where it cannot be kept safely:
	// the two errors naming just the candidate keep theirs, the one naming a
	// second URL is dropped whole.
	for _, want := range []string{"dial tcp: i/o timeout", "origin refused raw.example", redactedError} {
		if !strings.Contains(raw, want) {
			t.Errorf("expected %q in the log stream:\n%s", want, raw)
		}
	}
}

func mustPath(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || len(u.EscapedPath()) <= 1 {
		t.Fatalf("fixture %q has no path to check for", raw)
	}
	return u.EscapedPath()
}

func mustQuery(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		t.Fatalf("fixture %q has no query to check for", raw)
	}
	return u.RawQuery
}

// TestRejectLineKeepsRealClassifyErrors closes the gap every other test in this
// file leaves open. crawl.New wires the real classify.URL, but each test here
// replaces classifyFn with fixtureClassify, so nothing routed an error the real
// classify.URL built into a reject line — safeError's inventory of reachable
// shapes was checked by reading the code, not by running it. Each case below
// makes a real server produce a real error through the real classify.URL, with a
// candidate URL whose last path segment is the credential (the Marzban shape).
//
// Two assertions per case, and the SECOND is the one that bites. The path must
// not reach the log — that is the leak namesSecret guards. And the diagnosis must
// SURVIVE: a redacted message here means an error on this path started naming the
// candidate's path or query, which is exactly what the inventory claims does not
// happen. Changing classify.go's read-response wrap to `read response %s` with
// req.URL.EscapedPath() fails this test for that reason, and used to fail nothing.
//
// classify.URL's *StatusError is deliberately absent: classifyAll records it as
// rejectStatus with the code and a nil err, so it never reaches safeError.
func TestRejectLineKeepsRealClassifyErrors(t *testing.T) {
	t.Parallel()

	const credToken = "REALTOKEN8888"
	const credPath = "/sub/eyJhbGciOiJIUzI1NiJ9." + credToken
	cases := []struct {
		name string
		// setup returns the candidate URL and the client classify.URL must use.
		setup func(t *testing.T) (string, *http.Client)
		want  []string
	}{{
		// classify.go's read-response wrap: Content-Length promises more than the
		// handler delivers, and the partial body is flushed before the abort so
		// the failure lands in io.ReadAll rather than in client.Do.
		name: "read response",
		setup: func(t *testing.T) (string, *http.Client) {
			srv := tlsFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "4096")
				_, _ = w.Write([]byte("vless://truncated"))
				w.(http.Flusher).Flush() //nolint:forcetypeassert // httptest's ResponseWriter is one
				panic(http.ErrAbortHandler)
			})
			return srv.URL + credPath, srv.Client()
		},
		want: []string{"read response", "unexpected EOF"},
	}, {
		// The reachable *url.Error: net/http embeds the whole request URL, so
		// this is the case the substitution exists for.
		name: "do request",
		setup: func(*testing.T) (string, *http.Client) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			client := srv.Client()
			srv.Close()
			return srv.URL + credPath, client
		},
		want: []string{"do request", "connection refused"},
	}, {
		name: "response too large",
		setup: func(t *testing.T) (string, *http.Client) {
			srv := tlsFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
				chunk := bytes.Repeat([]byte("x"), 1<<16)
				for range (10 << 20 / (1 << 16)) + 1 {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			})
			return srv.URL + credPath, srv.Client()
		},
		want: []string{"response too large", "10485760"},
	}, {
		name: "validate url",
		setup: func(*testing.T) (string, *http.Client) {
			return "http://plain.example" + credPath, http.DefaultClient
		},
		want: []string{"validate url", "only https"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, client := tc.setup(t)
			_, err := classify.URL(context.Background(), client, fetch.SubscriptionURL(raw))
			if err == nil {
				t.Fatalf("classify.URL(%q) succeeded; the case produced no error to log", raw)
			}
			assertRealErrorSurvives(t, tc.name, raw, err, tc.want, credToken, credPath)
		})
	}
}

// assertRealErrorSurvives records err against raw and checks both halves of the
// invariant: no part of the credential reaches the line, and the diagnosis is not
// redacted away.
func assertRealErrorSurvives(t *testing.T, name, raw string, err error, want []string, secrets ...string) {
	t.Helper()

	var buf bytes.Buffer
	newRejects(zerolog.New(&buf)).record(fixChannel, raw, rejectFetch, 0, err)
	line := buf.String()
	for _, secret := range append(secrets, mustPath(t, raw)) {
		if strings.Contains(line, secret) {
			t.Errorf("reject line leaks %q from a real %s error:\n%s", secret, name, line)
		}
	}

	var m map[string]any
	if uErr := json.Unmarshal([]byte(strings.TrimSpace(line)), &m); uErr != nil {
		t.Fatalf("log line is not JSON (%v): %s", uErr, line)
	}
	got, _ := m["error"].(string)
	if got == redactedError || got == redactedPathError {
		t.Fatalf("a real %s error was redacted (%q), so classify or fetch now names a request "+
			"path or query and safeError's inventory is stale; raw error: %v", name, got, err)
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("error field %q lost %q from the real diagnosis %v", got, w, err)
		}
	}
}

func tlsFixtureServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestSafeErrorRedactsASchemelessPath exercises namesSecret itself, with the
// exact shape the guard exists for: an error naming the candidate's path (or its
// query) and nothing else, so neither the URL substitution nor the "://" check
// sees anything. Restoring the guard was ruled after a mutation proved the shape
// leaks verbatim without it while the whole suite stayed green.
func TestSafeErrorRedactsASchemelessPath(t *testing.T) {
	t.Parallel()

	u, err := url.Parse(fixPanel)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	qURL, err := url.Parse(fixLive)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	cases := map[string]struct {
		err  error
		raw  string
		u    *url.URL
		want string
	}{
		// The mutation: `read response %s` with req.URL.EscapedPath().
		"path only": {
			err: fmt.Errorf("read response %s: %w", u.EscapedPath(), io.ErrUnexpectedEOF),
			raw: fixPanel, u: u, want: redactedPathError,
		},
		"query only": {
			err: fmt.Errorf("read response %s: %w", qURL.RawQuery, io.ErrUnexpectedEOF),
			raw: fixLive, u: qURL, want: redactedPathError,
		},
		// Still the other rule's job: a scheme survives, so it is redactedError.
		"second url": {
			err: errors.New("refused redirect to https://elsewhere.example/x"),
			raw: fixPanel, u: u, want: redactedError,
		},
		// A real message must not be redacted for merely containing a "/".
		"clean diagnosis": {
			err: errors.New("read response: unexpected EOF"),
			raw: fixPanel, u: u, want: "read response: unexpected EOF",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := safeError(tc.err, tc.raw, tc.u); got != tc.want {
				t.Fatalf("safeError = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRejectSummaryCountsSumToTotal: the summary is the answer to "why did this
// cycle find fewer subscriptions", so its per-reason counts must account for
// every rejection. Completeness of the column set is enforced by `exhaustive`
// on the rejectField map literal, not here; what this pins is that report()
// emits every column and that the arithmetic holds.
func TestRejectSummaryCountsSumToTotal(t *testing.T) {
	t.Parallel()

	lines := runCycle(t, fixturePage).lines
	summary := withMsg(lines, "candidates rejected by reason")
	if len(summary) != 1 {
		t.Fatalf("got %d summary lines, want exactly one per cycle", len(summary))
	}

	total, ok := summary[0]["rejected"].(float64)
	if !ok {
		t.Fatalf("summary has no rejected total: %v", summary[0])
	}
	sum := 0.0
	for _, field := range rejectField {
		n, present := summary[0][field].(float64)
		if !present {
			t.Fatalf("summary is missing the %q count: %v", field, summary[0])
		}
		sum += n
	}
	if sum != total {
		t.Fatalf("per-reason counts sum to %v, rejected total is %v: %v", sum, total, summary[0])
	}
	// The fixture rejects eleven distinct candidates; a repeated link must not
	// inflate the total.
	const wantTotal = 11
	if total != wantTotal {
		t.Fatalf("rejected = %v, want %d distinct candidates", total, wantTotal)
	}
	if lineCount := len(withMsg(lines, "candidate rejected")); float64(lineCount) != total {
		t.Fatalf("%d per-candidate lines for %v rejections", lineCount, total)
	}
}

// TestRejectSummaryExcludesACandidateAcceptedElsewhere: the same link is
// reposted into many channels and classified once per channel, so a transient
// 503 in one and a live answer in another is routine. The cycle adopted the URL,
// so counting it as a rejection would make the headline number disagree with
// what the merge did. The per-candidate line stays — it records what that
// channel's fetch actually answered.
func TestRejectSummaryExcludesACandidateAcceptedElsewhere(t *testing.T) {
	t.Parallel()

	const flappy = "https://flappy.example/sub"
	var buf bytes.Buffer
	var mu sync.Mutex
	var seen []string
	c, _ := fixtureCrawler(t, zerolog.New(&buf), "", &seen, &mu)
	c.opts.Channels = []string{"chana", "chanb"}
	c.client = pageFetcher{pages: map[string]string{
		"https://t.me/s/chana": `<pre>` + flappy + `</pre>`,
		"https://t.me/s/chanb": `<pre>` + flappy + `</pre>`,
	}}
	var calls atomic.Int32
	c.classifyFn = func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
		if string(u) != flappy {
			return fixtureClassify(context.Background(), nil, u)
		}
		if calls.Add(1) == 1 {
			return classify.Result{}, &classify.StatusError{Code: http.StatusServiceUnavailable, Status: "503"}
		}
		return classify.Result{Nodes: 1}, nil
	}
	c.RunOnce(context.Background())

	var lines []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, l)
		}
		lines = append(lines, m)
	}
	summary := withMsg(lines, "candidates rejected by reason")
	if len(summary) != 1 {
		t.Fatalf("got %d summary lines, want one", len(summary))
	}
	if got := summary[0]["rejected"]; got != float64(0) {
		t.Fatalf("rejected = %v, want 0: the cycle adopted the URL", got)
	}
	if got := summary[0]["bad_status"]; got != float64(0) {
		t.Fatalf("bad_status = %v, want 0", got)
	}
	// The line itself is still there: one channel's fetch really did answer 503.
	if n := len(withMsg(lines, "candidate rejected")); n != 1 {
		t.Fatalf("got %d per-candidate lines, want the one 503 attempt", n)
	}
}

// TestRejectLinesAreCappedPerCycle: the per-candidate lines are INFO because the
// crawler has no reachable DEBUG level (see maxRejectLines), so the only thing
// standing between a repost-graph blow-up and an unbounded log is this cap. The
// counts stay complete regardless, and the withheld lines are declared.
func TestRejectLinesAreCappedPerCycle(t *testing.T) {
	t.Parallel()

	const over = 50
	candidates := maxRejectLines + over
	var page strings.Builder
	for i := range candidates {
		// Literal private IPs: rejected by the pre-fetch gate, so the cap is
		// exercised without any classify fan-out.
		fmt.Fprintf(&page, "<pre>https://10.%d.%d.1/sub</pre>", i/256, i%256)
	}

	lines := runCycle(t, page.String()).lines
	if got := len(withMsg(lines, "candidate rejected")); got != maxRejectLines {
		t.Fatalf("%d per-candidate lines, want the cap of %d", got, maxRejectLines)
	}

	summary := withMsg(lines, "candidates rejected by reason")
	if len(summary) != 1 {
		t.Fatalf("got %d summary lines, want one", len(summary))
	}
	if got := summary[0]["rejected"]; got != float64(candidates) {
		t.Fatalf("rejected = %v, want %d: the cap must withhold lines, not counts", got, candidates)
	}

	withheld := withMsg(lines, "per-candidate reject lines withheld by the cycle cap")
	if len(withheld) != 1 {
		t.Fatalf("got %d withheld lines, want exactly one", len(withheld))
	}
	if got := withheld[0]["suppressed"]; got != float64(over) {
		t.Fatalf("suppressed = %v, want %d", got, over)
	}
	if got := withheld[0]["cap"]; got != float64(maxRejectLines) {
		t.Fatalf("cap = %v, want %d", got, maxRejectLines)
	}
}

// TestRejectNoWithheldLineUnderTheCap: the withheld line must appear only when
// the cap actually trips, or an operator learns to ignore it.
func TestRejectNoWithheldLineUnderTheCap(t *testing.T) {
	t.Parallel()

	lines := runCycle(t, fixturePage).lines
	if got := withMsg(lines, "per-candidate reject lines withheld by the cycle cap"); len(got) != 0 {
		t.Fatalf("cycle under the cap declared %d withheld lines: %v", len(got), got)
	}
}

// TestRecheckDoesNotEnterTheDiscoverySummary: recheckManaged shares classifyAll,
// but its URLs are existing managed sources. Counting them would make the
// summary answer a different question than the one it is labelled with.
func TestRecheckDoesNotEnterTheDiscoverySummary(t *testing.T) {
	t.Parallel()

	lines := runCycle(t, fixturePage).lines
	// tg-keep-aaaaaa's URL is rechecked and answers 503; it is also a discovered
	// candidate, so exactly one line — the discovery one — may mention it.
	n := 0
	for _, l := range withMsg(lines, "candidate rejected") {
		if l["host"] == "transient.example" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("got %d lines for the rechecked URL, want 1 (discovery only)", n)
	}
	// hand.example is hand-added and never a candidate; it must not appear.
	if strings.Contains(fmt.Sprint(lines), "hand.example") {
		t.Fatalf("a hand-added source reached the candidate log: %v", lines)
	}
}

// TestRejectKeyDoesNotPinThePage: extractURLs hands out sub-slices of the
// scraped page (html.UnescapeString then urlRe.FindAllString, and
// strings.TrimRight narrows without copying), so a key kept for the whole cycle
// would hold its entire page — up to maxPageBytes, 8 MiB — alive until scan
// returns. The dedupe set needs the bytes of the URL, not of the page.
func TestRejectKeyDoesNotPinThePage(t *testing.T) {
	t.Parallel()

	const target = "https://10.1.2.3/sub"
	page := strings.Repeat("x", 4096) + " " + target + " " + strings.Repeat("y", 4096)
	urls := extractURLs(page)
	if len(urls) != 1 || urls[0] != target {
		t.Fatalf("extractURLs = %q, want exactly %q", urls, target)
	}
	if unsafe.StringData(urls[0]) == unsafe.StringData(target) {
		t.Fatal("fixture is not exercising the sub-slice case")
	}

	rej := newRejects(zerolog.Nop())
	rej.record("chan", urls[0], rejectInvalidURL, 0, nil)

	var key string
	for k := range rej.verdict {
		key = k
	}
	if key != target {
		t.Fatalf("recorded key = %q, want %q", key, target)
	}
	// html.UnescapeString returns its argument unchanged when there is no '&' —
	// and this page has none — so the sub-slice points into page itself. Compare
	// against the slice we were given rather than against target, which is a
	// separate constant.
	base := uintptr(unsafe.Pointer(unsafe.StringData(urls[0])))
	got := uintptr(unsafe.Pointer(unsafe.StringData(key)))
	if got == base {
		t.Fatal("the reject key is the page sub-slice itself, pinning the whole page")
	}
}

// TestHarvestedKeyDoesNotPinThePage: the ACCEPTED key is the longer-lived of the
// two, so the same sub-slice mechanic costs more here. keys(cand) feeds
// classifyAll, which copies every live URL into the cycle-wide live map
// scanChannel hands to RunOnce for mergeManaged — so this key holds its page
// past scan, not merely until it returns, and a page is up to maxPageBytes.
func TestHarvestedKeyDoesNotPinThePage(t *testing.T) {
	t.Parallel()

	const target = "https://harvest.example/sub"
	page := strings.Repeat("x", 4096) + " " + target + " " + strings.Repeat("y", 4096)
	urls := extractURLs(page)
	if len(urls) != 1 || urls[0] != target {
		t.Fatalf("extractURLs = %q, want exactly %q", urls, target)
	}
	base := uintptr(unsafe.Pointer(unsafe.StringData(urls[0])))

	var inline []string
	cand := (&Crawler{}).harvestPages([]string{page}, &inline, nil, fixChannel)
	if len(cand) != 1 {
		t.Fatalf("harvestPages accepted %d candidates, want 1: %v", len(cand), cand)
	}
	for key := range cand {
		if key != target {
			t.Fatalf("harvested key = %q, want %q", key, target)
		}
		if uintptr(unsafe.Pointer(unsafe.StringData(key))) == base {
			t.Fatal("the harvested key is the page sub-slice itself, pinning the whole page")
		}
	}
}

// TestRejectTrackingIsBounded: verdict is the only per-cycle accumulator this
// change adds, and unlike live it has no network brake — a URL that fails a
// candidate gate costs nothing but local string work, so one page of junk links
// could add hundreds of thousands of entries. Past the bound the dedupe set
// stops growing and the overflow is reported apart from the per-reason counts,
// because a repeat can no longer be told from a new candidate.
func TestRejectTrackingIsBounded(t *testing.T) {
	t.Parallel()

	const over = 5
	var buf bytes.Buffer
	rej := newRejects(zerolog.New(&buf))
	for i := range maxRejectTracked + over {
		rej.record("chan", fmt.Sprintf("https://10.0.0.1/sub/%d", i), rejectInvalidURL, 0, nil)
	}
	if len(rej.verdict) != maxRejectTracked {
		t.Fatalf("tracked %d urls, want the bound of %d", len(rej.verdict), maxRejectTracked)
	}
	if rej.untracked != over {
		t.Fatalf("untracked = %d, want %d", rej.untracked, over)
	}

	buf.Reset()
	rej.report(nil)
	var summary map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, l)
		}
		if m["message"] == "candidates rejected by reason" {
			summary = m
		}
	}
	if summary == nil {
		t.Fatal("no summary line")
	}
	if got := summary["rejected"]; got != float64(maxRejectTracked) {
		t.Fatalf("rejected = %v, want %d", got, maxRejectTracked)
	}
	if got := summary["untracked"]; got != float64(over) {
		t.Fatalf("untracked = %v, want %d", got, over)
	}
	if got := summary["invalid_url"]; got != float64(maxRejectTracked) {
		t.Fatalf("invalid_url = %v, want %d: the overflow must not fold into a reason", got, maxRejectTracked)
	}
}
