package crawl //nolint:testpackage // exercises unexported crawl helpers

import (
	"context"
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
			default:
				return classify.Result{}, nil // definitively not live
			}
		},
		logger: zerolog.Nop(),
	}
	live, unknown := c.classifyAll(context.Background(),
		[]string{"https://live.example/sub", "https://err.example/sub", "https://dead.example/sub"})

	if !live["https://live.example/sub"] || len(live) != 1 {
		t.Errorf("live = %v, want exactly the live URL", live)
	}
	if !unknown["https://err.example/sub"] || len(unknown) != 1 {
		t.Errorf("unknown = %v, want exactly the errored URL", unknown)
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
	)
	c := &Crawler{
		opts: Options{Prune: true},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL) (classify.Result, error) {
			switch string(u) {
			case urlLive:
				return classify.Result{Nodes: 1}, nil
			case urlErr:
				return classify.Result{}, errors.New("transient network error")
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
	if !byURL[urlErr] {
		t.Error("managed source with unknown (errored) status must be retained, not pruned")
	}
	if len(managed) != 2 {
		t.Errorf("managed = %v, want live+unknown (2 entries)", managed)
	}
}

func TestCandidate(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"https://is.wepogp.gay/x?payload=abc": true,
		"https://host.example/sub":            true,
		"https://t.me/chan":                   false, // telegram noise
		"https://cdn4.telesco.pe/file/x.jpg":  false, // telegram media cdn
		"https://192.168.1.1/sub":             false, // SSRF: private ip
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
