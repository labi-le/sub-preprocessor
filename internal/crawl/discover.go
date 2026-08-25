package crawl

import (
	"cmp"
	"context"
	"regexp"
	"strings"
	"time"

	"domains.lst/sub-preprocessor/internal/metrics"
)

// channelRe matches a t.me chat reference and captures the username slug, an
// optional numeric second segment, and any trailing query. The second segment
// is one shape for two things — a forum topic id or a post id — and stays
// advisory until scrapeChat settles it on the /s/ answer. Telegram usernames
// are 5-32 chars, start with a letter, and contain only letters/digits/
// underscore. The host is anchored: it must be preceded by the start of input
// or a non-hostname character, so hostnames that merely end in "t.me" (e.g.
// shortcut.me) don't match; a scheme still works because "/" is not a hostname
// character.
var channelRe = regexp.MustCompile(`(?:^|[^a-zA-Z0-9.-])t\.me/([a-zA-Z][a-zA-Z0-9_]{4,31})(?:/(\d+))?(\?[^\s"'<>]*)?`)

// reservedSlugs are t.me paths that are not channels.
var reservedSlugs = map[string]bool{
	"s": true, "share": true, "iv": true, "joinchat": true, "addstickers": true,
	"addemoji": true, "addtheme": true, "proxy": true, "socks": true, "setlanguage": true,
	"bg": true, "login": true, "confirmphone": true, "tg": true, "telegram": true,
}

// discoveredPages caps how many pages are fetched per cycle for channels the
// operator did not configure: reposted discoveries and remembered productive
// seeds alike, since both re-enter every cycle and their number only grows.
const discoveredPages = 3

// defaultMaxDiscovered bounds discovered (non-seed) visits per cycle when
// MaxChannels is 0. Unbounded fan-out is not a safe default at CRAWL_DEPTH=3:
// every productive channel promotes itself to a permanent seed, so each cycle's
// discoveries widen the next cycle's frontier.
const defaultMaxDiscovered = 200

// maxInlineAccum caps how many raw inline URIs are accumulated per cycle before
// dedupe: a single post could paste a huge list, so bound worst-case memory
// here (the later dedupe + InlineMax cap still applies to the survivors).
const maxInlineAccum = 20000

// chanRef is one crawl target. topic carries a Telegram forum-topic id: a
// group has no t.me/s/ preview at all (measured 2026-08-14: 200 with zero
// message wraps), so the topic is what makes such a chat readable. Seeds name
// one outright; discovered refs carry the numeric segment of a reposted link,
// which may be a topic or a post id — either way scrapeChat settles it
// empirically on the /s/ answer, costing a plain channel nothing.
type chanRef struct {
	slug  string
	topic string
}

// String renders the ref in the seed syntax channels.yaml uses, "<slug>" or
// "<slug>/<topic>". state remembers a productive ref in that form; dropping the
// topic there would re-seed the next cycle with the message-less t.me/s/ shape.
func (r chanRef) String() string {
	if r.topic == "" {
		return r.slug
	}
	return r.slug + "/" + r.topic
}

// scanNode is a chat queued for crawling at a given repost-graph depth.
// configured marks an operator-supplied seed, which gets the full page budget.
// carved marks a same-group carve-out child (see scanChannel): its admitting
// identity is the full ref against topicsSeen instead of the slug against
// visited, because its slug was by construction already visited.
type scanNode struct {
	ref        chanRef
	depth      int
	configured bool
	carved     bool
}

// origin is where a harvested URL was first seen this cycle. Both fields are
// zero on the recheck-revival path, which sees no channel at all.
type origin struct {
	Slug string // channel slug, "" when unknown
	Post uint64 // Telegram message id, 0 when unknown
}

// scan performs a relevance-gated breadth-first crawl of the channel repost
// graph. Seeds are the configured channels plus every remembered productive
// channel (st), all crawled at depth 0 and always expanded; a newly discovered
// chat is expanded only when it itself yielded at least one live subscription
// (thematic gate). Discovered (non-seed) visits are capped by MaxChannels, or
// defaultMaxDiscovered when that is 0; recursion depth by MaxDepth. Chats that
// yield a live sub are recorded into st (bounded by maxProductive) so they
// become seeds on future cycles, surviving days when their recent pages carry
// no live sub. Returns every live subscription URL found, mapped to the origin
// that first yielded it, and the URLs that received a DEFINITIVE not-live
// verdict this scan, for recordDead to stamp.
//
// dead is the cycle's remembered-dead set (state.Dead), threaded to classifyAll;
// nil-safe, and nil simply fetches everything.
func (c *Crawler) scan(ctx context.Context, st *state, dead map[string]time.Time) (map[string]origin, []string, []string) {
	live := map[string]origin{}
	var inline []string
	var deadOut []string
	discovered := 0
	var cursors cursorStats
	rej := newRejects(c.logger)

	topicsBefore := metrics.Crawl.Stats()

	seeds := c.buildSeeds(st)
	if len(seeds) == 0 {
		c.logger.Warn().Str("channels_file", c.opts.ChannelsPath).
			Msg("no seed channels; add them to channels.yaml or CRAWL_CHANNELS")
		return live, inline, nil
	}
	maxDiscovered := c.opts.MaxChannels
	if maxDiscovered <= 0 {
		maxDiscovered = defaultMaxDiscovered
	}
	// Two admission identities, applied by admitNode alone: a cross-channel
	// discovery keys on the slug — a chat seeded with a topic and reposted
	// without one is one target, and scraping it twice would feed cursorStats
	// a loss the group's message-less t.me/s/ listing cannot help producing —
	// while same-group carve-out children key on the full ref, their slug
	// being by construction already visited.
	visited := make(map[string]bool, len(seeds))
	topicsSeen := make(map[string]bool, len(seeds))
	queue := make([]scanNode, 0, len(seeds))
	for slug, s := range seeds {
		queue = append(queue, scanNode{ref: chanRef{slug: slug, topic: s.topic}, configured: s.configured})
	}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if !admitNode(n, visited, topicsSeen, &discovered, maxDiscovered) {
			continue
		}

		// Children enqueue unmarked: admitNode owns every refusal, and
		// pre-filtering here would double-bookkeep the very maps it marks.
		children, dead := c.scanChannel(ctx, n, st, dead, live, &inline, &cursors, rej)
		queue = append(queue, children...)
		deadOut = append(deadOut, dead...)
	}
	c.reportCursors(cursors)
	c.reportTopics(metrics.Crawl.Since(topicsBefore))
	rej.report(live)
	return live, inline, deadOut
}

// admitNode applies the two admission identities and the shared discovery
// budget, marking both maps on acceptance: a back-reference must die whichever
// map its re-discoverer checks against, so the marks ride processing itself.
func admitNode(n scanNode, visited, topicsSeen map[string]bool, discovered *int, maxDiscovered int) bool {
	if n.carved {
		if topicsSeen[n.ref.String()] {
			return false
		}
	} else if visited[n.ref.slug] {
		return false
	}
	if n.depth > 0 {
		if *discovered >= maxDiscovered {
			return false
		}
		*discovered++
		if n.carved {
			metrics.Crawl.TopicDiscovered.Add(1)
		}
	}
	visited[n.ref.slug] = true
	topicsSeen[n.ref.String()] = true
	return true
}

// seedSpec is a depth-0 crawl target: the forum topic to read the chat through if a
// seed entry named one, and whether the operator configured it (which buys the
// full page budget; a remembered productive channel pays the shallower
// discovered budget, because those accumulate across cycles and paying full
// Pages for each is what makes a cycle cost more than the last).
type seedSpec struct {
	topic      string
	configured bool
}

// buildSeeds collects the depth-0 seeds by slug. Several entries can name one
// chat — CRAWL_CHANNELS, the channels file and the productive memory are merged
// — and a chat is one crawl target, so the fields merge rather than the entries
// competing: configured wins, and the first topic named wins over none, because
// a group's t.me/s/ listing carries no message at all while a topic that turns
// out to be a permalink falls back to that listing.
func (c *Crawler) buildSeeds(st *state) map[string]seedSpec {
	file := loadChannels(c.opts.ChannelsPath, c.logger).Channels
	seeds := make(map[string]seedSpec, len(c.opts.Channels)+len(file)+len(st.Productive))
	addSeed := func(s string, configured bool) {
		ref := parseSeed(s)
		if ref.slug == "" {
			return
		}
		prev := seeds[ref.slug]
		seeds[ref.slug] = seedSpec{
			topic:      cmp.Or(prev.topic, ref.topic),
			configured: prev.configured || configured,
		}
	}
	for _, s := range c.opts.Channels {
		addSeed(s, true)
	}
	for _, s := range file {
		addSeed(s, true)
	}
	for _, s := range st.seeds() {
		addSeed(s, false)
	}
	return seeds
}

// cursorStats aggregates page-cursor outcomes over a cycle. Pagination hangs
// off one undocumented t.me markup attribute (cursorRe); if it is renamed,
// pageCursor returns "" everywhere, every channel silently degrades to a
// single page, and nothing in the per-channel log distinguishes that from a
// channel that genuinely has one page. Only the fleet-wide ratio does.
type cursorStats struct {
	// paged counts channels that returned a page and still had budget for
	// another, i.e. channels whose cursor actually mattered.
	paged int
	// lost counts how many of those produced no cursor.
	lost int
}

// cursorAlarmMin is how many cursor-relevant channels a cycle must have
// scraped before a 100%-loss ratio is evidence of a markup break rather than a
// handful of short channels.
const cursorAlarmMin = 5

// reportCursors warns when not one channel in the cycle yielded a page cursor.
// A single channel stopping at one page is normal; every channel doing so is
// the selector breaking.
func (c *Crawler) reportCursors(cs cursorStats) {
	if cs.paged < cursorAlarmMin || cs.lost < cs.paged {
		return
	}
	c.logger.Warn().Int("channels", cs.paged).
		Msg("no channel yielded a page cursor; t.me markup likely changed and every channel was scraped one page deep")
}

// topicAlarmMin is how many topic pages a cycle must have fetched before a
// zero live yield among them is evidence of an embed markup break rather than
// a run of genuinely dead topics.
const topicAlarmMin = 5

// reportTopics logs the cycle's forum-topic numbers and warns on the embed
// analogue of reportCursors' fleet-wide loss: every fetched topic page coming
// back without one live subscription means the discussion widget stopped
// carrying what the harvest reads. Deployments without CRAWL_HTTP see the
// counters only here.
func (c *Crawler) reportTopics(ts metrics.CrawlStats) {
	c.logger.Info().
		Int("pages", int(ts.TopicPages)).
		Int("live", int(ts.TopicLive)).
		Int("empty", int(ts.TopicEmpty)).
		Int("discovered", int(ts.TopicDiscovered)).
		Int("groupEmpty", int(ts.GroupEmpty)).
		Msg("forum topics scraped this cycle")
	if ts.TopicPages >= topicAlarmMin && ts.TopicLive == 0 {
		c.logger.Warn().Int("pages", int(ts.TopicPages)).
			Msg("no topic page yielded a live subscription; t.me embed markup likely changed and every topic was read for nothing")
	}
}

// scanChannel scrapes one chat, classifies its candidate URLs into live,
// records productivity in st, and returns the referenced chats queued at the
// next depth (nil when the thematic gate closes or the scrape yielded no
// pages). A ref naming the scraped chat itself survives only the self-exclusion
// carve-out below and comes back marked, so scan keys it against topicsSeen.
//
// dead is this cycle's remembered-dead set (state.Dead); classifyAll skips
// those URLs without fetching and reports them under rejectDead. nil-safe.
func (c *Crawler) scanChannel(ctx context.Context, n scanNode, st *state, dead map[string]time.Time, live map[string]origin, inline *[]string, cs *cursorStats, rej *rejects) (children []scanNode, deadOut []string) {
	pages, cursorLost, viaTopic, listingFailed := c.scrapeChat(ctx, n.ref, c.pagesFor(n))
	if cursorLost {
		cs.paged++
		cs.lost++
	} else if len(pages) > 1 {
		cs.paged++
	}
	// A bare discovered group has nothing left once its listing came back
	// without a message: count the dead end where discovery status is known.
	// listingFailed is excluded on purpose — an unreachable host is a transport
	// problem, not a dead chat — and visited keeps it to one attempt per cycle.
	if n.depth > 0 && n.ref.topic == "" && !listingFailed &&
		(len(pages) == 0 || !strings.Contains(pages[0], messageWrap)) {
		metrics.Crawl.GroupEmpty.Add(1)
		c.logger.Warn().Str("channel", n.ref.slug).
			Msg("discovered group listing carried no message and the link named no topic")
	}
	if len(pages) == 0 {
		return nil, nil
	}

	cand := c.harvestPages(pages, inline, rej, n.ref.slug)
	var found map[string]bool
	found, _, deadOut = c.classifyAll(ctx, keys(cand), dead, rej, n.ref.slug, cand)
	for u := range found {
		// First discoverer wins: BFS visits seeds before discovered channels, so
		// attribution prefers the operator-configured origin. The slug stays
		// bare because a topic id is not part of the chat's identity; the post
		// id beside it is per URL, from the message it was harvested in.
		//
		// The slug is rejoined here rather than carried per candidate: within
		// one channel it is this one string, and cand holding a copy of it per
		// URL is what put the harvest's B/op above HEAD's past ~30 candidates.
		if _, ok := live[u]; !ok {
			live[u] = origin{Slug: n.ref.slug, Post: cand[u]}
		}
	}
	if len(found) > 0 {
		key := n.ref.slug
		if viaTopic {
			key = n.ref.String()
		}
		st.record(key, time.Now())
	}
	if viaTopic && len(found) > 0 {
		metrics.Crawl.TopicLive.Add(1)
	}
	c.logger.Info().Str("channel", n.ref.String()).Int("depth", n.depth).
		Int("pages", len(pages)).Int("subs", len(found)).Msg("scanned channel")

	// Thematic gate: expand into referenced chats only from seeds or from
	// chats that actually produced subscriptions — a carve-out child faces
	// this same gate at its own dequeue, not its parent's yield.
	if n.depth >= c.opts.MaxDepth || (n.depth > 0 && len(found) == 0) {
		return nil, deadOut
	}

	return childRefs(pages, n, viaTopic), deadOut
}

// gateVerdict is the verdict candidate gave one rejected URL, memoized for the
// rest of the page it was found on. candidate is a pure function of the URL, so
// a repost within one page needs no second parse.
type gateVerdict struct {
	raw    string
	reason rejectReason
	err    error
}

// harvestPages pulls the subscription candidates out of a channel's scraped
// pages. Candidates come from every page, inline nodes only from the newest:
// a pasted node decays with the age of its message (12 of 162 alive at <=1d
// against 5 of 178 by 1-3d, at prod's own check gate) while a subscription
// link does not. Page position only proxies that age — a page is ~20 messages,
// so a slow channel's first page already spans a week, and a dormant one
// re-seeded from the 30-day state memory contributes a page of >30d nodes.
//
// The inline accumulator is cycle-wide and bounded twice: the guard stops a
// channel once the budget is spent, the truncation catches the single append
// that overshoots it, because one page can carry more URIs than the whole cap.
//
// Every URL the candidate gates turn down is recorded against its origin, so a
// link dropped before it was ever fetched is as visible as one that failed to
// classify. rej dedupes, which matters here: the same link is repeated across
// posts and pages.
//
// Attribution is per message: a page is walked as the messages data-post
// separates it into, and a URL takes the id of the one it sits in. A URL ahead
// of the first message belongs to the page's own chrome and keeps a zero post,
// the same id a page carrying no data-post at all yields.
//
// Only the id is returned. Every candidate of one call comes from the channel
// argument, so a full origin per URL would store that one slug len(cand) times;
// scanChannel rejoins it when it copies a live URL into the cycle-wide map,
// where the slug does vary.
func (c *Crawler) harvestPages(pages []string, inline *[]string, rej *rejects, channel string) map[string]uint64 {
	cand := map[string]uint64{}
	var (
		scratch []byte
		urls    []string
	)
	for i, p := range pages {
		// One unescape per page feeds both scans, and one scratch feeds every
		// page of the channel: text aliases scratch, so everything kept below
		// is copied out of it.
		text, buf := unescapeInto(scratch, p)
		scratch = buf
		urls = harvestPage(text, origin{Slug: channel}, cand, urls, rej)
		if i == 0 && c.opts.InlineEnabled && len(*inline) < maxInlineAccum {
			*inline = appendInlineNodes(*inline, text)
			if len(*inline) > maxInlineAccum {
				*inline = (*inline)[:maxInlineAccum]
			}
		}
	}
	return cand
}

// harvestPage gates one page's URLs into cand under the id of the message they
// sit in, and hands the URL slice back so the next page reuses it: the
// per-message scans share that one slice, so segmenting a page allocates
// nothing at all. o arrives with the post the page's chrome gets and carries
// the channel for the reject log, which wants the whole origin.
func harvestPage(text string, o origin, cand map[string]uint64, urls []string, rej *rejects) []string {
	// Sized off the whole page, so no message can grow the slice.
	if n := strings.Count(text, urlScheme); cap(urls) < n {
		urls = make([]string, 0, n)
	}
	// Reset per page: last.raw points into text, which the next page's unescape
	// overwrites.
	var last gateVerdict
	for rest := text; rest != ""; {
		seg, post, tail := nextMessage(rest)
		urls = appendURLs(urls[:0], seg)
		for _, raw := range urls {
			// Both dedupes cover the GATE, not just the clone below: candidate
			// parses raw and its validator parses the host, 2 allocations and
			// 192 B a call (BenchmarkCandidate/accept, 2026-08-18), while a page
			// reposting one link 20 times asked the same question 20 times.
			// cand answers it for an accepted link, last for the rejected one a
			// post repeats — and rej still sees every occurrence, so its dedupe
			// and its untracked counter are untouched. It cannot answer this
			// itself: its verdicts are cycle-wide, and a link one channel could
			// not classify must still be tried in the next (see
			// TestRejectSummaryExcludesACandidateAcceptedElsewhere).
			if _, dup := cand[raw]; dup {
				continue
			}
			if raw == last.raw {
				rej.record(o, raw, last.reason, 0, last.err)
				continue
			}
			ok, reason, err := candidate(raw)
			if !ok {
				last = gateVerdict{raw: raw, reason: reason, err: err}
				rej.record(o, raw, reason, 0, err)
				continue
			}
			// Clone for the same reason rejects.record does, and the accepted
			// key is the longer-lived of the two: keys(cand) feeds classifyAll,
			// which puts every live URL in the cycle-wide live map scanChannel
			// returns to RunOnce for mergeManaged, so an accepted key outlives
			// this page by the whole cycle. appendURLs hands out sub-slices of
			// text, so an uncloned 40-byte key keeps its entire page reachable
			// — up to maxPageBytes, 8 MiB. That is what a string sub-slice IS,
			// not a measurement. The pin predates the reject map and is not what
			// that fix removed. The VALUE needs no such copy: it is a number.
			cand[strings.Clone(raw)] = o.Post
		}
		o.Post, rest = post, tail
	}
	return urls
}

// scrapeChat returns the HTML of up to pages consecutive t.me/s pages for a
// chat, walking backward via the ?before= cursor. Fetches are sequential,
// which naturally rate-limits the crawler against t.me.
//
// The listing walk comes first even for a ref carrying a topic, because
// <chan>/<msgid> and <chat>/<topic> are the same shape and only the /s/ answer
// separates them: measured 2026-08-14, t.me/s/<channel> returns 20 message
// wraps and 20 cursors per page while t.me/s/<group> returns none, and a
// channel post's discussion embed returns one wrap — so the topic shape cannot
// be the probe, it succeeds for both. That settlement is empirical for every
// ref, not only operator seeds: a discovered <slug>/<digits> rides the same
// probe, so a reposted topic link settles exactly as a seeded one does, and a
// discovered group with no digits just finds its empty listing and stops. A
// topic-carrying ref whose listing was reached and carried no message is the
// group case: that empty listing and its cursor outcome are dropped and the
// topic is read instead, which costs a genuine forum seed one wasted fetch per
// cycle and a channel seed nothing. A listing that never came back is not that
// case and does not fall back: a transport or status error says nothing about
// the ref's shape, and a host that just rate-limited us must not be asked twice
// in one scrape.
//
// out is newest-first on every return path, and harvestPages takes its inline
// nodes from out[0] alone: reordering here silently ages that harvest.
//
// cursorLost reports that the walk stopped only because a fetched page carried
// no cursor while the page budget still had room — the one outcome that is
// indistinguishable from a short channel per-channel but diagnostic in
// aggregate; see cursorStats. viaTopic reports that out came from the topic
// embed rather than the listing — the fact the self-exclusion carve-out is
// gated on, since only then do the page's same-group links name sibling
// topics of a chat actually being read through one. listingFailed separates a
// listing the walk never read from one it read and found empty; see
// walkListing.
func (c *Crawler) scrapeChat(ctx context.Context, ref chanRef, pages int) (out []string, cursorLost, viaTopic, listingFailed bool) {
	out, cursorLost, listingFailed = c.walkListing(ctx, ref.slug, pages)
	if listingFailed || ref.topic == "" || (len(out) > 0 && strings.Contains(out[0], messageWrap)) {
		return out, cursorLost, false, listingFailed
	}
	return c.scrapeTopic(ctx, ref), false, true, false
}

// walkListing pages backward through a chat's t.me/s listing; see
// scrapeChat for what cursorLost means. fetchFailed separates a listing the
// walk never read from one it read and found empty, which is the only thing
// that tells a group apart from an unreachable host. It never coincides with
// cursorLost: a lost cursor is a page that was read.
func (c *Crawler) walkListing(ctx context.Context, slug string, pages int) (out []string, cursorLost, fetchFailed bool) {
	before := ""
	for range pages {
		u := "https://t.me/s/" + slug
		if before != "" {
			u += "?before=" + before
		}
		page, err := c.client.page(ctx, u)
		if err != nil {
			c.logger.Warn().Err(err).Str("channel", slug).Str("url", u).
				Msg("channel listing page fetch failed")
			return out, false, true
		}
		if page == "" {
			return out, false, false
		}
		out = append(out, page)
		cur := pageCursor(page)
		if cur == "" {
			return out, len(out) < pages, false
		}
		before = cur
	}
	return out, false, false
}

// topicQuery is the only shape measured (2026-08-14) to return a forum topic's
// messages and their links: t.me/s/<chat> answers 200 with zero message wraps
// for a group, and the <chat>/<topic>/<msg> permalink form answers with no
// external link at all. The topic id alone suffices; no message id is needed.
//
// comments_limit is HONOURED, up to a server ceiling around 96 messages. An
// earlier comment here read the limit as saturating at 3, generalised from one
// seed: mifa_world/1310 does return 3 wraps at 50, 200 and 500, but it is a
// channel post's comment thread that only holds 3 comments. A real forum topic
// scales — razlo4ka7/39 returns 49 wraps and 186 KB at 50, and 96 wraps at
// 400 KB at both 200 and 500. So 50 was reading half a topic: on that seed it
// saw 7 of 13 subscription candidates and 134 of 296 inline endpoints, and the
// candidates it could not see included the only durable source there.
//
// 200 is where the ceiling is reached, so a higher value only re-sends the same
// body. Walking message ids instead costs a design-estimated ~1182 requests
// for one pass and invites the rate limiter, which is still refused.
const topicQuery = "?embed=1&discussion=1&comments_limit=200"

// messageWrap is t.me's per-message container class in both the /s/ listing and
// the discussion widget. Its presence in a /s/ page is also what separates a
// channel from a group, and so which of the two shapes a ref is read through.
const messageWrap = "tgme_widget_message_wrap"

// scrapeTopic fetches a forum topic's newest messages as one page, which is the
// only way to reach a group's messages at all — for a discovered ref settling
// its hint exactly as for a seed. The discussion widget carries no ?before=
// cursor, so a topic is always one page and never enters cursorStats — a false
// cursor loss per forum seed would drag the fleet-wide ratio toward
// reportCursors' markup alarm.
//
// Getting here means the /s/ listing was reached and carried no message either,
// so a reachable body with no message wrap means this ref yields nothing at
// all: logged apart from a failed fetch, so a topic that went empty is never
// read as one that was never reachable. The counter split follows: only a body
// that came back counts toward TopicPages (and TopicEmpty when it wraps
// nothing), so empty/pages stays a markup-health ratio over everything the
// embed answered, and a rate-limited host moves neither.
func (c *Crawler) scrapeTopic(ctx context.Context, ref chanRef) []string {
	u := "https://t.me/" + ref.slug + "/" + ref.topic + topicQuery
	page, err := c.client.page(ctx, u)
	if err != nil {
		c.logger.Warn().Err(err).Str("channel", ref.String()).Msg("topic listing fetch failed")
		return nil
	}
	metrics.Crawl.TopicPages.Add(1)
	if !strings.Contains(page, messageWrap) {
		metrics.Crawl.TopicEmpty.Add(1)
		c.logger.Warn().Str("channel", ref.String()).Int("bytes", len(page)).
			Msg("topic listing carried no message; the topic is gone or was never one")
		return nil
	}
	return []string{page}
}

// pagesFor returns how many pages to fetch for a node: the full Pages budget for
// operator-configured seeds, the shallower discovered budget for everything else
// (remembered seeds and repost discoveries), bounding per-cycle cost.
func (c *Crawler) pagesFor(n scanNode) int {
	if n.configured {
		return c.opts.Pages
	}
	return min(c.opts.Pages, discoveredPages)
}

// extractRefs returns the distinct chat references across pages, excluding
// reserved paths and bot deep links (?start=). Unlike the bare slugs it
// replaced, a ref keeps its numeric second segment, or the digits of a
// ?topic=/?comment= query, as its topic. For another chat that topic is an
// advisory hint settled empirically like a seed's (scrapeChat); a ref naming
// the scraped chat itself is kept here unjudged — whether it recurses is the
// self-exclusion carve-out, which scanChannel decides against how that node
// was actually read, a fact no pure function of these pages knows.
//
// Dedupe is by full ref: one page naming both <chat> and <chat>/<topic> yields
// both, and scan admits at most one of them per cycle via its visited check.
func extractRefs(pages []string) []chanRef {
	seen := map[string]bool{}
	var out []chanRef
	for _, page := range pages {
		for _, m := range channelRe.FindAllStringSubmatch(page, -1) {
			slug := strings.ToLower(m[1])
			if strings.HasPrefix(m[3], "?start") {
				continue // bot deep link, not a chat
			}
			if reservedSlugs[slug] {
				continue
			}
			topic := m[2]
			if topic == "" {
				topic = queryTopic(m[3])
			}
			ref := chanRef{slug: slug, topic: topic}
			if seen[ref.String()] {
				continue
			}
			seen[ref.String()] = true
			out = append(out, ref)
		}
	}
	return out
}

// queryTopic digs the digits of ?topic= or ?comment= out of a t.me query; the
// comment form is how an anchored reply inside one topic names that topic.
func queryTopic(query string) string {
	for _, param := range [...]string{"topic=", "comment="} {
		_, digits, found := strings.Cut(query, param)
		if !found {
			continue
		}
		if end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
			digits = digits[:end]
		}
		if digits != "" {
			return digits
		}
	}
	return ""
}

// childRefs turns a scanned page's chat references into the next hop's nodes.
// The self-exclusion carve-out needs how this node was read — a fact no pure
// function of these pages knows — so scanChannel hands it in.
func childRefs(pages []string, n scanNode, viaTopic bool) []scanNode {
	var kids []scanNode
	for _, r := range extractRefs(pages) {
		if r.slug == n.ref.slug {
			if !viaTopic || r.topic == "" || r.topic == n.ref.topic {
				continue
			}
		}
		kids = append(kids, scanNode{ref: r, depth: n.depth + 1, carved: r.slug == n.ref.slug})
	}
	return kids
}

// parseSeed turns a seed entry (bare slug, @handle, "<slug>/<topic>", or a t.me
// URL of either) into a chanRef. A numeric second segment is kept as a forum
// topic although a channel permalink is the same shape: the ambiguity is not
// resolvable here and scrapeChat settles it on the /s/ answer instead, so a
// topic costs a channel seed nothing. A third segment (a permalink's message
// id) and any non-numeric segment are dropped, and a reserved path yields an
// empty ref the caller skips.
func parseSeed(s string) chanRef {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "@")
	if i := strings.Index(s, "t.me/"); i >= 0 {
		s = s[i+len("t.me/"):]
	}
	s = strings.TrimPrefix(s, "s/")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	slug, rest, _ := strings.Cut(s, "/")
	if reservedSlugs[slug] {
		return chanRef{}
	}
	topic, _, _ := strings.Cut(rest, "/")
	if !isDecimal(topic) {
		return chanRef{slug: slug}
	}
	return chanRef{slug: slug, topic: topic}
}

// isDecimal reports whether s is a non-empty run of ASCII digits.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
