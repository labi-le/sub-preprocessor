package crawl

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// channelRe matches a t.me channel reference and captures the username slug and
// any trailing query. Telegram usernames are 5-32 chars, start with a letter,
// and contain only letters/digits/underscore.
var channelRe = regexp.MustCompile(`(?:https?://)?t\.me/([a-zA-Z][a-zA-Z0-9_]{4,31})(?:/\d+)?(\?[^\s"'<>]*)?`)

// reservedSlugs are t.me paths that are not channels.
var reservedSlugs = map[string]bool{
	"s": true, "share": true, "iv": true, "joinchat": true, "addstickers": true,
	"addemoji": true, "addtheme": true, "proxy": true, "socks": true, "setlanguage": true,
	"bg": true, "login": true, "confirmphone": true, "tg": true, "telegram": true,
}

// scan performs a relevance-gated breadth-first crawl of the channel repost
// graph. Seeds are the configured channels plus every remembered productive
// channel (st), all crawled at depth 0 and always expanded; a newly discovered
// channel is expanded only when it itself yielded at least one live
// subscription (thematic gate). Discovered (non-seed) visits are capped by
// MaxChannels; recursion depth by MaxDepth. Channels that yield a live sub are
// recorded into st so they become permanent seeds on future cycles, surviving
// days when their recent pages carry no live sub. Returns every live
// subscription URL found.
func (c *Crawler) scan(ctx context.Context, st *state) map[string]bool {
	live := map[string]bool{}
	visited := map[string]bool{}
	discovered := 0

	type node struct {
		channel string
		depth   int
	}
	seeds := map[string]struct{}{}
	addSeed := func(s string) {
		if slug := normalizeSlug(s); slug != "" {
			seeds[slug] = struct{}{}
		}
	}
	for _, s := range c.opts.Channels {
		addSeed(s) // CRAWL_CHANNELS env
	}
	for _, s := range loadChannels(c.opts.ChannelsPath) {
		addSeed(s) // channels.yaml, re-read each cycle
	}
	for _, slug := range st.seeds() {
		seeds[slug] = struct{}{} // remembered productive channels
	}
	if len(seeds) == 0 {
		c.logger.Warn().Str("channels_file", c.opts.ChannelsPath).
			Msg("no seed channels; add them to channels.yaml or CRAWL_CHANNELS")
		return live
	}
	queue := make([]node, 0, len(seeds))
	for slug := range seeds {
		queue = append(queue, node{slug, 0})
	}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.channel == "" || visited[n.channel] {
			continue
		}
		if n.depth > 0 {
			if discovered >= c.opts.MaxChannels {
				continue
			}
			discovered++
		}
		visited[n.channel] = true

		pages := c.scrapeChannel(ctx, n.channel, c.pagesFor(n.depth))
		if len(pages) == 0 {
			continue
		}

		cand := map[string]struct{}{}
		for _, p := range pages {
			for _, raw := range extractURLs(p) {
				if candidate(raw) {
					cand[raw] = struct{}{}
				}
			}
		}
		found := c.classifyAll(ctx, keys(cand))
		for u := range found {
			live[u] = true
		}
		if len(found) > 0 {
			st.record(n.channel, time.Now())
		}
		c.logger.Info().Str("channel", n.channel).Int("depth", n.depth).
			Int("subs", len(found)).Msg("scanned channel")

		// Thematic gate: expand into referenced channels only from seeds or from
		// channels that actually produced subscriptions.
		if n.depth >= c.opts.MaxDepth || (n.depth > 0 && len(found) == 0) {
			continue
		}
		for _, ch := range extractChannels(pages, n.channel) {
			if !visited[ch] {
				queue = append(queue, node{ch, n.depth + 1})
			}
		}
	}
	return live
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
		if err != nil || page == "" {
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
	if c.opts.Pages < 3 {
		return c.opts.Pages
	}
	return 3
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
