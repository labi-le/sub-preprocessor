package crawl

import (
	"cmp"
	"context"
	"html"
	"regexp"
	"strings"
	"time"
)

// channelRe matches a t.me channel reference and captures the username slug and
// any trailing query. Telegram usernames are 5-32 chars, start with a letter,
// and contain only letters/digits/underscore. The host is anchored: it must be
// preceded by the start of input or a non-hostname character, so hostnames that
// merely end in "t.me" (e.g. shortcut.me) don't match; a scheme still works
// because "/" is not a hostname character.
var channelRe = regexp.MustCompile(`(?:^|[^a-zA-Z0-9.-])t\.me/([a-zA-Z][a-zA-Z0-9_]{4,31})(?:/\d+)?(\?[^\s"'<>]*)?`)

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

// chanRef is one crawl target. topic carries a Telegram forum-topic id when the
// seed named one: a group has no t.me/s/ preview at all (measured 2026-08-14:
// 200 with zero message wraps), so the topic is what makes such a chat readable.
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

// scanNode is a channel queued for crawling at a given repost-graph depth.
// configured marks an operator-supplied seed, which gets the full page budget.
type scanNode struct {
	ref        chanRef
	depth      int
	configured bool
}

// scan performs a relevance-gated breadth-first crawl of the channel repost
// graph. Seeds are the configured channels plus every remembered productive
// channel (st), all crawled at depth 0 and always expanded; a newly discovered
// channel is expanded only when it itself yielded at least one live
// subscription (thematic gate). Discovered (non-seed) visits are capped by
// MaxChannels, or defaultMaxDiscovered when that is 0; recursion depth by
// MaxDepth. Channels that yield a live sub are recorded into st (bounded by
// maxProductive) so they become seeds on future cycles, surviving
// days when their recent pages carry no live sub. Returns every live
// subscription URL found, mapped to the channel that first yielded it.
func (c *Crawler) scan(ctx context.Context, st *state) (map[string]string, []string) {
	live := map[string]string{}
	var inline []string
	discovered := 0
	var cursors cursorStats
	rej := newRejects(c.logger)

	seeds := c.buildSeeds(st)
	if len(seeds) == 0 {
		c.logger.Warn().Str("channels_file", c.opts.ChannelsPath).
			Msg("no seed channels; add them to channels.yaml or CRAWL_CHANNELS")
		return live, inline
	}
	maxDiscovered := c.opts.MaxChannels
	if maxDiscovered <= 0 {
		maxDiscovered = defaultMaxDiscovered
	}
	// A chat's identity is its slug alone: a chat seeded with a forum topic and
	// reposted without one is one target, and scraping it twice would also feed
	// cursorStats a page-cursor loss the group's message-less t.me/s/ listing
	// cannot help producing.
	visited := make(map[string]bool, len(seeds))
	queue := make([]scanNode, 0, len(seeds))
	for slug, s := range seeds {
		queue = append(queue, scanNode{ref: chanRef{slug: slug, topic: s.topic}, configured: s.configured})
	}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if visited[n.ref.slug] {
			continue
		}
		if n.depth > 0 {
			if discovered >= maxDiscovered {
				continue
			}
			discovered++
		}
		visited[n.ref.slug] = true

		for _, ch := range c.scanChannel(ctx, n, st, live, &inline, &cursors, rej) {
			if !visited[ch] {
				queue = append(queue, scanNode{ref: chanRef{slug: ch}, depth: n.depth + 1})
			}
		}
	}
	c.reportCursors(cursors)
	rej.report(live)
	return live, inline
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

// scanChannel scrapes one channel, classifies its candidate URLs into live,
// records productivity in st, and returns the referenced channels to expand
// into (nil when the thematic gate closes or the channel yielded no pages).
func (c *Crawler) scanChannel(ctx context.Context, n scanNode, st *state, live map[string]string, inline *[]string, cs *cursorStats, rej *rejects) []string {
	pages, cursorLost := c.scrapeChannel(ctx, n.ref, c.pagesFor(n))
	if cursorLost {
		cs.paged++
		cs.lost++
	} else if len(pages) > 1 {
		cs.paged++
	}
	if len(pages) == 0 {
		return nil
	}

	cand := c.harvestPages(pages, inline, rej, n.ref.slug)
	found, _ := c.classifyAll(ctx, keys(cand), rej, n.ref.slug)
	for u := range found {
		// First discoverer wins: BFS visits seeds before discovered channels,
		// so attribution prefers the operator-configured origin. The bare slug
		// is the origin, not the ref: a managed source name is permanent, and a
		// topic id is not part of the chat's identity.
		if _, ok := live[u]; !ok {
			live[u] = n.ref.slug
		}
	}
	if len(found) > 0 {
		st.record(n.ref.String(), time.Now())
	}
	c.logger.Info().Str("channel", n.ref.String()).Int("depth", n.depth).
		Int("pages", len(pages)).Int("subs", len(found)).Msg("scanned channel")

	// Thematic gate: expand into referenced channels only from seeds or from
	// channels that actually produced subscriptions.
	if n.depth >= c.opts.MaxDepth || (n.depth > 0 && len(found) == 0) {
		return nil
	}
	return extractChannels(pages, n.ref.slug)
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
// Every URL the candidate gates turn down is recorded against channel, so a link
// dropped before it was ever fetched is as visible as one that failed to
// classify. rej dedupes, which matters here: the same link is repeated across
// posts and pages.
func (c *Crawler) harvestPages(pages []string, inline *[]string, rej *rejects, channel string) map[string]struct{} {
	cand := map[string]struct{}{}
	for i, p := range pages {
		// Once per page, not once per extractor: page 0 feeds both scans, and
		// UnescapeString copies the whole page whenever it finds an '&'.
		text := html.UnescapeString(p)
		for _, raw := range extractURLs(text) {
			ok, reason, err := candidate(raw)
			if !ok {
				rej.record(channel, raw, reason, 0, err)
				continue
			}
			// Clone for the same reason rejects.record does, and the accepted
			// key is the longer-lived of the two: keys(cand) feeds classifyAll,
			// which puts every live URL in the cycle-wide live map scanChannel
			// returns to RunOnce for mergeManaged, so an accepted key outlives
			// this page by the whole cycle. extractURLs hands out sub-slices of
			// text (urlRe.FindAllString returns s[a:b], then strings.TrimRight
			// narrows without copying), so an uncloned 40-byte key keeps its
			// entire page reachable — up to maxPageBytes,
			// 8 MiB. That is what a string sub-slice IS, not a measurement. The
			// pin predates the reject map and is not what that fix removed.
			//
			// Guarded like record's dedupe rather than written blind: the same
			// link recurs across posts and pages, so a blind insert pays a copy
			// per OCCURRENCE where this pays one per DISTINCT url. The
			// difference an extra lookup buys back is arithmetic, not a sample —
			// a page repeating one link 20 times pays 20 copies against 1.
			// BenchmarkHarvestPages prices both halves.
			if _, dup := cand[raw]; !dup {
				cand[strings.Clone(raw)] = struct{}{}
			}
		}
		if i == 0 && c.opts.InlineEnabled && len(*inline) < maxInlineAccum {
			*inline = append(*inline, extractInlineNodes(text)...)
			if len(*inline) > maxInlineAccum {
				*inline = (*inline)[:maxInlineAccum]
			}
		}
	}
	return cand
}

// scrapeChannel returns the HTML of up to pages consecutive t.me/s pages for a
// channel, walking backward via the ?before= cursor. Fetches are sequential,
// which naturally rate-limits the crawler against t.me.
//
// The listing walk comes first even for a ref carrying a topic, because
// <chan>/<msgid> and <chat>/<topic> are the same shape and only the /s/ answer
// separates them: measured 2026-08-14, t.me/s/<channel> returns 20 message
// wraps and 20 cursors per page while t.me/s/<group> returns none, and a
// channel post's discussion embed returns one wrap — so the topic shape cannot
// be the probe, it succeeds for both. A topic-carrying ref whose listing was
// reached and carried no message is the group case: that empty listing and its
// cursor outcome are dropped and the topic is read instead, which costs a
// genuine forum seed one wasted fetch per cycle and a channel seed nothing.
// A listing that never came back is not that case and does not fall back: a
// transport or status error says nothing about the seed's shape, and a host
// that just rate-limited us must not be asked twice in one scrape.
//
// out is newest-first on every return path, and harvestPages takes its inline
// nodes from out[0] alone: reordering here silently ages that harvest.
//
// cursorLost reports that the walk stopped only because a fetched page carried
// no cursor while the page budget still had room — the one outcome that is
// indistinguishable from a short channel per-channel but diagnostic in
// aggregate; see cursorStats.
func (c *Crawler) scrapeChannel(ctx context.Context, ref chanRef, pages int) (out []string, cursorLost bool) {
	out, cursorLost, listingFailed := c.walkListing(ctx, ref.slug, pages)
	if listingFailed || ref.topic == "" || (len(out) > 0 && strings.Contains(out[0], messageWrap)) {
		return out, cursorLost
	}
	return c.scrapeTopic(ctx, ref), false
}

// walkListing pages backward through a channel's t.me/s listing; see
// scrapeChannel for what cursorLost means. fetchFailed separates a listing the
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
// The listing saturates at 3 message wraps for comments_limit 50/100/200
// (byte-identical responses), and that ceiling is accepted: a link rotating
// every 24h is only useful from the newest messages, while walking message ids
// instead measured ~1182 requests for one pass and invites the rate limiter.
const topicQuery = "?embed=1&discussion=1&comments_limit=50"

// messageWrap is t.me's per-message container class in both the /s/ listing and
// the discussion widget. Its presence in a /s/ page is also what separates a
// channel from a group, and so which of the two shapes a seed is read through.
const messageWrap = "tgme_widget_message_wrap"

// scrapeTopic fetches a forum topic's newest messages as one page, which is the
// only way to reach a group's messages at all. The discussion widget carries no
// ?before= cursor, so a topic is always one page and never enters cursorStats —
// a false cursor loss per forum seed would drag the fleet-wide ratio toward
// reportCursors' markup alarm.
//
// Getting here means the /s/ listing was reached and carried no message either,
// so a reachable body with no message wrap means this seed yields nothing at
// all: logged apart from a failed fetch, so a topic that went empty is never
// read as one that was never reachable.
func (c *Crawler) scrapeTopic(ctx context.Context, ref chanRef) []string {
	u := "https://t.me/" + ref.slug + "/" + ref.topic + topicQuery
	page, err := c.client.page(ctx, u)
	if err != nil {
		c.logger.Warn().Err(err).Str("channel", ref.String()).Msg("topic listing fetch failed")
		return nil
	}
	if !strings.Contains(page, messageWrap) {
		c.logger.Warn().Str("channel", ref.String()).Int("bytes", len(page)).
			Msg("topic listing carried no message; the seed's topic is gone or was never one")
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

// extractChannels returns the distinct channel slugs referenced across pages,
// excluding the channel itself, reserved paths, and bot deep links (?start=).
// Discoveries are bare because identity here is the slug: the three-segment
// t.me/<chat>/<topic>/<msg> form does say which topic it belongs to, but a
// reposted link names one topic of a chat arbitrarily, and which topic of a
// group is worth reading is a judgement only the operator's seed carries.
func extractChannels(pages []string, self string) []string {
	seen := map[string]bool{}
	var out []string
	for _, page := range pages {
		for _, m := range channelRe.FindAllStringSubmatch(page, -1) {
			slug := strings.ToLower(m[1])
			if strings.HasPrefix(m[2], "?start") {
				continue // bot deep link, not a channel
			}
			if reservedSlugs[slug] || slug == self || seen[slug] {
				continue
			}
			seen[slug] = true
			out = append(out, slug)
		}
	}
	return out
}

// parseSeed turns a seed entry (bare slug, @handle, "<slug>/<topic>", or a t.me
// URL of either) into a chanRef. A numeric second segment is kept as a forum
// topic although a channel permalink is the same shape: the ambiguity is not
// resolvable here and scrapeChannel settles it on the /s/ answer instead, so a
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
