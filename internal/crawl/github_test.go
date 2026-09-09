package crawl //nolint:testpackage // exercises the crawl-side GitHub glue: unexported naming, admission, budget and state helpers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/ghfind"
	"domains.lst/sub-preprocessor/internal/metrics"
)

// TestGhStemFoldsIntoTheSourceNameAlphabet pins the name-stem folding: a stem
// outside ^[a-z0-9-]+$ is one validatePrivate refuses, and the crawl can then
// never write private.yaml again, this cycle or any later one. Uppercase folds
// down, separators ('/', '_', runs of either) collapse to a single '-', a
// trailing dash is trimmed, and the stem is capped at maxGitHubStem so a long
// owner/repo pair cannot grow an unbounded name.
func TestGhStemFoldsIntoTheSourceNameAlphabet(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Owner/Repo":                      "gh-owner-repo",
		"owner/Repo_Name":                 "gh-owner-repo-name",
		"Owner__Name//Repo":               "gh-owner-name-repo", // runs of separators collapse to one dash
		"owner/repo_":                     "gh-owner-repo",      // trailing dash trimmed
		"owner/Repo/":                     "gh-owner-repo",      // trailing separator trimmed
		"Mixed_Case.Owner/With.Dots/Repo": "gh-mixed-case-owner-with-dots-repo",
		strings.Repeat("A", 30) + "/" + strings.Repeat("B", 30): "gh-" + strings.Repeat("a", 30) + "-" + strings.Repeat("b", 6), // cut at the 40-byte cap
		strings.Repeat("a", 36) + "_" + strings.Repeat("b", 30): "gh-" + strings.Repeat("a", 36),                                // cut on a separator: dangling dash trimmed
	}
	for in, want := range cases {
		got := ghStem(in)
		if got != want {
			t.Errorf("ghStem(%q) = %q, want %q", in, got, want)
		}
		if !sourceNameRe.MatchString(got) {
			t.Errorf("ghStem(%q) = %q, outside the source-name alphabet", in, got)
		}
		if len(got) > maxGitHubStem {
			t.Errorf("ghStem(%q) = %q, longer than the %d-byte cap", in, got, maxGitHubStem)
		}
	}
}

// TestUniqueNameSiblingOrdinals: a free stem is returned verbatim and a taken
// one walks the -2, -3, ... ordinals until one is free. A duplicate name is a
// validatePrivate refusal that freezes private.yaml, so the walk must never
// hand back a name the taken set already holds.
func TestUniqueNameSiblingOrdinals(t *testing.T) {
	t.Parallel()

	if got := uniqueName("gh-owner-repo", map[string]bool{}); got != "gh-owner-repo" {
		t.Errorf("free stem = %q, want it verbatim", got)
	}
	if got := uniqueName("gh-owner-repo", map[string]bool{"gh-owner-repo": true}); got != "gh-owner-repo-2" {
		t.Errorf("taken stem = %q, want gh-owner-repo-2", got)
	}
	if got := uniqueName("gh-owner-repo", map[string]bool{"gh-owner-repo": true, "gh-owner-repo-2": true}); got != "gh-owner-repo-3" {
		t.Errorf("taken stem and -2 = %q, want gh-owner-repo-3", got)
	}
}

// TestUniqueNameSeparatesCollidingStems: two different repositories whose
// folded names share the first maxGitHubStem bytes collide onto one ghStem,
// and the mint must still give their files distinct names. A duplicate would
// be refused by validatePrivate, leaving the crawl unable to write
// private.yaml at all.
func TestUniqueNameSeparatesCollidingStems(t *testing.T) {
	t.Parallel()

	// The two repos differ only past the 40-byte window, so their stems are
	// byte-identical while their feeds keep the full identity.
	repoA := strings.Repeat("x", 40) + "/first"
	repoB := strings.Repeat("x", 40) + "/second"
	stemA, stemB := ghStem(repoA), ghStem(repoB)
	if stemA != stemB {
		t.Fatalf("fixture stems differ (%q vs %q); the collision this test pins did not occur", stemA, stemB)
	}

	used := map[string]bool{}
	a := mintSource("https://raw.example/a/sub", source{}, origin{Stem: stemA, Feed: ghFeedPrefix + repoA}, used)
	b := mintSource("https://raw.example/b/sub", source{}, origin{Stem: stemB, Feed: ghFeedPrefix + repoB}, used)
	if a.Name == b.Name {
		t.Fatalf("colliding stems minted the duplicate name %q; validatePrivate would refuse the file", a.Name)
	}
	if a.Name != stemA || b.Name != stemA+"-2" {
		t.Errorf("names = %q, %q, want the bare stem then the first free ordinal", a.Name, b.Name)
	}
	if a.Feed != ghFeedPrefix+repoA || b.Feed != ghFeedPrefix+repoB {
		t.Errorf("feeds = %q, %q, want the full identities kept", a.Feed, b.Feed)
	}
	var pf privateFile
	pf.Subscriptions.Sources = []source{a, b}
	if err := validatePrivate(pf); err != nil {
		t.Fatalf("the pair a real cycle would write is refused: %v", err)
	}
}

// TestSourceNameUsesTheStemOrigin pins the GitHub naming path: an origin
// carrying a Stem names itself with it, an existing non-hash name still wins
// (the mint never renames an entry), a bare-hash name is the one exception
// that names no origin and upgrades, and Stem wins over Slug when both are
// set.
func TestSourceNameUsesTheStemOrigin(t *testing.T) {
	t.Parallel()

	const u = "https://raw.example/sub"
	o := origin{Stem: "gh-owner-repo", Feed: "gh:owner/repo"}

	if got := sourceName(u, "", o, map[string]bool{}); got != "gh-owner-repo" {
		t.Errorf("stem origin on a new url = %q, want the stem verbatim", got)
	}
	if got := sourceName(u, "", o, map[string]bool{"gh-owner-repo": true}); got != "gh-owner-repo-2" {
		t.Errorf("stem origin beside a taken stem = %q, want the first free ordinal", got)
	}
	// A name that carries an origin is never rewritten, stem or no stem: a
	// rename relabels every published node and buys nothing.
	if got := sourceName(u, "gh-old-repo", o, map[string]bool{}); got != "gh-old-repo" {
		t.Errorf("existing name = %q, want it kept over the stem", got)
	}
	// The bare hash is the exception: it upgrades onto the stem exactly as it
	// upgrades onto a slug.
	if got := sourceName(u, managedName(u), o, map[string]bool{}); got != "gh-owner-repo" {
		t.Errorf("bare-hash name = %q, want it upgraded onto the stem", got)
	}
	// A GitHub discovery that also carried a channel slug still names itself by
	// the stem: the slug alphabet cannot express owner and repository.
	if got := sourceName(u, "", origin{Stem: "gh-owner-repo", Slug: "chan", Post: 9}, map[string]bool{}); got != "gh-owner-repo" {
		t.Errorf("stem beside slug+post = %q, want the stem preferred", got)
	}
}

// TestMintSourceAttribution pins what one minted GitHub entry carries: the
// gh:<owner>/<repo> feed verbatim (never routed through channelSlug, which
// would strip the ':' and '/'), the sibling ordinal for a repository's second
// file, and the prev-entry handling that predates the phase — HWID carried
// across a rename, Feed frozen when the name is kept.
func TestMintSourceAttribution(t *testing.T) {
	t.Parallel()

	const (
		u  = "https://raw.example/owner/repo/f1"
		u2 = "https://raw.example/owner/repo/f2"
	)
	o := origin{Stem: "gh-owner-repo", Feed: "gh:owner/repo"}
	used := map[string]bool{}
	first := mintSource(u, source{}, o, used)
	if first.Name != "gh-owner-repo" || first.Feed != "gh:owner/repo" || !first.Managed || first.URL != u {
		t.Errorf("first mint = %+v, want name gh-owner-repo, feed gh:owner/repo, managed", first)
	}
	if !used[first.Name] {
		t.Error("the minted name did not join the taken-name set")
	}
	second := mintSource(u2, source{}, o, used)
	if second.Name != "gh-owner-repo-2" || second.Feed != "gh:owner/repo" {
		t.Errorf("second file of the repo = %+v, want the -2 ordinal with the same feed", second)
	}

	// A bare-hash entry rediscovered by GitHub upgrades onto the stem; the
	// operator's hwid rides along, and the feed is replaced by the new
	// attribution.
	hash := source{Name: managedName(u), URL: u, HWID: "dev-42", Managed: true}
	upgraded := mintSource(u, hash, o, map[string]bool{})
	if upgraded.Name != "gh-owner-repo" || upgraded.Feed != "gh:owner/repo" || upgraded.HWID != "dev-42" {
		t.Errorf("upgraded mint = %+v, want the stem name, the gh feed and the hwid kept", upgraded)
	}

	// A named entry rediscovered under a different origin is not relabelled:
	// the feed follows the name and changes exactly when it does.
	prev := source{Name: "gh-owner-repo", URL: u, Feed: "gh:owner/repo", Managed: true}
	kept := mintSource(u, prev, origin{Stem: "gh-other", Feed: "gh:other/repo"}, map[string]bool{"gh-owner-repo": true})
	if kept.Name != "gh-owner-repo" || kept.Feed != "gh:owner/repo" {
		t.Errorf("rediscovered entry = %+v, want name and feed kept verbatim", kept)
	}
}

// TestMintSourceKeepsTheTelegramSlugAttribution: the Telegram half of the mint
// writes the sanitized channel slug as the feed, byte-identical to what it
// wrote before the GitHub phase existed — the gh: label must not leak through
// that path, or ghManagedCount's ownership test would miscount.
func TestMintSourceKeepsTheTelegramSlugAttribution(t *testing.T) {
	t.Parallel()

	const u = "https://sub.example/vpn"
	o := origin{Slug: "VPN_Channel"}
	got := mintSource(u, source{}, o, map[string]bool{})
	if want := channelSlug("VPN_Channel"); got.Feed != want {
		t.Errorf("telegram feed = %q, want %q (channelSlug of the raw slug)", got.Feed, want)
	}
	if got.Name != "vpn-channel-1" || got.Feed != "vpn-channel" {
		t.Errorf("mint = %+v, want name vpn-channel-1 and feed vpn-channel", got)
	}
	if strings.ContainsAny(got.Feed, ":/") {
		t.Errorf("telegram feed %q carries a gh-style separator", got.Feed)
	}
}

// TestGhManagedCountCountsOnlyOwnedEntries: the count feeds the phase's source
// cap, so the population it measures decides whether the phase ever mints. Only
// entries that are BOTH managed and gh:-prefixed are this phase's work — a
// hand-added entry carrying a gh: feed is the operator's, and a managed
// Telegram entry is the channel mint's.
func TestGhManagedCountCountsOnlyOwnedEntries(t *testing.T) {
	t.Parallel()

	var pf privateFile
	pf.Subscriptions.Sources = []source{
		{Name: "gh-a", URL: "https://a.example/sub", Feed: "gh:owner/repo", Managed: true},
		{Name: "gh-b", URL: "https://b.example/sub", Feed: "gh:other/repo", Managed: true},
		{Name: "tg-c", URL: "https://c.example/sub", Feed: "vpn-channel", Managed: true},
		{Name: "hand-d", URL: "https://d.example/sub", Feed: "gh:hand/added"},
		{Name: "inline", Body: "cGF5bG9hZA==", Managed: true},
	}
	if got := ghManagedCount(pf); got != 2 {
		t.Errorf("ghManagedCount = %d, want 2: only managed gh: entries count, not the managed Telegram feed, the hand-added gh: entry or the inline aggregate", got)
	}
}

// TestGhKnownCoversTheCorpusAndTheDead: the finder must not offer as new a URL
// the overlay already carries (managed or hand-added) or one under a standing
// dead stamp; the inline aggregate is skipped because it carries a body, not a
// URL. A URL missing from this set is a candidate again, so a standing dead
// stamp would mint it right back.
func TestGhKnownCoversTheCorpusAndTheDead(t *testing.T) {
	t.Parallel()

	var pf privateFile
	pf.Subscriptions.Sources = []source{
		{Name: "managed", URL: "https://corpus.example/m", Managed: true},
		{Name: "hand", URL: "https://corpus.example/h"},
		{Name: "inline", Body: "cGF5bG9hZA==", Managed: true},
		{Name: "hand-empty", URL: ""},
	}
	dead := map[string]time.Time{
		"https://dead.example/1": time.Now().Add(time.Hour),
		"https://dead.example/2": time.Now().Add(time.Hour),
	}
	known := ghKnown(pf, dead)
	for _, u := range []string{"https://corpus.example/m", "https://corpus.example/h", "https://dead.example/1", "https://dead.example/2"} {
		if !known[u] {
			t.Errorf("ghKnown misses %s", u)
		}
	}
	if len(known) != 4 {
		t.Errorf("ghKnown = %v, want exactly the four URLs: empty-URL entries carry no identity", known)
	}
}

// offerFixture is a finder result with five accepted files across three repos,
// in richness order 10, 5, 4, 2, 1.
func offerFixture() ghfind.Result {
	return ghfind.Result{Repos: []ghfind.Repo{
		{Name: "aa/one", Accepted: []ghfind.File{
			{URL: "https://raw.example/aa/1", Novel: 5},
			{URL: "https://raw.example/aa/3", Novel: 1},
		}},
		{Name: "bb/two", Accepted: []ghfind.File{
			{URL: "https://raw.example/bb/4", Novel: 10},
			{URL: "https://raw.example/bb/5", Novel: 4},
		}},
		{Name: "cc/three", Accepted: []ghfind.File{
			{URL: "https://raw.example/cc/6", Novel: 2},
		}},
	}}
}

// TestAdmitGitHubAdmitsTheRichestIntoRoom: when the finder offers more files
// than the corpus may carry, exactly the room richest (novel-endpoint) files
// that are not already live are admitted — never an accident of iteration
// order — and every admitted file's repository is recorded in the state's
// GitHub memory.
func TestAdmitGitHubAdmitsTheRichestIntoRoom(t *testing.T) {
	t.Parallel()

	live := map[string]origin{}
	st := state{}
	(&Crawler{logger: zerolog.Nop()}).admitGitHub(&st, live, offerFixture(), 2)

	want := map[string]origin{
		"https://raw.example/bb/4": {Stem: "gh-bb-two", Feed: "gh:bb/two"},
		"https://raw.example/aa/1": {Stem: "gh-aa-one", Feed: "gh:aa/one"},
	}
	if !reflect.DeepEqual(live, want) {
		t.Errorf("live = %v, want %v: the two richest offers, named from their repos", live, want)
	}
	if len(st.Repos) != 2 || st.Repos["bb/two"].FirstSeen.IsZero() || st.Repos["aa/one"].FirstSeen.IsZero() {
		t.Fatalf("st.Repos = %v, want exactly the two admitted repositories recorded", st.Repos)
	}
	for r, e := range st.Repos {
		if !e.FirstSeen.Equal(e.LastFileAt) {
			t.Errorf("repo %s recorded first_seen %v != last_file_at %v", r, e.FirstSeen, e.LastFileAt)
		}
	}
}

// TestAdmitGitHubSkipsLiveURLsWithoutSpendingRoom: a URL the Telegram scan
// already found stays exactly as that scan left it and costs no room; the
// cap then admits the next richest files and remembers only their
// repositories.
func TestAdmitGitHubSkipsLiveURLsWithoutSpendingRoom(t *testing.T) {
	t.Parallel()

	live := map[string]origin{"https://raw.example/bb/4": {Slug: "chan"}}
	st := state{}
	(&Crawler{logger: zerolog.Nop()}).admitGitHub(&st, live, offerFixture(), 2)

	if got := live["https://raw.example/bb/4"]; got != (origin{Slug: "chan"}) {
		t.Errorf("pre-live url re-attributed to %+v, want the telegram origin kept", got)
	}
	if len(live) != 3 {
		t.Fatalf("live = %v, want the pre-live url plus two admissions", live)
	}
	for _, u := range []string{"https://raw.example/aa/1", "https://raw.example/bb/5"} {
		if _, ok := live[u]; !ok {
			t.Errorf("live misses the admitted %s", u)
		}
	}
	if _, ok := live["https://raw.example/cc/6"]; ok {
		t.Errorf("live admits %s: the skip must not buy a fourth file past room 2", "https://raw.example/cc/6")
	}
	if len(st.Repos) != 2 || st.Repos["cc/three"] != (repoState{}) {
		t.Errorf("st.Repos = %v, want aa/one and bb/two only: cc/three was never admitted", st.Repos)
	}
}

// TestAdmitGitHubBreaksNovelTiesByURL: the (Novel desc, URL asc) order is the
// whole of the cap's determinism — with the sort gone, map iteration would
// decide which of two equal-novelty files survives, admitting a different set
// every cycle.
func TestAdmitGitHubBreaksNovelTiesByURL(t *testing.T) {
	t.Parallel()

	res := ghfind.Result{Repos: []ghfind.Repo{
		{Name: "owner/repo", Accepted: []ghfind.File{
			{URL: "https://raw.example/z/sub", Novel: 7},
			{URL: "https://raw.example/a/sub", Novel: 7},
			{URL: "https://raw.example/m/sub", Novel: 1},
		}},
	}}
	live := map[string]origin{}
	(&Crawler{logger: zerolog.Nop()}).admitGitHub(&state{}, live, res, 1)

	want := map[string]origin{"https://raw.example/a/sub": {Stem: "gh-owner-repo", Feed: "gh:owner/repo"}}
	if !reflect.DeepEqual(live, want) {
		t.Errorf("live = %v, want %v: of two equal-novelty files the URL-ascending one survives", live, want)
	}
}

// TestAdmitGitHubOrderingIsInputOrderIndependent: the admission sort must make
// the cap's choice independent of the order the finder returned its repos in —
// a cycle that admits a different set on a reshuffled result is exactly the
// map-order accident this phase exists to avoid.
func TestAdmitGitHubOrderingIsInputOrderIndependent(t *testing.T) {
	t.Parallel()

	files := []ghfind.Repo{
		{Name: "aa/one", Accepted: []ghfind.File{{URL: "https://raw.example/1", Novel: 3}}},
		{Name: "bb/two", Accepted: []ghfind.File{{URL: "https://raw.example/2", Novel: 9}}},
		{Name: "cc/three", Accepted: []ghfind.File{{URL: "https://raw.example/3", Novel: 6}}},
	}
	admit := func(rs []ghfind.Repo) map[string]origin {
		live := map[string]origin{}
		(&Crawler{logger: zerolog.Nop()}).admitGitHub(&state{}, live, ghfind.Result{Repos: rs}, 2)
		return live
	}
	first := admit(files)
	shuffled := admit([]ghfind.Repo{files[2], files[0], files[1]})
	if !reflect.DeepEqual(first, shuffled) {
		t.Errorf("shuffled repo order changed the admission: %v vs %v", first, shuffled)
	}
	want := map[string]origin{
		"https://raw.example/2": {Stem: "gh-bb-two", Feed: "gh:bb/two"},
		"https://raw.example/3": {Stem: "gh-cc-three", Feed: "gh:cc/three"},
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("live = %v, want %v", first, want)
	}
}

// TestScanGitHubOffSwitches: Enabled=false and an empty Token both end the
// phase before it touches live, the state or the network. The crawler is
// built with a nil HTTP client and a corpus URL, so any reach past the
// switches would panic (nil dereference) instead of hanging or passing
// silently — the test fails loudly the moment an off-switch stops holding.
func TestScanGitHubOffSwitches(t *testing.T) {
	t.Parallel()

	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: "corpus", URL: "https://corpus.example/sub", Managed: true}}
	now := time.Now()
	seed := state{
		Productive: map[string]channelState{"chan": {FirstSeen: now, LastSubAt: now}},
		Managed:    map[string]managedState{"https://corpus.example/sub": {LastLiveAt: now}},
		Dead:       map[string]time.Time{"https://gone.example/sub": now.Add(time.Hour)},
		Repos:      map[string]repoState{"owner/repo": {FirstSeen: now, LastFileAt: now}},
		GitHub:     ghCursor{Code: 3, Repo: 5},
	}
	run := func(t *testing.T, opts GitHubOptions) {
		t.Helper()
		st := seed
		before := state{
			Productive: maps.Clone(seed.Productive),
			Managed:    maps.Clone(seed.Managed),
			Dead:       maps.Clone(seed.Dead),
			Repos:      maps.Clone(seed.Repos),
			GitHub:     seed.GitHub,
		}
		live := map[string]origin{"https://chan.example/sub": {Slug: "chan"}}
		beforeLive := maps.Clone(live)
		c := &Crawler{opts: Options{GitHub: opts}, logger: zerolog.Nop()} // httpClient nil on purpose
		c.scanGitHub(context.Background(), &st, pf, live, nil)
		if !reflect.DeepEqual(st, before) {
			t.Errorf("state mutated by a disabled pass: %+v -> %+v", before, st)
		}
		if !reflect.DeepEqual(live, beforeLive) {
			t.Errorf("live mutated by a disabled pass: %+v -> %+v", beforeLive, live)
		}
	}
	run(t, GitHubOptions{Enabled: false, Token: "token"})
	run(t, GitHubOptions{Enabled: true, Token: ""})
	run(t, GitHubOptions{}) // the zero value keeps the phase off
}

// TestScanGitHubSkipsWhenNoBudgetRemains: an already-expired cycle context
// gives the phase a non-positive budget, and the pass must end there — before
// the census, the finder or the state. The nil client makes any reach panic.
func TestScanGitHubSkipsWhenNoBudgetRemains(t *testing.T) {
	t.Parallel()

	var pf privateFile
	pf.Subscriptions.Sources = []source{{Name: "corpus", URL: "https://corpus.example/sub", Managed: true}}
	st := state{GitHub: ghCursor{Code: 1, Repo: 2}}
	live := map[string]origin{}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	(&Crawler{opts: Options{GitHub: GitHubOptions{Enabled: true, Token: "token"}}, logger: zerolog.Nop()}).
		scanGitHub(ctx, &st, pf, live, nil)

	if !reflect.DeepEqual(st, state{GitHub: ghCursor{Code: 1, Repo: 2}}) || len(live) != 0 {
		t.Errorf("expired-budget pass touched state (%+v) or live (%v)", st, live)
	}
}

// TestGithubBudgetBoundsThePhase: a context with no deadline (RunOnce called
// directly, tests) gets the cap, a far deadline is capped, a 10-minute
// deadline is halved to ~5 minutes, and an expired one is non-positive so
// scanGitHub skips the pass instead of starting a phase that cannot finish.
func TestGithubBudget(t *testing.T) {
	t.Parallel()

	if got := githubBudget(context.Background()); got != githubPhaseCap {
		t.Errorf("deadline-less budget = %v, want the cap %v", got, githubPhaseCap)
	}

	farCtx, farCancel := context.WithTimeout(context.Background(), time.Hour)
	defer farCancel()
	if got := githubBudget(farCtx); got != githubPhaseCap {
		t.Errorf("far-deadline budget = %v, want the cap %v", got, githubPhaseCap)
	}

	tenCtx, tenCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer tenCancel()
	if got := githubBudget(tenCtx); got > 5*time.Minute || got < 5*time.Minute-time.Second {
		t.Errorf("10-minute-deadline budget = %v, want ~5 minutes (half)", got)
	}

	expiredCtx, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expiredCancel()
	if got := githubBudget(expiredCtx); got > 0 {
		t.Errorf("expired-deadline budget = %v, want non-positive so the pass skips", got)
	}
}

// TestRecordGitHubRepoPreservesFirstSeen: a repository's first-seen moment is
// the memory's anchor and must survive every later revisit, which only
// advances last_file_at — the field the prune cutoff and the ordering read.
func TestRecordGitHubRepoPreservesFirstSeen(t *testing.T) {
	t.Parallel()

	var st state
	first := time.Now()
	st.recordGitHubRepo("owner/repo", first)
	if e := st.Repos["owner/repo"]; !e.FirstSeen.Equal(first) || !e.LastFileAt.Equal(first) {
		t.Fatalf("first record = %+v, want first_seen == last_file_at == %v", e, first)
	}
	if !st.dirty {
		t.Fatal("a first record must mark the state dirty")
	}
	later := first.Add(time.Hour)
	st.dirty = false
	st.recordGitHubRepo("owner/repo", later)
	if e := st.Repos["owner/repo"]; !e.FirstSeen.Equal(first) || !e.LastFileAt.Equal(later) {
		t.Errorf("revisit = %+v, want first_seen kept at %v and last_file_at advanced to %v", e, first, later)
	}
	if !st.dirty {
		t.Error("an advancing record must mark the state dirty")
	}
	if len(st.Repos) != 1 {
		t.Errorf("Repos = %v, want one entry: revisits must not duplicate", st.Repos)
	}
}

// TestRecordGitHubCursorDirtyOnlyOnMove: the search cursor is stored on every
// pass, and a pass that ends where it began must not dirty the state — the
// file is only worth rewriting when the grid actually moved, and a cursor
// that never moves would repeat the same queries every cycle.
func TestRecordGitHubCursorDirtyOnlyOnMove(t *testing.T) {
	t.Parallel()

	var st state
	st.recordGitHubCursor(0, 0) // equals the zero cursor: nothing moved
	if st.dirty {
		t.Error("recording the same position as the zero cursor must not dirty the state")
	}
	st.recordGitHubCursor(4, 7)
	if !st.dirty || st.GitHub != (ghCursor{Code: 4, Repo: 7}) {
		t.Fatalf("after first move: dirty=%v GitHub=%+v, want dirty with code 4 repo 7", st.dirty, st.GitHub)
	}
	st.dirty = false
	st.recordGitHubCursor(4, 7)
	if st.dirty {
		t.Error("re-recording the current position must not dirty the state")
	}
	st.recordGitHubCursor(4, 8)
	if !st.dirty {
		t.Error("a moved repo index must dirty the state")
	}
}

// TestGithubReposMostRecentFirstDeterministic: the productive-repo list feeds
// the finder's revisit budget, so its order — most recently productive first,
// name breaking ties — must be the same on every cycle, not a product of map
// iteration.
func TestGithubReposMostRecentFirstDeterministic(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := state{Repos: map[string]repoState{
		"b/repo": {LastFileAt: now},
		"a/repo": {LastFileAt: now.Add(-time.Hour)},
		"c/repo": {LastFileAt: now.Add(-time.Hour)},
		"d/repo": {LastFileAt: now.Add(-2 * time.Hour)},
	}}
	want := []string{"b/repo", "a/repo", "c/repo", "d/repo"}
	if got := st.githubRepos(); !slices.Equal(got, want) {
		t.Errorf("githubRepos = %v, want %v (most recent first, name tie-break)", got, want)
	}
}

// TestPruneGitHubReposDropsStaleAndCaps: the GitHub productive memory must
// forget repositories that stopped contributing before the cutoff and, once
// past maxProductiveRepos, keep only the most recently productive — otherwise
// one good harvest week decides every later cycle's cost forever.
func TestPruneGitHubReposDropsStaleAndCaps(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := state{Repos: map[string]repoState{
		"stale-1": {LastFileAt: now.Add(-48 * time.Hour)},
		"stale-2": {LastFileAt: now.Add(-30 * time.Hour)},
		"fresh-1": {LastFileAt: now.Add(-time.Hour)},
		"fresh-2": {LastFileAt: now},
	}}
	st.pruneGitHubRepos(now.Add(-24 * time.Hour))
	if len(st.Repos) != 2 || st.Repos["stale-1"] != (repoState{}) || st.Repos["stale-2"] != (repoState{}) {
		t.Fatalf("Repos = %v, want the two stale entries dropped", st.Repos)
	}
	if !st.dirty {
		t.Error("a prune that dropped entries must mark the state dirty")
	}
	st.dirty = false
	st.pruneGitHubRepos(now.Add(-24 * time.Hour))
	if st.dirty {
		t.Error("a prune that drops nothing must not dirty the state")
	}

	flood := state{Repos: make(map[string]repoState, maxProductiveRepos+7)}
	for i := range maxProductiveRepos + 7 {
		flood.Repos[fmt.Sprintf("repo%03d", i)] = repoState{LastFileAt: now.Add(-time.Duration(i) * time.Minute)}
	}
	flood.pruneGitHubRepos(now.Add(-24 * time.Hour)) // nothing stale: the cap alone must bite
	if len(flood.Repos) != maxProductiveRepos {
		t.Fatalf("Repos = %d entries, want the %d cap", len(flood.Repos), maxProductiveRepos)
	}
	if !flood.dirty {
		t.Error("an evicting prune must mark the state dirty")
	}
	if _, ok := flood.Repos["repo119"]; !ok {
		t.Error("the newest kept entry is missing")
	}
	for i := maxProductiveRepos; i < maxProductiveRepos+7; i++ {
		if _, ok := flood.Repos[fmt.Sprintf("repo%03d", i)]; ok {
			t.Errorf("repo%03d survived the cap; the oldest entries must be evicted", i)
		}
	}
}

// TestFoldOutcomeAdvancingPublishIncrementsBarren: a survivor-free fold on a
// reading whose publish timestamp is newer than the record's must advance the
// barren streak — the count the withdrawal half of the phase compares against
// opts.Probation. A fold that never stored would leave every source at zero
// and the window would never close.
func TestFoldOutcomeAdvancingPublishIncrementsBarren(t *testing.T) {
	t.Parallel()

	var st state
	first := time.Now()
	if got := st.foldOutcome("gh-a", 0, publishedAt(1), first); got != 1 {
		t.Fatalf("first barren fold = %d, want 1", got)
	}
	if rec := st.Probation["gh-a"]; rec.Barren != 1 || rec.Published != publishedAt(1) ||
		!rec.FirstSeen.Equal(first) || !rec.LastLive.IsZero() {
		t.Fatalf("record after first fold = %+v, want barren 1 at publish %d, first_seen %v and no last_live", rec, publishedAt(1), first)
	}
	if got := st.foldOutcome("gh-a", 0, publishedAt(2), first.Add(time.Hour)); got != 2 {
		t.Errorf("second barren fold = %d, want 2: each newly published survivor-free cycle must count", got)
	}
	if rec := st.Probation["gh-a"]; rec.Barren != 2 || rec.Published != publishedAt(2) {
		t.Errorf("record = %+v, want barren 2 at publish %d", rec, publishedAt(2))
	}
	if !st.dirty {
		t.Error("a fold that advanced a record must mark the state dirty")
	}
}

// TestFoldOutcomeSamePublishFoldedOnce pins the observation rule that keeps
// many crawler reads of ONE published snapshot — and the failed service cycles
// that re-render it without moving the clock — from condemning a source: a
// fold whose publish timestamp is not newer than the record's returns the
// stored count and changes nothing. Without the guard, every crawl cycle that
// reread the same exposition would advance the streak and a source would leave
// on one published cycle's silence.
func TestFoldOutcomeSamePublishFoldedOnce(t *testing.T) {
	t.Parallel()

	var st state
	now := time.Now()
	if got := st.foldOutcome("gh-a", 0, publishedAt(7), now); got != 1 {
		t.Fatalf("first fold = %d, want 1", got)
	}
	for i := range 5 {
		st.dirty = false
		if got := st.foldOutcome("gh-a", 0, publishedAt(7), now.Add(time.Duration(i+1)*time.Minute)); got != 1 {
			t.Fatalf("re-read %d of the same publish = %d, want the stored count 1", i+1, got)
		}
		if st.dirty {
			t.Fatalf("re-read %d of the same publish dirtied the state", i+1)
		}
	}
	if rec := st.Probation["gh-a"]; rec.Barren != 1 || rec.Published != publishedAt(7) || !rec.FirstSeen.Equal(now) {
		t.Errorf("record = %+v, want barren 1 at publish %d untouched by the re-reads", rec, publishedAt(7))
	}
}

// TestFoldOutcomeSurvivorResetsAndStampsLastLive: one survivor ends the barren
// streak and records when the source last proved itself, and a later
// survivor-free PUBLISHED cycle restarts the count from one rather than
// resuming it — a source that answered live mid-window must earn its own full
// window again.
func TestFoldOutcomeSurvivorResetsAndStampsLastLive(t *testing.T) {
	t.Parallel()

	var st state
	now := time.Now()
	st.foldOutcome("gh-a", 0, publishedAt(1), now)
	live := now.Add(time.Hour)
	if got := st.foldOutcome("gh-a", 3, publishedAt(2), live); got != 0 {
		t.Fatalf("survivor fold = %d, want 0: a live answer clears the streak", got)
	}
	if rec := st.Probation["gh-a"]; rec.Barren != 0 || rec.Published != publishedAt(2) || !rec.LastLive.Equal(live) {
		t.Fatalf("record after survivor = %+v, want barren 0 at publish %d and last_live %v", rec, publishedAt(2), live)
	}
	if got := st.foldOutcome("gh-a", 0, publishedAt(3), live.Add(time.Hour)); got != 1 {
		t.Errorf("post-survivor barren fold = %d, want 1: the window restarts at zero", got)
	}
	if rec := st.Probation["gh-a"]; !rec.LastLive.Equal(live) {
		t.Errorf("a barren fold moved last_live to %v; want the survivor stamp %v kept", rec.LastLive, live)
	}
}

// TestFoldOutcomeFirstSeenAnchoredOnce: a source's first fold records when it
// entered probation, and every later fold — survivor or not — preserves that
// moment. FirstSeen is the record's anchor, so a fold rewriting it would make
// every withdrawal look freshly minted.
func TestFoldOutcomeFirstSeenAnchoredOnce(t *testing.T) {
	t.Parallel()

	var st state
	now := time.Now()
	st.foldOutcome("gh-a", 1, publishedAt(3), now)
	if rec := st.Probation["gh-a"]; !rec.FirstSeen.Equal(now) {
		t.Fatalf("first-sight fold anchored first_seen at %v, want %v", rec.FirstSeen, now)
	}
	later := now.Add(3 * time.Hour)
	st.foldOutcome("gh-a", 0, publishedAt(4), later)
	st.foldOutcome("gh-a", 2, publishedAt(5), later.Add(time.Hour))
	if rec := st.Probation["gh-a"]; !rec.FirstSeen.Equal(now) {
		t.Errorf("first_seen moved to %v across later folds; want the anchor %v kept", rec.FirstSeen, now)
	}
}

// TestFoldOutcomeNoPublishOpensNoRecord: a service that has never published a
// list — stable_last_success_timestamp_seconds absent from the exposition and
// folded as 0 — has produced no observation, so a fold on publish 0 must
// neither open a record nor dirty the state: a source first seen there would
// otherwise carry a streak a later real publish builds on without the service
// ever having probed it.
func TestFoldOutcomeNoPublishOpensNoRecord(t *testing.T) {
	t.Parallel()

	var st state
	if got := st.foldOutcome("gh-a", 0, 0, time.Now()); got != 0 {
		t.Fatalf("fold on publish 0 = %d, want 0", got)
	}
	if len(st.Probation) != 0 || st.dirty {
		t.Fatalf("a never-published reading opened a record (%+v) or dirtied the state", st.Probation)
	}
	now := time.Now()
	if got := st.foldOutcome("gh-a", 0, publishedAt(1), now); got != 1 {
		t.Fatalf("first fold after a real publish = %d, want 1", got)
	}
	if rec := st.Probation["gh-a"]; !rec.FirstSeen.Equal(now) || rec.Published != publishedAt(1) {
		t.Errorf("record = %+v, want the anchor set on the first real publish, not the zero one", rec)
	}
}

// TestForgetProbationDropsOnlyTheNamedSources: a condemned source leaves
// private.yaml in the same cycle, and its probation record must not survive it
// — a rediscovery after the dead stamp expires would otherwise re-enter
// probation carrying the old streak. Only the named records go: a map built
// from a wrongly-collected name set would forget innocent sources.
func TestForgetProbationDropsOnlyTheNamedSources(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := state{Probation: map[string]probationState{
		"gh-a": {Barren: 2, Published: publishedAt(3), FirstSeen: now},
		"gh-b": {Barren: 2, Published: publishedAt(3), FirstSeen: now},
		"gh-c": {Barren: 2, Published: publishedAt(3), FirstSeen: now},
	}}
	st.dirty = false
	st.forgetProbation("gh-b", "gh-ghost") // a name no record holds is a no-op for it
	if st.Probation["gh-b"] != (probationState{}) {
		t.Errorf("forget left gh-b behind: %+v", st.Probation)
	}
	if st.Probation["gh-a"] == (probationState{}) || st.Probation["gh-c"] == (probationState{}) {
		t.Errorf("forget dropped gh-a or gh-c: %+v", st.Probation)
	}
	if !st.dirty {
		t.Error("a forget that dropped a record must mark the state dirty")
	}
	st.dirty = false
	st.forgetProbation("gh-a", "gh-c")
	if len(st.Probation) != 0 {
		t.Errorf("second forget left %+v, want the map empty", st.Probation)
	}
}

// TestPruneProbationDropsOnlyRecordsAbsentFromKnown: a record keyed by a name
// the current private.yaml cannot mint again is residue nothing else clears —
// a source withdrawn, retired or renamed would otherwise keep its streak alive
// in the state file until a hand edit. The known set decides exactly: a prune
// that dropped records it was handed would condemn sources that still exist.
func TestPruneProbationDropsOnlyRecordsAbsentFromKnown(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := state{Probation: map[string]probationState{
		"gh-a":    {Barren: 2, Published: publishedAt(3), FirstSeen: now},
		"gh-b":    {Barren: 2, Published: publishedAt(3), FirstSeen: now},
		"gh-gone": {Barren: 4, Published: publishedAt(9), FirstSeen: now},
	}}
	st.dirty = false
	st.pruneProbation(map[string]struct{}{"gh-a": {}, "gh-b": {}})
	if len(st.Probation) != 2 || st.Probation["gh-gone"] != (probationState{}) {
		t.Fatalf("prune kept %+v, want gh-a and gh-b only", st.Probation)
	}
	if !st.dirty {
		t.Error("a prune that dropped a record must mark the state dirty")
	}
}

// TestProbationPruneAndForgetNoOpLeaveStateClean pins the dirty gate shared by
// the two cleanup helpers. A no-op cleanup rewriting the whole state file is
// the bug this class guards against: the file runs past 0.5 MB at the Dead cap
// and is fsynced and renamed on every write, so a cycle that changed nothing
// must stop short of one.
func TestProbationPruneAndForgetNoOpLeaveStateClean(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := state{Probation: map[string]probationState{
		"gh-a": {Barren: 1, Published: publishedAt(2), FirstSeen: now},
		"gh-b": {Barren: 1, Published: publishedAt(2), FirstSeen: now},
	}}
	st.dirty = false
	st.forgetProbation("gh-ghost")
	if st.dirty {
		t.Error("forgetting a name no record holds dirtied the state")
	}
	st.pruneProbation(map[string]struct{}{"gh-a": {}, "gh-b": {}})
	if st.dirty {
		t.Error("pruning a known set that covers every record dirtied the state")
	}
	if len(st.Probation) != 2 {
		t.Errorf("no-op cleanup dropped records: %+v", st.Probation)
	}
}

// publishedAt returns the publish-clock value of the n-th successive reading
// in these fixtures: the fold's clock is stable_last_success_timestamp_seconds
// — a unix time — so the fixtures step it 60 s at a time. What the older
// fixtures spelled as service cycle 1, 2, 3 now reads as one published cycle
// per step; any increasing sequence would fold the same way, the clock is only
// compared, never stored as a time.
func publishedAt(n int) uint64 { return 1_000_000 + uint64(n)*60 }

// probationExposition renders the outcome families a gh source reading carries:
// the publish clock (stable_last_success_timestamp_seconds) and, per source,
// the tested/valid pair with the given survivor count. It is deliberately
// minimal — the parser ignores every other family the service renders.
func probationExposition(published uint64, survivors map[string]int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "stable_last_success_timestamp_seconds %d\n", published)
	for name, n := range survivors {
		fmt.Fprintf(&b, "stable_source_tested_nodes{feed=\"gh\",owner=\"crawler\",source=\"%s\"} %d\n", name, n)
		fmt.Fprintf(&b, "stable_source_valid_nodes{feed=\"gh\",owner=\"crawler\",source=\"%s\"} 0\n", name)
	}
	return b.String()
}

// readingStep is one request's answer from a scripted outcomes server: the
// publish timestamp the exposition reports and each named source's survivors.
type readingStep struct {
	published uint64
	survivors map[string]int
}

// probationServer serves one readingStep per request. Requests past the script
// get a bare clock line (a reading with no source rows), so a buggy extra
// fetch folds nothing and cannot silently condemn; the hit counter the test
// asserts catches it instead.
func probationServer(t *testing.T, script []readingStep) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(hits.Add(1)) - 1
		var step readingStep
		if i < len(script) {
			step = script[i]
		} else {
			step.published = publishedAt(i + 1)
		}
		io.WriteString(w, probationExposition(step.published, step.survivors))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// ghManagedFile returns a private file holding only gh-minted managed sources,
// the population githubWithdrawals is allowed to fold.
func ghManagedFile(names ...string) privateFile {
	var pf privateFile
	for _, name := range names {
		pf.Subscriptions.Sources = append(pf.Subscriptions.Sources, source{
			Name: name, URL: fmt.Sprintf("https://%s.example/sub", name),
			Feed: "gh:" + name + "/repo", Managed: true,
		})
	}
	return pf
}

// TestGithubWithdrawalsFailsSafe pins the withdrawal half's ground rule: a
// source must never be condemned on missing evidence. Each failure shape — an
// endpoint that refuses the connection, one answering 500, a 2xx body with no
// source rows, a reading whose publish timestamp has not moved past the fold
// already recorded, and a reading that simply did not probe the source — must
// withdraw nothing AND fold nothing, or a network blip would condemn the whole
// GitHub corpus at once. The residue record (a name the file no longer holds)
// is the tripwire for the "keep going anyway" bug: only a successful reading
// may prune residue, never an aborted one.
func TestGithubWithdrawalsFailsSafe(t *testing.T) {
	t.Parallel()

	const probation = 3
	now := time.Now().Add(-time.Hour)
	newFixture := func(t *testing.T, url string, residue bool, logBuf *bytes.Buffer) (*state, privateFile, *Crawler) {
		t.Helper()
		pf := ghManagedFile("gh-a", "gh-b")
		probs := map[string]probationState{
			"gh-a": {Barren: probation - 1, Published: publishedAt(7), FirstSeen: now},
			"gh-b": {Barren: probation - 1, Published: publishedAt(7), FirstSeen: now},
		}
		if residue {
			probs["gh-gone"] = probationState{Barren: probation - 1, Published: publishedAt(7), FirstSeen: now}
		}
		st := &state{Probation: probs}
		c := &Crawler{
			opts:       Options{GitHub: GitHubOptions{Enabled: true, Probation: probation, OutcomesURL: url}},
			httpClient: &http.Client{},
			logger:     zerolog.New(logBuf),
		}
		return st, pf, c
	}
	assertUntouched := func(t *testing.T, st *state, before map[string]probationState, got map[string]struct{}) {
		t.Helper()
		if len(got) != 0 {
			t.Fatalf("withdrawn = %v, want none: a failed reading must condemn nobody", got)
		}
		if !reflect.DeepEqual(st.Probation, before) {
			t.Fatalf("probation changed across a failed reading:\n before %+v\n after  %+v", before, st.Probation)
		}
	}

	t.Run("endpoint refuses the connection", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // the listener is gone; dialing refuses
		var logBuf bytes.Buffer
		st, pf, c := newFixture(t, url, true, &logBuf)
		before := maps.Clone(st.Probation)
		got := c.githubWithdrawals(context.Background(), st, pf)
		assertUntouched(t, st, before, got)
		if !strings.Contains(logBuf.String(), "outcomes unreadable") {
			t.Errorf("the refusal was not logged as unreadable, got %q", logBuf.String())
		}
	})

	t.Run("endpoint answers 500", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		var logBuf bytes.Buffer
		st, pf, c := newFixture(t, srv.URL, true, &logBuf)
		before := maps.Clone(st.Probation)
		got := c.githubWithdrawals(context.Background(), st, pf)
		assertUntouched(t, st, before, got)
		if !strings.Contains(logBuf.String(), "outcomes unreadable") {
			t.Errorf("the 500 was not logged as unreadable, got %q", logBuf.String())
		}
	})

	t.Run("a published clock with no source rows", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, probationExposition(publishedAt(8), nil)) // a publish that rendered no per-source rows
		}))
		defer srv.Close()
		var logBuf bytes.Buffer
		st, pf, c := newFixture(t, srv.URL, true, &logBuf)
		before := maps.Clone(st.Probation)
		got := c.githubWithdrawals(context.Background(), st, pf)
		assertUntouched(t, st, before, got)
		if !strings.Contains(logBuf.String(), "carry no sources") {
			t.Errorf("the empty reading was not logged, got %q", logBuf.String())
		}
	})

	t.Run("publish timestamp has not advanced", func(t *testing.T) {
		t.Parallel()
		// The records were folded at publish 1_000_420 and the reading reports
		// that publish again: any number of crawler reads — or failed service
		// cycles — re-rendering the frozen snapshot must count once.
		srv, hits := probationServer(t, []readingStep{
			{published: publishedAt(7), survivors: map[string]int{"gh-a": 0, "gh-b": 0}},
		})
		st, pf, c := newFixture(t, srv.URL, false, &bytes.Buffer{})
		before := maps.Clone(st.Probation)
		got := c.githubWithdrawals(context.Background(), st, pf)
		assertUntouched(t, st, before, got)
		if hits.Load() != 1 {
			t.Errorf("hits = %d, want 1: the endpoint must be read exactly once", hits.Load())
		}
	})

	t.Run("source absent from the reading", func(t *testing.T) {
		t.Parallel()
		// The publish advanced, but the reading carries only a source this phase
		// does not manage: the managed gh sources were not probed in that
		// published cycle, which is missing evidence, not a barren one.
		srv, hits := probationServer(t, []readingStep{
			{published: publishedAt(8), survivors: map[string]int{"some-telegram-source": 1}},
		})
		st, pf, c := newFixture(t, srv.URL, false, &bytes.Buffer{})
		before := maps.Clone(st.Probation)
		got := c.githubWithdrawals(context.Background(), st, pf)
		assertUntouched(t, st, before, got)
		if hits.Load() != 1 {
			t.Errorf("hits = %d, want 1: the endpoint must be read exactly once", hits.Load())
		}
	})
}

// probationDoomedURL is the URL ghManagedFile gives the "gh-solo" entry the two
// window tests condemn.
const probationDoomedURL = "https://gh-solo.example/sub"

func probationFold(t *testing.T, c *Crawler, st *state, pf privateFile) map[string]struct{} {
	t.Helper()
	return c.githubWithdrawals(context.Background(), st, pf)
}

// probationCondemnFold asserts the fold that ends the window: it withdraws
// exactly url, raises the process-wide counter once, and forgets the record.
func probationCondemnFold(t *testing.T, c *Crawler, st *state, pf privateFile, url string) {
	t.Helper()
	before := metrics.Crawl.GitHubWithdrawn.Load()
	got := probationFold(t, c, st, pf)
	if len(got) != 1 {
		t.Fatalf("withdrawn = %v, want exactly %s", got, url)
	}
	if _, ok := got[url]; !ok {
		t.Fatalf("withdrawn = %v, want it to contain %s", got, url)
	}
	if d := metrics.Crawl.GitHubWithdrawn.Load() - before; d != 1 {
		t.Errorf("withdrawn counter rose by %d, want 1", d)
	}
	if _, held := st.Probation["gh-solo"]; held {
		t.Errorf("condemned record survived the withdrawal: %+v", st.Probation)
	}
}

// TestGithubWithdrawalsWithdrawsAfterTheProbationWindow drives the withdrawal
// half end to end against a scripted metrics server: readings with an
// advancing publish timestamp fold into the state, and a source observed
// survivor-free across exactly opts.Probation published cycles is withdrawn
// where one cycle short is not.
func TestGithubWithdrawalsWithdrawsAfterTheProbationWindow(t *testing.T) {
	// Not parallel: asserts the process-wide withdrawn counter's delta.
	{
		srv, hits := probationServer(t, []readingStep{
			{published: publishedAt(1), survivors: map[string]int{"gh-solo": 0}},
			{published: publishedAt(2), survivors: map[string]int{"gh-solo": 0}},
			{published: publishedAt(3), survivors: map[string]int{"gh-solo": 0}},
		})
		c := &Crawler{
			opts:       Options{GitHub: GitHubOptions{Enabled: true, Probation: 3, OutcomesURL: srv.URL}},
			httpClient: srv.Client(),
			logger:     zerolog.Nop(),
		}
		pf := ghManagedFile("gh-solo")
		var st state

		if got := probationFold(t, c, &st, pf); len(got) != 0 {
			t.Fatalf("first fold withdrew %v, want none", got)
		}
		if rec := st.Probation["gh-solo"]; rec.Barren != 1 || rec.Published != publishedAt(1) || rec.FirstSeen.IsZero() || !rec.LastLive.IsZero() {
			t.Fatalf("record after first fold = %+v, want barren 1 at publish %d with an anchor and no last_live", rec, publishedAt(1))
		}
		if got := probationFold(t, c, &st, pf); len(got) != 0 {
			t.Fatalf("second fold withdrew %v, want none: one publish short of the window", got)
		}
		if rec := st.Probation["gh-solo"]; rec.Barren != 2 {
			t.Fatalf("record after second fold = %+v, want barren 2", rec)
		}
		probationCondemnFold(t, c, &st, pf, probationDoomedURL)
		if hits.Load() != 3 {
			t.Errorf("hits = %d, want 3: one reading per fold", hits.Load())
		}
	}
}

// TestGithubWithdrawalsSurvivorRestartsTheStreak: one probe survivor mid-window
// buys the source a fresh full window, so the publish that would have condemned
// it under a never-reset streak withdraws nothing.
func TestGithubWithdrawalsSurvivorRestartsTheStreak(t *testing.T) {
	// Not parallel: asserts the process-wide withdrawn counter's delta.
	{
		srv, hits := probationServer(t, []readingStep{
			{published: publishedAt(1), survivors: map[string]int{"gh-solo": 0}},
			{published: publishedAt(2), survivors: map[string]int{"gh-solo": 1}},
			{published: publishedAt(3), survivors: map[string]int{"gh-solo": 0}},
			{published: publishedAt(4), survivors: map[string]int{"gh-solo": 0}},
			{published: publishedAt(5), survivors: map[string]int{"gh-solo": 0}},
		})
		c := &Crawler{
			opts:       Options{GitHub: GitHubOptions{Enabled: true, Probation: 3, OutcomesURL: srv.URL}},
			httpClient: srv.Client(),
			logger:     zerolog.Nop(),
		}
		pf := ghManagedFile("gh-solo")
		var st state

		probationFold(t, c, &st, pf)
		probationFold(t, c, &st, pf) // publish 2: the source answers live
		if rec := st.Probation["gh-solo"]; rec.Barren != 0 || rec.Published != publishedAt(2) || rec.LastLive.IsZero() {
			t.Fatalf("record after the survivor = %+v, want barren 0 at publish %d with last_live stamped", rec, publishedAt(2))
		}
		// Publish 3 is where a never-reset streak would have hit the window; the
		// reset must have bought the source a fresh full window.
		if got := probationFold(t, c, &st, pf); len(got) != 0 {
			t.Fatalf("fold at publish 3 withdrew %v, want none: the streak restarted at the survivor", got)
		}
		if got := probationFold(t, c, &st, pf); len(got) != 0 {
			t.Fatalf("fold at publish 4 withdrew %v, want none: only two barren cycles since the reset", got)
		}
		if rec := st.Probation["gh-solo"]; rec.Barren != 2 {
			t.Fatalf("record after publish 4 = %+v, want barren 2", rec)
		}
		probationCondemnFold(t, c, &st, pf, probationDoomedURL) // publish 5: the third barren cycle since the reset
		if hits.Load() != 5 {
			t.Errorf("hits = %d, want 5: one reading per fold", hits.Load())
		}
	}
}

// TestGithubWithdrawalsFailedServiceCyclesDoNotCondemn pins the bug the clock
// change exists for. When the service's cycles error out, only
// stable_cycles_total (the attempt counter) and stable_cycle_failures_total
// move; the per-source gauges and stable_last_success_timestamp_seconds keep
// describing the last PUBLISHED report, frozen. Folding on the attempt counter
// would read that one survivor-free report once per crawler cycle and condemn
// the source on re-renders of a snapshot the service never re-published;
// folding on the publish timestamp must count the single published reading
// exactly once, no matter how many failed cycles pile up between crawler
// folds.
func TestGithubWithdrawalsFailedServiceCyclesDoNotCondemn(t *testing.T) {
	const probation = 2
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// stable_cycles_total rises on every request — a cycle attempting and
		// failing — while the publish clock and the per-source rows stay put.
		fmt.Fprintf(w, "stable_cycles_total %d\n", attempts.Add(1))
		io.WriteString(w, probationExposition(publishedAt(1), map[string]int{"gh-solo": 0}))
	}))
	defer srv.Close()
	c := &Crawler{
		opts:       Options{GitHub: GitHubOptions{Enabled: true, Probation: probation, OutcomesURL: srv.URL}},
		httpClient: srv.Client(),
		logger:     zerolog.Nop(),
	}
	pf := ghManagedFile("gh-solo")
	var st state
	for i := range 6 {
		if got := probationFold(t, c, &st, pf); len(got) != 0 {
			t.Fatalf("fold %d of a failed-service run withdrew %v; only a newly PUBLISHED survivor-free cycle may condemn", i+1, got)
		}
	}
	if rec := st.Probation["gh-solo"]; rec.Barren != 1 || rec.Published != publishedAt(1) ||
		rec.FirstSeen.IsZero() || !rec.LastLive.IsZero() {
		t.Fatalf("record after 6 folds over failed cycles = %+v, want barren 1 at publish %d: only the one published reading may count", rec, publishedAt(1))
	}
	if n := attempts.Load(); n != 6 {
		t.Errorf("outcomes reads = %d, want 6: one per fold", n)
	}
}

// TestGithubWithdrawalsCapsAWipeAtAQuarterPerCycle: one fold must not be able
// to withdraw the whole GitHub corpus. A single bad service cycle freezes
// every per-source row survivor-free at once — the shape of the mass-death
// verdict that made the crawler refuse bulk prunes (allowShrink) — and
// probation rides the curated deny path those guards deliberately do not
// police. withdrawRoom caps the damage at a quarter of the population, deepest
// barren first: this fold of 20 due sources withdraws exactly the 5
// longest-barren, logs the warn that the cap bit, and leaves the other 15
// holding their records for the next cycle.
func TestGithubWithdrawalsCapsAWipeAtAQuarterPerCycle(t *testing.T) {
	// Not parallel: asserts the process-wide withdrawn counter's delta.
	const probation = 3
	now := time.Now()
	names := make([]string, 0, 20)
	probs := make(map[string]probationState, 20)
	for i := range 20 {
		name := fmt.Sprintf("gh-%02d", i)
		names = append(names, name)
		// Two depths past the window, the state a wipe the cap has already
		// rationed leaves behind: the deep group crossed the window first and
		// has accrued more barren cycles since. The cap must release the
		// deepest first, not whatever order the file lists.
		barren := 5
		if i < 10 {
			barren = 10
		}
		probs[name] = probationState{Barren: barren, Published: publishedAt(0), FirstSeen: now}
	}
	allBarren := make(map[string]int, len(names))
	for _, name := range names {
		allBarren[name] = 0
	}
	srv, hits := probationServer(t, []readingStep{
		{published: publishedAt(1), survivors: allBarren},
	})
	var logBuf bytes.Buffer
	c := &Crawler{
		opts:       Options{GitHub: GitHubOptions{Enabled: true, Probation: probation, OutcomesURL: srv.URL}},
		httpClient: srv.Client(),
		logger:     zerolog.New(&logBuf),
	}
	pf := ghManagedFile(names...)
	st := &state{Probation: probs}

	before := metrics.Crawl.GitHubWithdrawn.Load()
	got := probationFold(t, c, st, pf)
	want := make(map[string]struct{}, 5)
	for i := range 5 { // the five deepest of the barren-10 group: gh-00..gh-04
		want[fmt.Sprintf("https://gh-%02d.example/sub", i)] = struct{}{}
	}
	if !maps.Equal(got, want) {
		t.Errorf("withdrawn = %v, want exactly the 5 longest-barren %v", got, want)
	}
	if d := metrics.Crawl.GitHubWithdrawn.Load() - before; d != 5 {
		t.Errorf("withdrawn counter rose by %d, want 5", d)
	}
	if !strings.Contains(logBuf.String(), "more sources came due") {
		t.Errorf("the cap biting was not logged, got %q", logBuf.String())
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1: one reading per fold", hits.Load())
	}
	if len(st.Probation) != 15 {
		t.Fatalf("records after the fold = %d, want 15: the capped 15 must keep theirs", len(st.Probation))
	}
	if rec := st.Probation["gh-05"]; rec.Barren != 11 || rec.Published != publishedAt(1) {
		t.Errorf("capped deep record = %+v, want barren 11 at publish %d kept for the next cycle", rec, publishedAt(1))
	}
	if rec := st.Probation["gh-19"]; rec.Barren != 6 || rec.Published != publishedAt(1) {
		t.Errorf("capped shallow record = %+v, want barren 6 at publish %d kept for the next cycle", rec, publishedAt(1))
	}
}

// TestGithubWithdrawalsFloorWithdrawsTheNextBatchNextCycle: withdrawRoom never
// drops below minWithdrawFloor, so a 4-source corpus that all comes due clears
// two sources per fold, not the quarter-share one — and the two the cap held
// back keep their records, so the next fold (the publish clock advanced)
// withdraws exactly the next batch. Without the floor a small corpus would
// dribble out one source per cycle; without the cap the first fold would take
// all four in one write.
func TestGithubWithdrawalsFloorWithdrawsTheNextBatchNextCycle(t *testing.T) {
	// Not parallel: asserts the process-wide withdrawn counter's delta.
	const probation = 3
	names := []string{"gh-a", "gh-b", "gh-c", "gh-d"}
	allBarren := make(map[string]int, len(names))
	for _, name := range names {
		allBarren[name] = 0
	}
	srv, hits := probationServer(t, []readingStep{
		{published: publishedAt(1), survivors: allBarren},
		{published: publishedAt(2), survivors: allBarren},
	})
	now := time.Now()
	probs := make(map[string]probationState, len(names))
	for _, name := range names {
		// All four already past the window, the state a capped earlier fold of
		// the same wipe leaves behind.
		probs[name] = probationState{Barren: probation, Published: publishedAt(0), FirstSeen: now}
	}
	c := &Crawler{
		opts:       Options{GitHub: GitHubOptions{Enabled: true, Probation: probation, OutcomesURL: srv.URL}},
		httpClient: srv.Client(),
		logger:     zerolog.Nop(),
	}
	pf := ghManagedFile(names...)
	st := &state{Probation: probs}
	url := func(name string) string { return fmt.Sprintf("https://%s.example/sub", name) }

	before := metrics.Crawl.GitHubWithdrawn.Load()
	got := probationFold(t, c, st, pf)
	want := map[string]struct{}{url("gh-a"): {}, url("gh-b"): {}}
	if !maps.Equal(got, want) {
		t.Fatalf("first fold withdrew %v, want the floor's %v: two of the four due", got, want)
	}
	if d := metrics.Crawl.GitHubWithdrawn.Load() - before; d != 2 {
		t.Errorf("withdrawn counter rose by %d, want 2", d)
	}
	// The held-back pair kept its records, deepened by this fold's reading.
	for _, name := range []string{"gh-c", "gh-d"} {
		if rec := st.Probation[name]; rec.Barren != probation+1 || rec.Published != publishedAt(1) {
			t.Errorf("held-back record %s = %+v, want barren %d at publish %d", name, rec, probation+1, publishedAt(1))
		}
	}
	// Next fold, the clock advanced: exactly the held-back pair is due now and
	// the floor lets both go.
	got = probationFold(t, c, st, pf)
	want = map[string]struct{}{url("gh-c"): {}, url("gh-d"): {}}
	if !maps.Equal(got, want) {
		t.Fatalf("second fold withdrew %v, want the next batch %v", got, want)
	}
	if d := metrics.Crawl.GitHubWithdrawn.Load() - before; d != 4 {
		t.Errorf("withdrawn counter rose by %d across two folds, want 4", d)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2: one reading per fold", hits.Load())
	}
}

// TestGithubWithdrawalsNeverCondemnsWhatItDoesNotOwn: the fold's population is
// the file's MANAGED gh:-feed entries only. A managed Telegram source (its
// feed is a channel slug, not gh:) and a sheltered hand-added entry that
// happens to carry a gh: feed are never this phase's to withdraw, however
// barren the service's reading reports them — the phase's cap counts the same
// population, so an ownership bug here would let probation evict the corpus's
// own or the operator's sources.
func TestGithubWithdrawalsNeverCondemnsWhatItDoesNotOwn(t *testing.T) {
	// Not parallel: githubWithdrawals condemns nothing here, so the counter is
	// untouched; the pin is the returned set and the folded population.
	const (
		controlURL   = "https://gh-ctrl.example/sub"
		telegramURL  = "https://tg.example/sub"
		shelteredURL = "https://hand-gh.example/sub"
	)

	newFixture := func(t *testing.T) (*Crawler, privateFile) {
		t.Helper()
		srv, hits := probationServer(t, []readingStep{
			{published: publishedAt(1), survivors: map[string]int{"gh-ctrl": 1, "tg-1": 0, "hand-gh": 0}},
			{published: publishedAt(2), survivors: map[string]int{"gh-ctrl": 1, "tg-1": 0, "hand-gh": 0}},
			{published: publishedAt(3), survivors: map[string]int{"gh-ctrl": 1, "tg-1": 0, "hand-gh": 0}},
			{published: publishedAt(4), survivors: map[string]int{"gh-ctrl": 1, "tg-1": 0, "hand-gh": 0}},
		})
		var pf privateFile
		pf.Subscriptions.Sources = []source{
			{Name: "gh-ctrl", URL: controlURL, Feed: "gh:ctrl/repo", Managed: true},
			{Name: "tg-1", URL: telegramURL, Feed: "some-channel", Managed: true},
			{Name: "hand-gh", URL: shelteredURL, Feed: "gh:hand/repo"}, // sheltered: not managed
		}
		c := &Crawler{
			opts:       Options{GitHub: GitHubOptions{Enabled: true, Probation: 3, OutcomesURL: srv.URL}},
			httpClient: srv.Client(),
			logger:     zerolog.Nop(),
		}
		t.Cleanup(func() {
			if hits.Load() != 4 {
				t.Errorf("hits = %d, want 4: one reading per fold", hits.Load())
			}
		})
		return c, pf
	}

	for _, name := range []string{"telegram source", "sheltered gh source"} {
		t.Run(name, func(t *testing.T) {
			c, pf := newFixture(t)
			var st state
			for range 4 {
				if got := c.githubWithdrawals(context.Background(), &st, pf); len(got) != 0 {
					t.Fatalf("withdrawn = %v, want none: nothing here is the phase's to withdraw", got)
				}
			}
			if len(st.Probation) != 1 {
				t.Fatalf("probation = %+v, want only the managed gh source folded", st.Probation)
			}
			if rec := st.Probation["gh-ctrl"]; rec.Barren != 0 || rec.Published != publishedAt(4) || rec.FirstSeen.IsZero() || rec.LastLive.IsZero() {
				t.Errorf("control record = %+v, want barren 0 at publish %d with both stamps", rec, publishedAt(4))
			}
		})
	}
}

// TestRunOnceProbationWithdrawsAndDoesNotRemint drives the withdrawal half
// through a real cycle: the outcomes endpoint condemns the gh-minted source on
// the first reading, which leaves private.yaml through the deny path while a
// dead stamp lands in the persisted state, and the next cycle — with a channel
// still advertising the URL, so an un-stamped URL would be minted right back —
// neither re-mints it nor spends another outcomes read. Probation=1 keeps the
// test to two cycles: one to condemn, one to prove the stamp holds.
func TestRunOnceProbationWithdrawsAndDoesNotRemint(t *testing.T) {
	// Not parallel: the cycle withdraws, incrementing the process-wide counter.
	const (
		ghURL = "https://doomed.example/sub"
		tgURL = "https://kept.example/sub"
	)
	dir := t.TempDir()
	priv := filepath.Join(dir, "private.yaml")
	statePath := filepath.Join(dir, ".crawler-state.json")

	var seeded privateFile
	seeded.Subscriptions.Sources = []source{
		{Name: "gh-owner-repo", URL: ghURL, Feed: "gh:owner/repo", Managed: true},
		{Name: "kept-1", URL: tgURL, Managed: true},
	}
	if err := writePrivate(priv, seeded); err != nil {
		t.Fatalf("seed private.yaml: %v", err)
	}

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		io.WriteString(w, "stable_last_success_timestamp_seconds 1755474123\n")
		fmt.Fprintf(w, "stable_source_tested_nodes{feed=\"gh:owner/repo\",owner=\"crawler\",source=\"gh-owner-repo\"} 0\n")
		fmt.Fprintf(w, "stable_source_valid_nodes{feed=\"gh:owner/repo\",owner=\"crawler\",source=\"gh-owner-repo\"} 0\n")
	}))
	defer srv.Close()

	var ghFetches, tgFetches atomic.Int64
	page := `<a href="` + ghURL + `">a</a> <a href="` + tgURL + `">b</a>`
	c := &Crawler{
		opts: Options{
			PrivatePath: priv,
			StatePath:   statePath,
			DeadTTL:     testDeadTTL,
			Channels:    []string{"chan"},
			Pages:       1,
			GitHub:      GitHubOptions{Enabled: true, Probation: 1, OutcomesURL: srv.URL},
		},
		client: pageFetcher{pages: map[string]string{"https://t.me/s/chan": page}},
		classifyFn: func(_ context.Context, _ *http.Client, u fetch.SubscriptionURL, _ string) (classify.Result, error) {
			if string(u) == ghURL {
				ghFetches.Add(1)
			} else {
				tgFetches.Add(1)
			}
			return classify.Result{Nodes: 1}, nil
		},
		httpClient: srv.Client(),
		logger:     zerolog.Nop(),
	}

	started := time.Now()
	c.RunOnce(context.Background())

	urls := map[string]bool{}
	for _, s := range sourcesByName(t, priv) {
		urls[s.URL] = true
	}
	if urls[ghURL] {
		t.Fatalf("the withdrawn source survived the merge: %+v", urls)
	}
	if !urls[tgURL] {
		t.Fatalf("the innocent managed source was dropped with it: %+v", urls)
	}
	st := loadState(statePath, zerolog.Nop())
	deadAt, ok := st.Dead[ghURL]
	if !ok {
		t.Fatalf("the withdrawn source was not dead-stamped: Dead = %v", st.Dead)
	}
	if !deadAt.After(started) {
		t.Errorf("dead stamp %v is not in the future (cycle started %v)", deadAt, started)
	}
	if _, held := st.Probation["gh-owner-repo"]; held {
		t.Errorf("the condemned record survived the cycle: %+v", st.Probation)
	}

	// Second cycle: the channel still advertises the URL. Without the dead
	// stamp it would classify live and be minted right back into the corpus.
	c.RunOnce(context.Background())

	urls = map[string]bool{}
	for _, s := range sourcesByName(t, priv) {
		urls[s.URL] = true
	}
	if urls[ghURL] {
		t.Fatalf("a dead-stamped URL was re-minted on the next cycle: %+v", urls)
	}
	if !urls[tgURL] {
		t.Fatalf("the kept source vanished on the second cycle: %+v", urls)
	}
	if n := ghFetches.Load(); n != 1 {
		t.Errorf("the withdrawn URL was classified %d times, want exactly the 1 that condemned it", n)
	}
	if n := tgFetches.Load(); n != 2 {
		t.Errorf("the kept URL was classified %d times, want 1 per cycle: the skip must not spill onto it", n)
	}
	if hits.Load() != 1 {
		t.Errorf("outcomes reads = %d, want 1: with no gh-managed source left the endpoint must not be read again", hits.Load())
	}
	st = loadState(statePath, zerolog.Nop())
	if deadAt, ok = st.Dead[ghURL]; !ok || !deadAt.After(time.Now()) {
		t.Errorf("the dead stamp did not survive the second cycle: Dead = %v", st.Dead)
	}
	if _, held := st.Probation["gh-owner-repo"]; held {
		t.Errorf("probation re-opened for the withdrawn source: %+v", st.Probation)
	}
}
