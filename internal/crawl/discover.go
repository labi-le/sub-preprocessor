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

// discoveredPages caps how many pages are fetched for non-seed channels.
const discoveredPages = 3

// maxInlineAccum caps how many raw inline URIs are accumulated per cycle before
// dedupe: a single post could paste a huge list, so bound worst-case memory
// here (the later dedupe + InlineMax cap still applies to the survivors).
const maxInlineAccum = 20000

// scanNode is a channel queued for crawling at a given repost-graph depth.
type scanNode struct {
	channel string
	depth   int
}

// scan performs a relevance-gated breadth-first crawl of the channel repost
// graph. Seeds are the configured channels plus every remembered productive
// channel (st), all crawled at depth 0 and always expanded; a newly discovered
// channel is expanded only when it itself yielded at least one live
// subscription (thematic gate). Discovered (non-seed) visits are capped by
// MaxChannels; recursion depth by MaxDepth. Channels that yield a live sub are
// recorded into st so they become permanent seeds on future cycles, surviving
// days when their recent pages carry no live sub. Returns every live
// subscription URL found, mapped to the channel that first yielded it.
func (c *Crawler) scan(ctx context.Context, st *state) (map[string]string, []string) {
	live := map[string]string{}
	var inline []string
	visited := map[string]bool{}
	discovered := 0

	seeds := c.buildSeeds(st)
	if len(seeds) == 0 {
		c.logger.Warn().Str("channels_file", c.opts.ChannelsPath).
			Msg("no seed channels; add them to channels.yaml or CRAWL_CHANNELS")
		return live, inline
	}
	queue := make([]scanNode, 0, len(seeds))
	for slug := range seeds {
		queue = append(queue, scanNode{slug, 0})
	}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.channel == "" || visited[n.channel] {
			continue
		}
		if n.depth > 0 {
			if c.opts.MaxChannels > 0 && discovered >= c.opts.MaxChannels {
				continue
			}
			discovered++
		}
		visited[n.channel] = true

		for _, ch := range c.scanChannel(ctx, n, st, live, &inline) {
			if !visited[ch] {
				queue = append(queue, scanNode{ch, n.depth + 1})
			}
		}
	}
	return live, inline
}

// buildSeeds collects the depth-0 seed channels: configured channels, the
// hot-reloaded channels file, and remembered productive channels.
func (c *Crawler) buildSeeds(st *state) map[string]struct{} {
	seeds := map[string]struct{}{}
	addSeed := func(s string) {
		if slug := normalizeSlug(s); slug != "" {
			seeds[slug] = struct{}{}
		}
	}
	for _, s := range c.opts.Channels {
		addSeed(s)
	}
	for _, s := range loadChannels(c.opts.ChannelsPath, c.logger) {
		addSeed(s)
	}
	for _, slug := range st.seeds() {
		seeds[slug] = struct{}{}
	}
	return seeds
}

// scanChannel scrapes one channel, classifies its candidate URLs into live,
// records productivity in st, and returns the referenced channels to expand
// into (nil when the thematic gate closes or the channel yielded no pages).
func (c *Crawler) scanChannel(ctx context.Context, n scanNode, st *state, live map[string]string, inline *[]string) []string {
	pages := c.scrapeChannel(ctx, n.channel, c.pagesFor(n.depth))
	if len(pages) == 0 {
		return nil
	}

	cand := map[string]struct{}{}
	for _, p := range pages {
		for _, raw := range extractURLs(p) {
			if candidate(raw) {
				cand[raw] = struct{}{}
			}
		}
		if c.opts.InlineEnabled && len(*inline) < maxInlineAccum {
			*inline = append(*inline, extractInlineNodes(p)...)
			if len(*inline) > maxInlineAccum {
				*inline = (*inline)[:maxInlineAccum] // hard-cap a large single-page burst
			}
		}
	}
	found, _ := c.classifyAll(ctx, keys(cand))
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
		Int("subs", len(found)).Msg("scanned channel")

	// Thematic gate: expand into referenced channels only from seeds or from
	// channels that actually produced subscriptions.
	if n.depth >= c.opts.MaxDepth || (n.depth > 0 && len(found) == 0) {
		return nil
	}
	return extractChannels(pages, n.channel)
}

// scrapeChannel returns the HTML of up to pages consecutive t.me/s pages for a
// channel, walking backward via the ?before= cursor. Fetches are sequential,
// which naturally rate-limits the crawler against t.me.
func (c *Crawler) scrapeChannel(ctx context.Context, channel string, pages int) []string {
	var out []string
	before := ""
	for range pages {
		u := "https://t.me/s/" + channel
		if before != "" {
			u += "?before=" + before
		}
		page, err := c.client.page(ctx, u)
		if err != nil {
			c.logger.Warn().Err(err).Str("channel", channel).Msg("channel page fetch failed")
			break
		}
		if page == "" {
			break
		}
		out = append(out, page)
		cur := pageCursor(page)
		if cur == "" {
			break
		}
		before = cur
	}
	return out
}

// pagesFor returns how many pages to fetch at a given depth: full depth for
// seeds, shallower for discovered channels to bound fan-out cost.
func (c *Crawler) pagesFor(depth int) int {
	if depth == 0 {
		return c.opts.Pages
	}
	if c.opts.Pages < discoveredPages {
		return c.opts.Pages
	}
	return discoveredPages
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
