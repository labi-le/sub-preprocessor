package crawl

import (
	"context"
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

// scanNode is a channel queued for crawling at a given repost-graph depth.
// configured marks an operator-supplied seed, which gets the full page budget.
type scanNode struct {
	channel    string
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
	visited := map[string]bool{}
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
	queue := make([]scanNode, 0, len(seeds))
	for slug, configured := range seeds {
		queue = append(queue, scanNode{channel: slug, configured: configured})
	}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.channel == "" || visited[n.channel] {
			continue
		}
		if n.depth > 0 {
			if discovered >= maxDiscovered {
				continue
			}
			discovered++
		}
		visited[n.channel] = true

		for _, ch := range c.scanChannel(ctx, n, st, live, &inline, &cursors, rej) {
			if !visited[ch] {
				queue = append(queue, scanNode{channel: ch, depth: n.depth + 1})
			}
		}
	}
	c.reportCursors(cursors)
	rej.report(live)
	return live, inline
}

// buildSeeds collects the depth-0 seed channels mapped to whether the operator
// configured them (CRAWL_CHANNELS or the channels file). Remembered productive
// channels are seeds too — always expanded — but not configured: they pay the
// shallower discovered page budget, because they accumulate across cycles and
// paying full Pages for each is what makes a cycle cost more than the last.
func (c *Crawler) buildSeeds(st *state) map[string]bool {
	seeds := map[string]bool{}
	addSeed := func(s string) {
		if slug := normalizeSlug(s); slug != "" {
			seeds[slug] = true
		}
	}
	for _, s := range c.opts.Channels {
		addSeed(s)
	}
	for _, s := range loadChannels(c.opts.ChannelsPath, c.logger).Channels {
		addSeed(s)
	}
	for _, slug := range st.seeds() {
		if _, ok := seeds[slug]; !ok {
			seeds[slug] = false
		}
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
	pages, cursorLost := c.scrapeChannel(ctx, n.channel, c.pagesFor(n))
	if cursorLost {
		cs.paged++
		cs.lost++
	} else if len(pages) > 1 {
		cs.paged++
	}
	if len(pages) == 0 {
		return nil
	}

	cand := c.harvestPages(pages, inline, rej, n.channel)
	found, _ := c.classifyAll(ctx, keys(cand), rej, n.channel)
	for u := range found {
		// First discoverer wins: BFS visits seeds before discovered channels,
		// so attribution prefers the operator-configured origin.
		if _, ok := live[u]; !ok {
			live[u] = n.channel
		}
	}
	if len(found) > 0 {
		st.record(n.channel, time.Now())
	}
	c.logger.Info().Str("channel", n.channel).Int("depth", n.depth).
		Int("pages", len(pages)).Int("subs", len(found)).Msg("scanned channel")

	// Thematic gate: expand into referenced channels only from seeds or from
	// channels that actually produced subscriptions.
	if n.depth >= c.opts.MaxDepth || (n.depth > 0 && len(found) == 0) {
		return nil
	}
	return extractChannels(pages, n.channel)
}

// harvestPages pulls the subscription candidates out of a channel's scraped
// pages and, in the same pass, accumulates its inline nodes. The inline
// accumulator is cycle-wide and capped: one prolific channel must not spend the
// whole budget, and a single page can overshoot the cap in one append.
//
// Every URL the candidate gates turn down is recorded against channel, so a link
// dropped before it was ever fetched is as visible as one that failed to
// classify. rej dedupes, which matters here: the same link is repeated across
// posts and pages.
func (c *Crawler) harvestPages(pages []string, inline *[]string, rej *rejects, channel string) map[string]struct{} {
	cand := map[string]struct{}{}
	for _, p := range pages {
		for _, raw := range extractURLs(p) {
			ok, reason, err := candidate(raw)
			if !ok {
				rej.record(channel, raw, reason, 0, err)
				continue
			}
			cand[raw] = struct{}{}
		}
		if c.opts.InlineEnabled && len(*inline) < maxInlineAccum {
			*inline = append(*inline, extractInlineNodes(p)...)
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
// cursorLost reports that the walk stopped only because a fetched page carried
// no cursor while the page budget still had room — the one outcome that is
// indistinguishable from a short channel per-channel but diagnostic in
// aggregate; see cursorStats.
func (c *Crawler) scrapeChannel(ctx context.Context, channel string, pages int) (out []string, cursorLost bool) {
	before := ""
	for range pages {
		u := "https://t.me/s/" + channel
		if before != "" {
			u += "?before=" + before
		}
		page, err := c.client.page(ctx, u)
		if err != nil {
			c.logger.Warn().Err(err).Str("channel", channel).Msg("channel page fetch failed")
			return out, false
		}
		if page == "" {
			return out, false
		}
		out = append(out, page)
		cur := pageCursor(page)
		if cur == "" {
			return out, len(out) < pages
		}
		before = cur
	}
	return out, false
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

// normalizeSlug turns a seed entry (bare slug, @handle, or t.me URL) into a
// lowercase channel slug.
func normalizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "@")
	if i := strings.Index(s, "t.me/"); i >= 0 {
		s = s[i+len("t.me/"):]
	}
	s = strings.TrimPrefix(s, "s/")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}
