package crawl //nolint:testpackage // exercises unexported crawl helpers

import (
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/rs/zerolog"
)

func TestMessageURLs(t *testing.T) {
	t.Parallel()

	m := &tg.Message{
		Message: "grab https://good.example/sub, also https://x.example/a).",
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityBold{}, // non-URL entity is ignored
			&tg.MessageEntityTextURL{URL: "https://hidden.example/link"},
		},
	}
	got := map[string]bool{}
	for _, u := range messageURLs(m) {
		got[u] = true
	}
	for _, want := range []string{
		"https://good.example/sub",    // trailing comma trimmed
		"https://x.example/a",         // trailing ")." trimmed
		"https://hidden.example/link", // hidden behind a text_url entity
	} {
		if !got[want] {
			t.Fatalf("messageURLs missing %q; got %v", want, got)
		}
	}
}

func TestAppendLive(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "private.yaml")
	c := &Crawler{opts: Options{PrivatePath: path}, logger: zerolog.Nop()}

	// Pre-seed a hand-added (unmanaged) source that must survive appends.
	seed := privateFile{}
	seed.Subscriptions.Sources = []source{{Name: "handpicked", URL: "https://hand.example/keep"}}
	if err := writePrivate(path, seed); err != nil {
		t.Fatal(err)
	}

	if n := c.appendLive(map[string]bool{
		"https://a.example/x": true,
		"https://b.example/y": true,
	}); n != 2 {
		t.Fatalf("first append added %d, want 2", n)
	}

	// Re-append one existing URL + one new: only the new one is added (dedupe by URL).
	if n := c.appendLive(map[string]bool{
		"https://a.example/x": true,
		"https://c.example/z": true,
	}); n != 1 {
		t.Fatalf("second append added %d, want 1", n)
	}

	pf, err := loadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Subscriptions.Sources) != 4 { // 1 hand + 3 managed
		t.Fatalf("expected 4 sources, got %d: %+v", len(pf.Subscriptions.Sources), pf.Subscriptions.Sources)
	}
	var keptHand bool
	for _, s := range pf.Subscriptions.Sources {
		if s.URL == "https://hand.example/keep" {
			keptHand = true
			continue
		}
		// managed sources carry a deterministic managedName so poll and push collapse.
		if s.Name != managedName(s.URL) {
			t.Fatalf("managed source %q has name %q, want %q", s.URL, s.Name, managedName(s.URL))
		}
	}
	if !keptHand {
		t.Fatal("appendLive dropped the hand-added source")
	}
}

func TestSeedChannels(t *testing.T) {
	t.Parallel()

	c := &Crawler{
		opts:   Options{Channels: []string{"@Foo", "https://t.me/Bar", "foo"}},
		logger: zerolog.Nop(),
	}
	got := c.seedChannels()
	// @Foo and "foo" normalize to the same slug (deduped); t.me/Bar -> bar.
	if len(got) != 2 || got[0] != "foo" || got[1] != "bar" {
		t.Fatalf("seedChannels = %v, want [foo bar]", got)
	}
}
