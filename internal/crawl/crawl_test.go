package crawl

import (
	"path/filepath"
	"regexp"
	"testing"
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
