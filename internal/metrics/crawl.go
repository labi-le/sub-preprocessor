package metrics

import (
	"net/http"
	"sync/atomic"
)

// CrawlCounters holds the crawler's lifetime traversal counters: the topic
// phase's five below, then the GitHub discovery phase's ten. Atomics rather
// than the Metrics mutex: the /metrics handler reads them while a cycle is
// writing, and each counter moves independently of the others.
//
// They deliberately ride no Metrics CycleReport: they are lifetime totals, not
// last-cycle gauges, and the crawler runs headless wherever CRAWL_HTTP is off —
// there the per-cycle reportTopics log line carries the topic numbers.
type CrawlCounters struct {
	// TopicPages counts topic-embed fetches that got a body back at all;
	// TopicLive is the subset that yielded at least one live subscription and
	// TopicEmpty the subset that answered with no message wrap, so live/pages
	// is yield and empty/pages is embed-markup health over one denominator.
	TopicPages atomic.Int64
	TopicLive  atomic.Int64
	TopicEmpty atomic.Int64
	// TopicDiscovered counts same-group topic edges admitted through the
	// carve-out under the shared per-cycle pool; GroupEmpty counts discovered
	// groups whose t.me/s listing was reached, carried no message, and came
	// with no topic hint.
	TopicDiscovered atomic.Int64
	GroupEmpty      atomic.Int64
	// GitHubSearches counts search API calls the GitHub phase spent, code and
	// repo searches alike — the spend the per-cycle GITHUB_SEARCH_* caps bound.
	GitHubSearches atomic.Int64
	// GitHubRepos counts repositories those searches admitted and treed;
	// GitHubSkipped counts the ones the freshness/archived/fork gates refused
	// before any tree call, so repos/(repos+skipped) is the admission share of
	// the searched population and a skipped-heavy pair means the queries are
	// surfacing stale or archived repositories, not that discovery failed.
	GitHubRepos   atomic.Int64
	GitHubSkipped atomic.Int64
	// GitHubProbed counts candidate file bodies fetched, GitHubLiveFiles the
	// subset that carried nodes and GitHubAccepted the subset that cleared the
	// marginal gate and reached the mint, so live/probed is candidate-filter
	// precision and accepted/live the novelty floor's cut. A probed line that
	// climbs while live stays flat means the candidate scoring admits junk;
	// accepted pinned at zero under a climbing live means the floor sits above
	// what the files add.
	GitHubProbed    atomic.Int64
	GitHubLiveFiles atomic.Int64
	GitHubAccepted  atomic.Int64
	// GitHubNovel counts distinct endpoints the accepted files added beyond
	// the census — the phase's yield; novel/accepted is the mean novelty per
	// accepted file that GITHUB_MIN_NOVEL polices.
	GitHubNovel atomic.Int64
	// GitHubSleeps counts rate-limit sleeps — the signal that the per-cycle
	// search caps sit above the token's limits — and GitHubErrors every failure
	// inside the phase: search/metadata/tree API errors, transport errors, and
	// per-candidate probe failures alike (a raw file that 404s between the tree
	// listing and the fetch, or a body over the subscription size cap, is
	// candidate churn, not a token or network fault). Two rises that call for
	// different knobs: lower GITHUB_SEARCH_* against the first; against the
	// second, a low steady rate is candidate churn, a step change the token,
	// the network, or a GitHub outage.
	GitHubSleeps atomic.Int64
	GitHubErrors atomic.Int64
	// GitHubWithdrawn counts GitHub-minted sources the probation fold withdrew
	// after GitHubOptions.Probation consecutive survivor-free service cycles.
	// A rise is the phase paying for its novelty-only intake: sources the
	// service's probe never validated leaving the corpus, each having cost its
	// standing fetch slot until then.
	GitHubWithdrawn atomic.Int64
}

// Crawl is the process-wide counter set: the crawler is a singleton per
// process, and its Serve surface renders through this package.
var Crawl CrawlCounters

// CrawlStats is a point-in-time read of the counter set, shaped for the
// per-cycle log line: scan snapshots before its loop and reports Since it.
type CrawlStats struct {
	TopicPages      int64
	TopicLive       int64
	TopicEmpty      int64
	TopicDiscovered int64
	GroupEmpty      int64
	GitHubSearches  int64
	GitHubRepos     int64
	GitHubSkipped   int64
	GitHubProbed    int64
	GitHubLiveFiles int64
	GitHubAccepted  int64
	GitHubNovel     int64
	GitHubSleeps    int64
	GitHubErrors    int64
	GitHubWithdrawn int64
}

// Stats snapshots the counters.
func (cc *CrawlCounters) Stats() CrawlStats {
	return CrawlStats{
		TopicPages:      cc.TopicPages.Load(),
		TopicLive:       cc.TopicLive.Load(),
		TopicEmpty:      cc.TopicEmpty.Load(),
		TopicDiscovered: cc.TopicDiscovered.Load(),
		GroupEmpty:      cc.GroupEmpty.Load(),
		GitHubSearches:  cc.GitHubSearches.Load(),
		GitHubRepos:     cc.GitHubRepos.Load(),
		GitHubSkipped:   cc.GitHubSkipped.Load(),
		GitHubProbed:    cc.GitHubProbed.Load(),
		GitHubLiveFiles: cc.GitHubLiveFiles.Load(),
		GitHubAccepted:  cc.GitHubAccepted.Load(),
		GitHubNovel:     cc.GitHubNovel.Load(),
		GitHubSleeps:    cc.GitHubSleeps.Load(),
		GitHubErrors:    cc.GitHubErrors.Load(),
		GitHubWithdrawn: cc.GitHubWithdrawn.Load(),
	}
}

// Since returns what accumulated since prev was taken. Cycles never overlap
// (the crawler holds its run lock), so no other reader mutates these between a
// scan's snapshot and its report.
func (cc *CrawlCounters) Since(prev CrawlStats) CrawlStats {
	cur := cc.Stats()
	return CrawlStats{
		TopicPages:      cur.TopicPages - prev.TopicPages,
		TopicLive:       cur.TopicLive - prev.TopicLive,
		TopicEmpty:      cur.TopicEmpty - prev.TopicEmpty,
		TopicDiscovered: cur.TopicDiscovered - prev.TopicDiscovered,
		GroupEmpty:      cur.GroupEmpty - prev.GroupEmpty,
		GitHubSearches:  cur.GitHubSearches - prev.GitHubSearches,
		GitHubRepos:     cur.GitHubRepos - prev.GitHubRepos,
		GitHubSkipped:   cur.GitHubSkipped - prev.GitHubSkipped,
		GitHubProbed:    cur.GitHubProbed - prev.GitHubProbed,
		GitHubLiveFiles: cur.GitHubLiveFiles - prev.GitHubLiveFiles,
		GitHubAccepted:  cur.GitHubAccepted - prev.GitHubAccepted,
		GitHubNovel:     cur.GitHubNovel - prev.GitHubNovel,
		GitHubSleeps:    cur.GitHubSleeps - prev.GitHubSleeps,
		GitHubErrors:    cur.GitHubErrors - prev.GitHubErrors,
		GitHubWithdrawn: cur.GitHubWithdrawn - prev.GitHubWithdrawn,
	}
}

// writeCrawl renders the crawler's counters. Label-less by construction: the
// traversal is fleet-wide, and `source` stays the only per-source label in
// this package's output.
func writeCrawl(w *exposition) {
	s := Crawl.Stats()
	counter(w, "stable_crawl_topic_pages_total", "Forum-topic embed pages the crawler fetched successfully.", s.TopicPages)
	counter(w, "stable_crawl_topic_live_total", "Fetched topic pages that yielded at least one live subscription.", s.TopicLive)
	counter(w, "stable_crawl_topic_empty_total", "Fetched topic pages that answered with a reachable body and no message wrap.", s.TopicEmpty)
	counter(w, "stable_crawl_topic_discovered_total", "Same-group topic edges admitted through the carve-out.", s.TopicDiscovered)
	counter(w, "stable_crawl_group_empty_total", "Discovered groups whose t.me/s listing was reached empty and came with no topic hint.", s.GroupEmpty)
	counter(w, "stable_crawl_github_searches_total", "GitHub search API calls the discovery phase spent.", s.GitHubSearches)
	counter(w, "stable_crawl_github_repos_total", "Repositories the GitHub phase admitted and treed.", s.GitHubRepos)
	counter(w, "stable_crawl_github_skipped_total", "Repositories the GitHub phase's freshness/archived/fork gates refused before a tree call.", s.GitHubSkipped)
	counter(w, "stable_crawl_github_probed_total", "Candidate file bodies the GitHub phase fetched.", s.GitHubProbed)
	counter(w, "stable_crawl_github_live_files_total", "Probed candidate files that carried nodes.", s.GitHubLiveFiles)
	counter(w, "stable_crawl_github_accepted_total", "Probed files that cleared the marginal gate and reached the mint.", s.GitHubAccepted)
	counter(w, "stable_crawl_github_novel_endpoints_total", "Distinct endpoints the accepted files added beyond the census.", s.GitHubNovel)
	counter(w, "stable_crawl_github_rate_sleeps_total", "Rate-limit sleeps the GitHub phase took; a rise means the per-cycle caps exceed the token's limits.", s.GitHubSleeps)
	counter(w, "stable_crawl_github_errors_total", "Failures inside the GitHub phase: search/metadata/tree API errors, transport errors, and per-candidate probe errors alike; a low steady rate is candidate churn, a step change the token, the network, or a GitHub outage.", s.GitHubErrors)
	counter(w, "stable_crawl_github_withdrawn_total", "GitHub-minted sources withdrawn after a probation window with zero probe survivors; a rise means novelty-minted sources the probe never validated are leaving the corpus.", s.GitHubWithdrawn)
}

// CrawlHandler serves the crawler's counters alone, for the GET /metrics route
// on the crawler's own control surface, which has no Metrics cycle report to
// render.
func CrawlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		e := newExposition(w)
		writeCrawl(e)
		e.flush()
	})
}
