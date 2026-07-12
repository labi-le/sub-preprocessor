package crawl

import (
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestExtractURLs(t *testing.T) {
	t.Parallel()

	page := `<a href="https://t.me/somechan">x</a>` +
		`<pre>https://is.wepogp.gay/bypass?payload=AbC%2Bd/e=</pre>` +
		`<img src="https://cdn4.telesco.pe/file/abc.jpg"/>` +
		`text https://host.example/api/filter?code=RU&amp;type=white, end`

	got := extractURLs(page)
	want := map[string]bool{
		"https://t.me/somechan":                              true,
		"https://is.wepogp.gay/bypass?payload=AbC%2Bd/e=":    true,
		"https://cdn4.telesco.pe/file/abc.jpg":               true,
		"https://host.example/api/filter?code=RU&type=white": true, // &amp; unescaped, trailing comma trimmed
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

func TestCandidate(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"https://is.wepogp.gay/x?payload=abc": true,  // real external https
		"https://host.example/sub":            true,  //
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
			`dup <a href="https://t.me/rap_ex/12">again</a>`,
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
