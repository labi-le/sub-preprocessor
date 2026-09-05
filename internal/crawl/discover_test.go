package crawl //nolint:testpackage // drives scan/extractRefs against unexported fixtures

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/metrics"
)

const (
	carveListURL   = "https://t.me/s/forumchat"
	carveJoinCard  = `<html><body>a group listing carries no message and no cursor</body></html>`
	carveParentURL = "https://t.me/forumchat/39" + topicQuery
)

// carveCrawler runs one scan over the given fixtures, logging into logBuf.
func carveCrawler(t *testing.T, opts Options, f pageFetcher, logBuf *bytes.Buffer) (map[string]origin, *state) {
	t.Helper()
	c := &Crawler{
		opts:   opts,
		client: f,
		classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
			return classify.Result{Nodes: 1}, nil
		},
		logger: zerolog.New(logBuf),
	}
	st := state{Productive: map[string]channelState{}}
	live, _, _ := c.scan(context.Background(), &st, nil)
	return live, &st
}

// wrapMsg is a minimal page body carrying one message wrap and one candidate.
func wrapMsg(sub string) string {
	return `<div class="tgme_widget_message_wrap"><a href="` + sub + `">x</a></div>`
}

// TestExtractRefsCarriesTopics pins the ref shapes discovery emits: the numeric
// second segment (or ?topic=/?comment= digits) rides along as the topic, bot
// deep links, reserved paths and lookalike hosts stay out, and dedupe is by
// the full ref rather than the slug.
func TestExtractRefsCarriesTopics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		page string
		want []chanRef
	}{
		{
			name: "three-segment permalink keeps the second segment",
			page: `<a href="https://t.me/forumchat/39/21206">x</a>`,
			want: []chanRef{{slug: "forumchat", topic: "39"}},
		},
		{
			name: "two-segment digits ride as the advisory hint they are",
			page: `<a href="https://t.me/d_code/26804">x</a>`,
			want: []chanRef{{slug: "d_code", topic: "26804"}},
		},
		{
			name: "bare mention stays bare",
			page: `<a href="https://t.me/rap_ex">@rap_ex</a>`,
			want: []chanRef{{slug: "rap_ex"}},
		},
		{
			name: "?topic= parses",
			page: `<a href="https://t.me/forumchat?topic=55">x</a>`,
			want: []chanRef{{slug: "forumchat", topic: "55"}},
		},
		{
			name: "?comment= parses",
			page: `<a href="https://t.me/forumchat?comment=9021">x</a>`,
			want: []chanRef{{slug: "forumchat", topic: "9021"}},
		},
		{
			name: "path digits beat query digits",
			page: `<a href="https://t.me/forumchat/39?comment=9">x</a>`,
			want: []chanRef{{slug: "forumchat", topic: "39"}},
		},
		{
			name: "bot deep link excluded",
			page: `<a href="https://t.me/govpn?start=evolution">GoVPN</a>`,
			want: nil,
		},
		{
			name: "reserved path excluded",
			page: `<a href="https://t.me/share/url?url=x">share</a>`,
			want: nil,
		},
		{
			name: "lookalike host excluded",
			page: `<a href="https://shortcut.me/abcdef">not telegram</a>`,
			want: nil,
		},
		{
			name: "self refs are kept unjudged for the caller",
			page: `<a href="https://t.me/forumchat/55">x</a>`,
			want: []chanRef{{slug: "forumchat", topic: "55"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractRefs([]string{tc.page})
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ref[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}

	t.Run("dedupe is by full ref", func(t *testing.T) {
		t.Parallel()
		page := `<a href="https://t.me/rap_ex">a</a>` +
			`<a href="https://t.me/rap_ex">b</a>` +
			`<a href="https://t.me/rap_ex/12">c</a>`
		got := extractRefs([]string{page})
		want := []chanRef{{slug: "rap_ex"}, {slug: "rap_ex", topic: "12"}}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("ref[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}

// TestDiscoveredRefSettlesBothWays pins that a discovered <slug>/<digits> flows
// through the same empirical settle a seed gets: a listing with messages is
// walked and the embed URL is never asked for; an empty listing buys exactly
// one embed fetch; a listing that never came back buys none.
func TestDiscoveredRefSettlesBothWays(t *testing.T) {
	t.Parallel()

	const (
		seedListURL = "https://t.me/s/srca"
		discListURL = "https://t.me/s/disc77"
		discEmbed   = "https://t.me/disc77/77" + topicQuery
		subSeed     = "https://sub.example/from-feed"
		subTopic    = "https://sub.example/from-topic"
		subFeed     = "https://sub.example/from-disc-feed"
	)
	seedPage := wrapMsg(subSeed) + `<a href="https://t.me/disc77/77">repost</a>`

	cases := []struct {
		name        string
		pages       map[string]string
		errs        map[string]error
		wantEmbed   int
		wantList    int
		wantLiveSub string
		wantRecord  string
	}{
		{
			name: "feed walked, embed never requested",
			pages: map[string]string{
				seedListURL: seedPage,
				discListURL: wrapMsg(subFeed),
			},
			wantEmbed:   0,
			wantList:    1,
			wantLiveSub: subFeed,
			wantRecord:  "disc77",
		},
		{
			name: "empty listing settles through exactly one embed fetch",
			pages: map[string]string{
				seedListURL: seedPage,
				discListURL: carveJoinCard,
				discEmbed:   wrapMsg(subTopic),
			},
			wantEmbed:   1,
			wantList:    1,
			wantLiveSub: subTopic,
			wantRecord:  "disc77/77",
		},
		{
			name: "transport failure never falls back",
			pages: map[string]string{
				seedListURL: seedPage,
			},
			errs:      map[string]error{discListURL: errors.New("bad status: 429 Too Many Requests")},
			wantEmbed: 0,
			wantList:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hits := map[string]int{}
			var logBuf bytes.Buffer
			opts := Options{Channels: []string{"srca"}, Pages: 1, MaxDepth: 3}
			live, st := carveCrawler(t, opts, pageFetcher{hits: hits, pages: tc.pages, errs: tc.errs}, &logBuf)

			if got := hits[discEmbed]; got != tc.wantEmbed {
				t.Errorf("embed fetched %d time(s), want %d", got, tc.wantEmbed)
			}
			if got := hits[discListURL]; got != tc.wantList {
				t.Errorf("listing fetched %d time(s), want %d", got, tc.wantList)
			}
			if tc.wantLiveSub != "" && live[tc.wantLiveSub].Slug != "disc77" {
				t.Errorf("live[%s] = %+v, want attribution to disc77", tc.wantLiveSub, live[tc.wantLiveSub])
			}
			if tc.wantRecord != "" {
				if _, ok := st.Productive[tc.wantRecord]; !ok {
					t.Errorf("productive memory holds %d entries, none named %s", len(st.Productive), tc.wantRecord)
				}
			}
		})
	}
}

// TestCarveOutGating pins the self-exclusion carve-out: same-group recursion
// fires only from a node actually read through its topic embed, only for a
// differing numeric topic, and still inside the shared per-cycle budget.
func TestCarveOutGating(t *testing.T) {
	t.Parallel()

	const (
		carveSiblingURL = "https://t.me/forumchat/55" + topicQuery
		subParent       = "https://sub.example/parent"
		subSibling      = "https://sub.example/sibling"
	)

	t.Run("fires only from an embed-read node for a differing topic", func(t *testing.T) {
		t.Parallel()

		parentPage := wrapMsg(subParent) +
			`<a href="https://t.me/forumchat/55">sibling topic</a>` +
			`<a href="https://t.me/forumchat/39?comment=1">own-topic reply</a>` +
			`<a href="https://t.me/forumchat">bare self link</a>`
		hits := map[string]int{}
		var logBuf bytes.Buffer
		opts := Options{Channels: []string{"forumchat/39"}, Pages: 1, MaxDepth: 3}
		carveCrawler(t, opts, pageFetcher{hits: hits, pages: map[string]string{
			carveListURL:    carveJoinCard,
			carveParentURL:  parentPage,
			carveSiblingURL: wrapMsg(subSibling),
		}}, &logBuf)
		if got := hits[carveSiblingURL]; got != 1 {
			t.Errorf("differing-topic sibling fetched %d time(s), want the 1 carve-out visit", got)
		}
		if got := hits[carveParentURL]; got != 1 {
			t.Errorf("parent topic fetched %d time(s); same-topic and bare self-links must not recurse", got)
		}
		if got := hits[carveListURL]; got != 2 {
			t.Errorf("/s/ probed %d time(s), want 2 — one wasted probe per visited topic node is the carve-out's documented cost", got)
		}
		if got := strings.Count(logBuf.String(), `"channel":"forumchat/39"`); got != 1 {
			t.Errorf("forumchat/39 scanned %d time(s), want 1 (log: %s)", got, logBuf.String())
		}
	})

	t.Run("permalink digits on a channel node do not carve", func(t *testing.T) {
		t.Parallel()

		const (
			srca2ListURL = "https://t.me/s/srca2"
			srca2Embed   = "https://t.me/srca2/55" + topicQuery
		)
		hits := map[string]int{}
		var logBuf bytes.Buffer
		opts := Options{Channels: []string{"srca2"}, Pages: 1, MaxDepth: 3}
		carveCrawler(t, opts, pageFetcher{hits: hits, pages: map[string]string{
			srca2ListURL: wrapMsg(subParent) + `<a href="https://t.me/srca2/55">own post permalink</a>`,
		}}, &logBuf)

		if got := hits[srca2Embed]; got != 0 {
			t.Errorf("self embed fetched %d time(s); a channel node's own permalinks stay excluded", got)
		}
		if got := hits[srca2ListURL]; got != 1 {
			t.Errorf("/s/ probed %d time(s), want 1 — a leaked carve-out child re-probes the same message-less-for-channels listing", got)
		}
	})

	t.Run("bare slug seed is absorbed by its remembered topics", func(t *testing.T) {
		t.Parallel()

		// carveCrawler seeds the state empty, so it cannot pre-load the
		// productive memory this case needs. The fixture is instead driven
		// through the real Crawler with a pre-seeded state: the configured
		// bare slug must be absorbed into its remembered topic (see the
		// buildSeeds doc), leaving the listing fetched once as the topic
		// probe and never scanned as a chat of its own.
		hits := map[string]int{}
		st := state{Productive: map[string]channelState{"forumchat/39": {}}}
		c := &Crawler{
			opts: Options{Channels: []string{"forumchat"}, Pages: 3, MaxDepth: 0},
			client: pageFetcher{hits: hits, pages: map[string]string{
				carveListURL:   carveJoinCard,
				carveParentURL: wrapMsg(subParent),
			}},
			classifyFn: func(_ context.Context, _ *http.Client, _ fetch.SubscriptionURL, _ string) (classify.Result, error) {
				return classify.Result{Nodes: 1}, nil
			},
			logger: zerolog.Nop(),
		}
		c.scan(context.Background(), &st, nil)
		if got := hits[carveListURL]; got != 1 {
			t.Errorf("t.me/s/forumchat fetched %d time(s), want the 1 topic probe; the bare slug seed must be absorbed, not scanned", got)
		}
	})

	t.Run("unproductive topics still spend the shared pool", func(t *testing.T) {
		t.Parallel()

		const (
			embed55 = "https://t.me/forumchat/55" + topicQuery
			embed66 = "https://t.me/forumchat/66" + topicQuery
		)
		parentPage := wrapMsg(subParent) +
			`<a href="https://t.me/forumchat/55">first sibling</a>` +
			`<a href="https://t.me/forumchat/66">second sibling</a>`
		hits := map[string]int{}
		var logBuf bytes.Buffer
		// MaxChannels bounds the whole discovered pool, cross-channel visits
		// and carve-out children alike; the first-dequeued sibling wins.
		opts := Options{Channels: []string{"forumchat/39"}, Pages: 1, MaxDepth: 3, MaxChannels: 1}
		carveCrawler(t, opts, pageFetcher{hits: hits, pages: map[string]string{
			carveListURL:   carveJoinCard,
			carveParentURL: parentPage,
		}}, &logBuf)

		if got := hits[embed55]; got != 1 {
			t.Errorf("first sibling fetched %d time(s), want 1", got)
		}
		if got := hits[embed66]; got != 0 {
			t.Errorf("second sibling fetched %d time(s), want 0 — the pool was spent", got)
		}
	})
}

// TestTopicFloodBounded pins the shared maxDiscovered pool against one page
// flooding hundreds of same-group topics: three admissions, not two hundred.
func TestTopicFloodBounded(t *testing.T) {
	t.Parallel()

	const subParent = "https://sub.example/parent"
	var links strings.Builder
	for i := 100; i < 200; i++ {
		links.WriteString(`<a href="https://t.me/forumchat/` + strconv.Itoa(i) + `">t</a>`)
	}
	hits := map[string]int{}
	var logBuf bytes.Buffer
	opts := Options{Channels: []string{"forumchat/39"}, Pages: 6, MaxDepth: 3, MaxChannels: 3}
	carveCrawler(t, opts, pageFetcher{hits: hits, pages: map[string]string{
		carveListURL:   carveJoinCard,
		carveParentURL: wrapMsg(subParent) + links.String(),
	}}, &logBuf)

	fetched := 0
	for i := 100; i < 200; i++ {
		fetched += hits["https://t.me/forumchat/"+strconv.Itoa(i)+topicQuery]
	}
	if fetched != 3 {
		t.Errorf("%d distinct topics fetched, want 3 — the shared maxDiscovered pool bounds the flood", fetched)
	}
	if got := hits[carveListURL]; got != 4 {
		t.Errorf("/s/ probed %d time(s), want 4 (parent plus one probe per admitted child)", got)
	}
}

// TestTopicBackReferenceDiesOnSeen pins that a topic naming its own chat back
// is a no-op once that ref was read this cycle.
func TestTopicBackReferenceDiesOnSeen(t *testing.T) {
	t.Parallel()

	const (
		carveChildURL = "https://t.me/forumchat/55" + topicQuery
		subParent     = "https://sub.example/parent"
		subSibling    = "https://sub.example/sibling"
	)
	childPage := wrapMsg(subSibling) +
		`<a href="https://t.me/forumchat/39?comment=7">back-reference</a>`
	hits := map[string]int{}
	var logBuf bytes.Buffer
	opts := Options{Channels: []string{"forumchat/39"}, Pages: 1, MaxDepth: 3}
	carveCrawler(t, opts, pageFetcher{hits: hits, pages: map[string]string{
		carveListURL:   carveJoinCard,
		carveParentURL: wrapMsg(subParent) + `<a href="https://t.me/forumchat/55">sibling</a>`,
		carveChildURL:  childPage,
	}}, &logBuf)

	if got := hits[carveParentURL]; got != 1 {
		t.Errorf("parent topic fetched %d time(s), want 1 — its back-reference must be a no-op", got)
	}
	if got := hits[carveChildURL]; got != 1 {
		t.Errorf("child topic fetched %d time(s), want 1", got)
	}
	for _, ref := range []string{"forumchat/39", "forumchat/55"} {
		if got := strings.Count(logBuf.String(), `"channel":"`+ref+`"`); got != 1 {
			t.Errorf("%s scanned %d time(s), want 1 (log: %s)", ref, got, logBuf.String())
		}
	}
}

// TestTopicProductiveKeyRoundTrip pins the '<slug>/<topic>' productive key
// surviving a save/load round trip byte for byte.
func TestTopicProductiveKeyRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".crawler-state.json")
	st := loadState(path, zerolog.Nop())
	st.record("forumchat/55", time.Now())
	if err := saveState(path, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	got := loadState(path, zerolog.Nop())
	if seeds := got.seeds(); len(seeds) != 1 || seeds[0] != "forumchat/55" {
		t.Errorf("seeds = %v, want [forumchat/55]", seeds)
	}
	if err = saveState(path, got); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Fatalf("round trip changed the state file:\n got: %s\nwant: %s", again, first)
	}
}

// TestPermalinkDiscoveryRecordsBareSlug pins the productive key of a chat read
// through its /s/ listing to the bare slug: permalink digits are an advisory
// hint, and one chat must not accrue a state key per linking post.
func TestPermalinkDiscoveryRecordsBareSlug(t *testing.T) {
	t.Parallel()

	const subParent = "https://sub.example/parent"
	opts := Options{Channels: []string{"parent"}, Pages: 1, MaxDepth: 2}
	_, st := carveCrawler(t, opts, pageFetcher{pages: map[string]string{
		"https://t.me/s/parent": wrapMsg(subParent) + `<a href="https://t.me/other/1234">repost</a>`,
		"https://t.me/s/other":  wrapMsg(subParent),
	}}, &bytes.Buffer{})

	for _, key := range []string{"parent", "other"} {
		if _, ok := st.Productive[key]; !ok {
			t.Errorf("productive keys %v miss %q", st.Productive, key)
		}
	}
	if _, ok := st.Productive["other/1234"]; ok {
		t.Error("permalink digits leaked into the productive key")
	}
}

// TestThematicGateDoesNotExpandBarrenChild pins the gate: a discovered channel
// whose pages yield no live subscription is crawled but not expanded, so the
// graph stays one hop deep around barren channels.
func TestThematicGateDoesNotExpandBarrenChild(t *testing.T) {
	t.Parallel()

	const (
		seedListURL       = "https://t.me/s/srca"
		childListURL      = "https://t.me/s/barrench"
		grandchildListURL = "https://t.me/s/deeperg"
		subSeed           = "https://sub.example/seed"
	)
	hits := map[string]int{}
	var logBuf bytes.Buffer
	opts := Options{Channels: []string{"srca"}, Pages: 1, MaxDepth: 3}
	c := &Crawler{
		opts: opts,
		client: pageFetcher{hits: hits, pages: map[string]string{
			seedListURL: wrapMsg(subSeed) + `<a href="https://t.me/barrench">repost</a>`,
			childListURL: wrapMsg("https://sub.example/child-never-live") +
				`<a href="https://t.me/deeperg">child's own repost</a>`,
		}},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if string(u) == subSeed {
				return classify.Result{Nodes: 1}, nil
			}
			return classify.Result{}, nil
		},
		logger: zerolog.New(&logBuf),
	}
	st := state{Productive: map[string]channelState{}}
	c.scan(context.Background(), &st, nil)

	if hits[childListURL] != 1 {
		t.Errorf("barren child fetched %d time(s), want exactly the one discovery visit", hits[childListURL])
	}
	if hits[grandchildListURL] != 0 {
		t.Error("a child that yielded nothing must not be expanded into its own references")
	}
}

// TestBareGroupDeadEndCounted pins item-14 accounting: a discovered group whose
// listing came back without a message and whose link named no topic warns once,
// counts once, is fetched once despite leftover budget, and ends the cycle.
func TestBareGroupDeadEndCounted(t *testing.T) {
	t.Parallel()

	const (
		seedListURL  = "https://t.me/s/srca"
		groupListURL = "https://t.me/s/deadgrp"
		subSeed      = "https://sub.example/seed"
	)
	before := metrics.Crawl.GroupEmpty.Load()
	hits := map[string]int{}
	var logBuf bytes.Buffer
	opts := Options{Channels: []string{"srca"}, Pages: 6, MaxDepth: 3}
	live, _ := carveCrawler(t, opts, pageFetcher{hits: hits, pages: map[string]string{
		seedListURL:  wrapMsg(subSeed) + `<a href="https://t.me/deadgrp">dead end</a>`,
		groupListURL: carveJoinCard,
	}}, &logBuf)

	if d := metrics.Crawl.GroupEmpty.Load() - before; d != 1 {
		t.Errorf("group-empty counter moved %d, want exactly 1", d)
	}
	if got := hits[groupListURL]; got != 1 {
		t.Errorf("dead group fetched %d time(s), want a single attempt despite the page budget", got)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "deadgrp") || !strings.Contains(logged, "no message and the link named no topic") {
		t.Errorf("log %s must name the slug and the dead-end reason", logged)
	}
	if len(live) == 0 || live[subSeed].Slug != "srca" {
		t.Errorf("live = %v, want the cycle to complete with the seed's own find", live)
	}
}

// A discovered GROUP whose listing fetch fails is a transport problem, not an
// empty chat: the group-empty counter must not move. Driven through
// scanChannel because scan drops a failed child before its accounting runs.
func TestBareGroupListingFailureNotCounted(t *testing.T) {
	t.Parallel()

	c := &Crawler{
		opts: Options{Channels: []string{"srca"}, Pages: 6, MaxDepth: 3},
		client: pageFetcher{errs: map[string]error{
			"https://t.me/s/deadgrp": errors.New("bad status: 429 Too Many Requests"),
		}},
		logger: zerolog.Nop(),
	}
	before := metrics.Crawl.GroupEmpty.Load()
	n := scanNode{ref: chanRef{slug: "deadgrp"}, depth: 1}
	c.scanChannel(context.Background(), n, &state{}, nil, map[string]origin{}, nil, &cursorStats{}, newRejects(zerolog.Nop()))
	if d := metrics.Crawl.GroupEmpty.Load() - before; d != 0 {
		t.Errorf("group-empty counter moved %d for a failed listing fetch, want 0", d)
	}
}

// TestTopicMetricsRendered drives a mixed fixture cycle and reads the numbers
// back off GET /metrics. It does not run in parallel: it asserts exact values
// of process-wide counters, and another test bumping them mid-cycle would make
// that a coin toss. Judging the render against an absolute Stats() snapshot
// taken at the response keeps that exactness across orderings and -count
// repetitions alike.
func TestTopicMetricsRendered(t *testing.T) {
	const (
		forumListURL = "https://t.me/s/forumchat"
		parentURL    = "https://t.me/forumchat/39" + topicQuery
		embed55      = "https://t.me/forumchat/55" + topicQuery
		embed77      = "https://t.me/forumchat/77" + topicQuery
		deadGrpURL   = "https://t.me/s/deadgrp"
		subSeed      = "https://sub.example/seed"
		subTopic     = "https://sub.example/topic"
	)

	before := metrics.Crawl.Stats()
	var logBuf bytes.Buffer
	opts := Options{Channels: []string{"forumchat/39"}, Pages: 1, MaxDepth: 3}
	carveCrawler(t, opts, pageFetcher{pages: map[string]string{
		forumListURL: carveJoinCard,
		parentURL: wrapMsg(subSeed) +
			`<a href="https://t.me/forumchat/55">live sibling</a>` +
			`<a href="https://t.me/forumchat/77">empty sibling</a>` +
			`<a href="https://t.me/deadgrp">dead group</a>`,
		embed55:    wrapMsg(subTopic),
		embed77:    `<html><body>reachable, but the topic is gone</body></html>`,
		deadGrpURL: carveJoinCard,
	}}, &logBuf)

	ts := metrics.Crawl.Since(before)
	cycle := crawlSampleIDs(ts)
	expect := map[string]int64{
		"stable_crawl_topic_pages_total":      3, // parent, live sibling and empty sibling embeds all answered
		"stable_crawl_topic_live_total":       2, // parent seed page and the live sibling
		"stable_crawl_topic_empty_total":      1,
		"stable_crawl_topic_discovered_total": 2, // both same-group edges admitted
		"stable_crawl_group_empty_total":      1,
	}
	for name, e := range expect {
		if cycle[name] != e {
			t.Errorf("cycle delta %s = %d, want %d", name, cycle[name], e)
		}
	}

	srv := httptest.NewServer(serveMux(context.Background(), &Crawler{logger: zerolog.Nop()}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err = buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	// GET /metrics renders LIFETIME totals, so the oracle is the absolute
	// counter set read as the response lands — equal to the render by
	// construction whatever earlier cycles or -count repetitions did.
	want := crawlSampleIDs(metrics.Crawl.Stats())
	rendered := renderedSamples(t, buf.String())
	for name, w := range want {
		got, ok := rendered[name]
		if !ok {
			t.Errorf("/metrics output misses %s:\n%s", name, buf.String())
			continue
		}
		if got != float64(w) {
			t.Errorf("%s renders %g, want %d", name, got, w)
		}
	}

	alarmAt := func(ts metrics.CrawlStats) bool {
		var b bytes.Buffer
		(&Crawler{logger: zerolog.New(&b)}).reportTopics(ts)
		return strings.Contains(b.String(), "markup likely changed")
	}
	if !alarmAt(metrics.CrawlStats{TopicPages: topicAlarmMin}) {
		t.Error("a full cycle of dead topic pages must warn")
	}
	if alarmAt(metrics.CrawlStats{TopicPages: topicAlarmMin - 1}) {
		t.Error("too few pages to distinguish a markup break from dead topics")
	}
	if alarmAt(metrics.CrawlStats{TopicPages: topicAlarmMin, TopicLive: 1}) {
		t.Error("one live page means the embed works; stay quiet")
	}
	var b bytes.Buffer
	(&Crawler{logger: zerolog.New(&b)}).reportTopics(ts)
	for _, field := range []string{"pages", "live", "empty", "discovered", "groupEmpty"} {
		if !strings.Contains(b.String(), `"`+field+`":`) {
			t.Errorf("per-cycle log line misses %q: %s", field, b.String())
		}
	}
}

// renderedSamples parses a Prometheus text exposition body into sample values.
func renderedSamples(t *testing.T, body string) map[string]float64 {
	t.Helper()
	rendered := map[string]float64{}
	for line := range strings.SplitSeq(body, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		f := strings.Fields(line)
		v, perr := strconv.ParseFloat(f[1], 64)
		if perr != nil {
			t.Fatalf("sample %q: %v", line, perr)
		}
		rendered[f[0]] = v
	}
	return rendered
}

// crawlSampleIDs maps the five counter fields of a CrawlStats snapshot onto
// their rendered sample names.
func crawlSampleIDs(ts metrics.CrawlStats) map[string]int64 {
	return map[string]int64{
		"stable_crawl_topic_pages_total":      ts.TopicPages,
		"stable_crawl_topic_live_total":       ts.TopicLive,
		"stable_crawl_topic_empty_total":      ts.TopicEmpty,
		"stable_crawl_topic_discovered_total": ts.TopicDiscovered,
		"stable_crawl_group_empty_total":      ts.GroupEmpty,
	}
}

// TestFixtureSlugsMatchChannelRe guards the fixture corpus against the one
// silent failure mode it has shown: a t.me link whose slug is shorter than
// channelRe's five-character floor matches nothing, so a test built on it
// passes without exercising the path it names. Reserved paths and the /s/
// preview prefix are exempt.
func TestFixtureSlugsMatchChannelRe(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("discover_test.go")
	if err != nil {
		t.Fatal(err)
	}
	// The capture floor sits below channelRe's on purpose: at channelRe's own
	// floor a sub-floor fixture slug is not captured at all and slips silently,
	// which is the very failure this guard turns loud.
	link := regexp.MustCompile(`(?:^|[^a-zA-Z0-9.-])t\.me/([a-zA-Z][a-zA-Z0-9_]{0,31})`)
	seen := map[string]bool{}
	for _, m := range link.FindAllStringSubmatch(string(src), -1) {
		slug := strings.ToLower(m[1])
		if reservedSlugs[slug] {
			continue
		}
		if !channelRe.MatchString("https://t.me/" + slug) {
			t.Errorf("fixture references t.me/%s, which channelRe cannot see (slug floor is five characters)", slug)
		}
		seen[slug] = true
	}
	if !seen["srca2"] || !seen["forumchat"] {
		t.Errorf("guard matched %v — its own link pattern drifted from the fixtures", seen)
	}
}

// TestTopicQueryKeepsCommentCeiling pins the query constant itself: the whole
// fixture corpus above is built on the embed URL shape topicQuery produces,
// and the 200 comment ceiling (measured where t.me stops returning more) is
// what those fixtures represent.
func TestTopicQueryKeepsCommentCeiling(t *testing.T) {
	if !strings.Contains(topicQuery, "comments_limit=200") {
		t.Errorf("topicQuery = %q, want it to keep comments_limit=200", topicQuery)
	}
}
