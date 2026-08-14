# Curating sources, and what the crawler may harvest

> **When to read this:** Read before adding, removing or auditing a `subscriptions.sources` entry, or before touching `internal/crawl`. The three gates a candidate must pass live here.

## Curating a subscription source list

`config-vassago/sources.yaml` is curated entirely by hand — no crawler writes into that
directory — and one measured round of it (2026-08-10) produced four findings that cost hours
to obtain. That file's header carries the round's numbers; what follows is the part that
outlives them, and it applies to `config/sources.yaml` just as well.

- **The biggest names marketed FOR whitelist bypass contribute nothing; the bulk comes from
  undifferentiated aggregators.** Measured across ten independent search channels:
  `github.com/zieng2/wl` and all 15 files of `igareck/vpn-configs-for-russia` (7992 stars)
  scored ZERO new whitelisted `server:port` against the configured pool. That is a statement
  about the famous ones, NOT about the class. Counting by the boundary that needs no
  judgement — whitelist marketing in the FILENAME — six of the 33 added qualify and carry
  377 of the 2247 marginal gains, 17%. State which boundary you used or the next reader
  derives a different answer: the configured `name:` happens to select the same six here,
  while whitelist marketing ANYWHERE in the URL selects eight and 519, 23%. The bulk
  came from aggregators making no such claim under every one of those readings, and the
  mechanism was measured on `zieng2/vless_universal.txt`, 325 nodes: 88 hold an IP genuinely
  inside the whitelist, 29 present a whitelisted SNI from a non-whitelisted IP, and 208 are
  ordinary foreign nodes. Those 29 are SNI spoofing — a different bypass from CIDR
  membership, and one this pipeline deliberately does not implement. Marketing predicts
  nothing and a star count predicts nothing; measuring against the CURRENT pool is the only
  signal.
- **A count is not a coverage, and a sum is not a union.** Ten agents each reported a sum of
  per-source `wl_new` that overstated their slice's true union by 2.2x to 3.3x — these
  sources overlap heavily. Worse, a marginal gain measured greedily against the sources
  accepted BEFORE it is a lower bound that cannot be added, subtracted or reordered: this
  round's 33 additions have gains summing to 2247, the 36 they were picked from showed a
  union of +2368, and the shipped list re-measures at 4006 against the 1709 baseline.
  Three separate measurements over three different sets; none is derivable from the
  others, and the last supersedes the rest. Union the sets. Never total the counts, and
  never derive a new figure by taking one source's number out of an old one.
  **And re-check every COMPARISON when you re-measure its operands.** A "staler than", a
  "more than half", a "2.3x" is a derived quantity that keeps its old truth value while the
  numbers around it move, so it survives exactly the sweep that fixes everything else. Two
  shipped here — three sources called "all staler than the 94-day reject" when two were
  fresher than it, and two survivors called "both worse than" a bar one of them sat inside —
  with every individual figure correct in both.
- **Yield alone is not selection. Three gates are, and a candidate must pass all of them.**
  1. FRESH. A frozen fork scores well ONCE, as an archive of servers the live upstream has
     since rotated out, then decays while still costing a DNS resolve every cycle. The
     signal used was the repository's commits atom feed — and that feed is this gate's hole:
     two `storage.yandexcloud.net` candidates publish none, sailed through untouched at 94
     and 122 days stale, and were caught by hand on `Last-Modified`. A candidate with no feed
     needs that manual check or the gate is not applied to it at all — and the hole is not
     caught at candidate time. It survives into the SHIPPED list: three no-feed sources were
     accepted and shipped at 52-104 days before the by-hand check reached them, so audit the
     entries you already have, not only the ones you are adding.
  2. FETCHABLE inside `fetch.timeout`, shipped at 3s, AND under the worker's 10 MiB body
     cap (`subscription.maxSubscriptionSize`), which rejects an oversized source with
     `response too large` so it loads NOTHING. Both halves bit this round: the
     best-yielding candidate found — 409969 nodes — takes 9s, and
     `gitverse.ru/LimonTH/proxy-list` `output/live` (forge-native; 404 on GitHub)
     downloads in 1.4s for +445 but is 10.4 MiB. Neither contributes a
     single node however good its content is, and the second was shipped for one cycle
     before the log said so. Measure best-of-3: the failure mode here is bimodal, not
     slow. A selection script that times the fetch but never weighs the body credits
     candidates production cannot use.
  3. MARGINAL, and worth its DNS. It must contribute whitelisted `server:port` that no
     ALREADY-ACCEPTED source carries — a source that only re-carries an accepted source's
     nodes is rejected however large it is, and that test alone condemned all sixteen
     removals — and the whole ADDED BLOCK must fit a per-cycle budget of resolved hosts.
     The cost unit of that budget is DISTINCT HOSTS per cycle, never node lines: past the
     `cidr` filter a non-whitelisted node is one cached lookup and dies pre-probe, so a large
     source's tail is not free and the budget a candidate is charged against is a host budget.
- **One sample is not evidence for removal.** These lists rotate within the hour. Six audit
  samples over 1.5h backed dropping 16 redundant sources, and four more (`aetris bijandi
  flat447 prominbro`) were deliberately KEPT because they were redundant in some samples and
  unique in others. A single sample showing a source adds nothing shows only that.

## What the Telegram crawler may harvest

Measured 2026-08-12. Pass rates below are at this instance's own gate
(`config/config.yaml` `subscriptions.check`: `rounds: 2`, `timeout: 1000ms`, `max_avg_ms: 800`);
the decay fit is on reachability at the looser 8000 ms gate, which is the arm large enough to fit.

- **A harvested `server:port` is a frozen snapshot; a harvested subscription URL is an
  annuity — never trade the second for the first.** Inline nodes decay with a fitted
  half-life of 10.34 d (95% CI [5.47, 16.58], n=1089; a floor parameter is rejected, so
  there is no immortal subpopulation to mine) and they start nearly dead: 12 of 162
  same-day nodes pass (7.4%). The crawler's managed URLs are 216 of 233 live (92.7%),
  none definitively dead, median in-service age 28.1 d over the 165 that carry a date.
- **The difference is the ASSET, not the quality — so "chat nodes are junk" is the wrong
  premise for a change here.** A ≤1 d chat node and a node a live `tg-*` subscription is
  serving right now are indistinguishable at that gate: 12/162 = 7.4% vs 17/200 = 8.5%,
  z=-0.38, p=0.70. What ages is the node, not the venue — which is why inline harvesting
  is restricted to each channel's NEWEST page instead of being switched off.
- **Do NOT build a forum/thread harvester.** All three seams work and were verified
  (`?embed=1&discussion=1&comments_limit=N` renders comments server-side, `&comment=<id>`
  is a deep cursor, `POST t.me/api/method` `loadComments` answers unauthenticated), and one
  topic holds 6036 distinct nodes over 46.5 d — more than 15 channels and 192 pages
  combined (5280). Struck anyway: its day-zero nodes pass at 3 of 112 (2.7%), WORSE than
  channels, so depth buys volume of DEAD nodes (~3 prod-gate nodes/day in steady state). A
  new endpoint re-opens nothing here; only a measurement beating 2.7% would.
- **Do NOT move the harvest to the vassago instance.** Chat/forum nodes are 3.0x LESS
  whitelist-fit than what it already subscribes to — 21 of 675 resolved keys (3.1%)
  against 1838 of 20049 (9.2%) — and 0 of the novel ones were alive.
- **`InlineMax` still binds after that restriction, so raising it buys more of the same.**
  The first cycle on the new rule wrote `inline:500` again — the seed set's newest pages
  alone carry more than the cap, so 500 is a truncation of comparable candidates, not a
  yield. What the number cannot tell you is which 500: seeds are walked in Go map order
  (`discover.go:78`), so the truncation point is arbitrary within one cycle's fresh pool.
  Raising the cap admits nodes from the same distribution — ~93% of which the earlier
  sources already carry — at one DNS resolve and one probe slot each.
