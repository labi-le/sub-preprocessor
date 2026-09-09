package crawl

import (
	"context"
	"sort"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/ghfind"
	"domains.lst/sub-preprocessor/internal/metrics"
)

// GitHubOptions configures the GitHub discovery phase. Enabled and a token are
// both required: unauthenticated code search does not exist, so a missing token
// disables the phase rather than degrading it (docs/guides/github.md).
type GitHubOptions struct {
	Enabled bool
	Token   string
	// CensusPath persists the endpoint census between cycles; empty disables
	// persistence and makes every cycle rebuild it from the whole corpus.
	CensusPath string
	// CensusTTL is how long a persisted census is reused. It bounds staleness
	// of the novelty baseline, and with it the risk of minting a source whose
	// endpoints the corpus has meanwhile absorbed.
	CensusTTL time.Duration
	// MaxSources caps how many GitHub-minted sources may exist at once. The
	// measured candidate universe is far larger than the corpus can carry
	// (155k novel endpoints over 578 repositories on 2026-09-07), so
	// acceptance, not discovery, is the scarce resource.
	MaxSources int
	// OutcomesURL is the service's metrics endpoint, where the phase reads the
	// per-source probe outcome it is judged by (stable_source_tested_nodes);
	// empty keeps the withdrawal half of the phase off. Probation is how many
	// consecutive survivor-free SERVICE cycles condemn a source — the fold's
	// mirror of the six-cycle retirement rule; 0 keeps the withdrawal half off
	// too. The pair is what lets MaxSources stay a bound on standing cost
	// rather than on intake forever: a batch that never proves itself leaves
	// on its own (docs/guides/github.md).
	OutcomesURL   string
	Probation     int
	Concurrency   int
	MaxRepos      int
	FilesPerRepo  int
	AcceptPerRepo int
	MinNovel      int
	Fresh         time.Duration
	SearchCode    int
	SearchRepo    int
}

// ghFeedPrefix marks a minted entry as this phase's work, in the entry's own
// feed field: the phase's source cap counts what it already owns, and the
// Telegram mint's slug attribution cannot collide with a label carrying ':'.
const ghFeedPrefix = "gh:"

// maxGitHubStem caps a minted GitHub name stem. Wider than maxSlug because
// owner and repository are both meaningful and a truncated stem collides into
// ordinals: "gh-morpheusadam-v2ray-config" is 29 bytes and still readable.
const maxGitHubStem = 40

// githubPhaseCap and githubPhaseShare bound one pass. The phase is auxiliary:
// the channel graph is what a cycle cannot resume, and the mint is what a cycle
// exists for, so a GitHub overrun — a census rebuild against a corpus of slow
// hosts, a secondary-limit sleep storm — must end the PHASE, never the cycle.
// The pass therefore gets at most half of whatever budget is left, and never
// more than the cap.
const (
	githubPhaseCap   = 20 * time.Minute
	githubPhaseShare = 2
)

// scanGitHub runs one GitHub discovery pass and folds its accepted files into
// live, the same map the Telegram scan fills, so everything downstream — the
// mint, the liveness streaks, the retirement window, the bulk-prune floor —
// treats a GitHub source exactly like any other managed source.
//
// dead is the cycle's remembered-dead set: a URL under a standing dead stamp is
// handed to the finder as already known, so it is never minted again while the
// stamp holds, matching the discovery path's own rule.
func (c *Crawler) scanGitHub(ctx context.Context, st *state, pf privateFile, live map[string]origin, dead map[string]time.Time) {
	opts := c.opts.GitHub
	if !opts.Enabled || opts.Token == "" {
		return
	}
	budget := githubBudget(ctx)
	if budget <= 0 {
		c.logger.Warn().Msg("github phase: no cycle budget left; skipping the pass")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	census := c.census(ctx, pf)
	if census == nil {
		c.logger.Error().Msg("github phase: no endpoint census; refusing to accept anything rather than minting against no baseline")
		metrics.Crawl.GitHubErrors.Add(1)
		return
	}
	room := opts.MaxSources - ghManagedCount(pf)
	if room <= 0 {
		c.logger.Info().Int("cap", opts.MaxSources).
			Msg("github phase: source cap reached; revisiting nothing this cycle")
		return
	}
	finder := ghfind.New(ghfind.Options{
		Token:         opts.Token,
		HTTP:          c.httpClient,
		MaxRepos:      opts.MaxRepos,
		FilesPerRepo:  opts.FilesPerRepo,
		AcceptPerRepo: opts.AcceptPerRepo,
		MinNovel:      opts.MinNovel,
		Fresh:         opts.Fresh,
		Concurrency:   opts.Concurrency,
		SearchCode:    opts.SearchCode,
		SearchRepo:    opts.SearchRepo,
		Logger:        c.logger,
	})
	res, err := finder.Discover(ctx, ghfind.Input{
		Census:     census,
		Productive: st.githubRepos(),
		Known:      ghKnown(pf, dead),
		Cursor:     ghfind.Cursor{Code: st.GitHub.Code, Repo: st.GitHub.Repo},
	})
	st.recordGitHubCursor(res.Cursor.Code, res.Cursor.Repo)
	ghReport(res.Stats)
	if err != nil {
		c.logger.Warn().Err(err).Msg("github phase ended early")
	}
	c.admitGitHub(st, live, res, room)
}

// withholdCondemned folds this cycle's service outcomes and withholds the
// GitHub sources probation condemns from the write that follows: a condemned
// source rides the same deny path as a curated one (mintRetained drops it and
// mergeManaged reports the count) and is dead-stamped, so the next cycle's
// discovery cannot re-mint it while the stamp holds. The fold and the stamp
// are persisted here because an unsaved fold is replayed against the old file
// next cycle, counting one service snapshot twice; the save is dirty-gated.
func (c *Crawler) withholdCondemned(ctx context.Context, st *state, pf privateFile, denied map[string]struct{}) {
	gh := c.opts.GitHub
	if !gh.Enabled || gh.Probation <= 0 || gh.OutcomesURL == "" {
		return
	}
	if withdrawn := c.githubWithdrawals(ctx, st, pf); len(withdrawn) > 0 {
		urls := make([]string, 0, len(withdrawn))
		for u := range withdrawn {
			denied[u] = struct{}{}
			urls = append(urls, u)
		}
		st.recordDead(urls, c.opts.DeadTTL, time.Now())
	}
	c.persistState(st)
}

// githubWithdrawals reads the service's per-source outcomes and returns the
// URLs of GitHub-minted sources whose probation has run out, folding this
// cycle's reading into the state as it goes. Probation is the withdrawal half
// of the phase, and it is deliberately second-hand: nothing the crawler can
// measure at mint time predicts whether the service's probe will keep a
// source's nodes (docs/guides/github.md), so a minted source gets Probation
// SERVICE cycles to prove itself on the probe that matters, and is condemned
// only on readings of it — never on a crawler-side guess.
//
// The fold fails safe: an unreadable endpoint, a parse failure, an empty
// reading or one whose cycle has not moved since the source's last fold
// withdraws nothing and folds nothing, because a source must never be
// condemned on missing evidence. Records for sources private.yaml no longer
// holds are pruned on every successful reading, and a condemned source's
// record is forgotten with it.
func (c *Crawler) githubWithdrawals(ctx context.Context, st *state, pf privateFile) map[string]struct{} {
	opts := c.opts.GitHub
	if !opts.Enabled || opts.Probation <= 0 || opts.OutcomesURL == "" {
		return nil
	}
	// The fold keys sources by NAME, which is the identity the service's
	// outcome metric carries (its source= label); the file's feed is the only
	// way to tell this phase's entries from the Telegram mint's.
	byName := make(map[string]string)
	for _, s := range pf.Subscriptions.Sources {
		if s.Managed && s.URL != "" && strings.HasPrefix(s.Feed, ghFeedPrefix) {
			byName[s.Name] = s.URL
		}
	}
	if len(byName) == 0 {
		return nil
	}
	reading, err := fetchOutcomes(ctx, c.httpClient, opts.OutcomesURL)
	if err != nil {
		c.logger.Warn().Err(err).Str("url", opts.OutcomesURL).
			Msg("github probation: outcomes unreadable; withdrawing nothing")
		return nil
	}
	if len(reading.Sources) == 0 {
		c.logger.Warn().Str("url", opts.OutcomesURL).
			Msg("github probation: outcomes carry no sources; withdrawing nothing")
		return nil
	}
	now := time.Now()
	names := make([]string, 0, len(byName))
	known := make(map[string]struct{}, len(byName))
	for name := range byName {
		names = append(names, name)
		known[name] = struct{}{}
	}
	// Sorted so the fold and its per-source lines read in file order, not map
	// order: map iteration would make a capped cycle's tie-breaks random.
	sort.Strings(names)
	due := st.foldReading(names, reading, opts.Probation, now)
	if room := withdrawRoom(len(byName)); len(due) > room {
		c.logger.Warn().Int("due", len(due)).Int("withdrawing", room).Int("population", len(byName)).
			Msg("github probation: more sources came due than one cycle may withdraw; the rest wait for the next reading")
		due = due[:room]
	}
	withdrawn := make(map[string]struct{}, len(due))
	condemned := make([]string, 0, len(due))
	for _, v := range due {
		u := byName[v.name]
		withdrawn[u] = struct{}{}
		condemned = append(condemned, v.name)
		c.logger.Info().Str("source", v.name).Str("url", u).
			Int("valid_nodes", v.valid).Int("barren_cycles", v.barren).
			Msg("github source withdrawn: zero probe survivors across the probation window")
	}
	st.pruneProbation(known)
	if len(condemned) > 0 {
		st.forgetProbation(condemned...)
		metrics.Crawl.GitHubWithdrawn.Add(int64(len(condemned)))
	}
	return withdrawn
}

// withdrawRoom bounds how many sources one cycle may withdraw. The crawler
// already refuses to delete a large share of the corpus in one write
// (allowShrink, bulkPruneMinDrop, bulkPrunePercent) because a provider outage
// or a tunnel restart fabricates a mass-death verdict; a probation cycle can
// fabricate the same verdict from one bad service cycle, and rides a path —
// the curated deny list — that those guards deliberately do not police. The
// floor keeps a small corpus collectible in one pass while turning a wipe into
// attrition an operator can see coming in the withdrawn counter.
func withdrawRoom(population int) int {
	return max(minWithdrawFloor, population*withdrawPercent/percentScale)
}

// withdrawPercent and minWithdrawFloor shape that bound: a quarter of the
// GitHub population per cycle, never fewer than two. A wholly barren corpus of
// five clears in three cycles; sixty takes about twelve, because the quarter
// is of the SHRINKING population and the floor only takes over at the tail.
// At hourly cycles that is half a day to empty the phase's whole corpus, which
// is the point: slow enough to notice in the withdrawn counter, fast enough
// that nothing barren lingers a week.
const (
	withdrawPercent  = 25
	minWithdrawFloor = 2
)

// githubBudget is how long this pass may run: half the cycle's remaining time,
// capped, so the recheck and the mint that follow keep the other half. A
// context with no deadline (RunOnce called directly, tests) gets the cap.
func githubBudget(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return githubPhaseCap
	}
	if share := time.Until(deadline) / githubPhaseShare; share < githubPhaseCap {
		return share
	}
	return githubPhaseCap
}

// admitGitHub folds accepted files into live, richest first, until the source
// cap's remaining room is spent. Ordering by novel endpoints is what makes the
// cap a choice rather than an accident of map iteration: when the phase finds
// more than the corpus may carry, the files that survive are the ones that add
// the most the corpus does not already have.
func (c *Crawler) admitGitHub(st *state, live map[string]origin, res ghfind.Result, room int) {
	type pick struct {
		file ghfind.File
		repo string
	}
	picks := make([]pick, 0, room)
	for _, repo := range res.Repos {
		for _, f := range repo.Accepted {
			picks = append(picks, pick{file: f, repo: repo.Name})
		}
	}
	sort.Slice(picks, func(i, j int) bool {
		if picks[i].file.Novel != picks[j].file.Novel {
			return picks[i].file.Novel > picks[j].file.Novel
		}
		return picks[i].file.URL < picks[j].file.URL
	})
	now := time.Now()
	admitted, novel := 0, 0
	for _, p := range picks {
		if admitted == room {
			break
		}
		if _, seen := live[p.file.URL]; seen {
			continue
		}
		live[p.file.URL] = origin{Stem: ghStem(p.repo), Feed: ghFeedPrefix + p.repo}
		st.recordGitHubRepo(p.repo, now)
		admitted++
		novel += p.file.Novel
	}
	metrics.Crawl.GitHubAccepted.Add(int64(admitted))
	metrics.Crawl.GitHubNovel.Add(int64(novel))
	c.logger.Info().Int("accepted", admitted).Int("offered", len(picks)).Int("room", room).
		Int("novel_endpoints", novel).Int("repos", len(res.Repos)).
		Int("skipped", res.Stats.ReposSkipped).
		Int("searches", res.Stats.SearchCalls).Int("probed", res.Stats.Probed).
		Int("live_files", res.Stats.LiveFiles).Int("sleeps", res.Stats.Sleeps).
		Int("errors", res.Stats.Errors).
		Msg("github phase finished")
}

// ghReport folds the finder's counters into the process-wide set. Accepted and
// novel endpoints are reported by admitGitHub instead: what the finder offers
// and what the cap admits are different numbers, and the mint sees the second.
func ghReport(s ghfind.Stats) {
	metrics.Crawl.GitHubSearches.Add(int64(s.SearchCalls))
	metrics.Crawl.GitHubRepos.Add(int64(s.Trees))
	metrics.Crawl.GitHubSkipped.Add(int64(s.ReposSkipped))
	metrics.Crawl.GitHubProbed.Add(int64(s.Probed))
	metrics.Crawl.GitHubLiveFiles.Add(int64(s.LiveFiles))
	metrics.Crawl.GitHubSleeps.Add(int64(s.Sleeps))
	metrics.Crawl.GitHubErrors.Add(int64(s.Errors))
}

// census returns the novelty baseline: the endpoints the whole shipped corpus
// already carries. A persisted census younger than the TTL is reused; otherwise
// every configured source URL is fetched once and folded. A rebuild that fails
// falls back to the stale file — an out-of-date baseline overstates novelty a
// little, while no baseline at all would mint blind.
func (c *Crawler) census(ctx context.Context, pf privateFile) *ghfind.Census {
	opts := c.opts.GitHub
	var (
		stale   *ghfind.Census
		staleAt time.Time
	)
	if opts.CensusPath != "" {
		loaded, at, err := ghfind.LoadCensus(opts.CensusPath)
		switch {
		case err != nil:
			c.logger.Warn().Err(err).Str("path", opts.CensusPath).Msg("census file unusable; rebuilding")
		case loaded != nil && time.Since(at) < opts.CensusTTL:
			c.logger.Debug().Int("endpoints", loaded.Len()).Time("built", at).Msg("census reused")
			return loaded
		default:
			stale, staleAt = loaded, at
		}
	}
	urls := c.corpusURLs(pf)
	if len(urls) == 0 {
		return stale
	}
	built, err := ghfind.BuildCensus(ctx, c.httpClient, urls, opts.Concurrency, c.logger)
	if err != nil {
		// Counted, not just logged: a rebuild that keeps failing — a corpus of
		// slow hosts, or one grown past what the phase budget can fetch —
		// silently ages the novelty baseline, and the stale-fallback path is
		// otherwise invisible to every panel. The phase then finds itself with
		// no budget left and yields nothing, which looks like a quiet cycle
		// rather than a fault.
		metrics.Crawl.GitHubErrors.Add(1)
		c.logger.Warn().Err(err).Int("sources", len(urls)).Dur("age", time.Since(staleAt)).
			Msg("census rebuild failed; falling back to the persisted one and accepting only against that older baseline")
		return stale
	}
	c.logger.Info().Int("sources", len(urls)).Int("endpoints", built.Len()).Msg("endpoint census built")
	if opts.CensusPath != "" {
		if saveErr := ghfind.SaveCensus(opts.CensusPath, built); saveErr != nil {
			c.logger.Warn().Err(saveErr).Str("path", opts.CensusPath).Msg("census save failed")
		}
	}
	return built
}

// corpusURLs collects every source URL the service currently fetches: the
// overlay's own entries plus the curated files the mint already reads for names
// and withholding. Novelty measured against a subset of that would overstate
// itself, which docs/guides/sources.md names as the counting defect.
func (c *Crawler) corpusURLs(pf privateFile) []string {
	seen := make(map[string]struct{}, len(pf.Subscriptions.Sources))
	urls := make([]string, 0, len(pf.Subscriptions.Sources))
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		if _, dup := seen[u]; dup {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	for _, s := range pf.Subscriptions.Sources {
		add(s.URL)
	}
	for u := range curatedURLs(c.opts.CuratedPaths, c.logger) {
		add(u)
	}
	return urls
}

// ghKnown names the URLs the finder must not offer as new: everything the
// overlay already carries, and everything under a standing dead stamp. Both are
// still probed — their endpoints belong in the running union, or a duplicate
// found elsewhere would read as novel — but neither can be minted again.
func ghKnown(pf privateFile, dead map[string]time.Time) map[string]bool {
	known := make(map[string]bool, len(pf.Subscriptions.Sources)+len(dead))
	for _, s := range pf.Subscriptions.Sources {
		if s.URL != "" {
			known[s.URL] = true
		}
	}
	for u := range dead {
		known[u] = true
	}
	return known
}

// ghManagedCount counts the sources this phase owns, by the feed label it mints.
func ghManagedCount(pf privateFile) int {
	n := 0
	for _, s := range pf.Subscriptions.Sources {
		if s.Managed && strings.HasPrefix(s.Feed, ghFeedPrefix) {
			n++
		}
	}
	return n
}

// ghStem builds a minted name stem from "owner/repo": the config source-name
// alphabet (^[a-z0-9-]+$), prefixed so a GitHub entry is recognisable in the
// file, capped at maxGitHubStem. The mint appends an ordinal when a repository
// contributes more than one file.
func ghStem(repo string) string {
	buf := make([]byte, 0, maxGitHubStem)
	buf = append(buf, 'g', 'h', '-')
	dash := false
	for i := 0; i < len(repo) && len(buf) < maxGitHubStem; i++ {
		ch := repo[i]
		switch {
		case ch >= 'A' && ch <= 'Z':
			buf = append(buf, ch+'a'-'A')
			dash = false
		case ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9':
			buf = append(buf, ch)
			dash = false
		case !dash:
			buf = append(buf, '-')
			dash = true
		}
	}
	return string(trimTrailingDash(buf))
}

func trimTrailingDash(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	return b
}
