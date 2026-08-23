package metrics

import (
	"net/http"
	"sync/atomic"
)

// CrawlCounters holds the crawler's five lifetime traversal counters. Atomics
// rather than the Metrics mutex: the /metrics handler reads them while a cycle
// is writing, and each counter moves independently of the others.
//
// They deliberately ride no Metrics CycleReport: they are lifetime totals, not
// last-cycle gauges, and the crawler runs headless wherever CRAWL_HTTP is off —
// there the per-cycle reportTopics log line carries the same numbers.
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
}

// Stats snapshots the counters.
func (cc *CrawlCounters) Stats() CrawlStats {
	return CrawlStats{
		TopicPages:      cc.TopicPages.Load(),
		TopicLive:       cc.TopicLive.Load(),
		TopicEmpty:      cc.TopicEmpty.Load(),
		TopicDiscovered: cc.TopicDiscovered.Load(),
		GroupEmpty:      cc.GroupEmpty.Load(),
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
	}
}

// writeCrawl renders the five counters. Label-less by construction: the
// traversal is fleet-wide, and `source` stays the only per-source label in
// this package's output.
func writeCrawl(w *exposition) {
	s := Crawl.Stats()
	counter(w, "stable_crawl_topic_pages_total", "Forum-topic embed pages the crawler fetched successfully.", s.TopicPages)
	counter(w, "stable_crawl_topic_live_total", "Fetched topic pages that yielded at least one live subscription.", s.TopicLive)
	counter(w, "stable_crawl_topic_empty_total", "Fetched topic pages that answered with a reachable body and no message wrap.", s.TopicEmpty)
	counter(w, "stable_crawl_topic_discovered_total", "Same-group topic edges admitted through the carve-out.", s.TopicDiscovered)
	counter(w, "stable_crawl_group_empty_total", "Discovered groups whose t.me/s listing was reached empty and came with no topic hint.", s.GroupEmpty)
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
