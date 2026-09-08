package crawl //nolint:testpackage // exercises the crawl-side GitHub glue: unexported naming, admission, budget and state helpers

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/ghfind"
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
