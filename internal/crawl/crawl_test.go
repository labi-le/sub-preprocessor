package crawl //nolint:testpackage // exercises unexported crawl helpers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

	got := extractURLs(page)
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
				return classify.Result{}, nil // definitively not live
			}
		},
		logger: zerolog.Nop(),
	}
	live, unknown := c.classifyAll(context.Background(),
		[]string{"https://live.example/sub", "https://err.example/sub", "https://dead.example/sub", "https://gone.example/sub"})

	if !live["https://live.example/sub"] || len(live) != 1 {
		t.Errorf("live = %v, want exactly the live URL", live)
	}
	if !unknown["https://err.example/sub"] || len(unknown) != 1 {
		t.Errorf("unknown = %v, want exactly the transport-errored URL", unknown)
	}
	if unknown["https://gone.example/sub"] {
		t.Error("a definitive non-2xx answer (StatusError) must not be treated as unknown")
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
	live, unknown := c.classifyAll(context.Background(), urls)
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
		urlHand = "https://hand.example/sub"
		urlLive = "https://live.example/sub"
		urlDead = "https://dead.example/sub"
		urlErr  = "https://err.example/sub"
		urlGone = "https://gone.example/sub"
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
			default:
				return classify.Result{}, nil // definitively not live
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
	}

	live := map[string]bool{}
	managedURL, unknown := c.recheckManaged(context.Background(), pf, live)
	next, managed := c.mergeManaged(pf, live, managedURL, unknown)

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
		t.Error("definitively dead managed source must be pruned")
	}
	if byURL[urlGone] {
		t.Error("managed source answering non-2xx must be pruned (host alive, subscription gone)")
	}
	if !byURL[urlErr] {
		t.Error("managed source with unknown (errored) status must be retained, not pruned")
	}
	if len(managed) != 2 {
		t.Errorf("managed = %v, want live+unknown (2 entries)", managed)
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

	next, managed := c.mergeManaged(pf, map[string]bool{}, map[string]bool{}, map[string]bool{})
	if len(next) != 1 || next[0].URL != urlNew {
		t.Fatalf("next = %v, want the mid-cycle addition retained", next)
	}
	if len(managed) != 1 {
		t.Fatalf("managed = %v, want 1 entry", managed)
	}
}

func TestCandidate(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"https://is.wepogp.gay/x?payload=abc": true,
		"https://host.example/sub":            true,
		"https://t.me/chan":                   false, // telegram noise
		"https://cdn4.telesco.pe/file/x.jpg":  false, // telegram media cdn
		"https://192.168.1.1/sub":             true,  // private ip allowed: crawler client is unrestricted
		"http://host.example/sub":             false, // not https
	}
	for u, want := range cases {
		if got := candidate(u); got != want {
			t.Errorf("candidate(%q) = %v, want %v", u, got, want)
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

	st := loadState(path) // missing file → empty
	if len(st.Productive) != 0 {
		t.Fatalf("missing state should be empty, got %+v", st.Productive)
	}
	st.record("rap_ex", now)
	st.record("o00000000i", now.Add(-1000*time.Hour)) // stale
	if err := saveState(path, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	got := loadState(path)
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

	st := loadState("")
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
	got := loadChannels(path, zerolog.Nop())
	want := []string{"o00000000i", "@rap_ex", "https://t.me/remiuc"}
	if len(got) != len(want) {
		t.Fatalf("loadChannels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("channel[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if c := loadChannels(filepath.Join(dir, "nope.yaml"), zerolog.Nop()); c != nil {
		t.Errorf("missing file should yield nil, got %v", c)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("channels: [not: valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := loadChannels(bad, zerolog.Nop()); c != nil {
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
		`escaped vless://u@h.example:443?x=1&amp;y=2#e ` +
		`just prose with a classy word and no proxies here ` +
		// scheme-substring tokens must NOT be captured (boundary guard).
		`pass://foo access://bar class://baz`

	got := extractInlineNodes(page)
	want := map[string]bool{
		"vless://uuid@1.2.3.4:443?security=tls#fast": true,
		"vmess://eyJhZGQiOiIxLjEuMS4xIn0=":           true, // trailing comma trimmed
		"trojan://pass@host.example:8443#t":          true,
		"ss://YWVzOnBhc3M@2.2.2.2:8388#s":            true, // trailing period trimmed
		"ssr://c3NyYmFzZTY0":                         true,
		"tuic://uuid@h6.example:443#tu":              true,
		"hysteria://p@h7.example:443":                true,
		"hysteria2://p@h3.example:443":               true,
		"hy2://p@h4.example:443":                     true,
		"anytls://p@h5.example:443":                  true,
		"vless://u@h.example:443?x=1&y=2#e":          true, // &amp; unescaped
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
