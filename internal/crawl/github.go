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
	MaxSources    int
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
