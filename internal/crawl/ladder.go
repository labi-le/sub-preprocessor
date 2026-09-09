package crawl

import (
	"context"
	"strconv"
	"time"
)

// A forum group has no t.me/s/ listing, so a bare ref to one is a dead end: the
// listing answers 200 with no message wrap and there is no topic id to read
// instead. Telegram publishes no list of a group's topics either, so the id has
// to come from somewhere — a repost that carried one, the same-group carve-out
// off a topic already being read, or an operator naming it in channels.yaml.
// The ladder is the fourth source: it probes the low id window once per group
// and remembers that it did.
//
// The window is 40 because a forum topic id is the message id assigned when the
// topic was created, and a forum's topics are overwhelmingly created before its
// traffic accumulates. Measured 2026-09-09 over ids 1..40 of the three
// most-referenced topicless groups in the corpus: aboutnpvforum 10 alive of 40,
// 6 carrying candidates (best /12, 42 URLs); samnetgroup 8 alive, 7 carrying
// (best /4, 136 URLs and 23 inline nodes); mrosko 7 alive, 3 carrying (best
// /13, 61 URLs). Ids far above the window exist — wildVF/16770 and /33450 are
// in channels.yaml — and are deliberately out of reach: reaching them means
// walking message ids, which the topicQuery comment prices at ~1182 requests
// for one pass and refuses.
const ladderIDs = 40

// ladderGroupsPerCycle bounds the ladder's share of a cycle at 5 groups, i.e.
// 200 extra requests. The population it sweeps is large but almost entirely
// recurring: 636 distinct topicless slugs over ~40h of logs against 110-143
// dead ends per cycle, so the same groups return every cycle and one sweep each
// is all that is ever needed. At this budget the standing population is covered
// in ~5 days, after which the ladder runs only for genuinely new groups.
// Raising it buys days off that sweep and spends them against t.me's rate
// limiter, which the crawler refuses to provoke elsewhere either.
const ladderGroupsPerCycle = 5

// ladder is the per-cycle group budget, carried down to the dead-end site.
type ladder struct {
	groups int
}

func newLadder() ladder { return ladder{groups: ladderGroupsPerCycle} }

// climbTopics probes a topicless group's low id window and records every topic
// carrying a subscription candidate as a productive ref, which is the shape
// buildSeeds seeds from: the hit becomes a depth-0 seed next cycle, and the
// same-group carve-out then reaches its siblings without another climb. The
// topic itself is not harvested here beyond the probe — it is read properly on
// that next cycle, under the normal budget.
//
// One climb per group: the verdict is remembered whether or not it found
// anything, because a window that holds no topic will not grow one. The memory
// expires on the same TTL that forgets a channel (prune), so a long-lived group
// is eventually re-swept rather than written off forever.
func (c *Crawler) climbTopics(ctx context.Context, slug string, st *state, lad *ladder, rej *rejects) {
	if _, done := st.Ladders[slug]; done {
		return
	}
	if lad.groups <= 0 {
		c.logger.Debug().Str("channel", slug).
			Msg("topic ladder budget spent this cycle; the group keeps its dead end for now")

		return
	}
	lad.groups--
	if st.Ladders == nil {
		st.Ladders = map[string]time.Time{}
	}
	st.Ladders[slug] = time.Now()

	alive, hits := 0, 0
	var junk []string
	for id := 1; id <= ladderIDs; id++ {
		if ctx.Err() != nil {
			return
		}
		pages := c.scrapeTopic(ctx, chanRef{slug: slug, topic: strconv.Itoa(id)})
		if len(pages) == 0 {
			continue
		}
		alive++
		if len(c.harvestPages(pages, &junk, rej, slug)) == 0 {
			continue
		}
		hits++
		st.record(slug+"/"+strconv.Itoa(id), time.Now())
	}
	c.ladderReport(slug, alive, hits)
}

// ladderReport keeps the two outcomes distinguishable the way the metric
// families do elsewhere: a swept group that found nothing is a fact about that
// group, while a sweep that never ran is a fact about the budget.
func (c *Crawler) ladderReport(slug string, alive, hits int) {
	if hits == 0 {
		c.logger.Warn().Str("channel", slug).Int("probed", ladderIDs).Int("alive", alive).
			Msg("topic ladder swept a group with no listing and found no subscription")

		return
	}
	c.logger.Info().Str("channel", slug).Int("probed", ladderIDs).Int("alive", alive).
		Int("seeded", hits).Msg("topic ladder swept a group with no listing")
}
