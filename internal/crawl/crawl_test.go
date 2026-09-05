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
	"strconv"
	"strings"
	"sync"
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

// TestExtractorsTakeAnUnescapedPage pins the contract that let the unescape move
// out of the scans and into harvestPages: page 0 feeds both of them, so a scan
// that unescapes for itself copies that page twice.
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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
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
	live, unknown, dead := c.classifyAll(context.Background(),
		[]string{"https://live.example/sub", "https://err.example/sub", "https://dead.example/sub", "https://gone.example/sub"}, nil, nil, "", nil, nil)
	// deadOut carries no order: workers append under the mutex as they finish,
	// and its only consumer writes a map.
	if !slices.Contains(dead, "https://dead.example/sub") ||
		!slices.Contains(dead, "https://gone.example/sub") || len(dead) != 2 {
		t.Errorf("dead = %v, want exactly the expired and the gone URL", dead)
	}

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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
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
	live, unknown, dead := c.classifyAll(context.Background(), []string{urlPortal, urlEmpty, urlExpired, urlGone}, nil, nil, "", nil, nil)
	if len(dead) != 2 {
		t.Errorf("dead = %v, want exactly the expired and gone URLs", dead)
	}

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
		classifyFn: func(context.Context, *http.Client, fetch.SubscriptionURL, string) (classify.Result, error) {
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
	live, unknown, _ := c.classifyAll(context.Background(), urls, nil, nil, "", nil, nil)
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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
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
		{Name: managedName(urlLive), URL: urlLive, Managed: true},
		{Name: managedName(urlDead), URL: urlDead, Managed: true},
		{Name: managedName(urlErr), URL: urlErr, Managed: true},
		{Name: managedName(urlGone), URL: urlGone, Managed: true},
		{Name: managedName(urlNodeless), URL: urlNodeless, Managed: true},
	}

	live := map[string]origin{}
	rr := c.recheckManaged(context.Background(), pf, live)
	next, managed, _, _ := c.mergeManaged(pf, live, rr, true, nil)

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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			status := code[string(u)]
			return classify.Result{}, &classify.StatusError{Code: status, Status: http.StatusText(status)}
		},
		logger: zerolog.Nop(),
	}
	live, unknown, _ := c.classifyAll(context.Background(), urls, nil, nil, "", nil, nil)

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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if code, bad := status[string(u)]; bad {
				return classify.Result{}, &classify.StatusError{Code: code, Status: http.StatusText(code)}
			}
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}
	var pf privateFile
	for _, u := range []string{urlLive, urlRateLimited, urlDeploying, urlBlocked, urlNotFound} {
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u, Managed: true})
	}

	live := map[string]origin{}
	rr := c.recheckManaged(context.Background(), pf, live)
	_, managed, _, _ := c.mergeManaged(pf, live, rr, true, nil)

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
// source that appears in the re-loaded private.yaml but was absent from the
// cycle-start snapshot was never checked this cycle and must be retained, even
// with pruning enabled.
func TestMergeRetainsMidCycleAdditions(t *testing.T) {
	t.Parallel()

	const urlNew = "https://midcycle.example/sub"
	c := &Crawler{opts: Options{Prune: true}, logger: zerolog.Nop()}

	// Re-loaded file contains a managed source unknown to the cycle snapshot.
	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: managedName(urlNew), URL: urlNew, Managed: true}}

	next, managed, _, _ := c.mergeManaged(pf, map[string]origin{}, recheckResult{}, true, nil)
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

// TestSourceNameAttribution covers the naming rules: a URL harvested from a known
// post is named for it, its later siblings queue behind it on ascending ordinals
// from 2, a channel with no post starts that walk at 1, an attributed name is
// never rewritten, the origin-less bare hash upgrades once an origin is known,
// and an unusable slug is the one thing that still reaches the bare hash.
func TestSourceNameAttribution(t *testing.T) {
	t.Parallel()

	const (
		u    = "https://host.example/sub"
		u2   = "https://host.example/other"
		u3   = "https://host.example/third"
		post = 3631
		attr = "vpn-channel-3631"
	)
	seen := origin{Slug: "VPN_Channel", Post: post}

	if got := sourceName(u, "", seen, map[string]bool{}); got != attr {
		t.Fatalf("new url name = %q, want %q", got, attr)
	}

	// A post is a container, not a URL: the later links in one post cannot have
	// the post's name, so they start at 2 — 1 would claim to be the first URL of
	// a post whose first URL already holds the bare stem.
	second := sourceName(u2, "", seen, map[string]bool{attr: true})
	if second != attr+"-2" {
		t.Fatalf("second url of one post = %q, want %q", second, attr+"-2")
	}
	third := sourceName(u3, "", seen, map[string]bool{attr: true, second: true})
	if third != attr+"-3" {
		t.Fatalf("third url of one post = %q, want %q", third, attr+"-3")
	}

	// The walk passes every taken sibling rather than stopping at the first
	// collision, so a post with a run of them still mints, and it mints past the
	// single-digit ordinals an earlier cycle or an operator may hold.
	taken := map[string]bool{attr: true}
	for n := 2; n <= 11; n++ {
		taken[fmt.Sprintf("%s-%d", attr, n)] = true
	}
	if got := sourceName(u2, "", seen, taken); got != attr+"-12" {
		t.Fatalf("name minted beside eleven taken siblings = %q, want %q", got, attr+"-12")
	}

	// Channel known, post not — the revival path, which sees no post at all. No
	// bare stem is ever offered there, so the walk starts at 1 instead.
	postless := origin{Slug: "VPN_Channel"}
	noPost := sourceName(u, "", postless, map[string]bool{})
	if noPost != "vpn-channel-1" {
		t.Fatalf("postless name = %q, want vpn-channel-1", noPost)
	}
	if got := sourceName(u2, "", postless, map[string]bool{noPost: true}); got != "vpn-channel-2" {
		t.Fatalf("second postless name = %q, want vpn-channel-2", got)
	}

	// No form is ever rewritten, whatever the URL is rediscovered under: a
	// rename relabels every published node and buys nothing.
	elsewhere := origin{Slug: "other_channel", Post: 77}
	for _, name := range []string{attr, second, noPost} {
		if kept := sourceName(u, name, elsewhere, map[string]bool{}); kept != name {
			t.Errorf("attributed name changed on rediscovery: %q -> %q", name, kept)
		}
	}

	// The bare hash is the one exception, naming no origin: it upgrades once an
	// origin is known...
	if up := sourceName(u, managedName(u), seen, map[string]bool{}); up != attr {
		t.Errorf("bare hash not upgraded: %q, want %q", up, attr)
	}
	// ...onto an ordinal when the stem is spoken for, the one thing the sha6
	// tail it replaces never depended on: the cycle's used set.
	if up := sourceName(u, managedName(u), seen, map[string]bool{attr: true}); up != attr+"-2" {
		t.Errorf("bare hash upgraded past a taken stem = %q, want %q", up, attr+"-2")
	}
	// ...and stays when nothing is known this cycle.
	if same := sourceName(u, managedName(u), origin{}, map[string]bool{}); same != managedName(u) {
		t.Errorf("bare hash changed without an origin: %q", same)
	}

	// A usable slug now always yields a free ordinal, so the bare hash is left
	// for the URL with nothing to be attributed to: a channel that survives none
	// of channelSlug's alphabet, post id or no post id.
	if fb := sourceName(u, "", origin{Slug: "???", Post: post}, map[string]bool{}); fb != managedName(u) {
		t.Errorf("unusable slug = %q, want the origin-less %q", fb, managedName(u))
	}

	// Every produced form satisfies the config source-name alphabet.
	re := regexp.MustCompile(`^[a-z0-9-]+$`)
	for _, n := range []string{attr, second, third, noPost, managedName(u)} {
		if !re.MatchString(n) {
			t.Errorf("name %q violates ^[a-z0-9-]+$", n)
		}
	}
}

// TestMergeUpgradesBareHashName proves the end-to-end upgrade: a bare-hash
// managed source rediscovered in a post this cycle is rewritten under the
// attributed name AND gains the feed with it, keeping one entry for the URL.
func TestMergeUpgradesBareHashName(t *testing.T) {
	t.Parallel()

	const u = "https://host.example/sub"
	c := &Crawler{opts: Options{Prune: true}, logger: zerolog.Nop()}
	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: managedName(u), URL: u, Managed: true}}

	live := map[string]origin{u: {Slug: "VPN_Channel", Post: 3631}}
	next, managed, _, _ := c.mergeManaged(pf, live, recheckResult{managedURL: map[string]bool{u: true}}, true, nil)
	if len(next) != 1 || len(managed) != 1 {
		t.Fatalf("next = %v managed = %v, want exactly one entry", next, managed)
	}
	if managed[0].Name != "vpn-channel-3631" {
		t.Errorf("name = %q, want the post-attributed form vpn-channel-3631", managed[0].Name)
	}
	if managed[0].Feed != "vpn-channel" {
		t.Errorf("feed = %q, want vpn-channel: the name and the feed are minted together", managed[0].Feed)
	}
}

// TestMergeReservesExistingManagedNames: <slug>-<postid> is per-POST, so two URLs
// of one post compete for one name. The incumbent's is returned verbatim by
// sourceName, so unless the merge reserves it a newcomer sorting ahead takes that
// exact string, and the duplicate stops private.yaml being written for good.
func TestMergeReservesExistingManagedNames(t *testing.T) {
	t.Parallel()

	const (
		urlNew       = "https://a.example/sub"
		urlIncumbent = "https://b.example/sub"
		attributed   = "chan-3631"
	)
	c := &Crawler{opts: Options{Prune: true}, logger: zerolog.Nop()}
	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: attributed, URL: urlIncumbent, Feed: "chan", Managed: true}}

	o := origin{Slug: "chan", Post: 3631}
	live := map[string]origin{urlNew: o, urlIncumbent: o}
	rr := recheckResult{managedURL: map[string]bool{urlIncumbent: true}}
	next, managed, deleted, _ := c.mergeManaged(pf, live, rr, true, nil)
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none", deleted)
	}
	byURL := map[string]source{}
	for _, s := range managed {
		byURL[s.URL] = s
	}
	if len(byURL) != 2 {
		t.Fatalf("managed = %+v, want one entry per url", managed)
	}
	if got := byURL[urlIncumbent]; got.Name != attributed || got.Feed != "chan" {
		t.Errorf("incumbent = %+v, want it still named %q with feed chan", got, attributed)
	}
	want := attributed + "-2"
	if got := byURL[urlNew]; got.Name != want || got.Feed != "chan" {
		t.Errorf("newcomer = %+v, want name %q with feed chan", got, want)
	}
	pf.Subscriptions.Sources = next
	if err := validatePrivate(pf); err != nil {
		t.Fatalf("the merge produced a file no cycle can write: %v", err)
	}
}

// TestMergeReservesShelteredURLs: an unmarked entry's URL is reserved the way
// its name is. Without the reservation a live rediscovery of that URL would
// mint a second, managed entry over it — a mirror that carries no hwid of its
// own, which over the operator's entry either duplicates the fetch
// byte-for-byte (the pair validatePrivate refuses to write when the sheltered
// entry is hwid-less, freezing private.yaml until the operator intervenes) or
// reads only the placeholder a device-limited panel serves to a header-less
// request. The merge skips the mint instead, so the URL stays owned by the
// operator's entry alone, exactly as the curated deny list withholds URLs the
// operator already curates.
func TestMergeReservesShelteredURLs(t *testing.T) {
	t.Parallel()

	const (
		handName = "hand-added"
		handURL  = "https://hand.example/sub"
		urlNew   = "https://new.example/sub"
	)
	c := &Crawler{opts: Options{Prune: true}, logger: zerolog.Nop()}
	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: handName, URL: handURL}}

	o := origin{Slug: "chan", Post: 3631}
	live := map[string]origin{handURL: o, urlNew: o}
	next, managed, _, _ := c.mergeManaged(pf, live, recheckResult{}, true, nil)
	byURL := map[string]source{}
	for _, s := range next {
		if _, dup := byURL[s.URL]; dup {
			t.Fatalf("two entries fetch one URL %s: %+v", s.URL, next)
		}
		byURL[s.URL] = s
	}
	if hand, ok := byURL[handURL]; !ok {
		t.Fatalf("the sheltered entry was dropped: %+v", next)
	} else if hand.Name != handName || hand.Managed {
		t.Errorf("sheltered entry = %+v, want %q verbatim and unmanaged", hand, handName)
	}
	if _, ok := byURL[urlNew]; !ok {
		t.Fatalf("the discovery was not minted: %+v", next)
	}
	if len(managed) != 1 {
		t.Errorf("managed = %+v, want only the discovery minted", managed)
	}
	pf.Subscriptions.Sources = next
	if err := validatePrivate(pf); err != nil {
		t.Fatalf("the merge composed a list no cycle can write: %v", err)
	}
}

// TestMergeSheltersOneURLUnderTwoHwids: a hand-authored pair fetching one URL
// under two hwids is the device-limited-panel shape config.Load admits, so
// the cycle must be able to rewrite the file that holds it. Both unmarked
// entries are sheltered verbatim with their hwids and a discovery on a new
// URL is minted beside them; the composed list passes validatePrivate, which
// the url-only key the write gate used to carry refused — freezing every
// write with a false "would be unloadable".
func TestMergeSheltersOneURLUnderTwoHwids(t *testing.T) {
	t.Parallel()

	const (
		url    = "https://shared.example/sub"
		hwidA  = "abcdef0123456789"
		hwidB  = "abcdef012345678a"
		urlNew = "https://new.example/sub"
	)
	c := &Crawler{opts: Options{Prune: true}, logger: zerolog.Nop()}
	var pf privateFile
	pf.Subscriptions.Sources = []source{
		{Name: "panel-a", URL: url, HWID: hwidA},
		{Name: "panel-b", URL: url, HWID: hwidB},
	}

	o := origin{Slug: "chan", Post: 3631}
	live := map[string]origin{url: o, urlNew: o}
	next, managed, _, _ := c.mergeManaged(pf, live, recheckResult{}, true, nil)
	if len(managed) != 1 || managed[0].URL != urlNew {
		t.Fatalf("managed = %+v, want only the discovery over %s minted", managed, urlNew)
	}
	byName := map[string]source{}
	for _, s := range next {
		byName[s.Name] = s
	}
	a, b := byName["panel-a"], byName["panel-b"]
	if a.HWID != hwidA || b.HWID != hwidB || a.Managed || b.Managed {
		t.Fatalf("the hwid pair was not sheltered verbatim: %+v", next)
	}
	pf.Subscriptions.Sources = next
	if err := validatePrivate(pf); err != nil {
		t.Fatalf("the merge composed a list no cycle can write: %v", err)
	}
}

// cursorReRef is the regexp pageCursor's hand scan replaced; TestPageCursor
// holds the scan equal to it over the committed page shapes and the edges the
// two could disagree on (PerfCrawl#4 demanded proof of equivalence, not trust).
var cursorReRef = regexp.MustCompile(`data-post="[^"]+/(\d+)"`)

// pageCursorRe is pageCursor's reference implementation: the smallest message
// id per cursorReRef, compared exactly as pageCursor compares its captures.
func pageCursorRe(page string) string {
	best := ""
	for _, m := range cursorReRef.FindAllStringSubmatch(page, -1) {
		if id := m[1]; best == "" || less(id, best) {
			best = id
		}
	}
	return best
}

func TestPageCursor(t *testing.T) {
	t.Parallel()

	// Committed t.me listing shapes from this package's fixtures, then the
	// adversarial edges where a hand scan could drift from the regexp: a value
	// hiding another attribute inside itself, an escaped boundary (the raw-page
	// read), leading zeros and over-wide runs the regexp accepts, a value that
	// opens with its slash, and unterminated values.
	pages := []string{
		`data-post="chan/3650" ... data-post="chan/3631" ... data-post="chan/3648"`,
		`<div class="tgme_widget_message_wrap" data-post="chan/3631">a</div>` +
			`<div class="tgme_widget_message_wrap" data-post="chan/3630">b</div>`,
		`<div data-post="chan/100"></div><pre>body</pre>`,
		`<a href="https://sub.example/c">c</a><div>me &amp; you</div>` +
			`<div data-post="chan/100"><a href="https://sub.example/a">a</a></div>` +
			`<div data-post=&quot;chan/200&quot;><a href="https://sub.example/d">d</a></div>`,
		`a<div data-post="chan/12">b`,
		`<div data-post="chat/7/12">b`,
		`data-post="chan/007" data-post="chan/8"`,
		`data-post="chan/123456789012345678901234567890"`,
		`data-post="chan/x" data-post="chan/9">b`,
		`data-post="/12"`,
		`data-post="chan/"`,
		`data-post="chan/12`,
		`data-post="unterminated data-post="chan/9" tail`,
		`data-post="x/5 data-post=unterminated/3"`,
		`data-post="ab/5 data-post="chan/9"`,
		"no posts here",
		"",
	}
	for _, page := range pages {
		if got, want := pageCursor(page), pageCursorRe(page); got != want {
			t.Errorf("pageCursor(%q) = %q, cursorReRef gives %q", page, got, want)
		}
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
	bodyA := []source{{Name: inlineSourceName, Body: "AAAA", Managed: true}}
	bodyB := []source{{Name: inlineSourceName, Body: "BBBB", Managed: true}}
	if sameSources(bodyA, bodyB) {
		t.Error("sources differing only in Body must differ")
	}

	// And only in ownership. Adoption is not what this catches: it rewrites Name
	// and Feed too, and both sides of RunOnce's gate are post-adoption anyway,
	// which is what pf.adopted exists for. This is the comparator's own
	// invariant: the mark is part of an entry's identity.
	if sameSources(a, []source{{Name: "x", URL: "u1", Managed: true}, {Name: "y", URL: "u2"}}) {
		t.Error("sources differing only in Managed must differ")
	}
	if sameSources(a, []source{{Name: "x", URL: "u1", Feed: "chan"}, {Name: "y", URL: "u2"}}) {
		t.Error("sources differing only in Feed must differ")
	}
	if sameSources(a, []source{{Name: "x", URL: "u1", HWID: "abcdef0123456789"}, {Name: "y", URL: "u2"}}) {
		t.Error("sources differing only in HWID must differ")
	}
}

// TestPrivateRoundTripPreservesUnmanaged: a write followed by a load must return
// the same set, ownership and attribution included — the fields are the crawler's
// only memory of both, and a load that dropped them would shelter its own entries
// and hand the operator's to the prune.
func TestPrivateRoundTripPreservesUnmanaged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "private.yaml")
	var pf privateFile
	pf.Subscriptions.Sources = []source{
		{Name: "my-private", URL: "https://example.com/sub"},
		{Name: "chan-abc123", URL: "https://is.wepogp.gay/x?payload=abc", Feed: "chan", Managed: true},
	}
	if err := writePrivate(path, pf); err != nil {
		t.Fatalf("writePrivate: %v", err)
	}
	got, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if !sameSources(pf.Subscriptions.Sources, got.Subscriptions.Sources) {
		t.Fatalf("roundtrip mismatch: %+v", got.Subscriptions.Sources)
	}
	if raw, readErr := os.ReadFile(path); readErr != nil {
		t.Fatalf("read private.yaml: %v", readErr)
	} else if strings.Contains(string(raw), "managed: false") {
		t.Errorf("a hand-added entry was written a managed key:\n%s", raw)
	}
}

// TestPrivateCycleKeepsHandAddedHWID: the cycle rewrites private.yaml in full
// from the source struct, so an operator's hwid has to survive load -> merge ->
// write. Losing it leaves a source that still fetches 200 while the panel
// answers the HWID-error placeholder: nothing errors, and only that source's
// own stable_source_published_nodes falling to 0 against a nodes_total of 1
// says so.
func TestPrivateCycleKeepsHandAddedHWID(t *testing.T) {
	t.Parallel()

	const (
		urlHand    = "https://hand.example/sub"
		urlManaged = "https://managed.example/sub"
		hwid       = "abcdef0123456789"
		seed       = "subscriptions:\n  sources:\n" +
			"    - name: hand-added\n      url: " + urlHand + "\n      hwid: " + hwid + "\n" +
			"    - name: chan-3631\n      url: " + urlManaged + "\n      feed: chan\n      managed: true\n      hwid: " + hwid + "\n"
	)
	path := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}
	pf, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}

	c := &Crawler{opts: Options{Prune: true}, logger: zerolog.Nop()}
	live := map[string]origin{urlManaged: {Slug: "chan", Post: 3631}}
	rr := recheckResult{managedURL: map[string]bool{urlManaged: true}}
	next, _, deleted, _ := c.mergeManaged(pf, live, rr, true, nil)
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none", deleted)
	}
	pf.Subscriptions.Sources = next
	if writeErr := writePrivate(path, pf); writeErr != nil {
		t.Fatalf("writePrivate: %v", writeErr)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read private.yaml: %v", err)
	}
	if !strings.Contains(string(raw), "hwid: "+hwid) {
		t.Fatalf("the cycle stripped the hand-added hwid:\n%s", raw)
	}
	var got privateFile
	if unmarshalErr := yaml.Unmarshal(raw, &got); unmarshalErr != nil {
		t.Fatalf("unmarshal private.yaml: %v", unmarshalErr)
	}
	byName := map[string]source{}
	for _, s := range got.Subscriptions.Sources {
		byName[s.Name] = s
	}
	if hand := byName["hand-added"]; hand.HWID != hwid || hand.URL != urlHand || hand.Managed {
		t.Errorf("hand-added entry = %+v, want it unchanged with hwid %q", hand, hwid)
	}
	if managed := byName["chan-3631"]; managed.HWID != hwid {
		t.Errorf("the crawler stripped the hwid a managed entry already carried: %+v", managed)
	}
}

// TestRunOnceAdoptsLegacyNames is the migration, end to end: a private.yaml
// holding prefixed entries beside a hand-added one comes out of one cycle with
// the prefix gone, ownership and feed written, and the hand-added entry not
// touched in any field. The cycle finds nothing new and deletes nothing, which
// is the point — the adoption alone has to be enough to earn the write, or the
// corpus stays prefixed forever.
func TestRunOnceAdoptsLegacyNames(t *testing.T) {
	t.Parallel()

	const (
		urlAttributed = "https://attributed.example/sub"
		urlHashOnly   = "https://hashonly.example/sub"
		urlHand       = "https://hand.example/sub"
		seed          = "subscriptions:\n  sources:\n" +
			"    - name: hand-added\n      url: " + urlHand + "\n" +
			"    - name: tg-chan-aaaaaa\n      url: " + urlAttributed + "\n" +
			"    - name: tg-abc0123456\n      url: " + urlHashOnly + "\n"
	)
	priv := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(priv, []byte(seed), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	c := &Crawler{
		opts:   Options{PrivatePath: priv, Prune: true},
		client: pageFetcher{},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}
	c.RunOnce(context.Background())

	raw, err := os.ReadFile(priv)
	if err != nil {
		t.Fatalf("read private.yaml: %v", err)
	}
	if strings.Contains(string(raw), "tg-") {
		t.Fatalf("a prefixed name survived the cycle:\n%s", raw)
	}
	var pf privateFile
	if unmarshalErr := yaml.Unmarshal(raw, &pf); unmarshalErr != nil {
		t.Fatalf("unmarshal private.yaml: %v", unmarshalErr)
	}
	got := map[string]source{}
	for _, s := range pf.Subscriptions.Sources {
		got[s.URL] = s
	}
	if len(got) != 3 {
		t.Fatalf("want all three entries kept, got %+v", pf.Subscriptions.Sources)
	}

	// The attributed name keeps its hash tail forever — post ids are only for
	// names minted after the cutover — and its slug becomes the feed.
	if s := got[urlAttributed]; s.Name != "chan-aaaaaa" || s.Feed != "chan" || !s.Managed {
		t.Errorf("adopted entry = %+v, want name chan-aaaaaa, feed chan, managed", s)
	}
	// A hash-only name named no channel, so there is no feed to recover.
	if s := got[urlHashOnly]; s.Name != "abc0123456" || s.Feed != "" || !s.Managed {
		t.Errorf("adopted hash-only entry = %+v, want name abc0123456, no feed, managed", s)
	}
	if s := got[urlHand]; s.Name != "hand-added" || s.Feed != "" || s.Managed {
		t.Errorf("hand-added entry = %+v, want it untouched and unmarked", s)
	}
	if strings.Contains(string(raw), "managed: false") {
		t.Errorf("the sheltered entry was written a managed key:\n%s", raw)
	}
}

// TestLoadPrivateAdoptionAvoidsACollision: stripping the prefix can land on a
// name the file already holds, and a duplicate makes validatePrivate refuse the
// write — this cycle and every later one, since the load would re-collide every
// time. The bare hash is unique by construction, so the adopted entry takes it
// and, naming no origin, upgrades on its next rediscovery. The channel it came
// from was never in doubt, though, so the feed is recovered from the stripped
// name and survives the fallback the name could not.
func TestLoadPrivateAdoptionAvoidsACollision(t *testing.T) {
	t.Parallel()

	const (
		urlAdopted = "https://adopted.example/sub"
		seed       = "subscriptions:\n  sources:\n" +
			"    - name: chan-aaaaaa\n      url: https://curated.example/sub\n" +
			"    - name: tg-chan-aaaaaa\n      url: " + urlAdopted + "\n"
	)
	path := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	pf, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if len(pf.Subscriptions.Sources) != 2 {
		t.Fatalf("sources = %+v, want both entries", pf.Subscriptions.Sources)
	}
	if got := pf.Subscriptions.Sources[0]; got.Name != "chan-aaaaaa" || got.Managed {
		t.Errorf("curated entry = %+v, want it untouched and unmarked", got)
	}
	got := pf.Subscriptions.Sources[1]
	if got.Name != managedName(urlAdopted) || !got.Managed {
		t.Errorf("adopted entry = %+v, want the bare hash %q and managed", got, managedName(urlAdopted))
	}
	if got.Feed != "chan" {
		t.Errorf("adopted entry feed = %q, want chan: the fallback renamed it, it did not unlearn its channel", got.Feed)
	}
	if writeErr := validatePrivate(pf); writeErr != nil {
		t.Errorf("the adopted file is unwritable: %v", writeErr)
	}
}

// TestLoadPrivateAdoptionIsOneShot: the mark, not the name, records that an entry
// has been adopted. channelSlug folds "_" to "-", so the channel tg_vpn slugs to
// tg-vpn, and reading the name re-adopts what that channel mints on every load:
// another forced write, another rename, and the recorded feed erased.
func TestLoadPrivateAdoptionIsOneShot(t *testing.T) {
	t.Parallel()

	const (
		urlLegacy = "https://legacy.example/sub"
		urlMinted = "https://minted.example/sub"
		seed      = "subscriptions:\n  sources:\n" +
			"    - name: tg-tg-vpn-1444c8\n      url: " + urlLegacy + "\n" +
			"    - name: tg-vpn-123\n      url: " + urlMinted +
			"\n      feed: tg-vpn\n      managed: true\n"
	)
	path := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}
	want := []source{
		{Name: "tg-vpn-1444c8", URL: urlLegacy, Feed: "tg-vpn", Managed: true},
		{Name: "tg-vpn-123", URL: urlMinted, Feed: "tg-vpn", Managed: true},
	}

	first, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if !first.adopted {
		t.Fatal("the unmarked prefixed entry was not adopted")
	}
	if !slices.Equal(first.Subscriptions.Sources, want) {
		t.Fatalf("first load = %+v, want %+v", first.Subscriptions.Sources, want)
	}
	if writeErr := writePrivate(path, first); writeErr != nil {
		t.Fatalf("writePrivate: %v", writeErr)
	}

	second, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("second loadPrivate: %v", err)
	}
	if second.adopted {
		t.Errorf("the migration re-fired: %+v", second.Subscriptions.Sources)
	}
	if !slices.Equal(second.Subscriptions.Sources, want) {
		t.Errorf("second load = %+v, want the file unchanged", second.Subscriptions.Sources)
	}
}

func TestLoadPrivateMissingFile(t *testing.T) {
	t.Parallel()

	got, exists, err := loadPrivate(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if exists {
		t.Error("a missing file must report that it does not exist")
	}
	if len(got.Subscriptions.Sources) != 0 {
		t.Fatalf("missing file should yield no sources, got %+v", got.Subscriptions.Sources)
	}
}

// TestRunOnceRefusesToRebuildAMissingCorpus pins finding 2's guard: private.yaml
// absent while the state file remembers a managed corpus is a lost file (or an
// unmounted bind), not an empty corpus, and the cycle must refuse the write —
// one cycle's discoveries alone would silently replace hundreds of harvested
// sources and wipe their liveness streaks. The fixture also proves the genuine
// first cycle still works: absent file, state remembering nothing, the write
// goes through.
func TestRunOnceRefusesToRebuildAMissingCorpus(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	statePath := filepath.Join(dir, "state.json")
	if err := saveState(statePath, state{Managed: map[string]managedState{
		"https://lost.example/sub": {NotLiveCycles: 2},
	}}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	var logBuf bytes.Buffer
	c := &Crawler{
		opts: Options{
			Channels:    []string{"chan"},
			PrivatePath: priv,
			StatePath:   statePath,
			Pages:       1,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": wrapMsg("https://sub.example/new")}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}
	c.RunOnce(context.Background())
	if _, err := os.Stat(priv); !os.IsNotExist(err) {
		t.Fatalf("refused cycle must leave the absent private.yaml alone, stat: %v", err)
	}
	if !strings.Contains(logBuf.String(), "private.yaml holds no managed sources while state remembers a managed corpus") {
		t.Errorf("the refusal was not logged at error level:\n%s", logBuf.String())
	}
	// The liveness streaks of the missing corpus must survive the refused
	// cycle: they are the memory that keeps the guard armed and the evidence
	// that restores the retirement verdicts once the file is back.
	if st := loadState(statePath, zerolog.Nop()); len(st.Managed) != 1 {
		t.Errorf("state lost the corpus's liveness records: %+v", st.Managed)
	}

	// A genuine first cycle — state remembering nothing — still builds the file.
	os.Remove(statePath)
	var fresh bytes.Buffer
	c.logger = zerolog.New(&fresh)
	c.RunOnce(context.Background())
	got, err := os.ReadFile(priv)
	if err != nil {
		t.Fatalf("first cycle must write the corpus it discovered: %v", err)
	}
	if !strings.Contains(string(got), "sub.example") {
		t.Errorf("first cycle wrote %s, want its discovery in it", got)
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
	got := extractRefs(pages)

	// Dedupe is by full ref, so rap_ex bare and rap_ex/12 both survive here;
	// scan's visited check is what admits at most one per cycle. The self
	// permalink survives too — extractRefs no longer judges self-refs, the
	// carve-out in scanChannel does, against how the node was read.
	want := map[string]bool{"d_code/26804": true, "rap_ex": true, "rap_ex/12": true, "o00000000i/3631": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want refs %v", got, keysOf(want))
	}
	for _, ref := range got {
		if !want[ref.String()] {
			t.Errorf("unexpected ref %q (bot/reserved should be excluded)", ref.String())
		}
	}
}

// TestParseSeed pins the seed forms channels.yaml documents. The forum one is
// load-bearing: a group answers t.me/s/ with no message at all, so an entry that
// names a topic must keep it, and a permalink's message id must not survive as
// one — the <chat>/<topic>/<msg> shape was measured to return no link at all.
func TestParseSeed(t *testing.T) {
	t.Parallel()

	cases := map[string]chanRef{
		"o00000000i":                            {slug: "o00000000i"},
		"@rap_ex":                               {slug: "rap_ex"},
		"https://t.me/rap_ex":                   {slug: "rap_ex"},
		"https://t.me/s/chan01":                 {slug: "chan01"},
		"  spaced  ":                            {slug: "spaced"},
		"forumchat/1310":                        {slug: "forumchat", topic: "1310"},
		"T.me/Forum/123":                        {slug: "forum", topic: "123"},
		"https://t.me/forumchat/1310/21206":     {slug: "forumchat", topic: "1310"},
		"https://t.me/forumchat/1310?comment=9": {slug: "forumchat", topic: "1310"},
		"https://t.me/chan01/about":             {slug: "chan01"},
		"https://t.me/share/url?url=x":          {},
	}
	for in, want := range cases {
		if got := parseSeed(in); got != want {
			t.Errorf("parseSeed(%q) = %+v, want %+v", in, got, want)
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
		// A portful http/socks URI parses as a Node, so only isInlineScheme keeps
		// a client-setup snippet out of the harvest; both must stay uncaptured.
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

// pageFetcher is a network-free fetchClient returning canned HTML per URL, or a
// canned error for a URL in errs: the crawler must not report a page it read and
// found nothing in the same way as one it never reached. A non-nil hits counts
// requests per URL, for the tests whose subject is a fetch that must or must not
// happen at all.
type pageFetcher struct {
	pages map[string]string
	errs  map[string]error
	hits  map[string]int
}

func (f pageFetcher) page(_ context.Context, u string) (string, error) {
	if f.hits != nil {
		f.hits[u]++
	}
	if err := f.errs[u]; err != nil {
		return "", err
	}
	return f.pages[u], nil
}

// TestRunOnceHarvestsInlineNodes drives a full cycle against a stub fetcher: the
// single scraped page carries four inline URIs, two of which collide on
// server:port. With InlineMax=2 the crawler writes an inline source whose
// base64 Body holds the first two distinct nodes (dedup first-wins, then cap).
func TestRunOnceHarvestsInlineNodes(t *testing.T) {
	t.Parallel()

	priv := writeEmptyPrivate(t, t.TempDir())

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
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
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
		if pf.Subscriptions.Sources[i].Name == inlineSourceName {
			inline = &pf.Subscriptions.Sources[i]
		}
	}
	if inline == nil {
		t.Fatalf("no inline source written: %+v", pf.Subscriptions.Sources)
	}
	if inline.URL != "" {
		t.Errorf("inline source must have empty URL, got %q", inline.URL)
	}
	if !inline.Managed {
		t.Error("the inline source is minted every cycle, so it must carry managed: true")
	}
	decoded, err := base64.StdEncoding.DecodeString(inline.Body)
	if err != nil {
		t.Fatalf("inline Body is not valid base64: %v", err)
	}
	want := "vless://a@1.1.1.1:443#n1\nvless://c@2.2.2.2:443#n2"
	if string(decoded) != want {
		t.Fatalf("inline Body = %q, want %q", decoded, want)
	}
}

// TestRunOnceHarvestsInlineFromNewestPageAndLinksFromEveryPage pins both halves
// of the harvest asymmetry across one channel's two pages: the older page's
// inline node is dropped because a server:port decays with the message carrying
// it, while that same page's subscription link still becomes a managed source.
func TestRunOnceHarvestsInlineFromNewestPageAndLinksFromEveryPage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	priv := writeEmptyPrivate(t, dir)

	const (
		newest = "vless://a@1.1.1.1:443#newest"
		older  = "vless://b@2.2.2.2:443#older"
		subURL = "https://older.example/sub"
	)
	// data-post is what pageCursor reads, so page one hands scrapeChat the
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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
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
		case s.Name == inlineSourceName:
			inline = s
		case s.Managed:
			if managed != nil {
				t.Fatalf("want exactly one managed url source, got %+v", pf.Subscriptions.Sources)
			}
			managed = s
		}
	}
	if inline == nil {
		t.Fatalf("no inline source written: %+v", pf.Subscriptions.Sources)
	}
	decoded, err := base64.StdEncoding.DecodeString(inline.Body)
	if err != nil {
		t.Fatalf("inline Body is not valid base64: %v", err)
	}
	body := string(decoded)
	if !strings.Contains(body, newest) {
		t.Errorf("inline Body = %q, want it to carry the newest page's node %q", body, newest)
	}
	if strings.Contains(body, older) {
		t.Errorf("inline Body = %q, must not carry the older page's node %q", body, older)
	}

	if managed == nil {
		t.Fatalf("the older page's link %q reached no managed source: %+v", subURL, pf.Subscriptions.Sources)
	}
	if managed.URL != subURL {
		t.Errorf("managed source URL = %q, want the older page's link %q", managed.URL, subURL)
	}
	// The older page carries no data-post, so the link has no post to be named
	// for and takes the first free ordinal beside the slug, which on an empty
	// file is 1.
	if managed.Name != "chan-1" {
		t.Errorf("managed source name = %q, want the postless chan-1 form sourceName produces", managed.Name)
	}
	if managed.Feed != "chan" {
		t.Errorf("managed source feed = %q, want chan", managed.Feed)
	}
}

// TestRunOnceAttributesBothLinksOfOnePost: a post is a container, not a URL, so
// the two links in one message cannot share its name. The first in sorted-URL
// order takes <slug>-<postid> and the second the ordinal beside it — sorted,
// because mergeManaged names in that order (sort.Strings) and a randomized order
// would hand the bare form to whichever URL won the map.
func TestRunOnceAttributesBothLinksOfOnePost(t *testing.T) {
	t.Parallel()

	priv := writeEmptyPrivate(t, t.TempDir())

	const (
		post   = "3631"
		urlOne = "https://a.example/sub"
		urlTwo = "https://b.example/sub"
	)
	page := `<div class="tgme_widget_message_wrap" data-post="chan/` + post + `">` +
		`<a href="` + urlOne + `">one</a> <a href="` + urlTwo + `">two</a></div>`

	c := &Crawler{
		opts:   Options{Channels: []string{"chan"}, PrivatePath: priv, Pages: 1},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}
	c.RunOnce(context.Background())

	pf, _, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	got := map[string]source{}
	for _, s := range pf.Subscriptions.Sources {
		got[s.URL] = s
	}
	if len(got) != 2 {
		t.Fatalf("want both links of the post managed, got %+v", pf.Subscriptions.Sources)
	}
	if s := got[urlOne]; s.Name != "chan-"+post || s.Feed != "chan" || !s.Managed {
		t.Errorf("first link = %+v, want name chan-%s, feed chan, managed", s, post)
	}
	if want := "chan-" + post + "-2"; got[urlTwo].Name != want {
		t.Errorf("second link name = %q, want %q", got[urlTwo].Name, want)
	}
	if s := got[urlTwo]; s.Feed != "chan" || !s.Managed {
		t.Errorf("second link = %+v, want feed chan and managed", s)
	}
}

// hasInlineSource reports whether an inline source exists in private.yaml.
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
		if pf.Subscriptions.Sources[i].Name == inlineSourceName {
			return true
		}
	}
	return false
}

// TestRunOnceInlineDisabled: with InlineEnabled=false the crawler must not write
// an inline source even though the scraped page carries inline URIs.
func TestRunOnceInlineDisabled(t *testing.T) {
	t.Parallel()
	priv := writeEmptyPrivate(t, t.TempDir())

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
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	if hasInlineSource(t, priv) {
		t.Fatal("inline source written despite InlineEnabled=false")
	}
}

// TestRunOnceYieldsInlineNameToHandAddedEntry: after the cutover `inline` is an
// ordinary name -- ownership is the field, not the name -- so an operator may
// hold it. A hand-added entry named `inline` is sheltered into kept, and an
// aggregate appended beside it would make validatePrivate refuse the duplicate
// on this cycle and on every later one: the discovery lost, the file never
// written again. The crawler yields the name instead, because a hand-added entry
// is never renamed: it skips the inline harvest, says so at WARN, finishes the
// cycle, and converges.
func TestRunOnceYieldsInlineNameToHandAddedEntry(t *testing.T) {
	t.Parallel()

	const (
		urlHand    = "https://hand.example/sub"
		urlNew     = "https://new.example/sub"
		mintedName = "chan-3631"
	)
	priv := filepath.Join(t.TempDir(), "private.yaml")
	seed := "subscriptions:\n  sources:\n" +
		"    - name: " + inlineSourceName + "\n      url: " + urlHand + "\n"
	if err := os.WriteFile(priv, []byte(seed), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	page := `<div class="tgme_widget_message_wrap" data-post="chan/3631">` +
		`<a href="` + urlNew + `">sub</a> vless://a@1.1.1.1:443#n1</div>`

	var logBuf bytes.Buffer
	c := &Crawler{
		opts: Options{
			Channels:      []string{"chan"},
			PrivatePath:   priv,
			Pages:         1,
			InlineEnabled: true,
			InlineMax:     5,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}
	c.RunOnce(context.Background())

	if strings.Contains(logBuf.String(), "write private.yaml failed") {
		t.Fatalf("the cycle refused to write the file it merged:\n%s", logBuf.String())
	}
	got := sourcesByName(t, priv)
	hand, held := got[inlineSourceName]
	if !held {
		t.Fatalf("the operator's entry is gone: %+v", got)
	}
	if want := (source{Name: inlineSourceName, URL: urlHand}); hand != want {
		t.Errorf("operator's entry = %+v, want %+v unrenamed and untouched", hand, want)
	}
	if _, minted := got[mintedName]; !minted {
		t.Errorf("the discovery was lost to the skipped aggregate: %+v", got)
	}
	if !hasWarnNaming(logBuf.String(), inlineSourceName) {
		t.Errorf("no WARN naming the entry that holds the name:\n%s", logBuf.String())
	}

	// The yield has to be stable, not merely survivable: a cycle that proposes
	// the same file must report no change rather than rewrite it forever.
	logBuf.Reset()
	c.RunOnce(context.Background())
	if !strings.Contains(logBuf.String(), "no change") {
		t.Errorf("second cycle did not converge:\n%s", logBuf.String())
	}
}

// hasWarnNaming reports whether one zerolog line is a warning carrying
// source=name, so a test can pin the level and the subject together instead of
// finding them on two unrelated lines.
func hasWarnNaming(logs, name string) bool {
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, `"level":"warn"`) && strings.Contains(line, `"source":"`+name+`"`) {
			return true
		}
	}
	return false
}

// writeEmptyPrivate seeds an empty managed-overlay file at dir/private.yaml
// and returns its path.
func writeEmptyPrivate(t *testing.T, dir string) string {
	t.Helper()
	priv := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return priv
}

// sourcesByName loads private.yaml keyed by name, refusing a duplicate: that is
// the one file no later cycle can write.
func sourcesByName(t *testing.T, priv string) map[string]source {
	t.Helper()
	pf, _, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	got := map[string]source{}
	for _, s := range pf.Subscriptions.Sources {
		if _, dup := got[s.Name]; dup {
			t.Fatalf("duplicate name %q written: %+v", s.Name, pf.Subscriptions.Sources)
		}
		got[s.Name] = s
	}
	return got
}

// aggregateNodes decodes the inline aggregate's Body back into the node URIs it
// packs.
func aggregateNodes(t *testing.T, got map[string]source) string {
	t.Helper()
	agg, ok := got[inlineSourceName]
	if !ok {
		t.Fatalf("no inline aggregate written: %+v", got)
	}
	decoded, err := base64.StdEncoding.DecodeString(agg.Body)
	if err != nil {
		t.Fatalf("aggregate Body is not valid base64: %v", err)
	}
	return string(decoded)
}

// TestRunOnceSheltersHandAddedBodySource: an unmarked `body:` entry is an inline
// payload the operator pasted, a documented shape (config.SubscriptionSource),
// and nobody regenerates it -- so the merge must drop only the crawler's own
// aggregate, which carries the field. The cycle mints a source and writes its own
// aggregate too, so the shelter is proven against a write that really happens;
// the terminal counts prove the sheltered entry is carried once, and a second
// cycle over a changed page proves the aggregate is still the one thing dropped.
func TestRunOnceSheltersHandAddedBodySource(t *testing.T) {
	t.Parallel()

	const (
		handName  = "my-pasted-nodes"
		handNode  = "vless://u@9.9.9.9:443#hand"
		urlNew    = "https://new.example/sub"
		minted    = "chan-3631"
		firstNode = "vless://a@1.1.1.1:443#n1"
		laterNode = "vless://b@2.2.2.2:443#n2"
		chanPage  = "https://t.me/s/chan"
	)
	handBody := base64.StdEncoding.EncodeToString([]byte(handNode))
	priv := filepath.Join(t.TempDir(), "private.yaml")
	seed := "subscriptions:\n  sources:\n" +
		"    - name: " + handName + "\n      body: " + handBody + "\n"
	if err := os.WriteFile(priv, []byte(seed), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	post := func(node string) string {
		return `<div class="tgme_widget_message_wrap" data-post="chan/3631">` +
			`<a href="` + urlNew + `">sub</a> ` + node + `</div>`
	}
	pages := map[string]string{chanPage: post(firstNode)}

	var logBuf bytes.Buffer
	c := &Crawler{
		opts: Options{
			Channels:      []string{"chan"},
			PrivatePath:   priv,
			Pages:         1,
			InlineEnabled: true,
			InlineMax:     5,
		},
		client: pageFetcher{pages: pages},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}

	c.RunOnce(context.Background())

	got := sourcesByName(t, priv)
	hand, held := got[handName]
	if !held {
		t.Fatalf("the operator's body source was deleted: %+v", got)
	}
	if want := (source{Name: handName, Body: handBody}); hand != want {
		t.Errorf("operator's entry = %+v, want %+v written back unchanged", hand, want)
	}
	if _, ok := got[minted]; !ok {
		t.Fatalf("no source minted, so the shelter was not tested against a write: %+v", got)
	}
	if nodes := aggregateNodes(t, got); nodes != firstNode {
		t.Errorf("aggregate = %q, want this cycle's harvest %q alone", nodes, firstNode)
	}
	// Counted once: three entries written, one managed URL source, one node in
	// the aggregate. A sheltered body entry is none of the crawler's tallies.
	if !strings.Contains(logBuf.String(), `"managed":1`) ||
		!strings.Contains(logBuf.String(), `"inline":1`) ||
		!strings.Contains(logBuf.String(), `"total":3`) {
		t.Errorf("terminal counts double-count or lose the sheltered entry:\n%s", logBuf.String())
	}

	pages[chanPage] = post(laterNode)
	logBuf.Reset()
	c.RunOnce(context.Background())

	got = sourcesByName(t, priv)
	if nodes := aggregateNodes(t, got); nodes != laterNode {
		t.Errorf("aggregate = %q after a changed page, want %q: the stale one was sheltered instead of dropped", nodes, laterNode)
	}
	// The aggregate has no URL, so reaching the merge's URL gate would both log
	// a spurious refusal and put it in deleted, inflating the prune floor's drop.
	if strings.Contains(logBuf.String(), "dropping managed source") {
		t.Errorf("the stale aggregate reached the URL gate instead of being dropped by the merge:\n%s", logBuf.String())
	}
	if want := (source{Name: handName, Body: handBody}); got[handName] != want {
		t.Errorf("operator's entry = %+v after a second cycle, want %+v", got[handName], want)
	}
	if !strings.Contains(logBuf.String(), `"total":3`) {
		t.Errorf("the second cycle did not write the same three entries:\n%s", logBuf.String())
	}
}

// TestRunOnceNoInlineNodes: inline harvesting is on but the pages carry zero
// proxy URIs, so buildInlineSource returns ok=false and no inline is written.
func TestRunOnceNoInlineNodes(t *testing.T) {
	t.Parallel()
	priv := writeEmptyPrivate(t, t.TempDir())

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
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	if hasInlineSource(t, priv) {
		t.Fatal("inline source written despite no inline nodes")
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
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u, Managed: true})
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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if string(u) == urls[0] {
				return classify.Result{Nodes: 1}, nil
			}
			return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
		},
		logger: zerolog.New(&logBuf),
	}

	c.RunOnce(context.Background())

	got, _, err := loadPrivate(priv)
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
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u, Managed: true})
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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
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

	got, _, err := loadPrivate(priv)
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
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u, Managed: true})
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
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if gone[string(u)] {
				return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
			}
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}

	c.RunOnce(context.Background())

	got, _, err := loadPrivate(priv)
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
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u, Managed: true})
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
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
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
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u, Managed: true})
	}
	priv := filepath.Join(t.TempDir(), "private.yaml")
	if err := writePrivate(priv, pf); err != nil {
		t.Fatalf("seed private.yaml: %v", err)
	}

	var logBuf bytes.Buffer
	c := &Crawler{
		opts:   Options{PrivatePath: priv, Prune: true},
		client: pageFetcher{},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
		},
		logger: zerolog.New(&logBuf),
	}

	c.RunOnce(context.Background())

	got, _, err := loadPrivate(priv)
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

// retireFixture is a managed corpus wired to a stub classify: every URL in
// nodeless answers 200 with no node (the panel placeholder finding 16 is about),
// every other one serves a node, and the state file starts with the given
// liveness streaks.
type retireFixture struct {
	c         *Crawler
	priv      string
	statePath string
	log       *bytes.Buffer
}

func newRetireFixture(t *testing.T, urls []string, nodeless map[string]bool, seeded map[string]managedState) retireFixture {
	t.Helper()

	var pf privateFile
	for _, u := range urls {
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{Name: managedName(u), URL: u, Managed: true})
	}
	dir := t.TempDir()
	f := retireFixture{
		priv:      filepath.Join(dir, "private.yaml"),
		statePath: filepath.Join(dir, ".crawler-state.json"),
		log:       &bytes.Buffer{},
	}
	if err := writePrivate(f.priv, pf); err != nil {
		t.Fatalf("seed private.yaml: %v", err)
	}
	if err := saveState(f.statePath, state{Managed: seeded}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	f.c = &Crawler{
		opts:   Options{PrivatePath: f.priv, StatePath: f.statePath, StateTTL: 30 * oneDay, Prune: true},
		client: pageFetcher{},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if nodeless[string(u)] {
				return classify.Result{}, nil
			}
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(f.log),
	}
	return f
}

func (f retireFixture) kept(t *testing.T) map[string]bool {
	t.Helper()

	pf, _, err := loadPrivate(f.priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	out := make(map[string]bool, len(pf.Subscriptions.Sources))
	for _, s := range pf.Subscriptions.Sources {
		out[s.URL] = true
	}
	return out
}

func (f retireFixture) streaks(t *testing.T) map[string]managedState {
	t.Helper()

	return loadState(f.statePath, zerolog.Nop()).Managed
}

// TestRunOnceRetiresASourceNotLiveForTheWholeWindow is the acceptance contract for
// finding 16's accrual leak: a harvested 24h-rotating link whose body becomes the
// panel's placeholder classifies nodeless-2xx, which is undetermined and so kept
// forever — at two classify fetches per cycle, one as a rediscovered candidate and
// one in recheckManaged. The staleRetireCycles-th consecutive not-live cycle, once
// staleRetireAfter of wall clock has also passed, is what finally retires it.
func TestRunOnceRetiresASourceNotLiveForTheWholeWindow(t *testing.T) {
	t.Parallel()

	const (
		urlStale = "https://rotating.example/saber"
		urlLive  = "https://live.example/sub"
	)
	now := time.Now()
	f := newRetireFixture(t,
		[]string{urlStale, urlLive},
		map[string]bool{urlStale: true},
		map[string]managedState{urlStale: {
			LastLiveAt:    now.Add(-staleRetireAfter - 2*time.Hour),
			NotLiveSince:  now.Add(-staleRetireAfter - time.Hour),
			NotLiveCycles: staleRetireCycles - 1,
		}})

	f.c.RunOnce(context.Background())

	kept := f.kept(t)
	if kept[urlStale] {
		t.Errorf("a source not live for %d cycles and %s must be retired, got %v", staleRetireCycles, staleRetireAfter, kept)
	}
	if !kept[urlLive] {
		t.Error("the source still serving nodes must survive")
	}
	if !strings.Contains(f.log.String(), "was live, then not live") {
		t.Errorf("a source with a recorded live answer must be logged as such, got %q", f.log.String())
	}
}

// TestRunOnceKeepsASourceShortOfTheRetirementWindow pins both halves of the
// window, because either alone is defeatable: wall clock alone lets a crawler
// resuming after days of downtime retire on a single bad answer, and a cycle count
// alone is burned through in minutes by the CRAWL_HTTP trigger running cycles back
// to back.
func TestRunOnceKeepsASourceShortOfTheRetirementWindow(t *testing.T) {
	t.Parallel()

	const (
		urlStale = "https://rotating.example/saber"
		urlLive  = "https://live.example/sub"
	)
	now := time.Now()
	for name, seeded := range map[string]managedState{
		"one cycle short": {
			NotLiveSince:  now.Add(-staleRetireAfter - time.Hour),
			NotLiveCycles: staleRetireCycles - 2,
		},
		"a day short": {
			NotLiveSince:  now.Add(-time.Hour),
			NotLiveCycles: staleRetireCycles,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newRetireFixture(t,
				[]string{urlStale, urlLive},
				map[string]bool{urlStale: true},
				map[string]managedState{urlStale: seeded})

			f.c.RunOnce(context.Background())

			if !f.kept(t)[urlStale] {
				t.Errorf("%s of the window, the source must be kept: %+v", name, f.streaks(t)[urlStale])
			}
		})
	}
}

// TestRunOnceLiveAnswerResetsTheRetirementClock: the window measures a CONTINUOUS
// not-live run, so one live answer clears both the count and the anchor. Without
// the reset a source that goes quiet for a day, comes back, and then has one bad
// cycle is retired on the strength of that old anchor.
func TestRunOnceLiveAnswerResetsTheRetirementClock(t *testing.T) {
	t.Parallel()

	const (
		urlTarget = "https://recovering.example/sub"
		urlLive   = "https://live.example/sub"
	)
	now := time.Now()
	nodeless := map[string]bool{}
	f := newRetireFixture(t,
		[]string{urlTarget, urlLive},
		nodeless,
		map[string]managedState{urlTarget: {
			NotLiveSince:  now.Add(-staleRetireAfter - time.Hour),
			NotLiveCycles: staleRetireCycles - 1,
		}})

	f.c.RunOnce(context.Background())

	after := f.streaks(t)[urlTarget]
	if !after.NotLiveSince.IsZero() || after.NotLiveCycles != 0 {
		t.Fatalf("a live answer must clear the streak, got %+v", after)
	}
	if after.LastLiveAt.IsZero() {
		t.Error("a live answer must be recorded, so the retirement log can tell it from a source with no history")
	}

	nodeless[urlTarget] = true
	f.c.RunOnce(context.Background())

	if !f.kept(t)[urlTarget] {
		t.Error("one not-live cycle after a live one must not retire: the clock restarted")
	}
	if got := f.streaks(t)[urlTarget].NotLiveCycles; got != 1 {
		t.Errorf("not_live_cycles = %d after the reset, want 1", got)
	}
}

// TestRunOnceGrandfathersManagedSourcesWithNoStreakRecord is the corpus guard. The
// state file has never held per-URL liveness, so on the first cycle after this
// ships every managed source (261 on the live instance) has no record. Reading a
// missing record as "stale since the zero time" condemns all of them at once, and
// the bulk-prune floor does not save the corpus: it refuses the proposal once, then
// honours the same proposal past bulkPruneConfirmAfter — and the corpus is
// unrecoverable. A streak must therefore be anchored at the observation that opens
// it, so the first not-live cycle only starts the clock.
func TestRunOnceGrandfathersManagedSourcesWithNoStreakRecord(t *testing.T) {
	t.Parallel()

	const total = 12
	urls := make([]string, 0, total)
	nodeless := map[string]bool{}
	for i := range total {
		u := fmt.Sprintf("https://g%02d.example/sub", i)
		urls = append(urls, u)
		if i > 0 { // urls[0] keeps serving nodes, so no cycle here is a dark one
			nodeless[u] = true
		}
	}
	f := newRetireFixture(t, urls, nodeless, nil)

	for range staleRetireCycles + 2 {
		f.c.RunOnce(context.Background())
	}

	if kept := f.kept(t); len(kept) != total {
		t.Fatalf("corpus is %d of %d after %d cycles; a missing record was read as ancient staleness",
			len(kept), total, staleRetireCycles+2)
	}
	if strings.Contains(f.log.String(), "condemning managed source") {
		t.Errorf("no source may be condemned on its first cycles of history, got %q", f.log.String())
	}
	for u, m := range f.streaks(t) {
		if nodeless[u] && m.NotLiveSince.IsZero() {
			t.Errorf("%s: not_live_since is zero, which reads as stale since the zero time", u)
		}
	}
}

// TestRunOnceStaleRetirementObeysTheBulkPruneFloor: retirement is a deletion like
// any other, so a cycle whose streaks run out on a third of the corpus at once
// still has to be confirmed by a later cycle.
func TestRunOnceStaleRetirementObeysTheBulkPruneFloor(t *testing.T) {
	t.Parallel()

	const (
		total  = 40
		doomed = 15
	)
	now := time.Now()
	urls := make([]string, 0, total)
	nodeless := map[string]bool{}
	seeded := map[string]managedState{}
	for i := range total {
		u := fmt.Sprintf("https://b%02d.example/sub", i)
		urls = append(urls, u)
		if i < doomed {
			nodeless[u] = true
			seeded[u] = managedState{
				NotLiveSince:  now.Add(-staleRetireAfter - time.Hour),
				NotLiveCycles: staleRetireCycles - 1,
			}
		}
	}
	f := newRetireFixture(t, urls, nodeless, seeded)

	f.c.RunOnce(context.Background())

	if kept := f.kept(t); len(kept) != total {
		t.Fatalf("corpus is %d of %d; %d retirements at once must be refused pending confirmation", len(kept), total, doomed)
	}
	if !strings.Contains(f.log.String(), "bulk prune floor tripped") {
		t.Errorf("the refusal must be logged at error level, got %q", f.log.String())
	}
	if got := len(loadState(f.statePath, zerolog.Nop()).BulkPruneURLs); got != doomed {
		t.Errorf("recorded proposal holds %d URLs, want the %d retirements", got, doomed)
	}
}

// TestRunOnceDarkCycleAdvancesNoStreak: a cycle that learned nothing is a
// crawler-side fault (egress down, DNS interception, a proxy answering every
// request itself), so its not-live answers are not evidence about any source and
// must not push a streak towards retirement — otherwise a night of failed cycles
// spends the whole window on faults, and the first cycle that recovers enough to
// prune deletes everything that has not come back yet.
func TestRunOnceDarkCycleAdvancesNoStreak(t *testing.T) {
	t.Parallel()

	const url = "https://quiet.example/sub"
	now := time.Now()
	seeded := managedState{
		NotLiveSince:  now.Add(-staleRetireAfter - time.Hour),
		NotLiveCycles: staleRetireCycles - 1,
	}
	f := newRetireFixture(t, []string{url}, map[string]bool{url: true}, map[string]managedState{url: seeded})

	f.c.RunOnce(context.Background())

	if !f.kept(t)[url] {
		t.Error("a dark cycle must prune nothing")
	}
	if got := f.streaks(t)[url].NotLiveCycles; got != seeded.NotLiveCycles {
		t.Errorf("not_live_cycles = %d, want the seeded %d: a dark cycle is not evidence", got, seeded.NotLiveCycles)
	}
}

// TestAgeManagedDropsRecordsTheCorpusNoLongerHolds: private.yaml is the authority
// on which sources exist, so the streak map is bounded by it — a retired, blocked
// or hand-deleted URL leaves no record behind to grow the state file forever.
func TestAgeManagedDropsRecordsTheCorpusNoLongerHolds(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := state{Managed: map[string]managedState{
		"https://gone.example/sub": {NotLiveCycles: 3, NotLiveSince: now.Add(-time.Hour)},
		"https://here.example/sub": {NotLiveCycles: 3, NotLiveSince: now.Add(-time.Hour)},
	}}

	st.ageManaged(map[string]bool{"https://here.example/sub": true}, nil, now)

	if _, ok := st.Managed["https://gone.example/sub"]; ok {
		t.Errorf("record for a URL outside the corpus survived: %+v", st.Managed)
	}
	if got := st.Managed["https://here.example/sub"].NotLiveCycles; got != 4 {
		t.Errorf("not_live_cycles = %d, want 4", got)
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

// TestBuildSeedsSeedsEveryRememberedTopic: the seed set is keyed by the full
// ref, not the slug, so a group remembered productive in several topics is one
// depth-0 target PER topic — collapsing them by slug kept whichever topic the
// productive map ranged over first and silently dropped the rest from the
// cycle. A bare <slug> entry is absorbed into its slug's topic entries, its
// configured flag riding onto them, so the chat is still read through each
// topic rather than also probed through its message-less listing.
func TestBuildSeedsSeedsEveryRememberedTopic(t *testing.T) {
	t.Parallel()

	c := &Crawler{opts: Options{Channels: []string{"forumchat", "elsewhere"}}, logger: zerolog.Nop()}
	st := state{Productive: map[string]channelState{
		"forumchat/1310": {},
		"forumchat/55":   {},
		"remembered":     {},
	}}

	seeds := c.buildSeeds(&st)
	if len(seeds) != 4 {
		t.Fatalf("seeds = %+v, want 4 targets: both remembered topics, elsewhere, remembered", seeds)
	}
	if _, ok := seeds["forumchat"]; ok {
		t.Error("the bare forumchat entry survived beside its topics; the group would be probed through its message-less listing for nothing")
	}
	for topic, configured := range map[string]bool{"forumchat/1310": true, "forumchat/55": true} {
		spec, ok := seeds[topic]
		if !ok {
			t.Errorf("seeds missing the remembered topic %q: %+v", topic, keys(seeds))
			continue
		}
		if spec.ref.String() != topic {
			t.Errorf("seeds[%q].ref = %+v, want the topic ref", topic, spec.ref)
		}
		if spec.configured != configured {
			t.Errorf("seeds[%q].configured = %v, want %v (the absorbed bare entry's configured budget)", topic, spec.configured, configured)
		}
	}
	if got := seeds["elsewhere"]; got.ref.slug != "elsewhere" || !got.configured {
		t.Errorf("elsewhere = %+v, want the configured bare seed kept (no topic absorbs it)", got)
	}
	if got := seeds["remembered"]; got.configured {
		t.Errorf("remembered = %+v, want the shallower discovered budget", got)
	}
}

// TestPagesForBudget: only operator-configured seeds get the full page budget.
// Remembered seeds and repost discoveries pay the shallower one — they accumulate
// across cycles, and paying full price for each is what made every cycle cost
// more than the last.
func TestPagesForBudget(t *testing.T) {
	t.Parallel()

	c := &Crawler{opts: Options{Pages: 6}}
	if got := c.pagesFor(scanNode{ref: chanRef{slug: "a"}, configured: true}); got != 6 {
		t.Errorf("configured seed budget = %d, want 6", got)
	}
	if got := c.pagesFor(scanNode{ref: chanRef{slug: "b"}}); got != discoveredPages {
		t.Errorf("remembered seed budget = %d, want %d", got, discoveredPages)
	}
	small := &Crawler{opts: Options{Pages: 1}}
	if got := small.pagesFor(scanNode{ref: chanRef{slug: "b"}, depth: 2}); got != 1 {
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
	pf.Subscriptions.Sources = []source{{Name: "abc123", URL: "https://10.0.0.5/sub", Managed: true}}

	if err := writePrivate(path, pf); err == nil {
		t.Fatal("writePrivate must refuse a source with a non-public literal-IP host")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a refused write must not create the file; stat err = %v", err)
	}
}

// TestWritePrivateFetchIdentityMatchesConfigLoad: the write gate keys the
// duplicate rule the way config.Load does — on the fetch, url AND hwid — so
// it admits exactly the files the service will load. Round 3 re-keyed the
// loader because x-hwid changes the request and a device-limited panel serves
// each value a different payload, so a private.yaml holding one URL under two
// hwids loads and must also write, or the crawler would freeze every cycle on
// a "would be unloadable" error that is false. The byte-identical pair — same
// url under the same hwid, the empty value included — stays refused.
func TestWritePrivateFetchIdentityMatchesConfigLoad(t *testing.T) {
	t.Parallel()

	const (
		url  = "https://shared.example/sub"
		hwid = "abcdef0123456789"
	)
	write := func(sources ...source) error {
		var pf privateFile
		pf.Subscriptions.Sources = sources
		return writePrivate(filepath.Join(t.TempDir(), "private.yaml"), pf)
	}
	if err := write(
		source{Name: "one", URL: url, HWID: hwid},
		source{Name: "two", URL: url, HWID: "abcdef012345678a"},
	); err != nil {
		t.Fatalf("same url under two hwids must write: %v", err)
	}
	// Empty vs set is a distinct fetch too (no header vs header).
	if err := write(
		source{Name: "one", URL: url},
		source{Name: "two", URL: url, HWID: hwid},
	); err != nil {
		t.Fatalf("same url under empty vs set hwid must write: %v", err)
	}
	for name, sources := range map[string][]source{
		"same hwid": {
			{Name: "one", URL: url, HWID: hwid},
			{Name: "two", URL: url, HWID: hwid},
		},
		"no hwid": {
			{Name: "one", URL: url},
			{Name: "two", URL: url},
		},
	} {
		err := write(sources...)
		if err == nil {
			t.Errorf("%s: the byte-identical fetch pair must be refused", name)
			continue
		}
		if !strings.Contains(err.Error(), "already fetched as") {
			t.Errorf("%s: error %q must name the duplicate fetch", name, err)
		}
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
			pages, lost, _, _ := c.scrapeChat(context.Background(), chanRef{slug: "chan"}, tc.budget)
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

// testDeadTTL is any positive window: the memory is on, and every test that
// cares about expiry sets its own stamps rather than waiting one out. The
// production default lives in main (CRAWL_DEAD_TTL), not in this package.
const testDeadTTL = 720 * time.Hour

// TestRunOnceWithholdingIsNotADeletion pins the half of the deny funnel that no
// other test can see: a withholding must not be counted as a deletion, or a
// curated set broadened over URLs the crawler already mirrors would trip the
// bulk-prune floor and refuse the write — turning an operator action into a
// two-cycle standoff. 15 of 20 managed URLs go at once here, which is over both
// arms of the floor (minDrop 10 AND 30%).
func TestRunOnceWithholdingIsNotADeletion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	var pf privateFile
	var curated, page strings.Builder
	curated.WriteString("subscriptions:\n  sources:\n")
	for i := range 20 {
		u := fmt.Sprintf("https://panel-%02d.example/sub", i)
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources,
			source{Name: fmt.Sprintf("chan-%d", i), URL: u, Managed: true})
		fmt.Fprintf(&page, `<a href="%s">x</a> `, u)
		if i < 15 {
			fmt.Fprintf(&curated, "    - name: curated-%02d\n      url: %s\n", i, u)
		}
	}
	if err := writePrivate(priv, pf); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(dir, "sources.yaml")
	if err := os.WriteFile(sources, []byte(curated.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")

	c := &Crawler{
		opts: Options{
			PrivatePath:  priv,
			Channels:     []string{"chan"},
			CuratedPaths: []string{sources},
			StatePath:    statePath,
			DeadTTL:      testDeadTTL,
			Pages:        1,
			Prune:        true,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page.String()}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	got, _, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if n := len(got.Subscriptions.Sources); n != 5 {
		t.Errorf("sources = %d, want the 5 uncurated URLs: the write must go through, not wait for a second cycle", n)
	}
	for _, s := range got.Subscriptions.Sources {
		if i, convErr := strconv.Atoi(s.URL[len("https://panel-") : len("https://panel-")+2]); convErr != nil || i < 15 {
			t.Errorf("a curated URL survived the merge: %s", s.URL)
		}
	}
	if st := loadState(statePath, zerolog.Nop()); len(st.BulkPruneURLs) != 0 {
		t.Errorf("the floor recorded a refused proposal %v; a withholding is not a deletion", st.BulkPruneURLs)
	}
}

// TestClassifyAllRecordsSkipsAndVerdictsSafely covers the skip branch's lock.
// It runs on the CALLER goroutine inside the loop that spawns the workers, so it
// touches rejects while a worker from an earlier iteration may be inside
// rej.record — a lost write there is `fatal error: concurrent map writes` in
// production, a crashed crawler rather than a wrong verdict. Every other fixture
// classifies instantly, so the two never overlap and the race is invisible.
func TestClassifyAllRecordsSkipsAndVerdictsSafely(t *testing.T) {
	t.Parallel()

	const n = 64
	urls := make([]string, 0, 2*n)
	dead := make(map[string]time.Time, n)
	for i := range n {
		slow := fmt.Sprintf("https://slow-%02d.example/sub", i)
		gone := fmt.Sprintf("https://gone-%02d.example/sub", i)
		urls = append(urls, slow, gone)
		dead[gone] = time.Now().Add(time.Hour)
	}
	c := &Crawler{
		opts: Options{DeadTTL: testDeadTTL},
		classifyFn: func(context.Context, *http.Client, fetch.SubscriptionURL, string) (classify.Result, error) {
			time.Sleep(time.Millisecond)
			return classify.Result{}, &classify.StatusError{Code: http.StatusServiceUnavailable, Status: "503 Service Unavailable"}
		},
		logger: zerolog.Nop(),
	}
	rej := newRejects(zerolog.Nop())

	_, unknown, deadOut := c.classifyAll(context.Background(), urls, dead, rej, "chan", nil, nil)

	if len(unknown) != n {
		t.Errorf("unknown = %d, want %d: a 503 proves nothing about the subscription", len(unknown), n)
	}
	if len(deadOut) != 0 {
		t.Errorf("deadOut = %v, want empty: a skipped URL produces no fresh verdict and a 503 is not one", deadOut)
	}
	if len(rej.verdict) != 2*n {
		t.Errorf("rej.verdict = %d, want %d: a lost write dropped a record", len(rej.verdict), 2*n)
	}
	if rej.noted != n {
		t.Errorf("noted = %d, want %d withheld from the line budget", rej.noted, n)
	}
}

// TestRunOnceWithholdsCuratedURLs pins the successor to CRAWL-7's blocked list:
// a URL the operator already curates by hand is never mirrored into the managed
// corpus, however live it classifies, while its neighbour on the same page is
// harvested as usual. Withholding is not a death verdict, so it must leave no
// dead record behind — the two mechanisms have different revocation paths.
func TestRunOnceWithholdsCuratedURLs(t *testing.T) {
	t.Parallel()

	const (
		curatedURL = "https://curated.example/sub"
		freshURL   = "https://fine.example/sub"
	)
	dir := t.TempDir()
	priv := writeEmptyPrivate(t, dir)
	statePath := filepath.Join(dir, "state.json")
	sources := filepath.Join(dir, "sources.yaml")
	if err := os.WriteFile(sources,
		[]byte("subscriptions:\n  sources:\n    - name: curated-one\n      url: "+curatedURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	page := `<a href="` + curatedURL + `">a</a> <a href="` + freshURL + `">b</a>`
	c := &Crawler{
		opts: Options{
			PrivatePath:  priv,
			Channels:     []string{"chan"},
			CuratedPaths: []string{sources},
			StatePath:    statePath,
			DeadTTL:      testDeadTTL,
			Pages:        1,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	got, _, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	urls := map[string]bool{}
	for _, s := range got.Subscriptions.Sources {
		urls[s.URL] = true
	}
	if urls[curatedURL] {
		t.Errorf("a curated URL was mirrored anyway: %+v", got.Subscriptions.Sources)
	}
	if !urls[freshURL] {
		t.Errorf("the uncurated URL must still be harvested, got %+v", got.Subscriptions.Sources)
	}
	if st := loadState(statePath, zerolog.Nop()); len(st.Dead) != 0 {
		t.Errorf("withholding recorded a dead stamp %v; only a definitive verdict may", st.Dead)
	}
}

// TestRunOnceRemembersDeadAndSkipsRefetch pins the whole point of the dead
// memory: a definitively gone URL costs exactly one classify request, ever.
// The second cycle must not spend one on it even though the channel still
// advertises the link, while a live sibling on the same page is unaffected.
func TestRunOnceRemembersDeadAndSkipsRefetch(t *testing.T) {
	t.Parallel()

	const (
		goneURL = "https://gone.example/sub"
		liveURL = "https://alive.example/sub"
	)
	dir := t.TempDir()
	priv := writeEmptyPrivate(t, dir)
	statePath := filepath.Join(dir, "state.json")
	var goneFetches, liveFetches atomic.Int64
	page := `<a href="` + goneURL + `">a</a> <a href="` + liveURL + `">b</a>`
	c := &Crawler{
		opts: Options{
			PrivatePath: priv,
			Channels:    []string{"chan"},
			StatePath:   statePath,
			DeadTTL:     testDeadTTL,
			Pages:       1,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if string(u) == goneURL {
				goneFetches.Add(1)
				return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
			}
			liveFetches.Add(1)
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())
	st := loadState(statePath, zerolog.Nop())
	if _, ok := st.Dead[goneURL]; !ok {
		t.Fatalf("a 404 must be remembered, got Dead = %v", st.Dead)
	}
	if _, ok := st.Dead[liveURL]; ok {
		t.Errorf("a live URL must never be stamped dead, got Dead = %v", st.Dead)
	}

	c.RunOnce(context.Background())
	if n := goneFetches.Load(); n != 1 {
		t.Errorf("the remembered URL was classified %d times, want exactly the 1 that condemned it", n)
	}
	if n := liveFetches.Load(); n != 2 {
		t.Errorf("the live URL was classified %d times, want 1 per cycle: the skip must not spill onto it", n)
	}
}

// TestRunOnceDarkCycleRecordsNoDead pins the fault gate over the new memory. A
// broken egress that answers 404 for everything is exactly the shape dark()
// exists to catch, and stamping that cycle's verdicts would withhold the whole
// corpus for a month over one outage.
func TestRunOnceDarkCycleRecordsNoDead(t *testing.T) {
	t.Parallel()

	const (
		managedURL = "https://managed.example/sub"
		pageURL    = "https://candidate.example/sub"
	)
	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources:\n    - name: chan-1\n      url: "+
		managedURL+"\n      managed: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	c := &Crawler{
		opts: Options{
			PrivatePath: priv,
			Channels:    []string{"chan"},
			StatePath:   statePath,
			DeadTTL:     testDeadTTL,
			Pages:       1,
			Prune:       true,
		},
		client: pageFetcher{pages: map[string]string{
			"https://t.me/s/chan": `<a href="` + pageURL + `">a</a>`,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	if st := loadState(statePath, zerolog.Nop()); len(st.Dead) != 0 {
		t.Errorf("a dark cycle recorded %v; its not-live answers are evidence about the crawler, not any source", st.Dead)
	}
}

// TestRunOnceEmptyCorpusRecordsNoDead covers the hole dark() structurally cannot
// see: it needs rr.checked > 0, so a fresh deploy (or one rebuilt after losing
// private.yaml) behind an egress that 404s everything has no recheck to prove
// anything with. Stamping there is self-concealing — later healthy cycles skip
// those URLs without a request — so recording requires a live answer somewhere,
// not merely the absence of a dark verdict.
func TestRunOnceEmptyCorpusRecordsNoDead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := writeEmptyPrivate(t, dir)
	statePath := filepath.Join(dir, "state.json")
	c := &Crawler{
		opts: Options{
			PrivatePath: priv,
			Channels:    []string{"chan"},
			StatePath:   statePath,
			DeadTTL:     testDeadTTL,
			Pages:       1,
			Prune:       true,
		},
		client: pageFetcher{pages: map[string]string{
			"https://t.me/s/chan": `<a href="https://a.example/sub">a</a> <a href="https://b.example/sub">b</a>`,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{}, &classify.StatusError{Code: http.StatusNotFound, Status: "404 Not Found"}
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	if st := loadState(statePath, zerolog.Nop()); len(st.Dead) != 0 {
		t.Errorf("a cycle that proved nothing live recorded %v; nothing in it showed the egress works", st.Dead)
	}
}

// TestRejectSummaryExcludesNotedFromSuppressed: `suppressed` answers "how many
// per-candidate lines did the cycle cap withhold". A noteDead record spends no
// line by design, so counting it there would report hundreds of withheld lines
// nobody ever wanted — a wrong number replacing the one this fix removed.
func TestRejectSummaryExcludesNotedFromSuppressed(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	rej := newRejects(zerolog.New(&buf))
	for i := range 5 {
		rej.noteDead(fmt.Sprintf("https://gone-%d.example/sub", i))
	}
	rej.report(map[string]origin{})

	out := buf.String()
	if strings.Contains(out, "suppressed") {
		t.Errorf("withholdings nobody logged were reported as suppressed lines:\n%s", out)
	}
	if !strings.Contains(out, `"dead":5`) {
		t.Errorf("the per-reason summary must still count them:\n%s", out)
	}
}

// TestRunOnceReclassifiesAfterDeadRecordExpiry pins the revival route: expiry is
// the whole mechanism by which a panel that came back re-enters, and it must
// need no operator edit.
func TestRunOnceReclassifiesAfterDeadRecordExpiry(t *testing.T) {
	t.Parallel()

	const revivedURL = "https://revived.example/sub"
	dir := t.TempDir()
	priv := writeEmptyPrivate(t, dir)
	statePath := filepath.Join(dir, "state.json")
	seed := state{
		Productive: map[string]channelState{},
		Dead:       map[string]time.Time{revivedURL: time.Now().Add(-time.Minute)},
	}
	if err := saveState(statePath, seed); err != nil {
		t.Fatal(err)
	}

	c := &Crawler{
		opts: Options{
			PrivatePath: priv,
			Channels:    []string{"chan"},
			StatePath:   statePath,
			DeadTTL:     testDeadTTL,
			Pages:       1,
		},
		client: pageFetcher{pages: map[string]string{
			"https://t.me/s/chan": `<a href="` + revivedURL + `">a</a>`,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	got, _, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if len(got.Subscriptions.Sources) != 1 || got.Subscriptions.Sources[0].URL != revivedURL {
		t.Errorf("an expired record must not withhold: sources = %+v", got.Subscriptions.Sources)
	}
	if st := loadState(statePath, zerolog.Nop()); len(st.Dead) != 0 {
		t.Errorf("the expired record must be gone, got %v", st.Dead)
	}
}

// TestRunOnceDeadTTLZeroWithholdsNothing pins the off switch as documented in
// README and sources.md: DeadTTL 0 must stop WITHHOLDING as well as recording,
// or records nothing will ever re-stamp go on suppressing fetches for as long
// as their old expiry runs.
func TestRunOnceDeadTTLZeroWithholdsNothing(t *testing.T) {
	t.Parallel()

	const heldURL = "https://held.example/sub"
	dir := t.TempDir()
	priv := writeEmptyPrivate(t, dir)
	statePath := filepath.Join(dir, "state.json")
	// Unexpired by a wide margin: only the off switch can admit this URL.
	if err := saveState(statePath, state{
		Productive: map[string]channelState{},
		Dead:       map[string]time.Time{heldURL: time.Now().Add(24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	var fetches atomic.Int64
	c := &Crawler{
		opts: Options{
			PrivatePath: priv,
			Channels:    []string{"chan"},
			StatePath:   statePath,
			DeadTTL:     0,
			Pages:       1,
		},
		client: pageFetcher{pages: map[string]string{
			"https://t.me/s/chan": `<a href="` + heldURL + `">a</a>`,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			fetches.Add(1)
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	if n := fetches.Load(); n != 1 {
		t.Errorf("classify calls = %d, want 1: a held record must not withhold once the memory is off", n)
	}
	got, _, err := loadPrivate(priv)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if len(got.Subscriptions.Sources) != 1 || got.Subscriptions.Sources[0].URL != heldURL {
		t.Errorf("sources = %+v, want the URL harvested", got.Subscriptions.Sources)
	}
}

// TestRunOnceKeepsChannelMemoryOnMidRecheckAbort pins the save the scan earns.
// The recheck is network-bound over the whole managed corpus and cycleBudget
// aborts exactly there on a crawl whose fan-out outgrew its interval, so a save
// that waited for the merge would discard the productive-channel memory the scan
// had just learned — memory nothing reconstructs, and every entry of which is a
// depth-0 seed for later cycles. The recheck's own half-collected verdicts are
// deliberately NOT saved: a partial cycle is never evidence a source died.
func TestRunOnceKeepsChannelMemoryOnMidRecheckAbort(t *testing.T) {
	t.Parallel()

	const (
		managedURL = "https://managed.example/sub"
		freshURL   = "https://fresh.example/sub"
	)
	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources:\n    - name: chan-1\n      url: "+
		managedURL+"\n      managed: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Crawler{
		opts: Options{
			PrivatePath: priv,
			Channels:    []string{"chan"},
			StatePath:   statePath,
			StateTTL:    720 * time.Hour,
			DeadTTL:     testDeadTTL,
			Pages:       1,
			Prune:       true,
		},
		client: pageFetcher{pages: map[string]string{
			"https://t.me/s/chan": `<a href="` + freshURL + `">a</a>`,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if string(u) == managedURL {
				// The recheck: the budget expiring here is the real case.
				cancel()
				return classify.Result{}, context.Canceled
			}
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(ctx)

	st := loadState(statePath, zerolog.Nop())
	if len(st.Productive) == 0 {
		t.Error("the scan's channel memory was discarded by an abort in the recheck; nothing reconstructs it")
	}
	if len(st.Dead) != 0 {
		t.Errorf("a partial cycle recorded %v; its verdicts are not evidence", st.Dead)
	}
}

// TestScrapeChannelForumTopic pins the fetch shape a seed's topic selects and the
// outcomes the log has to keep apart. A group answers t.me/s/ with 200 and zero
// messages, so "read a listing and found nothing in it" must never be reported
// as "never reachable" — and a topic has no cursor to lose, which keeps forum
// seeds out of the fleet-wide ratio reportCursors alarms on.
func TestScrapeChannelForumTopic(t *testing.T) {
	t.Parallel()

	const (
		topicURL = "https://t.me/forumchat/1310" + topicQuery
		listURL  = "https://t.me/s/forumchat"
	)
	withMessage := `<div class="tgme_widget_message_wrap"><a href="https://sub.example/x">x</a></div>`
	noMessage := `<html><body>a group has a page, just no message on it</body></html>`

	cases := []struct {
		name       string
		ref        chanRef
		budget     int
		pages      map[string]string
		errs       map[string]error
		wantPages  int
		wantLog    string
		wantNotLog string
	}{
		{
			name:      "forum topic",
			ref:       chanRef{slug: "forumchat", topic: "1310"},
			budget:    6,
			pages:     map[string]string{topicURL: withMessage},
			wantPages: 1,
		},
		{
			name:      "same chat without a topic keeps the t.me/s/ shape",
			ref:       chanRef{slug: "forumchat"},
			budget:    1,
			pages:     map[string]string{listURL: withMessage},
			wantPages: 1,
		},
		{
			name:       "reachable but message-less",
			ref:        chanRef{slug: "forumchat", topic: "1310"},
			budget:     6,
			pages:      map[string]string{topicURL: noMessage},
			wantPages:  0,
			wantLog:    "carried no message",
			wantNotLog: "fetch failed",
		},
		{
			name:       "never reachable",
			ref:        chanRef{slug: "forumchat", topic: "1310"},
			budget:     6,
			errs:       map[string]error{topicURL: errors.New("bad status: 404 Not Found")},
			wantPages:  0,
			wantLog:    "fetch failed",
			wantNotLog: "carried no message",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var logBuf bytes.Buffer
			c := &Crawler{
				client: pageFetcher{pages: tc.pages, errs: tc.errs},
				logger: zerolog.New(&logBuf),
			}
			pages, lost, _, _ := c.scrapeChat(context.Background(), tc.ref, tc.budget)
			logged := logBuf.String()
			if len(pages) != tc.wantPages {
				t.Fatalf("pages = %d, want %d (log: %s)", len(pages), tc.wantPages, logged)
			}
			if lost {
				t.Errorf("cursorLost = true with %d of %d pages; nothing here can lose a cursor", len(pages), tc.budget)
			}
			assertTopicLog(t, logged, tc.wantLog, tc.wantNotLog)
		})
	}
}

// assertTopicLog checks the outcome a forum-seed scrape reported. An empty
// wantLog means the scrape produced a page, and a quiet log is the assertion:
// the two failure warns must be distinguishable from each other, and both must
// name the seed the way the operator wrote it, topic included.
func assertTopicLog(t *testing.T, logged, wantLog, wantNotLog string) {
	t.Helper()
	if wantLog == "" {
		if logged != "" {
			t.Errorf("a listing that yielded a page must stay quiet, logged: %s", logged)
		}
		return
	}
	if !strings.Contains(logged, wantLog) {
		t.Errorf("log %s does not carry %q", logged, wantLog)
	}
	if strings.Contains(logged, wantNotLog) {
		t.Errorf("log %s carries %q, the other outcome", logged, wantNotLog)
	}
	if !strings.Contains(logged, `"channel":"forumchat/1310"`) {
		t.Errorf("log %s must name the seed as the operator wrote it", logged)
	}
}

// TestScrapeChannelPermalinkSeedWalksTheListing pins the other half of the seed
// form channels.yaml and routes.md advertise: a plain channel permalink,
// t.me/<chan>/<msgid>, is byte-for-byte the shape of <chat>/<topic>, so the
// topic id parsed out of one must cost the seed nothing. The discussion embed
// cannot be the probe that separates them — measured 2026-08-14, a channel
// post's embed answers 200 with ONE message wrap, so reading it first passes and
// silently replaces a 20-messages-per-page listing walk with a single message.
// The /s/ answer is the probe: 20 wraps for a channel, none for a group.
func TestScrapeChannelPermalinkSeedWalksTheListing(t *testing.T) {
	t.Parallel()

	const (
		entry    = "https://t.me/dailyv2ry/1234"
		topicURL = "https://t.me/dailyv2ry/1234" + topicQuery
		listURL  = "https://t.me/s/dailyv2ry"
	)
	hits := map[string]int{}
	var logBuf bytes.Buffer
	c := &Crawler{
		client: pageFetcher{
			hits: hits,
			pages: map[string]string{
				topicURL:                 `<div class="tgme_widget_message_wrap">the permalinked post, alone</div>`,
				listURL:                  `<div class="tgme_widget_message_wrap"></div><div data-post="dailyv2ry/3631"></div>`,
				listURL + "?before=3631": `<div class="tgme_widget_message_wrap"></div>`,
			},
		},
		logger: zerolog.New(&logBuf),
	}
	pages, lost, _, _ := c.scrapeChat(context.Background(), parseSeed(entry), 6)
	logged := logBuf.String()
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want the 2 the listing walk yields (log: %s)", len(pages), logged)
	}
	if got := hits[topicURL]; got != 0 {
		t.Errorf("the discussion embed was fetched %d time(s); a channel with a listing must never be read through it", got)
	}
	if got := hits[listURL]; got != 1 {
		t.Errorf("t.me/s/dailyv2ry requested %d times, want 1", got)
	}
	if !lost {
		t.Error("cursorLost = false; the last listing page carried no cursor with budget left, which is a real loss and this seed's vote is as good as any channel's")
	}
	if logged != "" {
		t.Errorf("a seed read through its listing must stay quiet, logged: %s", logged)
	}
}

// TestScrapeChannelGroupSeedFallsBackToItsTopic pins the inversion from the
// other side, the case the whole feature exists for: a group answers t.me/s/
// with 200 and zero message wraps (measured 2026-08-14 on the shipped seed's
// chat), which is not a page to harvest and not an error to log. That empty
// listing must not be counted as a page, must not vote in cursorStats — it has
// no cursor either, so a group seed would otherwise report a 100% loss every
// cycle — and the topic has to be read instead.
func TestScrapeChannelGroupSeedFallsBackToItsTopic(t *testing.T) {
	t.Parallel()

	const (
		topicURL = "https://t.me/forumchat/1310" + topicQuery
		listURL  = "https://t.me/s/forumchat"
		subURL   = "https://sub.example/today"
	)
	hits := map[string]int{}
	var logBuf bytes.Buffer
	c := &Crawler{
		client: pageFetcher{
			hits: hits,
			pages: map[string]string{
				listURL:  `<html><body>a group has a page, just no message on it</body></html>`,
				topicURL: `<div class="tgme_widget_message_wrap"><a href="` + subURL + `">x</a></div>`,
			},
		},
		logger: zerolog.New(&logBuf),
	}
	pages, lost, _, _ := c.scrapeChat(context.Background(), parseSeed("forumchat/1310"), 6)
	logged := logBuf.String()
	if len(pages) != 1 || !strings.Contains(pages[0], subURL) {
		t.Fatalf("pages = %v, want exactly the topic listing (log: %s)", pages, logged)
	}
	if got := hits[listURL]; got != 1 {
		t.Errorf("t.me/s/forumchat requested %d times, want the 1 probe", got)
	}
	if got := hits[topicURL]; got != 1 {
		t.Errorf("topic listing requested %d times, want 1", got)
	}
	if lost {
		t.Error("cursorLost = true; a group's message-less listing is not a channel that lost its cursor")
	}
	if logged != "" {
		t.Errorf("a group seed read through its topic is routine, logged: %s", logged)
	}
}

// TestScrapeChannelListingFetchFailureDoesNotReadTheTopic pins the other half of
// the dispatch: only a listing that was REACHED and carried no message is the
// group case. A 429 says nothing about the seed's shape, and the topic read is
// a second fetch aimed at the host that just refused the first — per seed, per
// cycle. The embed here would answer with a message wrap, so a fetch count of
// zero can only come from the failure being distinguished from an empty page.
func TestScrapeChannelListingFetchFailureDoesNotReadTheTopic(t *testing.T) {
	t.Parallel()

	const (
		topicURL = "https://t.me/forumchat/1310" + topicQuery
		listURL  = "https://t.me/s/forumchat"
		subURL   = "https://sub.example/today"
	)
	hits := map[string]int{}
	var logBuf bytes.Buffer
	c := &Crawler{
		client: pageFetcher{
			hits: hits,
			pages: map[string]string{
				topicURL: `<div class="tgme_widget_message_wrap"><a href="` + subURL + `">x</a></div>`,
			},
			errs: map[string]error{listURL: errors.New("bad status: 429 Too Many Requests")},
		},
		logger: zerolog.New(&logBuf),
	}
	pages, lost, _, _ := c.scrapeChat(context.Background(), parseSeed("forumchat/1310"), 6)
	logged := logBuf.String()
	if got := hits[topicURL]; got != 0 {
		t.Errorf("the discussion embed was fetched %d time(s); a host that just failed the listing must not be asked again", got)
	}
	if got := hits[listURL]; got != 1 {
		t.Errorf("t.me/s/forumchat requested %d times, want the 1 that failed", got)
	}
	if len(pages) != 0 {
		t.Errorf("pages = %v, want none from a listing that never came back", pages)
	}
	if lost {
		t.Error("cursorLost = true; a fetch that failed read no page and so cannot have lost its cursor")
	}
	if !strings.Contains(logged, "channel listing page fetch failed") || !strings.Contains(logged, `"url":"`+listURL+`"`) {
		t.Errorf("log %s must name the listing as what failed", logged)
	}
	if strings.Contains(logged, "carried no message") || strings.Contains(logged, "topic") {
		t.Errorf("log %s blames the topic for a listing failure", logged)
	}
}

// TestScanCrawlsAChatOnceWhateverItsSeedShape pins the cross-channel crawl
// identity: the slug. A chat seeded with a forum topic and reposted bare by
// another channel is one target, so t.me/s/<chat> is fetched exactly once —
// the probe that tells a group from a channel. The second, bare visit is the
// damaging one: it keeps the group's message-less listing as a page, and a
// page with no cursor and budget left is a cursor loss, so a chat already read
// through its topic would report a 100% loss into cursorStats. A visit never
// made is a vote never cast. (Same-slug topic SEEDS are the deliberate
// exception: one depth-0 seed per remembered topic, pinned by
// TestScanSeedsEveryRememberedForumTopic.)
func TestScanCrawlsAChatOnceWhateverItsSeedShape(t *testing.T) {
	t.Parallel()

	const (
		topicURL = "https://t.me/forumchat/1310" + topicQuery
		listURL  = "https://t.me/s/forumchat"
		otherURL = "https://t.me/s/otherchan"
		subURL   = "https://sub.example/x"
	)
	hits := map[string]int{}
	var logBuf bytes.Buffer
	c := &Crawler{
		opts: Options{Channels: []string{"forumchat/1310", "otherchan"}, Pages: 6, MaxDepth: 2},
		client: pageFetcher{
			hits: hits,
			pages: map[string]string{
				topicURL: `<div class="tgme_widget_message_wrap"><a href="` + subURL + `">today</a></div>`,
				otherURL: `<div class="tgme_widget_message_wrap"><a href="https://t.me/forumchat/9">repost</a></div>`,
				listURL:  `<html><body>a group listing carries no message and no cursor</body></html>`,
			},
		},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}
	st := state{Productive: map[string]channelState{}}

	live, _, _ := c.scan(context.Background(), &st, nil)
	logged := logBuf.String()
	if got := hits[listURL]; got != 1 {
		t.Errorf("t.me/s/forumchat fetched %d time(s), want the 1 group probe; a second visit would vote a false cursor loss", got)
	}
	if got := strings.Count(logged, `"channel":"forumchat`); got != 1 {
		t.Errorf("forumchat scanned %d times, want 1 (log: %s)", got, logged)
	}
	if live[subURL].Slug != "forumchat" {
		t.Errorf("live = %v, want %s attributed to the bare chat", live, subURL)
	}
}

// TestScanSeedsEveryRememberedForumTopic drives finding 1's fix end to end: a
// group remembered productive in several topics must be re-seeded on EVERY one
// of them. The slug-keyed buildSeeds used to collapse the topics into one seed
// chosen by the productive map's range order, so N-1 topics were silently
// dropped from the cycle (and aged out of the state as if they had gone
// stale); now each topic's embed is fetched exactly once per cycle, whatever
// the map order, and the bare configured slug is absorbed — the group's
// message-less listing is fetched once per topic probe and never scanned on
// its own.
func TestScanSeedsEveryRememberedForumTopic(t *testing.T) {
	t.Parallel()

	const listURL = "https://t.me/s/forumchat"
	topics := []string{"7", "9", "11"}
	pages := map[string]string{listURL: carveJoinCard}
	for _, topic := range topics {
		pages["https://t.me/forumchat/"+topic+topicQuery] = wrapMsg("https://sub.example/topic-" + topic)
	}
	for run := range 2 { // the admission must not depend on seed-map range order
		hits := map[string]int{}
		st := state{Productive: map[string]channelState{}}
		for _, topic := range topics {
			st.record("forumchat/"+topic, time.Now())
		}
		c := &Crawler{
			opts:   Options{Channels: []string{"forumchat"}, Pages: 3, MaxDepth: 0},
			client: pageFetcher{hits: hits, pages: pages},
			classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
				return classify.Result{Nodes: 1}, nil
			},
			logger: zerolog.Nop(),
		}
		live, _, _ := c.scan(context.Background(), &st, nil)
		for _, topic := range topics {
			embed := "https://t.me/forumchat/" + topic + topicQuery
			if got := hits[embed]; got != 1 {
				t.Errorf("run %d: topic %s embed fetched %d time(s), want exactly 1 — every remembered topic must be seeded", run, topic, got)
			}
			if _, ok := st.Productive["forumchat/"+topic]; !ok {
				t.Errorf("run %d: productive memory lost the %s topic it was re-seeded from", run, topic)
			}
		}
		if got := hits[listURL]; got != len(topics) {
			t.Errorf("run %d: listing fetched %d time(s), want %d — one probe per topic seed and no standalone bare-slug scan", run, got, len(topics))
		}
		if len(live) != len(topics) {
			t.Errorf("run %d: live = %+v, want one subscription per topic", run, live)
		}
	}
}

// TestRunOnceHarvestsForumTopic drives a whole cycle over the seed form an
// operator writes for a forum topic. The listing's links go through the ordinary
// harvest, Telegram's own hosts are not candidates, the source is attributed to
// the bare chat (a managed name is permanent, a topic id is not), and the
// productive ref is remembered WITH its topic — remembering the bare chat would
// re-seed the next cycle with the t.me/s/ shape that answers a group nothing.
func TestRunOnceHarvestsForumTopic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := writeEmptyPrivate(t, dir)
	statePath := filepath.Join(dir, ".crawler-state.json")
	const subURL = "https://sub.example/rotating"
	page := `<div class="tgme_widget_message_wrap">` +
		`<a href="https://t.me/forumchat/1310/21206">permalink</a>` +
		`<script src="https://oauth.tg.dev/js/telegram-widget.js?24"></script>` +
		`<a href="` + subURL + `">today's link</a></div>`

	c := &Crawler{
		opts: Options{
			Channels:    []string{"forumchat/1310"},
			PrivatePath: priv,
			StatePath:   statePath,
			StateTTL:    30 * oneDay, // without a TTL the prune cutoff is now, and now-recorded memory is stale
			Pages:       6,
		},
		client: pageFetcher{pages: map[string]string{
			"https://t.me/forumchat/1310" + topicQuery: page,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if string(u) != subURL {
				t.Errorf("fetched %q; only the listing's external link is a candidate", u)
			}
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.Nop(),
	}

	c.RunOnce(context.Background())

	b, err := os.ReadFile(priv)
	if err != nil {
		t.Fatalf("read private.yaml: %v", err)
	}
	var pf privateFile
	if unmarshalErr := yaml.Unmarshal(b, &pf); unmarshalErr != nil {
		t.Fatalf("unmarshal private.yaml: %v", unmarshalErr)
	}
	if len(pf.Subscriptions.Sources) != 1 {
		t.Fatalf("want exactly the topic's one live link managed, got %+v", pf.Subscriptions.Sources)
	}
	got := pf.Subscriptions.Sources[0]
	if got.URL != subURL {
		t.Errorf("managed URL = %q, want %q", got.URL, subURL)
	}
	if !strings.HasPrefix(got.Name, "forumchat-") {
		t.Errorf("name = %q, want it attributed to the bare chat, forumchat- and no topic", got.Name)
	}
	if strings.Contains(got.Name, "1310") {
		t.Errorf("name = %q; a permanent name must not carry the topic id", got.Name)
	}

	st := loadState(statePath, zerolog.Nop())
	if _, ok := st.Productive["forumchat/1310"]; !ok {
		t.Errorf("productive memory = %+v, want the seed remembered with its topic", st.Productive)
	}
}

const (
	migrationChannels   = 46
	migrationEntries    = 463
	migrationOwned      = 456
	migrationSheltered  = 7
	migrationOriginless = 2
	// legacyTailHex is the per-URL tail the PRE-CUTOVER mint appended. The mint
	// stopped producing it — an ordinal took its place — but 454 live names wear
	// it forever, so the adoption fixtures below are built with it and the
	// stripping code stays.
	legacyTailHex = 6
)

type feedFanout struct {
	slug string
	urls int
}

// migrationFanout is the fan-out the pre-cutover corpus carried: 46 channels, 26
// of them with more than one URL, 450 attributed names in all
// (docs/guides/sources.md, reading of 2026-08-18). The slugs are shapes rather
// than channels — one begins with the legacy prefix, several end in a
// dash-digit run, one ends in six hex characters — because those are the names a
// parsing fold gets wrong.
func migrationFanout() []feedFanout {
	fanout := []feedFanout{
		{"feed-01", 75},
		{"file-vpn-2", 43},
		{"feed-03", 30},
		{"feed-04", 26},
		{"tg-vpn", 23},
		{"feed-2026", 21},
		{"feed-c0ffee", 20},
		{"feed-08", 19},
		{"feed-09", 18},
		{"feed-10", 17},
		{"feed-11", 16},
		{"feed-12", 15},
		{"collide-c", 14},
		{"feed-14", 13},
		{"feed-15", 12},
		{"feed-16", 11},
		{"collide-m", 10},
		{"feed-18", 9},
		{"feed-x-7", 8},
		{"feed-20", 6},
		{"feed-21", 5},
		{"feed-22", 5},
		{"feed-23", 4},
		{"feed-24", 4},
		{"feed-25", 3},
		{"feed-26", 3},
	}
	for i := len(fanout) + 1; i <= migrationChannels; i++ {
		fanout = append(fanout, feedFanout{slug: fmt.Sprintf("feed-%d", i), urls: 1})
	}
	return fanout
}

// migrationCorpus builds a pre-cutover private.yaml in the shape prod holds one
// — the attributed fan-out, the hand-added unprefixed entries beside it, the
// inline body entry, a legacy bare hash — and returns the file bytes with the
// exact list one loadPrivate must produce from them. The awkward members every
// other adoption test lacks are here: a slug that itself begins "tg-", curated
// names ending in digits, two stripped names that land on a name the file already
// holds (one hand-added, one already migrated), and the two prefixed names the
// pre-cutover mint could never have produced — a bare "tg-" and an unmarked
// post-cutover tg-vpn-123 — which must survive sheltered.
func migrationCorpus(t *testing.T) (fileBytes []byte, want []source) {
	t.Helper()

	var pre privateFile
	pre.Subscriptions.Sources = make([]source, 0, migrationEntries)
	want = make([]source, 0, migrationEntries)
	add := func(before, after source) {
		pre.Subscriptions.Sources = append(pre.Subscriptions.Sources, before)
		want = append(want, after)
	}
	keep := func(s source) { add(s, s) }

	keep(source{Name: "commsub", URL: "https://hand.example/sub"})
	for _, ch := range migrationFanout() {
		for i := range ch.urls {
			u := fmt.Sprintf("https://%s.example/sub/%d", ch.slug, i)
			name := ch.slug + "-" + urlHash(u, legacyTailHex)
			add(source{Name: legacyManagedPrefix + name, URL: u},
				source{Name: name, URL: u, Feed: ch.slug, Managed: true})
		}
	}

	keep(source{Name: legacyManagedPrefix, URL: "https://bare.example/sub"})

	dupHand := "https://collide-c.example/sub/dup"
	dupHandName := "collide-c-" + urlHash(dupHand, legacyTailHex)
	add(source{Name: legacyManagedPrefix + dupHandName, URL: dupHand},
		source{Name: managedName(dupHand), URL: dupHand, Feed: "collide-c", Managed: true})
	keep(source{Name: dupHandName, URL: "https://hand.example/collide"})

	dupOwned := "https://collide-m.example/sub/dup"
	dupOwnedName := "collide-m-" + urlHash(dupOwned, legacyTailHex)
	add(source{Name: legacyManagedPrefix + dupOwnedName, URL: dupOwned},
		source{Name: managedName(dupOwned), URL: dupOwned, Feed: "collide-m", Managed: true})
	keep(source{Name: dupOwnedName, URL: "https://collide-m.example/sub/other", Feed: "collide-m", Managed: true})

	hashOnly := "https://hashonly.example/sub"
	add(source{Name: legacyManagedPrefix + "a1b2c3d4e5", URL: hashOnly},
		source{Name: "a1b2c3d4e5", URL: hashOnly, Managed: true})

	body := base64.StdEncoding.EncodeToString([]byte("vless://node@203.0.113.7:443"))
	add(source{Name: legacyManagedPrefix + inlineSourceName, Body: body},
		source{Name: inlineSourceName, Body: body, Managed: true})

	keep(source{Name: "feed-01-4021", URL: "https://feed-01.example/sub/post", Feed: "feed-01", Managed: true})
	for _, n := range []string{"flat447", "file-vpn-2", "fastnodes-fi"} {
		keep(source{Name: n, URL: "https://" + n + ".example/sub"})
	}
	keep(source{Name: "tg-vpn-123", URL: "https://tg-vpn.example/sub/template"})

	if len(want) != migrationEntries {
		t.Fatalf("fixture holds %d entries, want %d", len(want), migrationEntries)
	}
	b, err := yaml.Marshal(pre)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b, want
}

// TestMigrationOverACorpusFixture runs the one-time adoption over that corpus:
// every other adoption test builds two or three entries by hand, which cannot
// show that the whole file survives one load, stays writable, and settles.
func TestMigrationOverACorpusFixture(t *testing.T) {
	t.Parallel()

	fileBytes, want := migrationCorpus(t)
	path := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(path, fileBytes, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	first, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if !first.adopted {
		t.Fatal("the corpus was not marked adopted, so an idle cycle would never write the migration out")
	}
	got := first.Subscriptions.Sources
	if len(got) != len(want) {
		t.Fatalf("kept %d entries, want all %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err = validatePrivate(first); err != nil {
		t.Fatalf("the migrated corpus is unwritable: %v", err)
	}
	checkMigratedShape(t, got)

	if err = writePrivate(path, first); err != nil {
		t.Fatalf("writePrivate: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the written file: %v", err)
	}
	second, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("second loadPrivate: %v", err)
	}
	if second.adopted {
		t.Error("the migration re-fired on the file it had just written")
	}
	if !slices.Equal(second.Subscriptions.Sources, want) {
		t.Errorf("second load = %+v, want the written list", second.Subscriptions.Sources)
	}
	if err = writePrivate(path, second); err != nil {
		t.Fatalf("second writePrivate: %v", err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read the written file: %v", err)
	}
	if !bytes.Equal(written, again) {
		t.Error("a later cycle rewrote the migrated file")
	}
	if n := managedCount(second); n != migrationOwned-1 {
		t.Errorf("managedCount = %d, want %d: every owned entry but the inline body", n, migrationOwned-1)
	}
}

// checkMigratedShape holds the properties the corpus must have after adoption:
// ownership on exactly the crawler's entries, a recovered feed wherever the old
// name named a channel, and nothing left for a second migration to claim.
func checkMigratedShape(t *testing.T, got []source) {
	t.Helper()

	owned, withFeed, sheltered := 0, 0, 0
	feeds := make(map[string]struct{}, migrationChannels)
	for _, s := range got {
		if needsAdoption(s) {
			t.Errorf("%q would be adopted again on the next load", s.Name)
		}
		if !s.Managed {
			sheltered++
			if s.Feed != "" {
				t.Errorf("sheltered %q was written feed %q", s.Name, s.Feed)
			}
			continue
		}
		owned++
		if s.Feed != "" {
			withFeed++
			feeds[s.Feed] = struct{}{}
		}
	}
	if owned != migrationOwned {
		t.Errorf("managed entries = %d, want %d", owned, migrationOwned)
	}
	if sheltered != migrationSheltered {
		t.Errorf("sheltered entries = %d, want %d", sheltered, migrationSheltered)
	}
	if owned-withFeed != migrationOriginless {
		t.Errorf("managed entries without a feed = %d, want %d: the legacy bare hash and the "+
			"inline body, the only two adopted names that never named a channel", owned-withFeed, migrationOriginless)
	}
	if len(feeds) != migrationChannels {
		t.Errorf("recovered feeds = %d, want one per channel (%d)", len(feeds), migrationChannels)
	}
	for _, n := range []string{"commsub", "flat447", "file-vpn-2", "fastnodes-fi", legacyManagedPrefix, "tg-vpn-123"} {
		i := slices.IndexFunc(got, func(s source) bool { return s.Name == n })
		if i < 0 {
			t.Errorf("sheltered %q was lost", n)
			continue
		}
		if got[i].Managed || got[i].Feed != "" {
			t.Errorf("sheltered %q = %+v, want it untouched and unmarked", n, got[i])
		}
	}
}

// TestMigrationCrashBeforeTheWrite: adoption happens in memory and writePrivate
// replaces the file only through an fsynced temp and a rename, so the window
// between them can lose the migration but cannot publish half of it. A lost
// write costs one replay; a refused one leaves the previous file intact.
func TestMigrationCrashBeforeTheWrite(t *testing.T) {
	t.Parallel()

	fileBytes, want := migrationCorpus(t)
	path := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(path, fileBytes, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	first, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read the fixture: %v", err)
	}
	if !bytes.Equal(onDisk, fileBytes) {
		t.Fatal("the load itself touched private.yaml, so a crash could expose a partial migration")
	}
	if err = os.WriteFile(path+".tmp", []byte("subscriptions:\n  sources:\n    - name: trunc"), 0o600); err != nil {
		t.Fatalf("plant a crashed write's temp file: %v", err)
	}
	replay, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("replay loadPrivate: %v", err)
	}
	if !replay.adopted || !slices.Equal(replay.Subscriptions.Sources, want) {
		t.Error("the next cycle did not reproduce the lost migration")
	}

	if err = writePrivate(path, first); err != nil {
		t.Fatalf("writePrivate: %v", err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the written file: %v", err)
	}
	doomed := first
	doomed.Subscriptions.Sources = append(slices.Clone(first.Subscriptions.Sources), first.Subscriptions.Sources[0])
	if writeErr := writePrivate(path, doomed); writeErr == nil {
		t.Error("writePrivate published a duplicate name")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read the written file: %v", err)
	}
	if !bytes.Equal(good, after) {
		t.Error("the refused write damaged the file it declined to replace")
	}
}

// TestLoadPrivateAdoptionSeparatesTwoEntriesOnOneURL: two legacy entries can name
// one URL after a hand edit, and if both strip onto names the file already holds
// they both fall to the same per-URL hash. That duplicate NAME would make
// validatePrivate refuse the write on this and every later cycle, so the second
// falls further, onto the hash of the name being migrated. The URL itself is
// still duplicated, and the file is now unwritable for that too: neither adopted
// entry carries a hwid, so the two perform the same header-less fetch, and
// config.Load refuses two sources sharing a url AND hwid — a write that
// published this shape would brick the next boot. The migration must refuse
// and leave the operator's duplicate for the hand that made it.
func TestLoadPrivateAdoptionSeparatesTwoEntriesOnOneURL(t *testing.T) {
	t.Parallel()

	const (
		shared = "https://shared.example/sub"
		seed   = "subscriptions:\n  sources:\n" +
			"    - name: dup-a1b2c3\n      url: https://hand.example/one\n" +
			"    - name: dup-d4e5f6\n      url: https://hand.example/two\n" +
			"    - name: tg-dup-a1b2c3\n      url: " + shared + "\n" +
			"    - name: tg-dup-d4e5f6\n      url: " + shared + "\n"
	)
	path := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	pf, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if err = validatePrivate(pf); err == nil || !strings.Contains(err.Error(), "already fetched as") {
		t.Fatalf("validatePrivate = %v, want the identical-fetch refusal (same url, neither entry carries a hwid) config.Load now applies", err)
	}
	first, second := pf.Subscriptions.Sources[2], pf.Subscriptions.Sources[3]
	if first.Name != managedName(shared) || !first.Managed || first.Feed != "dup" {
		t.Errorf("first adopted entry = %+v, want the per-URL hash %q, managed, feed dup", first, managedName(shared))
	}
	want := urlHash("tg-dup-d4e5f6", unattributedNameHex)
	if second.Name != want || !second.Managed || second.Feed != "dup" {
		t.Errorf("second adopted entry = %+v, want the migrated name's hash %q, managed, feed dup", second, want)
	}
}

// TestLoadPrivateSheltersPostCutoverNames: the mint still produces tg- names,
// because channelSlug folds "_" to "-" and the channel tg_vpn slugs to tg-vpn. An
// operator who copies such an entry as a template and leaves the field off must
// keep it: adopting on the prefix alone would seize the entry, mark it managed and
// hand it to the prune, which is the failure this wave exists to remove. Only a
// name wearing a shape the pre-cutover mint produced is claimed.
func TestLoadPrivateSheltersPostCutoverNames(t *testing.T) {
	t.Parallel()

	const seed = "subscriptions:\n  sources:\n" +
		"    - name: tg-vpn-123\n      url: https://template.example/sub\n" +
		"    - name: tg-\n      url: https://bare.example/sub\n" +
		"    - name: tg-notahash\n      url: https://hand.example/sub\n"
	path := filepath.Join(t.TempDir(), "private.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}

	pf, _, err := loadPrivate(path)
	if err != nil {
		t.Fatalf("loadPrivate: %v", err)
	}
	if pf.adopted {
		t.Error("the migration fired on a file holding no pre-cutover mint, forcing a write every cycle")
	}
	for _, s := range pf.Subscriptions.Sources {
		if s.Managed || s.Feed != "" {
			t.Errorf("entry %+v was claimed by the crawler", s)
		}
	}
	if got := pf.Subscriptions.Sources[0].Name; got != "tg-vpn-123" {
		t.Errorf("name = %q, want tg-vpn-123 kept verbatim", got)
	}
}

func writeCurated(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sources.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write sources.yaml: %v", err)
	}
	return path
}

// mergeWithCurated runs one cycle's merge over a fixed one-URL fixture, varying
// only the curated path list, so the tests below compare like with like.
func mergeWithCurated(t *testing.T, curatedPaths []string, o origin) ([]source, string) {
	t.Helper()
	const u = "https://host.example/sub"
	var buf bytes.Buffer
	c := &Crawler{opts: Options{Prune: true, CuratedPaths: curatedPaths}, logger: zerolog.New(&buf)}
	_, managed, _, _ := c.mergeManaged(privateFile{}, map[string]origin{u: o}, recheckResult{}, true, nil)
	return managed, buf.String()
}

// TestMergeAvoidsCuratedName pins the hazard the curated seeding exists for:
// the overlay already holds names in the mint's own shape, and one duplicate
// refuses the WHOLE merged config (config.go:1013), not the offending entry.
// The stray `enabled` key is part of the test — the crawler reads names and
// holds no opinion about the keys config.Load owns.
func TestMergeAvoidsCuratedName(t *testing.T) {
	t.Parallel()

	const curated = "chan-3631"
	path := writeCurated(t, "subscriptions:\n  sources:\n"+
		"    - name: "+curated+"\n      url: https://curated.example/sub\n      enabled: true\n")

	managed, _ := mergeWithCurated(t, []string{path}, origin{Slug: "chan", Post: 3631})
	if len(managed) != 1 {
		t.Fatalf("managed = %+v, want one entry", managed)
	}
	if managed[0].Name == curated {
		t.Fatalf("minted the curated name %q; the next config load refuses every source", curated)
	}
	// The mint walked past the taken stem rather than giving up on the slug.
	if !strings.HasPrefix(managed[0].Name, curated+"-") {
		t.Errorf("name = %q, want a %q descendant", managed[0].Name, curated)
	}

	// The same collision on an EXISTING entry is a different hole: sourceName
	// returns the incumbent name verbatim, so the mint never walks and the
	// duplicate must be caught when the entry is retained. The logger must
	// report it at error level, naming the entry, or a reload that freezes the
	// live config stays invisible.
	const urlExisting = "https://existing.example/sub"
	var buf bytes.Buffer
	filePath := writeCurated(t, "subscriptions:\n  sources:\n    - name: "+curated+"\n")
	live := map[string]origin{urlExisting: {Slug: "chan", Post: 3631}}
	c := &Crawler{opts: Options{Prune: true, CuratedPaths: []string{filePath}, PrivatePath: filePath}, logger: zerolog.New(&buf)}
	pf := privateFile{}
	pf.Subscriptions.Sources = []source{{Name: curated, URL: urlExisting, Managed: true}}
	_, managed2, _, _ := c.mergeManaged(pf, live, recheckResult{}, true, nil)
	if len(managed2) != 1 || managed2[0].Name != curated {
		t.Fatalf("retained = %+v, want the existing entry kept under its name", managed2)
	}
	if !strings.Contains(buf.String(), `"level":"error"`) || !strings.Contains(buf.String(), curated) {
		t.Errorf("the collision was not logged at error level naming the entry:\n%s", buf.String())
	}
}

// TestMergeAvoidsCuratedNameWithoutAPost covers the post-unknown family, whose
// first candidate is the small integer where hand-written names cluster
// (config/sources.yaml holds wepogp-1 and wepogp-4, read 2026-08-19).
func TestMergeAvoidsCuratedNameWithoutAPost(t *testing.T) {
	t.Parallel()

	const curated = "chan-1"
	path := writeCurated(t, "subscriptions:\n  sources:\n    - name: "+curated+"\n")

	managed, _ := mergeWithCurated(t, []string{path}, origin{Slug: "chan"})
	if len(managed) != 1 {
		t.Fatalf("managed = %+v, want one entry", managed)
	}
	if managed[0].Name == curated {
		t.Fatalf("minted the curated name %q; the next config load refuses every source", curated)
	}
	if !strings.HasPrefix(managed[0].Name, "chan-") {
		t.Errorf("name = %q, want it still attributed to chan", managed[0].Name)
	}
}

// TestMergeAvoidsCuratedNameInEitherPath: CRAWL_CURATED names sources.yaml and
// config.yaml, whose own subscriptions.sources list may hold names too, so a
// name reserved by one file must be reserved by the other — validateSources
// covers the whole merged list, and seeding one of the two would fail closed on
// the whole config for a name the crawler could see.
func TestMergeAvoidsCuratedNameInEitherPath(t *testing.T) {
	t.Parallel()

	const curated = "chan-3631"
	o := origin{Slug: "chan", Post: 3631}
	holder := "subscriptions:\n  sources:\n    - name: " + curated + "\n"
	other := writeCurated(t, "subscriptions:\n  sources:\n    - name: unrelated-9\n")

	inFirst, _ := mergeWithCurated(t, []string{writeCurated(t, holder), other}, o)
	inSecond, _ := mergeWithCurated(t, []string{other, writeCurated(t, holder)}, o)
	if len(inSecond) != 1 {
		t.Fatalf("managed = %+v, want one entry", inSecond)
	}
	if inSecond[0].Name == curated {
		t.Fatalf("minted %q held by the second path; the next config load refuses every source", curated)
	}
	if !slices.Equal(inFirst, inSecond) {
		t.Errorf("the name's position in the list changed the mint: %+v then %+v", inFirst, inSecond)
	}
}

// TestMergeCuratedSkipsMissingAndBlankPaths: neither an unmounted file nor an
// empty entry (CRAWL_CURATED=",/config/config.yaml") is an error, and neither
// may cost a later path its names.
func TestMergeCuratedSkipsMissingAndBlankPaths(t *testing.T) {
	t.Parallel()

	const curated = "chan-3631"
	o := origin{Slug: "chan", Post: 3631}
	present := writeCurated(t, "subscriptions:\n  sources:\n    - name: "+curated+"\n")
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	managed, logged := mergeWithCurated(t, []string{missing, "", present}, o)
	if len(managed) != 1 {
		t.Fatalf("managed = %+v, want one entry", managed)
	}
	if managed[0].Name == curated {
		t.Fatalf("a missing file and a blank entry ahead of %s cost it its names", present)
	}
	if logged != "" {
		t.Errorf("a missing file and a blank entry are normal, not warnings: %s", logged)
	}
}

// TestMergeCuratedFileFailuresKeepTheCycle: a missing overlay is normal and a
// malformed one must not stop discovery, so both yield what no curated file at
// all yields — the malformed one loudly, because falling back to private-only
// seeding is exactly what re-opens the collision hazard. What it must not do is
// take the paths beside it down with it.
func TestMergeCuratedFileFailuresKeepTheCycle(t *testing.T) {
	t.Parallel()

	o := origin{Slug: "chan", Post: 3631}
	baseline, quiet := mergeWithCurated(t, nil, o)
	if len(baseline) != 1 {
		t.Fatalf("baseline = %+v, want one entry", baseline)
	}
	if quiet != "" {
		t.Errorf("an unset curated path logged something: %s", quiet)
	}

	got, logged := mergeWithCurated(t, []string{filepath.Join(t.TempDir(), "nope.yaml")}, o)
	if !slices.Equal(got, baseline) {
		t.Errorf("a missing curated file changed the cycle: %+v, want %+v", got, baseline)
	}
	if logged != "" {
		t.Errorf("a missing curated file is normal, not a warning: %s", logged)
	}

	broken := writeCurated(t, "subscriptions:\n  sources:\n    - name: [oops\n")
	got, logged = mergeWithCurated(t, []string{broken}, o)
	if !slices.Equal(got, baseline) {
		t.Errorf("a malformed curated file changed the cycle: %+v, want %+v", got, baseline)
	}
	if !strings.Contains(logged, broken) || !strings.Contains(logged, "curated") {
		t.Errorf("the parse failure was not reported with its path: %s", logged)
	}

	sound := writeCurated(t, "subscriptions:\n  sources:\n    - name: "+baseline[0].Name+"\n")
	got, logged = mergeWithCurated(t, []string{broken, sound}, o)
	if len(got) != 1 {
		t.Fatalf("managed = %+v, want one entry", got)
	}
	if got[0].Name == baseline[0].Name {
		t.Errorf("the malformed path cost %s its names: minted %q", sound, got[0].Name)
	}
	if !strings.Contains(logged, broken) {
		t.Errorf("the parse failure was not reported with its path: %s", logged)
	}
}

// TestRunOnceLogsCuratedCount: a mis-set CRAWL_CURATED disables the seeding in
// silence, and this count on the cycle's terminal line is the only place 0
// against a shipped overlay holding dozens becomes visible. Deduped across the
// paths, because it reports the names the mint had to avoid, not the entries read.
func TestRunOnceLogsCuratedCount(t *testing.T) {
	t.Parallel()

	priv := filepath.Join(t.TempDir(), "private.yaml")
	first := writeCurated(t, "subscriptions:\n  sources:\n    - name: kept-1\n    - name: kept-2\n")
	second := writeCurated(t, "subscriptions:\n  sources:\n    - name: kept-2\n    - name: kept-3\n")

	var buf bytes.Buffer
	c := &Crawler{
		opts: Options{
			Channels:     []string{"chan"},
			PrivatePath:  priv,
			CuratedPaths: []string{first, "", second},
			Pages:        1,
		},
		client: pageFetcher{pages: map[string]string{
			"https://t.me/s/chan": `<div class="tgme_widget_message_wrap" data-post="chan/3631">` +
				`<a href="https://new.example/sub">sub</a></div>`,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&buf),
	}
	c.RunOnce(context.Background())
	var terminal map[string]any
	for _, m := range decodeLines(t, buf.String()) {
		if m["message"] == "private.yaml updated" {
			terminal = m
		}
	}
	if terminal == nil {
		t.Fatalf("the cycle wrote nothing, so the terminal line is untested:\n%s", buf.String())
	}
	if terminal["curated"] != float64(3) {
		t.Errorf("terminal line = %v, want curated=3 (kept-2 counted once)", terminal)
	}
}

// TestRunOnceRefusesToRebuildAnEmptyCorpus pins the content half of the
// absent-corpus refusal (LogicSources#4): a private.yaml that EXISTS but holds
// no managed URL sources — a truncation, `> private.yaml`, a failed external
// write — is the same empty corpus a missing file is, and rewriting it from one
// cycle's discoveries would wipe the harvested corpus and its liveness streaks
// exactly as the missing-file guard refuses to. Hand-added entries do not make
// a corpus: they are nobody's to prune, and st.Managed remembers only managed
// URLs, so a file holding only hand-added entries refuses the same way.
func TestRunOnceRefusesToRebuildAnEmptyCorpus(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty file", "subscriptions:\n  sources: []\n"},
		{"hand-added only", "subscriptions:\n  sources:\n    - name: mine\n      url: https://hand.example/sub\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertEmptyCorpusRefused(t, tc.body)
		})
	}
}

// assertEmptyCorpusRefused runs one cycle against a private.yaml holding no
// managed sources while the state remembers a managed corpus, and pins the
// refusal: the file stays untouched, the error is logged, the liveness memory
// survives — and a genuine first cycle over the same file still builds the
// corpus from its discoveries.
func assertEmptyCorpusRefused(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(priv, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveState(statePath, state{Managed: map[string]managedState{
		"https://lost.example/sub": {NotLiveCycles: 2},
	}}); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	c := &Crawler{
		opts: Options{
			Channels:    []string{"chan"},
			PrivatePath: priv,
			StatePath:   statePath,
			Pages:       1,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": wrapMsg("https://sub.example/new")}},
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}
	c.RunOnce(context.Background())

	got, err := os.ReadFile(priv)
	if err != nil {
		t.Fatalf("refused cycle must leave the file alone: %v", err)
	}
	if strings.Contains(string(got), "sub.example") {
		t.Errorf("the refused cycle rewrote the empty corpus from its discoveries:\n%s", got)
	}
	if strings.Contains(body, "hand.example") && !strings.Contains(string(got), "hand.example") {
		t.Errorf("the refused cycle dropped the hand-added entry:\n%s", got)
	}
	if !strings.Contains(logBuf.String(), "private.yaml holds no managed sources while state remembers a managed corpus") {
		t.Errorf("the refusal was not logged at error level:\n%s", logBuf.String())
	}
	// The liveness streaks of the lost corpus must survive the refused cycle:
	// they keep the guard armed until the file is restored or the state
	// deliberately deleted.
	if st := loadState(statePath, zerolog.Nop()); len(st.Managed) != 1 {
		t.Errorf("state lost the corpus's liveness records: %+v", st.Managed)
	}

	// A genuine first cycle — the same file, state remembering nothing — still
	// builds the corpus.
	os.Remove(statePath)
	c.logger = zerolog.Nop()
	c.RunOnce(context.Background())
	got, err = os.ReadFile(priv)
	if err != nil {
		t.Fatalf("first cycle must write the corpus it discovered: %v", err)
	}
	if !strings.Contains(string(got), "sub.example") {
		t.Errorf("first cycle wrote %s, want its discovery in it", got)
	}
}

// TestRunOnceRefusesToRebuildACorpusTruncatedMidCycle pins the reload half of
// the empty-corpus refusal: loadCycle guards the file as read at cycle start,
// but the merge re-reads it after a cycle that takes minutes to hours, and an
// overlay emptied between the two reads — an editor save or failed external
// write landing in that window — must refuse exactly as one emptied before the
// cycle would. The cycle-start snapshot was full, so an existence test at the
// reload sees nothing wrong; only re-running the content test there catches
// the wipe before the merge rebuilds the corpus from this cycle's discoveries.
func TestRunOnceRefusesToRebuildACorpusTruncatedMidCycle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	statePath := filepath.Join(dir, "state.json")
	const (
		corpusURL = "https://corpus.example/sub"
		discovery = "https://sub.example/new"
	)
	if err := os.WriteFile(priv, []byte("subscriptions:\n  sources:\n"+
		"    - name: corpus-1\n      url: "+corpusURL+"\n      feed: corpus\n      managed: true\n"), 0o644); err != nil {
		t.Fatalf("write private.yaml: %v", err)
	}
	if err := saveState(statePath, state{Managed: map[string]managedState{
		corpusURL: {NotLiveCycles: 2},
	}}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	var logBuf bytes.Buffer
	c := &Crawler{
		opts: Options{
			Channels:    []string{"chan"},
			PrivatePath: priv,
			StatePath:   statePath,
			Pages:       1,
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": wrapMsg(discovery)}},
		// The truncation lands on the cycle's first classify call: loadCycle's
		// read has already seen the full corpus, and the merge's re-read is
		// still to come — the window an external write needs.
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if err := os.WriteFile(priv, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
				return classify.Result{}, err
			}
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(&logBuf),
	}
	c.RunOnce(context.Background())

	if !strings.Contains(logBuf.String(), "private.yaml holds no managed sources while state remembers a managed corpus") {
		t.Errorf("the refusal was not logged at error level:\n%s", logBuf.String())
	}
	got, err := os.ReadFile(priv)
	if err != nil {
		t.Fatalf("refused cycle must leave the file alone: %v", err)
	}
	if strings.Contains(string(got), discovery) {
		t.Errorf("the refused cycle rebuilt the truncated corpus from its discoveries:\n%s", got)
	}
	// The liveness streaks of the truncated corpus must survive the refused
	// cycle: they keep the guard armed until the file is restored or the
	// state deliberately deleted.
	if st := loadState(statePath, zerolog.Nop()); len(st.Managed) != 1 {
		t.Errorf("state lost the corpus's liveness records: %+v", st.Managed)
	}

	// A genuine first cycle — the same truncated file, state remembering
	// nothing — still builds the corpus from its discoveries.
	os.Remove(statePath)
	c.logger = zerolog.Nop()
	c.RunOnce(context.Background())
	got, err = os.ReadFile(priv)
	if err != nil {
		t.Fatalf("first cycle must write the corpus it discovered: %v", err)
	}
	if !strings.Contains(string(got), discovery) {
		t.Errorf("first cycle wrote %s, want its discovery in it", got)
	}
}

// TestRecheckCarriesManagedHWID pins the hwid thread (LogicSources#2): the
// liveness fetch of a managed entry must carry the entry's hwid exactly like
// the worker's, or a device-limited panel's header-less answer reads nodeless
// and the entry retires while the worker keeps publishing real nodes from it.
// The recheck is the only liveness path with a per-source config to read one
// from; discovery candidates have no hwid and must send none.
func TestRecheckCarriesManagedHWID(t *testing.T) {
	t.Parallel()

	const (
		urlHWID = "https://hwid.example/sub"
		urlBare = "https://bare.example/sub"
		hwid    = "abcdef0123456789"
	)
	var mu sync.Mutex
	seen := map[string]string{}
	c := &Crawler{
		opts: Options{Prune: true},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, h string) (classify.Result, error) {
			mu.Lock()
			seen[string(u)] = h
			mu.Unlock()
			// The panel serves its real payload only to the hwid-carrying
			// fetch; a header-less one gets the placeholder.
			if h == "" {
				return classify.Result{}, nil
			}
			return classify.Result{Nodes: 2}, nil
		},
		logger: zerolog.Nop(),
	}
	var pf privateFile
	pf.Subscriptions.Sources = []source{
		{Name: managedName(urlHWID), URL: urlHWID, Managed: true, HWID: hwid},
		{Name: managedName(urlBare), URL: urlBare, Managed: true},
	}

	live := map[string]origin{}
	rr := c.recheckManaged(context.Background(), pf, live)
	if rr.checked != 2 {
		t.Fatalf("rechecked %d URLs, want 2", rr.checked)
	}
	mu.Lock()
	gotHWID, gotBare, bareSeen := seen[urlHWID], seen[urlBare], true
	if _, ok := seen[urlBare]; !ok {
		bareSeen = false
	}
	mu.Unlock()
	if !bareSeen {
		t.Fatalf("recheck classified %v, want both managed URLs", seen)
	}
	if gotHWID != hwid {
		t.Errorf("hwid entry fetched with hwid %q, want %q", gotHWID, hwid)
	}
	if gotBare != "" {
		t.Errorf("bare entry fetched with hwid %q, want none sent", gotBare)
	}
	if rr.revived != 1 {
		t.Errorf("revived = %d, want 1: only the hwid-carrying fetch saw real nodes", rr.revived)
	}
	if !rr.unknown[urlBare] {
		t.Error("the bare entry's placeholder must read undetermined, not live and not dead")
	}
	_, managed, _, _ := c.mergeManaged(pf, live, rr, true, nil)
	kept := map[string]bool{}
	for _, s := range managed {
		kept[s.URL] = true
	}
	if !kept[urlHWID] || !kept[urlBare] {
		t.Errorf("both entries must survive the merge (live and undetermined), got %v", kept)
	}
}

// TestPersistStateSkipsUnchanged pins the dirty gate behind PerfCrawl#2: a
// cycle whose state did not change must not marshal and atomically rewrite the
// whole state file (it can exceed 0.5 MB at the Dead cap). saveState stays
// unconditional for direct callers — tests seed files with it — so the gate
// lives in Crawler.persistState, the cycle path.
func TestPersistStateSkipsUnchanged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	c := &Crawler{opts: Options{StatePath: path}, logger: zerolog.Nop()}
	var st state
	c.persistState(&st)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a clean state must not create the file, stat err: %v", err)
	}
	st.record("chan", time.Now())
	c.persistState(&st)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("a dirty state must be written: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	c.persistState(&st)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("a state unchanged since the last write was rewritten")
	}
	// The mutators re-arm the gate: the next change must land on disk.
	st.record("chan2", time.Now())
	c.persistState(&st)
	if got := loadState(path, zerolog.Nop()); len(got.Productive) != 2 {
		t.Errorf("second change was not persisted: %v", got.Productive)
	}
}

// TestProbeBudgetDefaultsToTheWorkersFetchTimeout pins LogicSources#1's bound:
// the liveness probe must not outlive the worker's per-source fetch, so an
// embedder that never threads Options.FetchTimeout still gets the 3s default
// mirroring config's subscriptions.fetch.timeout — never an unbounded probe,
// and never the old 15s that admitted sources the worker could not fetch.
func TestProbeBudgetDefaultsToTheWorkersFetchTimeout(t *testing.T) {
	t.Parallel()

	if got := (&Crawler{}).probeBudget(); got != defaultProbeTimeout {
		t.Errorf("unthreaded probe budget = %v, want the mirrored default %v", got, defaultProbeTimeout)
	}
	if got := (&Crawler{opts: Options{FetchTimeout: -time.Second}}).probeBudget(); got != defaultProbeTimeout {
		t.Errorf("negative probe budget = %v, want the mirrored default %v", got, defaultProbeTimeout)
	}
	if got := (&Crawler{opts: Options{FetchTimeout: 5 * time.Second}}).probeBudget(); got != 5*time.Second {
		t.Errorf("threaded probe budget = %v, want 5s", got)
	}
}
