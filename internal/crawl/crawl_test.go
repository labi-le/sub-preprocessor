package crawl //nolint:testpackage // exercises unexported crawl helpers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/fetch"
)

func TestExtractURLs(t *testing.T) {
	t.Parallel()

	page := `<a href="https://t.me/somechan">x</a>` +
		`<pre>https://is.wepogp.gay/bypass?payload=AbC%2Bd/e=</pre>` +
		`<img src="https://cdn4.telesco.pe/file/abc.jpg"/>` +
		`text https://host.example/api/filter?code=RU&amp;type=white, end` +
		` nbsp https://nb.example/sub&nbsp;tail`

	got := extractURLs(html.UnescapeString(page))
	want := map[string]bool{
		"https://t.me/somechan":                              true,
		"https://is.wepogp.gay/bypass?payload=AbC%2Bd/e=":    true,
		"https://cdn4.telesco.pe/file/abc.jpg":               true,
		"https://host.example/api/filter?code=RU&type=white": true, // &amp; unescaped, trailing comma trimmed
		"https://nb.example/sub":                             true, // &nbsp; (U+00A0) terminates the URL
	}
	if len(got) != len(want) {
		t.Fatalf("got %d urls %v, want %d", len(got), got, len(want))
	}
	for _, u := range got {
		if !want[u] {
			t.Errorf("unexpected url %q", u)
		}
	}
}

// TestExtractorsTakeAnUnescapedPage pins the contract that let the
// html.UnescapeString move out of the scans and into harvestPages: page 0 feeds
// both of them, so a scan that unescapes for itself copies that page twice.
func TestExtractorsTakeAnUnescapedPage(t *testing.T) {
	t.Parallel()

	const (
		page     = `link https://sub.example/a?x=1&amp;y=2 <pre>vless://u@1.2.3.4:443?a=1&amp;b=2#n</pre>`
		wantURL  = `https://sub.example/a?x=1&amp;y=2`
		wantNode = `vless://u@1.2.3.4:443?a=1&amp;b=2#n`
	)

	if got := extractURLs(page); len(got) != 1 || got[0] != wantURL {
		t.Fatalf("extractURLs = %q, want [%q] with the entity left alone", got, wantURL)
	}
	if got := extractInlineNodes(page); len(got) != 1 || got[0] != wantNode {
		t.Fatalf("extractInlineNodes = %q, want [%q] with the entity left alone", got, wantNode)
	}

	var inline []string
	c := &Crawler{opts: Options{InlineEnabled: true}}
	cand := (*Crawler).harvestPages(c, []string{page}, &inline, nil, "chan")
	if _, ok := cand[`https://sub.example/a?x=1&y=2`]; !ok || len(cand) != 1 {
		t.Fatalf("harvested %v, want the decoded url alone", cand)
	}
	if len(inline) != 1 || inline[0] != `vless://u@1.2.3.4:443?a=1&b=2#n` {
		t.Fatalf("harvested inline %q, want the decoded node", inline)
	}
}

func TestClassifyAllDistinguishesUnknownFromDead(t *testing.T) {
	t.Parallel()

	c := &Crawler{
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			switch string(u) {
			case "https://live.example/sub":
				return classify.Result{Nodes: 1}, nil
			case "https://err.example/sub":
				return classify.Result{}, errors.New("transient network error")
			case "https://gone.example/sub":
				return classify.Result{}, fmt.Errorf("wrap: %w", &classify.StatusError{Code: 404, Status: "404 Not Found"})
			default:
				// The origin advertised an expiry already past: the one 2xx
				// answer that proves the subscription is over.
				return classify.Result{Nodes: 9, Expired: true}, nil
			}
		},
		logger: zerolog.Nop(),
	}
	live, unknown := c.classifyAll(context.Background(),
		[]string{"https://live.example/sub", "https://err.example/sub", "https://dead.example/sub", "https://gone.example/sub"}, nil, "")

	if !live["https://live.example/sub"] || len(live) != 1 {
		t.Errorf("live = %v, want exactly the live URL", live)
	}
	if !unknown["https://err.example/sub"] || len(unknown) != 1 {
		t.Errorf("unknown = %v, want exactly the transport-errored URL", unknown)
	}
	if unknown["https://gone.example/sub"] {
		t.Error("a definitive non-2xx answer (StatusError) must not be treated as unknown")
	}
	if unknown["https://dead.example/sub"] {
		t.Error("an origin-advertised past expiry is definitive; it must not be treated as unknown")
	}
}

// TestClassifyAllKeepsNodeless200Undetermined pins CRAWL-V3: a 200 carrying no
// proxy-scheme node means the host answered and told us nothing, not that the
// subscription is gone. PP-07 tightened the parser to count only real node
// schemes, so a captive portal, a Cloudflare interstitial and a panel login page
// all arrive here with Nodes == 0 — and a URL in neither returned set is one
// mergeManaged deletes, unrecoverably. Only a gone status and an
// origin-advertised expiry may stay definitive.
func TestClassifyAllKeepsNodeless200Undetermined(t *testing.T) {
	t.Parallel()

	const (
		urlPortal  = "https://portal.example/sub"  // 200 + interstitial HTML
		urlEmpty   = "https://empty.example/sub"   // 200 + empty body
		urlExpired = "https://expired.example/sub" // 200 + nodes, past expiry
		urlGone    = "https://gone.example/sub"    // 404
	)
	c := &Crawler{
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			switch string(u) {
			case urlExpired:
				return classify.Result{Nodes: 12, Expired: true}, nil
			case urlGone:
				return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
			default:
				return classify.Result{}, nil
			}
		},
		logger: zerolog.Nop(),
	}
	live, unknown := c.classifyAll(context.Background(), []string{urlPortal, urlEmpty, urlExpired, urlGone}, nil, "")

	if len(live) != 0 {
		t.Fatalf("live = %v, want none: no URL served a node", live)
	}
	for _, u := range []string{urlPortal, urlEmpty} {
		if !unknown[u] {
			t.Errorf("%s answered 200 with no node: undetermined, not a death sentence", u)
		}
	}
	if unknown[urlExpired] {
		t.Error("an origin-advertised past expiry is definitive; it must stay prunable")
	}
	if unknown[urlGone] {
		t.Error("404 is definitive; it must stay prunable")
	}
}

func TestClassifyAllBoundsConcurrency(t *testing.T) {
	t.Parallel()

	var cur, peak atomic.Int32
	c := &Crawler{
		classifyFn: func(context.Context, *http.Client, fetch.SubscriptionURL) (classify.Result, error) {
			n := cur.Add(1)
			defer cur.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}
	urls := make([]string, 0, 4*classifyConcurrency)
	for i := range cap(urls) {
		urls = append(urls, fmt.Sprintf("https://h%d.example/sub", i))
	}
	live, unknown := c.classifyAll(context.Background(), urls, nil, "")
	if len(live) != len(urls) || len(unknown) != 0 {
		t.Fatalf("live=%d unknown=%d, want %d/0", len(live), len(unknown), len(urls))
	}
	if p := peak.Load(); p > classifyConcurrency {
		t.Fatalf("peak in-flight classifications %d exceed classifyConcurrency %d", p, classifyConcurrency)
	}
}

func TestRecheckRetainsUnknownPrunesDead(t *testing.T) {
	t.Parallel()

	const (
		urlHand     = "https://hand.example/sub"
		urlLive     = "https://live.example/sub"
		urlDead     = "https://dead.example/sub"
		urlErr      = "https://err.example/sub"
		urlGone     = "https://gone.example/sub"
		urlNodeless = "https://nodeless.example/sub"
	)
	c := &Crawler{
		opts: Options{Prune: true},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			switch string(u) {
			case urlLive:
				return classify.Result{Nodes: 1}, nil
			case urlErr:
				return classify.Result{}, errors.New("transient network error")
			case urlGone:
				return classify.Result{}, &classify.StatusError{Code: 410, Status: "410 Gone"}
			case urlNodeless:
				return classify.Result{}, nil // 200, but not one node in the body
			default:
				// Definitively over: the origin advertised a past expiry.
				return classify.Result{Nodes: 4, Expired: true}, nil
			}
		},
		logger: zerolog.Nop(),
	}
	var pf privateFile
	pf.Subscriptions.Sources = []source{
		{Name: "hand-added", URL: urlHand},
		{Name: managedName(urlLive), URL: urlLive},
		{Name: managedName(urlDead), URL: urlDead},
		{Name: managedName(urlErr), URL: urlErr},
		{Name: managedName(urlGone), URL: urlGone},
		{Name: managedName(urlNodeless), URL: urlNodeless},
	}

	live := map[string]string{}
	rr := c.recheckManaged(context.Background(), pf, live)
	next, managed, _ := c.mergeManaged(pf, live, rr, true, nil)

	byURL := map[string]bool{}
	for _, s := range next {
		byURL[s.URL] = true
	}
	if !byURL[urlHand] {
		t.Error("hand-added source must be preserved")
	}
	if !byURL[urlLive] {
		t.Error("still-live managed source must be kept")
	}
	if byURL[urlDead] {
		t.Error("a managed source whose origin advertised a past expiry must be pruned")
	}
	if byURL[urlGone] {
		t.Error("managed source answering a definitively gone status must be pruned")
	}
	if !byURL[urlErr] {
		t.Error("managed source with unknown (errored) status must be retained, not pruned")
	}
	if !byURL[urlNodeless] {
		t.Error("a managed source answering 200 with no node is undetermined: it must be retained")
	}
	if len(managed) != 3 {
		t.Errorf("managed = %v, want live+errored+nodeless (3 entries)", managed)
	}
}

// TestClassifyAllTreatsTransientStatusAsUnknown pins the verdict per status
// code. A URL missing from both sets is what makes the caller delete it, so only
// a definitively gone answer may land there; every retryable status has to be
// undetermined.
func TestClassifyAllTreatsTransientStatusAsUnknown(t *testing.T) {
	t.Parallel()

	transient := []int{
		http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooEarly,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}
	gone := []int{http.StatusNotFound, http.StatusGone, http.StatusUnavailableForLegalReasons}

	urlFor := func(code int) string { return fmt.Sprintf("https://s%d.example/sub", code) }
	code := map[string]int{}
	urls := make([]string, 0, len(transient)+len(gone))
	for _, status := range append(append([]int{}, transient...), gone...) {
		u := urlFor(status)
		urls = append(urls, u)
		code[u] = status
	}

	c := &Crawler{
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			status := code[string(u)]
			return classify.Result{}, &classify.StatusError{Code: status, Status: http.StatusText(status)}
		},
		logger: zerolog.Nop(),
	}
	live, unknown := c.classifyAll(context.Background(), urls, nil, "")

	if len(live) != 0 {
		t.Fatalf("live = %v, want none: no URL answered 2xx", live)
	}
	for _, status := range transient {
		if !unknown[urlFor(status)] {
			t.Errorf("status %d must be undetermined, not a death sentence", status)
		}
	}
	for _, status := range gone {
		if unknown[urlFor(status)] {
			t.Errorf("status %d is definitive; it must not be undetermined", status)
		}
	}
}

// TestRecheckKeepsTransientStatusPrunesGone is the anti-data-loss contract of the
// recheck: a rate-limited panel (429), a provider mid-deploy (503) and a WAF
// refusing a non-browser client (403) all keep their source, because a pruned URL
// is unrecoverable — it only returns if the same channel post is rescraped.
func TestRecheckKeepsTransientStatusPrunesGone(t *testing.T) {
	t.Parallel()

	const (
		urlLive        = "https://live.example/sub"
		urlRateLimited = "https://ratelimited.example/sub"
		urlDeploying   = "https://deploying.example/sub"
		urlBlocked     = "https://blocked.example/sub"
		urlNotFound    = "https://notfound.example/sub"
	)
	status := map[string]int{
		urlRateLimited: http.StatusTooManyRequests,
		urlDeploying:   http.StatusServiceUnavailable,
		urlBlocked:     http.StatusForbidden,
		urlNotFound:    http.StatusNotFound,
	}
	c := &Crawler{
		opts: Options{Prune: true},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			if code, bad := status[string(u)]; bad {
				return classify.Result{}, &classify.StatusError{Code: code, Status: http.StatusText(code)}
			}
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}
	var pf privateFile
	for _, u := range []string{urlLive, urlRateLimited, urlDeploying, urlBlocked, urlNotFound} {
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u})
	}

	live := map[string]string{}
	rr := c.recheckManaged(context.Background(), pf, live)
	_, managed, _ := c.mergeManaged(pf, live, rr, true, nil)

	kept := map[string]bool{}
	for _, s := range managed {
		kept[s.URL] = true
	}
	if !kept[urlLive] {
		t.Error("a source that still serves nodes must be kept")
	}
	for _, u := range []string{urlRateLimited, urlDeploying, urlBlocked} {
		if !kept[u] {
			t.Errorf("%s answered %d, which is transient: the source must be kept", u, status[u])
		}
	}
	if kept[urlNotFound] {
		t.Error("404 is definitive: the source must be pruned")
	}
}

// TestMergeRetainsMidCycleAdditions covers the lost-update guard: a managed
// (tg-*) source that appears in the re-loaded private.yaml but was absent from
// the cycle-start snapshot was never checked this cycle and must be retained,
// even with pruning enabled.
func TestMergeRetainsMidCycleAdditions(t *testing.T) {
	t.Parallel()

	const urlNew = "https://midcycle.example/sub"
	c := &Crawler{opts: Options{Prune: true}, logger: zerolog.Nop()}

	// Re-loaded file contains a managed source unknown to the cycle snapshot.
	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: managedName(urlNew), URL: urlNew}}

	next, managed, _ := c.mergeManaged(pf, map[string]string{}, recheckResult{}, true, nil)
	if len(next) != 1 || next[0].URL != urlNew {
		t.Fatalf("next = %v, want the mid-cycle addition retained", next)
	}
	if len(managed) != 1 {
		t.Fatalf("managed = %v, want 1 entry", managed)
	}
}

func TestCandidate(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		ok     bool
		reason rejectReason
	}{
		"https://is.wepogp.gay/x?payload=abc": {ok: true},
		"https://host.example/sub":            {ok: true},
		"https://t.me/chan":                   {reason: rejectNoiseHost},
		"https://cdn4.telesco.pe/file/x.jpg":  {reason: rejectNoiseHost},
		// Literal private ip and non-https: config.Load would reject the source.
		"https://192.168.1.1/sub": {reason: rejectInvalidURL},
		"http://host.example/sub": {reason: rejectInvalidURL},
		// 127.0.0.1 in the encodings netip.ParseAddr does not answer for and
		// getaddrinfo does. candidate is the ONLY gate in front of a
		// channel-supplied URL — the crawler's client has no dial-time guard —
		// so a host that reads as a name here is fetched.
		"https://2130706433/sub": {reason: rejectInvalidURL},
		"https://0x7f000001/sub": {reason: rejectInvalidURL},
		"https://127.1/sub":      {reason: rejectInvalidURL},
		"https://0177.0.0.1/sub": {reason: rejectInvalidURL},
		// A numeric-looking hostname is not a numeric host: only a host
		// inet_aton would read WHOLE as a number is refused.
		"https://12345.example.com/sub": {ok: true},
	}
	for u, want := range cases {
		got, reason, err := candidate(u)
		if got != want.ok {
			t.Errorf("candidate(%q) = %v, want %v", u, got, want.ok)
		}
		if reason != want.reason {
			t.Errorf("candidate(%q) reason = %q, want %q", u, reason, want.reason)
		}
		if !want.ok && want.reason == rejectInvalidURL && err == nil {
			t.Errorf("candidate(%q) returned no error for an invalid url", u)
		}
	}
}

func TestManagedName(t *testing.T) {
	t.Parallel()

	re := regexp.MustCompile(`^[a-z0-9-]+$`)
	u := "https://is.wepogp.gay/x?payload=abc"
	n1 := managedName(u)
	n2 := managedName(u)
	if n1 != n2 {
		t.Fatalf("managedName not deterministic: %q vs %q", n1, n2)
	}
	if !re.MatchString(n1) {
		t.Fatalf("managedName %q must satisfy ^[a-z0-9-]+$", n1)
	}
	if managedName("https://other/x") == n1 {
		t.Fatalf("different URLs must produce different names")
	}
}

func TestChannelSlug(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"VPN_Channel":                            "vpn-channel",
		"mychannel":                              "mychannel",
		"__weird__":                              "weird",
		"a__b":                                   "a-b",
		"":                                       "",
		"???":                                    "",
		"verylongchannelnamefartoolongforalabel": "verylongchannelnamefarto", // capped at 24
	}
	for in, want := range cases {
		if got := channelSlug(in); got != want {
			t.Errorf("channelSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSourceNameAttribution covers the naming rules: new URLs get the
// channel-attributed form, attributed names are stable, legacy hash names
// upgrade once a channel is known, and a collision falls back to the hash form.
func TestSourceNameAttribution(t *testing.T) {
	t.Parallel()

	const u = "https://host.example/sub"
	nameRe := regexp.MustCompile(`^tg-vpn-channel-[0-9a-f]{6}$`)

	got := sourceName(u, "", "VPN_Channel", map[string]bool{})
	if !nameRe.MatchString(got) {
		t.Fatalf("new url name = %q, want tg-vpn-channel-<sha6>", got)
	}

	// Attributed names never change, even when rediscovered elsewhere.
	if kept := sourceName(u, got, "other_channel", map[string]bool{}); kept != got {
		t.Errorf("attributed name changed on rediscovery: %q -> %q", got, kept)
	}

	// Legacy hash names upgrade when the URL is seen in a channel...
	if up := sourceName(u, managedName(u), "VPN_Channel", map[string]bool{}); !nameRe.MatchString(up) {
		t.Errorf("legacy name not upgraded: %q", up)
	}
	// ...but stay when the origin is unknown this cycle.
	if same := sourceName(u, managedName(u), "", map[string]bool{}); same != managedName(u) {
		t.Errorf("legacy name changed without a channel: %q", same)
	}

	// A name collision falls back to the unique hash form.
	used := map[string]bool{got: true}
	if fb := sourceName(u, "", "VPN_Channel", used); fb != managedName(u) {
		t.Errorf("collision fallback = %q, want %q", fb, managedName(u))
	}

	// Every produced form satisfies the config source-name alphabet.
	re := regexp.MustCompile(`^[a-z0-9-]+$`)
	for _, n := range []string{got, managedName(u)} {
		if !re.MatchString(n) {
			t.Errorf("name %q violates ^[a-z0-9-]+$", n)
		}
	}
}

// TestMergeUpgradesLegacyName proves the end-to-end rename: a legacy-named
// managed source rediscovered in a channel this cycle is rewritten under its
// attributed name, keeping exactly one entry for the URL.
func TestMergeUpgradesLegacyName(t *testing.T) {
	t.Parallel()

	const u = "https://host.example/sub"
	c := &Crawler{opts: Options{Prune: true}, logger: zerolog.Nop()}
	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: managedName(u), URL: u}}

	live := map[string]string{u: "VPN_Channel"}
	next, managed, _ := c.mergeManaged(pf, live, recheckResult{managedURL: map[string]bool{u: true}}, true, nil)
	if len(next) != 1 || len(managed) != 1 {
		t.Fatalf("next = %v managed = %v, want exactly one entry", next, managed)
	}
	if want := regexp.MustCompile(`^tg-vpn-channel-[0-9a-f]{6}$`); !want.MatchString(managed[0].Name) {
		t.Errorf("name = %q, want attributed form", managed[0].Name)
	}
}

func TestPageCursor(t *testing.T) {
	t.Parallel()

	page := `data-post="chan/3650" ... data-post="chan/3631" ... data-post="chan/3648"`
	if got := pageCursor(page); got != "3631" {
		t.Fatalf("pageCursor = %q, want 3631", got)
	}
	if got := pageCursor("no posts here"); got != "" {
		t.Fatalf("pageCursor(empty) = %q, want empty", got)
	}
}

func TestSameSources(t *testing.T) {
	t.Parallel()

	a := []source{{Name: "x", URL: "u1"}, {Name: "y", URL: "u2"}}
	reordered := []source{{Name: "y", URL: "u2"}, {Name: "x", URL: "u1"}}
	added := []source{{Name: "x", URL: "u1"}, {Name: "y", URL: "u2"}, {Name: "z", URL: "u3"}}

	if !sameSources(a, reordered) {
		t.Error("reordered sets should be equal")
	}
	if sameSources(a, added) {
		t.Error("different-length sets must differ")
	}

	// Sources differing only in Body must be detected as different.
	bodyA := []source{{Name: "tg-inline", Body: "AAAA"}}
	bodyB := []source{{Name: "tg-inline", Body: "BBBB"}}
	if sameSources(bodyA, bodyB) {
		t.Error("sources differing only in Body must differ")
	}
}

func TestPrivateRoundTripPreservesUnmanaged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "private.yaml")
	var pf privateFile
	pf.Subscriptions.Sources = []source{
		{Name: "my-private", URL: "https://example.com/sub"},
		{Name: "tg-abc123", URL: "https://is.wepogp.gay/x?payload=abc"},
	}
	if err := writePrivate(path, pf); err != nil {
		t.Fatalf("writePrivate: %v", err)
	}
	got, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if !sameSources(pf.Subscriptions.Sources, got.Subscriptions.Sources) {
		t.Fatalf("roundtrip mismatch: %+v", got.Subscriptions.Sources)
	}
}

func TestLoadPrivateMissingFile(t *testing.T) {
	t.Parallel()

	got, err := loadPrivate(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(got.Subscriptions.Sources) != 0 {
		t.Fatalf("missing file should yield no sources, got %+v", got.Subscriptions.Sources)
	}
}

func TestExtractChannels(t *testing.T) {
	t.Parallel()

	pages := []string{
		`Forwarded from <a href="https://t.me/d_code/26804">Код Дурова</a>` +
			`text <a href="https://t.me/rap_ex">@rap_ex</a>` +
			`bot <a href="https://t.me/govpn?start=evolution">GoVPN</a>` +
			`self <a href="https://t.me/o00000000i/3631">x</a>` +
			`canon <a href="https://t.me/s/o00000000i">s</a>` +
			`share <a href="https://t.me/share/url?url=x">s</a>` +
			`dup <a href="https://t.me/rap_ex/12">again</a>` +
			`lookalike <a href="https://shortcut.me/abcdef">not telegram</a>` +
			`bare lookalike shortcut.me/ghijkl end`,
	}
	got := extractChannels(pages, "o00000000i")

	want := map[string]bool{"d_code": true, "rap_ex": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want channels %v", got, keysOf(want))
	}
	for _, ch := range got {
		if !want[ch] {
			t.Errorf("unexpected channel %q (bot/self/reserved should be excluded)", ch)
		}
	}
}

func TestNormalizeSlug(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"o00000000i":            "o00000000i",
		"@rap_ex":               "rap_ex",
		"https://t.me/rap_ex":   "rap_ex",
		"https://t.me/s/chan01": "chan01",
		"T.me/Foo/123":          "foo",
		"  spaced  ":            "spaced",
	}
	for in, want := range cases {
		if got := normalizeSlug(in); got != want {
			t.Errorf("normalizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStateRecordSaveLoadPrune(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".crawler-state.json")
	now := time.Now()

	st := loadState(path, zerolog.Nop()) // missing file → empty
	if len(st.Productive) != 0 {
		t.Fatalf("missing state should be empty, got %+v", st.Productive)
	}
	st.record("rap_ex", now)
	st.record("o00000000i", now.Add(-1000*time.Hour)) // stale
	if err := saveState(path, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	got := loadState(path, zerolog.Nop())
	if len(got.Productive) != 2 {
		t.Fatalf("roundtrip: got %d entries, want 2", len(got.Productive))
	}

	got.prune(now.Add(-720 * time.Hour)) // 30d cutoff
	if _, ok := got.Productive["rap_ex"]; !ok {
		t.Error("fresh channel must survive prune")
	}
	if _, ok := got.Productive["o00000000i"]; ok {
		t.Error("stale channel must be pruned")
	}
	if seeds := got.seeds(); len(seeds) != 1 || seeds[0] != "rap_ex" {
		t.Errorf("seeds after prune = %v, want [rap_ex]", seeds)
	}
}

func TestStateEmptyPathDisabled(t *testing.T) {
	t.Parallel()

	st := loadState("", zerolog.Nop())
	st.record("x", time.Now())
	if err := saveState("", st); err != nil {
		t.Fatalf("saveState with empty path must be a no-op, got %v", err)
	}
}

func TestLoadChannels(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "channels.yaml")
	content := "channels:\n  - o00000000i\n  - \"@rap_ex\"\n  - https://t.me/remiuc\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadChannels(path, zerolog.Nop()).Channels
	want := []string{"o00000000i", "@rap_ex", "https://t.me/remiuc"}
	if len(got) != len(want) {
		t.Fatalf("loadChannels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("channel[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if c := loadChannels(filepath.Join(dir, "nope.yaml"), zerolog.Nop()).Channels; c != nil {
		t.Errorf("missing file should yield nil, got %v", c)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("channels: [not: valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := loadChannels(bad, zerolog.Nop()).Channels; c != nil {
		t.Errorf("malformed file should yield nil, got %v", c)
	}
}

func TestNextDaily(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{base.Add(3 * time.Hour), base.Add(4 * time.Hour)},                                            // 03:00 -> today 04:00
		{base.Add(5 * time.Hour), base.Add(28 * time.Hour)},                                           // 05:00 -> tomorrow 04:00
		{base.Add(4 * time.Hour), base.Add(28 * time.Hour)},                                           // exactly 04:00 -> tomorrow (strictly after)
		{time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 4, 0, 0, 0, time.UTC)}, // year rollover
	}
	for _, c := range cases {
		if got := nextDaily(c.now, 4, 0); !got.Equal(c.want) {
			t.Errorf("nextDaily(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestExtractInlineNodes(t *testing.T) {
	t.Parallel()

	page := `<a href="https://sub.example/list">https://sub.example/list</a>` +
		`<pre>vless://uuid@1.2.3.4:443?security=tls#fast</pre>` +
		` vmess://eyJhZGQiOiIxLjEuMS4xIn0=, ` +
		`<code>trojan://pass@host.example:8443#t</code>` +
		`text ss://YWVzOnBhc3M@2.2.2.2:8388#s. ` +
		`ssr://c3NyYmFzZTY0 tuic://uuid@h6.example:443#tu ` +
		`hysteria://p@h7.example:443 hysteria2://p@h3.example:443 ` +
		`hy2://p@h4.example:443 anytls://p@h5.example:443 ` +
		`mierus://u:pw@h8.example?port=2999&protocol=TCP#mi ` +
		`escaped vless://u@h.example:443?x=1&amp;y=2#e ` +
		`just prose with a classy word and no proxies here ` +
		// A portful http/socks URI parses as a Node, so only inlineRe keeps a
		// client-setup snippet out of the harvest; both must stay uncaptured.
		`set your client to socks5://127.0.0.1:1080 or read https://example.com:8443/docs ` +
		// scheme-substring tokens must NOT be captured (boundary guard).
		`pass://foo access://bar class://baz`

	got := extractInlineNodes(html.UnescapeString(page))
	want := map[string]bool{
		"vless://uuid@1.2.3.4:443?security=tls#fast":         true,
		"vmess://eyJhZGQiOiIxLjEuMS4xIn0=":                   true, // trailing comma trimmed
		"trojan://pass@host.example:8443#t":                  true,
		"ss://YWVzOnBhc3M@2.2.2.2:8388#s":                    true, // trailing period trimmed
		"ssr://c3NyYmFzZTY0":                                 true,
		"tuic://uuid@h6.example:443#tu":                      true,
		"hysteria://p@h7.example:443":                        true,
		"hysteria2://p@h3.example:443":                       true,
		"hy2://p@h4.example:443":                             true,
		"anytls://p@h5.example:443":                          true,
		"mierus://u:pw@h8.example?port=2999&protocol=TCP#mi": true,
		"vless://u@h.example:443?x=1&y=2#e":                  true, // &amp; unescaped
	}
	if len(got) != len(want) {
		t.Fatalf("got %d inline nodes %v, want %d", len(got), got, len(want))
	}
	for _, u := range got {
		if !want[u] {
			t.Errorf("unexpected inline node %q", u)
		}
	}
}

// pageFetcher is a network-free fetchClient returning canned HTML per URL.
type pageFetcher struct{ pages map[string]string }

func (f pageFetcher) page(_ context.Context, u string) (string, error) {
	return f.pages[u], nil
}

// TestRunOnceHarvestsInlineNodes drives a full cycle against a stub fetcher: the
// single scraped page carries four inline URIs, two of which collide on
// server:port. With InlineMax=2 the crawler writes a tg-inline source whose
// base64 Body holds the first two distinct nodes (dedup first-wins, then cap).
func TestRunOnceHarvestsInlineNodes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	page := `<pre>vless://a@1.1.1.1:443#n1</pre>` +
		` vless://b@1.1.1.1:443#dup ` + // same server:port as n1 -> deduped
		`<code>vless://c@2.2.2.2:443#n2</code>` +
		` vless://d@3.3.3.3:443#n3 ` // dropped by InlineMax=2 cap

	c := &Crawler{
		opts: Options{
			Channels:      []string{"chan"},
			PrivatePath:   priv,
			Pages:         1,
			MaxDepth:      0,
			InlineEnabled: true,
			InlineMax:     2,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL) (classify.Result, error) {
			return classify.Result{}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	var pf privateFile
	b, err := os.ReadFile(priv)
	if err != nil {
		t.Fatalf("read private.yaml: %v", err)
	}
	if unmarshalErr := yaml.Unmarshal(b, &pf); unmarshalErr != nil {
		t.Fatalf("unmarshal private.yaml: %v", unmarshalErr)
	}

	var inline *source
	for i := range pf.Subscriptions.Sources {
		if pf.Subscriptions.Sources[i].Name == "tg-inline" {
			inline = &pf.Subscriptions.Sources[i]
		}
	}
	if inline == nil {
		t.Fatalf("no tg-inline source written: %+v", pf.Subscriptions.Sources)
	}
	if inline.URL != "" {
		t.Errorf("tg-inline source must have empty URL, got %q", inline.URL)
	}
	decoded, err := base64.StdEncoding.DecodeString(inline.Body)
	if err != nil {
		t.Fatalf("tg-inline Body is not valid base64: %v", err)
	}
	want := "vless://a@1.1.1.1:443#n1\nvless://c@2.2.2.2:443#n2"
	if string(decoded) != want {
		t.Fatalf("tg-inline Body = %q, want %q", decoded, want)
	}
}

// TestRunOnceHarvestsInlineFromNewestPageAndLinksFromEveryPage pins both halves
// of the harvest asymmetry across one channel's two pages: the older page's
// inline node is dropped because a server:port decays with the message carrying
// it, while that same page's subscription link still becomes a managed source.
func TestRunOnceHarvestsInlineFromNewestPageAndLinksFromEveryPage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	const (
		newest = "vless://a@1.1.1.1:443#newest"
		older  = "vless://b@2.2.2.2:443#older"
		subURL = "https://older.example/sub"
	)
	// data-post is what pageCursor reads, so page one hands scrapeChannel the
	// ?before= key of the second, older page.
	page1 := `<div data-post="chan/100"></div><pre>` + newest + `</pre>`
	page2 := `<pre>` + older + `</pre><a href="` + subURL + `">sub</a>`

	c := &Crawler{
		opts: Options{
			Channels:      []string{"chan"},
			PrivatePath:   priv,
			Pages:         2,
			MaxDepth:      0,
			InlineEnabled: true,
			InlineMax:     5, // above both nodes: the cap must not be what drops one
		},
		client: pageFetcher{pages: map[string]string{
			"https://t.me/s/chan":            page1,
			"https://t.me/s/chan?before=100": page2,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			if string(u) == subURL {
				return classify.Result{Nodes: 1}, nil
			}
			return classify.Result{}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	var pf privateFile
	b, err := os.ReadFile(priv)
	if err != nil {
		t.Fatalf("read private.yaml: %v", err)
	}
	if unmarshalErr := yaml.Unmarshal(b, &pf); unmarshalErr != nil {
		t.Fatalf("unmarshal private.yaml: %v", unmarshalErr)
	}

	var inline, managed *source
	for i := range pf.Subscriptions.Sources {
		s := &pf.Subscriptions.Sources[i]
		switch {
		case s.Name == managedPrefix+"inline":
			inline = s
		case strings.HasPrefix(s.Name, managedPrefix):
			if managed != nil {
				t.Fatalf("want exactly one managed url source, got %+v", pf.Subscriptions.Sources)
			}
			managed = s
		}
	}
	if inline == nil {
		t.Fatalf("no tg-inline source written: %+v", pf.Subscriptions.Sources)
	}
	decoded, err := base64.StdEncoding.DecodeString(inline.Body)
	if err != nil {
		t.Fatalf("tg-inline Body is not valid base64: %v", err)
	}
	body := string(decoded)
	if !strings.Contains(body, newest) {
		t.Errorf("tg-inline Body = %q, want it to carry the newest page's node %q", body, newest)
	}
	if strings.Contains(body, older) {
		t.Errorf("tg-inline Body = %q, must not carry the older page's node %q", body, older)
	}

	if managed == nil {
		t.Fatalf("the older page's link %q reached no managed source: %+v", subURL, pf.Subscriptions.Sources)
	}
	if managed.URL != subURL {
		t.Errorf("managed source URL = %q, want the older page's link %q", managed.URL, subURL)
	}
	if nameRe := regexp.MustCompile(`^tg-chan-[0-9a-f]{6}$`); !nameRe.MatchString(managed.Name) {
		t.Errorf("managed source name = %q, want the tg-<slug>-<sha6> form sourceName produces", managed.Name)
	}
}

// hasInlineSource reports whether a tg-inline source exists in private.yaml.
func hasInlineSource(t *testing.T, priv string) bool {
	t.Helper()
	b, err := os.ReadFile(priv)
	if err != nil {
		t.Fatalf("read private.yaml: %v", err)
	}
	var pf privateFile
	if unmarshalErr := yaml.Unmarshal(b, &pf); unmarshalErr != nil {
		t.Fatalf("unmarshal private.yaml: %v", unmarshalErr)
	}
	for i := range pf.Subscriptions.Sources {
		if pf.Subscriptions.Sources[i].Name == "tg-inline" {
			return true
		}
	}
	return false
}

// TestRunOnceInlineDisabled: with InlineEnabled=false the crawler must not write
// a tg-inline source even though the scraped page carries inline URIs.
func TestRunOnceInlineDisabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	page := `<pre>vless://a@1.1.1.1:443#n1</pre> vless://c@2.2.2.2:443#n2`
	c := &Crawler{
		opts: Options{
			Channels:      []string{"chan"},
			PrivatePath:   priv,
			Pages:         1,
			MaxDepth:      0,
			InlineEnabled: false,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL) (classify.Result, error) {
			return classify.Result{}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	if hasInlineSource(t, priv) {
		t.Fatal("tg-inline source written despite InlineEnabled=false")
	}
}

// TestRunOnceNoInlineNodes: inline harvesting is on but the pages carry zero
// proxy URIs, so buildInlineSource returns ok=false and no tg-inline is written.
func TestRunOnceNoInlineNodes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	page := `<pre>just prose, a classy pass://foo link, and no proxies here</pre>`
	c := &Crawler{
		opts: Options{
			Channels:      []string{"chan"},
			PrivatePath:   priv,
			Pages:         1,
			MaxDepth:      0,
			InlineEnabled: true,
			InlineMax:     2,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL) (classify.Result, error) {
			return classify.Result{}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	if hasInlineSource(t, priv) {
		t.Fatal("tg-inline source written despite no inline nodes")
	}
}

// TestRunOnceRefusesBulkPrune: a cycle that would delete most of the managed
// corpus at once must write nothing and log an error. private.yaml has no backup,
// so one bad cycle may never wipe it — a later cycle has to confirm the deletion.
func TestRunOnceRefusesBulkPrune(t *testing.T) {
	t.Parallel()

	const total = 20
	urls := make([]string, 0, total)
	var pf privateFile
	for i := range total {
		u := fmt.Sprintf("https://p%02d.example/sub", i)
		urls = append(urls, u)
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u})
	}
	priv := filepath.Join(t.TempDir(), "private.yaml")
	if err := writePrivate(priv, pf); err != nil {
		t.Fatalf("seed private.yaml: %v", err)
	}

	var logBuf bytes.Buffer
	c := &Crawler{
		opts:   Options{PrivatePath: priv, Prune: true},
		client: pageFetcher{},
		// urls[0] still answers, so this is not a learned-nothing cycle: the
		// bulk-prune floor alone has to stop the write.
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			if string(u) == urls[0] {
				return classify.Result{Nodes: 1}, nil
			}
			return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
		},
		logger: zerolog.New(&logBuf),
	}

	c.RunOnce(context.Background())

	got, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if len(got.Subscriptions.Sources) != total {
		t.Fatalf("private.yaml holds %d sources, want the original %d untouched", len(got.Subscriptions.Sources), total)
	}
	if !strings.Contains(logBuf.String(), "bulk prune floor tripped") {
		t.Errorf("prune floor must log at error level, got %q", logBuf.String())
	}
}

// TestRunOnceKeepsNodeless200PrunesGone is the cycle-level acceptance contract
// for CRAWL-V3: a source answering 200 with no proxy-scheme node in the body — a
// captive portal, an interstitial, a panel login page — stays in private.yaml,
// while a 404 is still removed. Before the fix both left the URL out of live and
// out of unknown, which mergeManaged reads identically: delete.
func TestRunOnceKeepsNodeless200PrunesGone(t *testing.T) {
	t.Parallel()

	const (
		urlLive     = "https://live.example/sub"
		urlPortal   = "https://portal.example/sub"
		urlNotFound = "https://notfound.example/sub"
	)
	var pf privateFile
	for _, u := range []string{urlLive, urlPortal, urlNotFound} {
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u})
	}
	priv := filepath.Join(t.TempDir(), "private.yaml")
	if err := writePrivate(priv, pf); err != nil {
		t.Fatalf("seed private.yaml: %v", err)
	}

	c := &Crawler{
		opts:   Options{PrivatePath: priv, Prune: true},
		client: pageFetcher{},
		// urlLive revives, so the cycle is not a learned-nothing one and prune
		// stays enabled; three sources are far under the bulk-prune floor.
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			switch string(u) {
			case urlLive:
				return classify.Result{Nodes: 3}, nil
			case urlNotFound:
				return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
			default:
				return classify.Result{}, nil
			}
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	got, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	kept := map[string]bool{}
	for _, s := range got.Subscriptions.Sources {
		kept[s.URL] = true
	}
	if !kept[urlLive] {
		t.Error("a source still serving nodes must be kept")
	}
	if !kept[urlPortal] {
		t.Errorf("a 200 with zero nodes must keep the source, got %+v", got.Subscriptions.Sources)
	}
	if kept[urlNotFound] {
		t.Error("404 is definitive: the source must be pruned")
	}
}

// TestRunOnceStaleRefusalCannotAuthorizeADifferentPrune is the cycle-level
// acceptance contract for CRAWL-V2. The state file carries a bulk-prune refusal
// recorded seven hours ago for some other proposal (a bare timestamp is exactly
// what the pre-fix code wrote). This cycle condemns 15 of 40 sources — a set that
// record never described — and the old code, seeing only that the timestamp was
// older than bulkPruneConfirmAfter, honoured it and deleted all 15 on the spot.
func TestRunOnceStaleRefusalCannotAuthorizeADifferentPrune(t *testing.T) {
	t.Parallel()

	const (
		total  = 40
		doomed = 15
	)
	urls := make([]string, 0, total)
	var pf privateFile
	for i := range total {
		u := fmt.Sprintf("https://s%02d.example/sub", i)
		urls = append(urls, u)
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u})
	}
	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := writePrivate(priv, pf); err != nil {
		t.Fatalf("seed private.yaml: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	if err := saveState(statePath, state{BulkPruneAt: time.Now().Add(-7 * time.Hour)}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	gone := map[string]bool{}
	for _, u := range urls[:doomed] {
		gone[u] = true
	}
	var logBuf bytes.Buffer
	c := &Crawler{
		opts:   Options{PrivatePath: priv, StatePath: statePath, Prune: true},
		client: pageFetcher{},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			if gone[string(u)] {
				return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
			}
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}

	c.RunOnce(context.Background())

	got, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if len(got.Subscriptions.Sources) != total {
		t.Fatalf("private.yaml holds %d sources, want all %d: a refusal recorded for another proposal authorized this one",
			len(got.Subscriptions.Sources), total)
	}
	if !strings.Contains(logBuf.String(), "bulk prune floor tripped") {
		t.Errorf("the unmatched proposal must be refused and logged, got %q", logBuf.String())
	}
	// The refusal must now describe THIS proposal, so the confirming cycle can
	// be checked against it.
	after := loadState(statePath, zerolog.Nop())
	if len(after.BulkPruneURLs) != doomed {
		t.Errorf("recorded proposal = %v, want the %d condemned URLs", after.BulkPruneURLs, doomed)
	}
}

// TestRunOnceNoChangeWithdrawsBulkPruneRecord: a cycle whose merge reproduces
// private.yaml exactly proposes no deletion, which withdraws any pending
// bulk-prune refusal. Without this the record survived every quiet cycle —
// clearBulkPrune was reachable only from allowShrink, which five earlier returns
// in RunOnce skip — and stood ready to authorize whatever the next fault proposed.
func TestRunOnceNoChangeWithdrawsBulkPruneRecord(t *testing.T) {
	t.Parallel()

	const total = 6
	var pf privateFile
	for i := range total {
		u := fmt.Sprintf("https://q%02d.example/sub", i)
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u})
	}
	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := writePrivate(priv, pf); err != nil {
		t.Fatalf("seed private.yaml: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	pending := state{BulkPruneAt: time.Now().Add(-time.Hour), BulkPruneURLs: condemnedSet(20)}
	if err := saveState(statePath, pending); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	var logBuf bytes.Buffer
	c := &Crawler{
		opts:   Options{PrivatePath: priv, StatePath: statePath, Prune: true},
		client: pageFetcher{},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}

	c.RunOnce(context.Background())

	if !strings.Contains(logBuf.String(), "no change") {
		t.Fatalf("expected a no-change cycle, got %q", logBuf.String())
	}
	after := loadState(statePath, zerolog.Nop())
	if !after.BulkPruneAt.IsZero() || after.BulkPruneURLs != nil {
		t.Errorf("pending proposal survived a cycle that proposed nothing: at=%v urls=%v",
			after.BulkPruneAt, after.BulkPruneURLs)
	}
}

// condemnedSet builds the sorted set of n distinct managed URLs a cycle proposes
// to delete. The %03d keeps lexical and numeric order identical, which
// sameProposal's merge walk requires.
func condemnedSet(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("https://c%03d.example/sub", i))
	}
	return out
}

// TestAllowShrinkCountsDeletionsNotNetChange: the floor used to compare the
// managed count before and after, but "after" includes every source the same
// cycle discovered, so each new find bought back one deletion. A correlated
// death event that also discovered a few sources slipped straight under the
// documented 30% ceiling and deleted far more than it.
func TestAllowShrinkCountsDeletionsNotNetChange(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                   string
		before, deleted, after int
		want                   bool
	}{
		// 15 of 40 condemned while 10 are discovered: net shrink is only 5, so
		// the old net-based floor waved it through and deleted 37% of the
		// corpus. Keyed on deletions it is 15/40 = 37% and must be refused.
		{"discoveries mask a mass deletion", 40, 15, 35, false},
		// The same deletions with no discoveries: the old code caught this one,
		// so the case guards against over-correcting in the other direction.
		{"mass deletion, nothing found", 40, 15, 25, false},
		// Ordinary churn on a large corpus: over the absolute floor, well under
		// the ratio.
		{"routine churn", 150, 20, 130, true},
		// Small corpus: 4 of 12 is 33%, over the ratio but under the absolute
		// floor, so it proceeds. Both conditions must hold to refuse.
		{"small corpus below the absolute floor", 12, 4, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := &Crawler{opts: Options{}, logger: zerolog.Nop()}
			var st state
			if got := c.allowShrink(&st, tc.before, condemnedSet(tc.deleted), tc.after); got != tc.want {
				t.Fatalf("allowShrink(before=%d, deleted=%d, after=%d) = %v, want %v",
					tc.before, tc.deleted, tc.after, got, tc.want)
			}
		})
	}
}

// TestRunOnceDarkCycleWritesNothing: a cycle that discovers nothing and revives
// nothing has learned nothing about its sources — a crawler egress fault (tunnel
// down, DNS interception, a proxy answering every request itself) looks exactly
// like this — so it must prune nothing, however definitive the answers look. The
// set here is small enough that the bulk-prune floor would not have caught it.
func TestRunOnceDarkCycleWritesNothing(t *testing.T) {
	t.Parallel()

	const total = 5
	var pf privateFile
	for i := range total {
		u := fmt.Sprintf("https://d%02d.example/sub", i)
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u})
	}
	priv := filepath.Join(t.TempDir(), "private.yaml")
	if err := writePrivate(priv, pf); err != nil {
		t.Fatalf("seed private.yaml: %v", err)
	}

	var logBuf bytes.Buffer
	c := &Crawler{
		opts:   Options{PrivatePath: priv, Prune: true},
		client: pageFetcher{},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL) (classify.Result, error) {
			return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
		},
		logger: zerolog.New(&logBuf),
	}

	c.RunOnce(context.Background())

	got, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if len(got.Subscriptions.Sources) != total {
		t.Fatalf("private.yaml holds %d sources, want all %d retained", len(got.Subscriptions.Sources), total)
	}
	if !strings.Contains(logBuf.String(), "pruning nothing") {
		t.Errorf("a learned-nothing cycle must log at error level, got %q", logBuf.String())
	}
}

// TestConfirmBulkPruneRequiresLaterCycle: the first proposal to delete a large
// slice of the corpus is refused and remembered, only a cycle at least
// bulkPruneConfirmAfter later carries it out, and doing so consumes the record so
// the next mass deletion earns its own confirmation.
func TestConfirmBulkPruneRequiresLaterCycle(t *testing.T) {
	t.Parallel()

	now := time.Now()
	proposal := condemnedSet(20)
	var st state
	if allow, changed := st.confirmBulkPrune(now, proposal); allow || !changed {
		t.Fatalf("first proposal: allow=%v changed=%v, want false and a record to persist", allow, changed)
	}
	if st.BulkPruneAt.IsZero() || !slices.Equal(st.BulkPruneURLs, proposal) {
		t.Fatalf("a refusal must remember when and what: at=%v urls=%v", st.BulkPruneAt, st.BulkPruneURLs)
	}
	if allow, changed := st.confirmBulkPrune(now.Add(bulkPruneConfirmAfter-time.Minute), proposal); allow || changed {
		t.Errorf("inside the window the same proposal must be refused and leave the record alone, got allow=%v changed=%v", allow, changed)
	}
	if !st.BulkPruneAt.Equal(now) {
		t.Errorf("BulkPruneAt = %v, want the original %v: re-arming every cycle pushes the deadline out forever", st.BulkPruneAt, now)
	}
	if allow, _ := st.confirmBulkPrune(now.Add(bulkPruneConfirmAfter), proposal); !allow {
		t.Error("the same proposal past the confirmation window must be honoured")
	}
	if !st.BulkPruneAt.IsZero() || st.BulkPruneURLs != nil {
		t.Error("an honoured proposal must be consumed, not left standing")
	}
	st.BulkPruneAt, st.BulkPruneURLs = now, proposal
	if !st.clearBulkPrune() {
		t.Error("clearBulkPrune must report that it forgot a pending proposal")
	}
	if !st.BulkPruneAt.IsZero() || st.BulkPruneURLs != nil {
		t.Error("clearBulkPrune must forget the pending proposal")
	}
	if st.clearBulkPrune() {
		t.Error("clearBulkPrune must report no change when nothing is pending")
	}
}

// TestConfirmBulkPruneRefusesADifferentProposal pins CRAWL-V2 at the record
// level: the stored refusal is consent to nothing, so a later cycle condemning a
// different set must start its own waiting period instead of inheriting this one.
// With a bare timestamp the second proposal below was honoured on the spot and
// deleted a set no cycle had ever refused.
func TestConfirmBulkPruneRefusesADifferentProposal(t *testing.T) {
	t.Parallel()

	now := time.Now()
	first := condemnedSet(20)
	// A disjoint set of the same size, so neither direction of the overlap test
	// can mistake it for the recorded proposal.
	other := make([]string, 0, len(first))
	for i := range len(first) {
		other = append(other, fmt.Sprintf("https://z%03d.example/sub", i))
	}

	var st state
	if allow, _ := st.confirmBulkPrune(now, first); allow {
		t.Fatal("the first proposal must be refused")
	}

	later := now.Add(bulkPruneConfirmAfter)
	if allow, _ := st.confirmBulkPrune(later, other); allow {
		t.Error("a refusal recorded for one set must not authorize deleting a different one")
	}
	if !st.BulkPruneAt.Equal(later) || !slices.Equal(st.BulkPruneURLs, other) {
		t.Errorf("the different proposal must replace the record with its own: at=%v urls=%v", st.BulkPruneAt, st.BulkPruneURLs)
	}
	// The superseded set has no standing either: it is now the odd one out.
	if allow, _ := st.confirmBulkPrune(later.Add(bulkPruneConfirmAfter), first); allow {
		t.Error("a superseded proposal must not be confirmable")
	}
}

// TestConfirmBulkPruneToleratesProposalDrift: the fingerprint must not demand an
// identical set, or a real mass expiry would never converge — one source
// recovering or one more dying between the two cycles would restart the wait
// forever. Within bulkPruneOverlap the proposal is the same event.
func TestConfirmBulkPruneToleratesProposalDrift(t *testing.T) {
	t.Parallel()

	now := time.Now()
	first := condemnedSet(20)
	// Two of the twenty recovered and one new death joined, so 18 of the 20
	// recorded URLs are re-proposed: 90%, inside bulkPruneOverlap.
	drifted := append(slices.Clone(first[2:]), "https://d999.example/sub")

	var st state
	if allow, _ := st.confirmBulkPrune(now, first); allow {
		t.Fatal("the first proposal must be refused")
	}
	if allow, _ := st.confirmBulkPrune(now.Add(bulkPruneConfirmAfter), drifted); !allow {
		t.Errorf("a proposal within %d%% of the refused set must confirm it, not restart the wait", bulkPruneOverlap)
	}
}

// TestConfirmBulkPruneExpiresAStaleRecord: consent gathered a day ago is consent
// on day-old evidence. Past bulkPruneRecordTTL the same proposal counts as brand
// new, so it is refused and has to wait again.
func TestConfirmBulkPruneExpiresAStaleRecord(t *testing.T) {
	t.Parallel()

	now := time.Now()
	proposal := condemnedSet(20)
	var st state
	if allow, _ := st.confirmBulkPrune(now, proposal); allow {
		t.Fatal("the first proposal must be refused")
	}

	stale := now.Add(bulkPruneRecordTTL + time.Minute)
	if allow, _ := st.confirmBulkPrune(stale, proposal); allow {
		t.Error("a record older than bulkPruneRecordTTL must not authorize a deletion")
	}
	if !st.BulkPruneAt.Equal(stale) {
		t.Errorf("BulkPruneAt = %v, want the re-armed %v", st.BulkPruneAt, stale)
	}
	if allow, _ := st.confirmBulkPrune(stale.Add(bulkPruneConfirmAfter), proposal); !allow {
		t.Error("the re-armed record must confirm once its own window elapses")
	}
}

// TestStatePruneCapsProductive: the productive memory is the seed set, and seeds
// drive the whole cycle's cost, so it must stay bounded — the TTL alone lets one
// live sub a month keep a channel forever.
func TestStatePruneCapsProductive(t *testing.T) {
	t.Parallel()

	now := time.Now()
	var st state
	const extra = 50
	for i := range maxProductive + extra {
		st.record(fmt.Sprintf("chan%04d", i), now.Add(-time.Duration(i)*time.Minute))
	}

	st.prune(now.Add(-720 * time.Hour)) // nothing is stale; only the cap applies

	if len(st.Productive) != maxProductive {
		t.Fatalf("productive = %d entries, want the cap %d", len(st.Productive), maxProductive)
	}
	if _, ok := st.Productive["chan0000"]; !ok {
		t.Error("the most recently productive channel must survive the cap")
	}
	last := fmt.Sprintf("chan%04d", maxProductive+extra-1)
	if _, ok := st.Productive[last]; ok {
		t.Errorf("%s is the least recently productive channel; it must be dropped", last)
	}
}

// TestPagesForBudget: only operator-configured seeds get the full page budget.
// Remembered seeds and repost discoveries pay the shallower one — they accumulate
// across cycles, and paying full price for each is what made every cycle cost
// more than the last.
func TestPagesForBudget(t *testing.T) {
	t.Parallel()

	c := &Crawler{opts: Options{Pages: 6}}
	if got := c.pagesFor(scanNode{channel: "a", configured: true}); got != 6 {
		t.Errorf("configured seed budget = %d, want 6", got)
	}
	if got := c.pagesFor(scanNode{channel: "b"}); got != discoveredPages {
		t.Errorf("remembered seed budget = %d, want %d", got, discoveredPages)
	}
	small := &Crawler{opts: Options{Pages: 1}}
	if got := small.pagesFor(scanNode{channel: "b", depth: 2}); got != 1 {
		t.Errorf("budget = %d, want never more than Pages (1)", got)
	}
}

// TestWritePrivateRefusesUnloadableSource: the crawler must never author a file
// the service cannot load. One rejected source fails config.Load for the whole
// config, which is fatal at startup, and compose restarts the container forever.
func TestWritePrivateRefusesUnloadableSource(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "private.yaml")
	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: "tg-abc123", URL: "https://10.0.0.5/sub"}}

	if err := writePrivate(path, pf); err == nil {
		t.Fatal("writePrivate must refuse a source with a non-public literal-IP host")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a refused write must not create the file; stat err = %v", err)
	}
}

// TestLoadStateMalformedFileIsNotClobbered pins CRAWL-5. loadState used to
// swallow the unmarshal error and hand back empty state, which saveState then
// wrote over the real file at the end of the same cycle — destroying up to
// StateTTL of productive-channel memory silently and irreversibly.
func TestLoadStateMalformedFileIsNotClobbered(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".crawler-state.json")
	corrupt := []byte(`{"productive": {"rap_ex": {"first_seen":`) // truncated mid-write
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	st := loadState(path, zerolog.New(&logBuf))
	if len(st.Productive) != 0 {
		t.Fatalf("a malformed file must not yield half-decoded state, got %+v", st.Productive)
	}
	if !strings.Contains(logBuf.String(), "malformed") {
		t.Errorf("an unreadable state file must be logged, got %q", logBuf.String())
	}

	st.record("newchan", time.Now())
	if err := saveState(path, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, corrupt) {
		t.Fatalf("saveState overwrote a state file it could not read:\n got: %s\nwant: %s", got, corrupt)
	}
}

// TestScrapeChannelReportsLostCursor pins the detection half of CRAWL-6: a page
// carrying no ?before= cursor while the page budget still had room is the exact
// signature of the t.me markup changing, and it must be distinguishable from a
// channel that simply ran out of pages.
func TestScrapeChannelReportsLostCursor(t *testing.T) {
	t.Parallel()

	withCursor := `<div data-post="chan/3631"></div>`
	noCursor := `<div>nothing paginated here</div>`

	cases := []struct {
		name      string
		pages     map[string]string
		budget    int
		wantPages int
		wantLost  bool
	}{
		{
			name:      "cursor selector broke",
			pages:     map[string]string{"https://t.me/s/chan": noCursor},
			budget:    6,
			wantPages: 1,
			wantLost:  true,
		},
		{
			name:      "budget exhausted, not the cursor",
			pages:     map[string]string{"https://t.me/s/chan": withCursor},
			budget:    1,
			wantPages: 1,
			wantLost:  false,
		},
		{
			name: "channel genuinely ends after two pages",
			pages: map[string]string{
				"https://t.me/s/chan":             withCursor,
				"https://t.me/s/chan?before=3631": noCursor,
			},
			budget:    2,
			wantPages: 2,
			wantLost:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Crawler{client: pageFetcher{pages: tc.pages}, logger: zerolog.Nop()}
			pages, lost := c.scrapeChannel(context.Background(), "chan", tc.budget)
			if len(pages) != tc.wantPages {
				t.Errorf("pages = %d, want %d", len(pages), tc.wantPages)
			}
			if lost != tc.wantLost {
				t.Errorf("cursorLost = %v, want %v", lost, tc.wantLost)
			}
		})
	}
}

// TestReportCursorsWarnsOnlyOnFleetWideLoss pins the reporting half of CRAWL-6.
// One short channel is normal and must stay quiet; every cursor-relevant channel
// in the cycle losing its cursor is the selector breaking and must be loud.
func TestReportCursorsWarnsOnlyOnFleetWideLoss(t *testing.T) {
	t.Parallel()

	warned := func(cs cursorStats) bool {
		var logBuf bytes.Buffer
		c := &Crawler{logger: zerolog.New(&logBuf)}
		c.reportCursors(cs)
		return strings.Contains(logBuf.String(), "page cursor")
	}

	if !warned(cursorStats{paged: cursorAlarmMin, lost: cursorAlarmMin}) {
		t.Error("a cycle where no channel yielded a cursor must warn")
	}
	if warned(cursorStats{paged: cursorAlarmMin, lost: cursorAlarmMin - 1}) {
		t.Error("one channel still paginating means the selector works; stay quiet")
	}
	if warned(cursorStats{paged: cursorAlarmMin - 1, lost: cursorAlarmMin - 1}) {
		t.Error("too few channels to distinguish a markup break from short channels")
	}
	if warned(cursorStats{}) {
		t.Error("a cycle that scraped nothing must not warn")
	}
}

// TestRunOnceHonoursBlockedList pins CRAWL-7: deleting a harvested source by
// hand does not stick, because the next cycle rediscovers the URL in a channel
// and re-adds it. The channels.yaml blocked list is the only supported way to
// retire one for good.
func TestRunOnceHonoursBlockedList(t *testing.T) {
	t.Parallel()

	const (
		blockedURL = "https://abusive.example/sub"
		keptURL    = "https://fine.example/sub"
	)
	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	channels := filepath.Join(dir, "channels.yaml")
	if err := os.WriteFile(channels, []byte("channels:\n  - chan\nblocked:\n  - "+blockedURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	page := `<a href="` + blockedURL + `">a</a> <a href="` + keptURL + `">b</a>`
	c := &Crawler{
		opts: Options{
			PrivatePath:  priv,
			ChannelsPath: channels,
			Pages:        1,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	got, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	urls := map[string]bool{}
	for _, s := range got.Subscriptions.Sources {
		urls[s.URL] = true
	}
	if urls[blockedURL] {
		t.Errorf("a blocked URL was harvested anyway: %+v", got.Subscriptions.Sources)
	}
	if !urls[keptURL] {
		t.Errorf("the unblocked URL must still be harvested, got %+v", got.Subscriptions.Sources)
	}
}
