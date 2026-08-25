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
     cap (`subscription.MaxSubscriptionSize`), which rejects an oversized source with
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
  premise for a change here.** A ≤1 d chat node and a node a live crawler-managed subscription is
  serving right now are indistinguishable at that gate: 12/162 = 7.4% vs 17/200 = 8.5%,
  z=-0.38, p=0.70. What ages is the node, not the venue — which is why inline harvesting
  is restricted to each channel's NEWEST page instead of being switched off.
- **A forum/thread harvester is still out; bounded targeted in-forum traversal is in
  (decision reversed 2026-08-23).** All three seams work and were verified
  (`?embed=1&discussion=1&comments_limit=N` renders comments server-side, `&comment=<id>`
  is a deep cursor, `POST t.me/api/method` `loadComments` answers unauthenticated), and one
  topic holds 6036 distinct nodes over 46.5 d — more than 15 channels and 192 pages
  combined (5280). The struck measurement priced the UNBOUNDED shape, and that verdict stands:
  its day-zero INLINE nodes passed at 3 of 112 (2.7%), worse than channels, which is what
  exhaustive deep-history mining buys under the 10.34 d half-life above and the 7.4% same-day
  base rate — an unbounded pass costs an estimated ~1182 requests and returns mostly dead nodes
  (design-cost estimate, 2026-08-23; refused then and now). What the crawler does instead is the
  bounded opposite: it reads only each topic's NEWEST embed window (~96–200 replies; live probe,
  2026-08-23 — no static pagination exists, so newest-window-only is a property, not a choice),
  expands only along proven-productive edges (a discovered topic is itself expanded only if its
  own page yielded a live subscription — the gate above, not a new one), spends at most one GET
  per topic per cycle inside the existing caps (the shared `MaxChannels` pool of 200 counting
  topic visits with cross-channel ones, `discoveredPages`, `CRAWL_DEPTH`; `comments_limit` stays
  200; zero new knobs), and admits same-group hops only through a narrow carve-out from the
  slug-equals-self exclusion: the scanned node must have been read through its topic embed this
  cycle, the child ref must carry a different numeric topic, and the child faces the same
  productivity gate at its own dequeue.
- **The measurement that re-opened this question is pre-committed, and it can re-close it.**
  After at least 14 daily cycles, compare the DAY-ZERO pass rate of topic-origin vs
  channel-origin discovered subscription candidates over the SAME window at the SAME gate.
  Never set that figure against the 92.7% managed annuity baseline at the top of this section
  (steady-state survivors — a different population), and never conflate it with the 2.7%
  inline-node population without saying so. Primary kill switch, self-repealing: if the
  topic-origin day-zero rate does not beat the recorded 2.7%, revert the feature commit and
  restore the do-not-build bullet recording BOTH measurements. Secondary signals: discovered
  topics against the 200-slot budget headroom, the empty/pages ratio (embed markup health), and
  cycle wall-time delta against the pre-deploy median.
- **Do NOT move the harvest to the vassago instance.** Chat/forum nodes are 3.0x LESS
  whitelist-fit than what it already subscribes to — 21 of 675 resolved keys (3.1%)
  against 1838 of 20049 (9.2%) — and 0 of the novel ones were alive.
- **`InlineMax` still binds after that restriction, so raising it buys more of the same.**
  The first cycle on the new rule wrote `inline:500` again — the seed set's newest pages
  alone carry more than the cap, so 500 is a truncation of comparable candidates, not a
  yield. What the number cannot tell you is which 500: seeds are walked in Go map order
  (`scan`'s `for slug, s := range seeds`, `discover.go:126`), so the truncation point is
  arbitrary within one cycle's fresh pool.
  Raising the cap admits nodes from the same distribution — ~93% of which the earlier
  sources already carry — at one DNS resolve and one probe slot each.

## What a managed source entry means

A crawler-written source is named `<channel-slug>-<postid>` by `sourceName`
(`internal/crawl/crawl.go:699`): the slug is the Telegram channel folded into the config name
alphabet by `channelSlug` (lowercase, `_` to `-`, capped at 24 bytes), and `postid` is the
decimal id of the Telegram message the URL was harvested from. `seyedng-3631` reads "the
subscription the crawler found in @seyedng, post 3631".

The name is now a LABEL and nothing more. Everything a reader or the exporter needs to know
about an entry sits beside it as data: `managed: true` says the crawler owns it and
`feed: <slug>` says which channel it came from (`SubscriptionSource.Managed` and `.Feed`,
`internal/config/config.go:577,582`). The crawler writes `managed: true` on every entry it mints.
`feed:` it writes as the channel slug whenever the name is NEW and the origin named a channel; an
entry whose name it keeps verbatim keeps the `feed:` it already had, and a mint that saw no usable
slug records none (`mintSource`, `internal/crawl/crawl.go:541-548`). And `feed:` is not the
crawler's alone: unlike `managed` it grants nothing and only groups, so a curated entry may set it
too (`config.go:578-582`). An entry WITHOUT `managed` is hand-added and sheltered from rewrite
and prune, so an operator who forgets the field is safe by default. `managed: true` in a
git-tracked file is REFUSED twice,
and deliberately not by statement order: `mergeSourcesOverlay` (`config.go:1041`) refuses it in
`config/sources.yaml` before appending it (`:1053-1057`), so that overlay stays policed wherever
it is merged, and `validateSources` (`:1469`) refuses it again over the merged list
(`:1472-1473`), which is what covers `config.yaml`'s own entries. Both raise one wording,
`errManagedInCuratedFile` (`:1033`). Only the pass after the `private.yaml` merge allows the
mark (`:1013`), where it is the whole point. Three narrower name forms exist. `<slug>-<postid>-N`
carries a URL of one post that did not take the bare stem, N counting from 2 because that stem
was offered to the post's FIRST URL first. `<slug>-N` carries a known slug whose origin post is
not, N counting from 1 instead, that family having no bare stem to be offered at all. In both, N is
the lowest ordinal free in the cycle's taken-name set. And `<sha10>` — the first 10 hex of the
sha256 of the SUBSCRIPTION URL — carries a URL whose slug is unusable, and NOTHING else now that
an unbounded ordinal always finds a free name.
The inline harvest is the fixed name `inline` (`inlineSourceName`,
`internal/crawl/crawl.go:844`) — the one entry the crawler writes whose name it derived from
nothing, and therefore the one an operator can collide with by accident. It holds the aggregate of
inline node URIs harvested across messages and channels, with `managed: true`, a `body:` and no
`url:` (`buildInlineSource`, `crawl.go:851`). **Nothing reserves that name.** No validator refuses
it to anyone — `inline` is a legal curated name and one that loads (`config_test.go:901`) — and
before the cutover the `tg-` prefix on `tg-inline` announced the crawler's namespace where nothing
announces it now. So the crawler YIELDS the name instead of owning it. Hand-add an entry called
`inline` to `config/private.yaml` — with a `body:` or a `url:`, either way — and the merge shelters
it like any other unmarked entry (`crawl.go:461,473-477`), and rather than append a duplicate beside
it the crawler skips its own inline entry for that cycle, logging WARN `a hand-added entry holds the
inline aggregate's name; skipping the inline harvest rather than writing a duplicate` and leaving
`inline:0` on the `private.yaml updated` line whenever that cycle writes. Nothing else in the
cycle changes — sources still merge, prune and get written — but that cycle's inline harvest is
discarded, and every later cycle's too for as long as the name is held. The crawler does not rename
your entry to take its name back, because a hand-added entry is never renamed; only deleting the
entry by hand returns the name. The same name in a GIT-TRACKED file costs more and earns no
warning, because the yield cannot reach it: the crawler reads `private.yaml`, `channels.yaml`, its
own state file, and the NAMES on every `CRAWL_CURATED` path (`loadPrivate`, `channels.go:45`,
`state.go:290`, `curatedNames`) — and that last read feeds the MINT's taken-name set alone. `inline`
is not minted: it is a fixed constant, and the skip that protects it tests the `private.yaml`
entries the cycle is about to write and nothing else (the `slices.ContainsFunc` gate in `RunOnce`).
So an `inline` sitting in `config/sources.yaml`, or in `config.yaml` itself, is still invisible to
it — both files are read now, but only into the mint's taken-name set, which `inline` never enters.
It cannot see the collision,
appends `inline` to `private.yaml` as usual, and the MERGED list then carries the name twice — which
`validateSources("")` refuses (`config.go:1013,1478-1479`) and `config.Load` turns into
`private config: subscriptions.sources: duplicate name "inline"`, so the service does not start
until one of the two entries goes. Reserving the literal name in config validation was weighed and
refused: it would fail files that load today, and it would put authority back into a string, which
is the very thing `managed:` replaced.

That last hazard is the general one, and `inline` is only its most reachable case. Inside
`private.yaml` the MINTED forms are already safe: `mergeManaged` seeds `used` from every name the
file holds, hand-added ones included (`crawl.go:459-479`), and `sourceName` consults it before
returning a candidate (`crawl.go:708,715`), so an operator holding `seyedng-3631` makes the crawler
mint `seyedng-3631-2` instead of colliding — it yields there too. And `used` no longer stops at
`private.yaml`: the crawler now reads every `CRAWL_CURATED` path and seeds THEIR names FIRST
(`curatedNames`, from `Options.CuratedPaths` — a comma-separated list defaulting to
`/config/sources.yaml,/config/config.yaml`), which closes a hazard that was live until this change.
BOTH files are seeded, not just the overlay: `validateSources` rules on the whole MERGED list and
`config.yaml` carries `subscriptions.sources` of its own, so seeding one of the two would fail
closed on the entire config over a name the crawler had in front of it. Curated names do
wear the minted shape: the shipped overlay carries `wepogp-1` and `wepogp-4`
(`config/sources.yaml:91,93`) and vassago's `kort0881-vless-042` and `kreemchek-26`
(`config-vassago/sources.yaml:248,200`, read 2026-08-19). `wepogp-1` is precisely what the mint
produces for the first postless URL of a channel slugging to `wepogp`, so had it minted that
string the merged list would carry the name twice and `config.Load` would fail exactly as above —
at startup a crash loop, and at runtime the WORSE case, silent, the reloader keeping the previous
settings while every later crawler write is ignored. Names ONLY are read: no url, no body, nothing
`config.Load` owns, and neither file is ever crawled. The names union across the paths, a blank
entry (`CRAWL_CURATED=",/config/config.yaml"`) is skipped, and a missing file is normal and silent.
A read or parse failure warns naming that path (`read curated sources failed; minting without that
file's curated names`, or `unmarshal ...`) and the cycle goes on with the paths beside it — running
the collision risk for THAT file's names rather than stopping discovery, which is why the failure is
never silent. How many names were reserved rides the cycle's `private.yaml updated` line as
`curated`: 0 against an overlay holding dozens is the only way a mis-set path shows itself. So
naming a curated entry `<word>-<digits>` still puts it in the mint's own alphabet, but the files no
longer have to stay disjoint by luck.

Names minted before the cutover were ADOPTED, not re-derived: the migration stripped `tg-`,
set `managed: true` and backfilled `feed:` from the name it was stripping. The prefix alone does
not trigger it, and could not, because the prefix is not a mark even here: `channelSlug` folds
`_` to `-`, so the channel `tg_vpn` slugs to `tg-vpn` and every name minted from it — `tg-vpn-123`
— legitimately begins `tg-` while being nobody's migration. So `needsAdoption`
(`crawl.go:1107`) requires the rest of the name to wear a shape the pre-cutover mint actually
produced as well: `inline`, a `-` plus 6-hex tail `legacyFeed` can read a slug off (`crawl.go:1120`),
or a bare 10 hex (`unattributedNameRe`, `crawl.go:63`). An unmarked `tg-vpn-123` wears none of the
three, so it stays SHELTERED like any other unmarked entry — the merge writes it back verbatim
(`crawl.go:476`) — rather than being seized into the prune. The mark settles the crawler's own
side: a marked entry is never read again (`crawl.go:1108`), so a real `tg-vpn-123456` is safe by
its field. An UNMARKED name wearing one of those shapes is adopted, and nothing can tell it from a
pre-cutover mint: a 6-digit post id is also 6 hex digits, so a hand-added `tg-vpn-123456` is
exactly what an attributed mint looked like and is claimed as slug `vpn` plus hash, where
`tg-vpn-123` is too short to be. That is by construction and not a miss — before the cutover the
shape WAS the mark, and no entry records which side wrote it. So a dated reading
below naming `tg-dailyv2ry` names `dailyv2ry` now, and an adopted `<slug>-<sha6>` keeps that
hash tail forever — post-id attribution reaches only names minted after the cutover, since a
rename churns `private.yaml` and relabels every published node. The decimal ordinal that has
since replaced the hash in the mint changes nothing there either: it applies to FUTURE mints
only, so the corpus never converges on the new shape, and `legacyFeed` goes on stripping a
6-hex tail by design. That is most of the corpus and will stay so — measured on prod
2026-08-19, over the 504 entries the cycle that wrote at 14:21:01+03:00 left in
`config/private.yaml`, 454 carry a 6-character tail holding at least one `a`–`f` digit, so no
post id can account for those, and a further 18 wear a 6-character all-decimal tail that a
6-digit post id is indistinguishable from, exactly as
above. The backfill is sound where a
general rule is not, and that distinction is the whole design: an attributed pre-cutover name was
exactly `tg-` + slug + `-` + 6 hex with no post segment, so ONE known tail comes off
unambiguously, while the two unattributed shapes it also adopts — the bare hash and `inline` —
record no channel and are left naming themselves.

A LIVE ambiguity sits beside that dead one, in the mint as it stands: `<slug>-N` and
`<slug>-<postid>` share ONE string space, because a postless name counts from 1 over the same
small integers a post id occupies. The postless family is no edge case — every URL harvested off
a page that carries no post id lands in it — so `chan-2` is both the second postless URL of
`chan` and the first URL of its post 2. Nothing in the name tells the two apart — `feed:` still
names the channel truthfully, and only the order the cycle minted in decided which family took
the stem. The ordinal inherits the collision rather than escaping it: with `chan-1` and `chan-2`
already taken by two postless URLs, the first URL genuinely from post 2 mints `chan-2-2` and its
sibling `chan-2-3`, so the tail the grammar reads as a post's SECOND URL is that post's FIRST.
The count a `-N` carries is therefore trustworthy only against the other names sharing that stem
in that same cycle, never as a position in the post. The shape is the owner's explicit choice and
is not changing, which is why the ambiguity is written down here instead of designed away.

- **All three Prometheus labels are read, none is parsed.** Each of the four per-source
  counters (`stable_source_{nodes_total,valid_nodes,tested_nodes,published_nodes}`) carries
  `source`, `feed` and `owner`. `source` is the `name:` VERBATIM — `seyedng-3631`, slug and
  post id. `feed` is the `feed:` field, or the name when the field is absent — `sourceFeed` tests
  `Feed != ""` and nothing else (`internal/metrics/metrics.go:376-380`), so ownership does not
  enter it. The origin-less `<sha10>` form and any entry that set no `feed:` therefore name
  themselves, while a curated entry that DID set one keeps it (`config.go:578-582`, asserted by
  `config_test.go:1099-1109` and rendered as `owner="curated"` beside a divergent feed at
  `internal/metrics/testdata/exposition.golden:119`). Every URL of one channel shares
  one `feed` (`seyedng`) whatever tail its own name carries. `owner` is `crawler` when the
  entry carries `managed: true` and `curated` when it does not, which makes `owner="crawler"`
  exactly WRITE AUTHORITY over `config/private.yaml` — the same field the rewrite and prune
  paths read, and still not a claim about Telegram.
- **Attribution is a field because it cannot be recovered from a name, and nothing downstream of
  the write can reconstruct it.** A fold that strips one trailing hash leaves the collision
  form on its post rather than its channel — `seyedng-3631-1444c8` would answer `seyedng-3631`
  — so each post that yielded several URLs gets a bucket of its own. How often that happens is
  UNMEASURED, and 26 of 46 channels carrying more than one URL does not bound it: that reading
  counts URLs per CHANNEL, over a corpus whose names carried no post segment at all (see the
  collision-form bullet below). The argument needs no frequency — one collision-form name is one
  wrong row, and nothing in the fold tells it from a correct one. A greedy or repeated strip
  fixes that and breaks worse, on a channel this corpus already carries: `channelSlug` folds `_`
  to `-` and keeps digits, so the second-largest channel in the reading below has the slug
  `file-vpn-2`, and `file-vpn-2-1444c8` strips down to `file-vpn` while `file-vpn-2-3631` — a
  post id — must strip to `file-vpn-2`. The two are indistinguishable as strings. Nothing in a
  name says which trailing digit run was the slug's own. Curated
  names share the minted shape too: counted over both shipped overlays 2026-08-18, `wepogp-1`
  and `wepogp-4` (`config/sources.yaml:91,93`) would collapse onto one `wepogp` row that no
  channel produced, and six of vassago's 52 names do the same — `kort0881-vless-042` and
  `-041` onto `kort0881-vless`, `kreemchek-26` onto `kreemchek`. So the mint writes the slug
  down at the one moment it has it, and no reader guesses.
  `stable_source_dropped_nodes` is the one family carrying neither `feed` nor `owner` —
  `source` and `reason` only — so per-source drops stay a question you ask by name.
- **The URL is not the name, and the decisive reason is that the name is PUBLISHED.**
  `Merge` labels every node `<source>-NNN` (`internal/stable/merge.go:88-91`), and that
  label is what a client ends up displaying — though it is not the whole node name. Under
  the shipped config the `annotate` chain writes `[GEO:xx]` into that name and the stable
  worker prepends its own `[SPD:<n>M] ` ahead of THAT, in that order and not the reverse
  (`internal/rewrite/rewrite.go:15`), so the speed tag lands leftmost. All 172 lines
  fetched from `/stable.txt` on prod 2026-08-15 17:23Z are in that order; one of them ends
  `#[SPD:54M] [GEO:GB] hiddifycode-03a8d8-002` — tags first, source label last. A URL
  in that slot would hand every private or paid panel link to everyone who fetches the
  list. The second reason is merely mechanical and would not save it on its own: the
  config source-name alphabet is `^[a-z0-9-]+$` (`sourceNameRe`,
  `internal/config/config.go:115`, mirrored for the crawler's own write-back by its own
  `sourceNameRe` at `internal/crawl/crawl.go:1135`), and `:` `/` `.` `?` all fall outside it.
- **The collision form exists because neither a channel nor a post is a source.** One
  channel routinely publishes several distinct subscription URLs, so the slug alone is not
  unique — and a post is a container too, so the post id does not settle it: one message
  carrying two panel links is two sources. Only the URL is per-source, which is why a URL of one
  post that did not take the bare stem takes `<slug>-<postid>-2`, `-3`, and so on. **The ordinal
  is not per-URL unique the way the `sha6` it replaced was, and that loss is the thing to
  know.** The hash was a pure function of the URL; the ordinal is a function of the SET the
  name was minted beside — the curated names, the cycle-start `private.yaml`, and
  `mergeManaged`'s sorted-URL fill order — so a corpus rebuilt after `private.yaml` is lost
  re-derives names that SHIFT wherever the URL set changed, and every shift is a new
  Prometheus series on the `source` label. The opposite case needs no rebuild and no lost
  file at all: `used` is seeded from the curated paths and the cycle-start `private.yaml`
  and nothing else, so a name whose URL was pruned in an earlier cycle is absent from the
  next walk, and the next postless URL of that channel takes it — UNRELATED to the pruned
  one — so a live `source` series changes subject in place. Pre-cutover only the bare
  `<slug>-<postid>` arm could reuse a retired name, and only within one post: the sibling tail
  was a hash of the URL, so a retired `<slug>-<postid>-<sha6>` could come back to that URL and
  to no other. The ordinal opens reuse to the whole postless family at channel scope, and to
  `<slug>-<postid>-N` inside a post. Fill order already decided WHICH URL of a post took the
  bare `<slug>-<postid>`, so this widens an order dependence rather than adding a new class of
  one. The per-post multiplicity the form exists for is no longer unmeasured.
  Measured on prod 2026-08-19 over the 504 entries the 14:21:01+03:00 cycle left in
  `config/private.yaml`: 37 names are ones the 07:50 pre-adoption snapshot cannot account
  for, i.e. post-cutover mints — 29 bare `<slug>-<postid>`, 5 of the collision form, 2
  postless and `inline`. Three posts yielded more than one URL, and one yielded FOUR:
  `azadnet-23978` plus three siblings, which under the ordinal read `azadnet-23978-2`, `-3`
  and `-4`. Those 37 arrived in two cycles, by the `total` on their `private.yaml updated`
  lines: 467 → 501 at 13:23:09+03:00 and 501 → 504 at 14:21:01+03:00. What follows is
  HISTORICAL, kept because it is what the form was retained on while there was no count,
  counted over `config/private.yaml` as the crawler left it at
  17:20Z on 2026-08-15, before the cutover, 357 `name:` entries, of which 354 carried the
  attributed `tg-<slug>-<sha6>` shape and 3 did not: `commsub` (this file's only hand-named
  entry), one legacy `tg-<10 hex>` (`tg-96c4d7c7a7`) and `tg-inline`. Only those 354 had a
  channel to map, and they landed on 46 distinct channels, 26 of which carried more than one
  URL. An hour earlier the same four counts read 350 / 347 / 45 / 25. That 26-of-46
  measures that a CHANNEL is not a source, which is why the name carries a post id at all;
  it was never a reading of the collision form and could not have been one, every one of
  those 354 names carrying `tg-<slug>-<sha6>` with no post segment.
- **Read every count in this section as a dated reading, never a constant.** The crawler
  adds sources every hour: `stable_sources_total` went 230 → 396 over the fourteen days
  to 2026-08-15 17:23Z, so any total here is stale within hours. That metric is the live
  figure, drawn as panel 3 "Sources OK / total" at the top of the Grafana dashboard;
  prefer it to any number below. It counts BOTH overlays — `config/sources.yaml`'s 46
  curated names as well as the crawler's private ones — and it lags the file on disk by up
  to one cycle, because a reload only reaches the worker when its next cycle starts. At
  17:23Z it read 396 = the 46 curated names plus the 350 private ones the 17:09Z cycle
  loaded (`stable_last_success_timestamp_seconds`), while the file on disk already held
  357 — the lag, in one arithmetic.
- **Fan-out is real, but it counts LINKS, not panels.** The largest channels on prod
  2026-08-15 17:20Z were 71 (`dailyv2ry`), 42 (`file-vpn-2`), 29 (`proxytglte`),
  27 (`v2raytunsub`), 24 (`hiddifycode`) and 23 (`holost-vpn`) — 216 of the 354
  attributed names in six channels. How many PANELS stand behind those names is decided by
  what the URLs return, not by how they look, and only a fetch settles it. All 165 URLs of
  `dailyv2ry`, `file-vpn-2`, `proxytglte` and `holost-vpn`, fetched at 17:22Z
  on 2026-08-15 with the service's own User-Agent (`mihomo-geofeed-preprocessor/0.1`,
  `internal/fetch/fetch.go:20`), answered 200, and the URL misleads in both directions.
  Eight of `holost-vpn`'s 23 sit at eight different repository paths under one GitHub
  account (`Ai123999`) and return one byte-identical 33101-byte body, all with md5
  `2193975ecadf359d136da4658ca7a1e6` and 121 server endpoints each — so eight names are
  one payload; two of `proxytglte`'s 29 do the same at 3473 endpoints, differing only
  in how the URL spells the git ref. Yet `file-vpn-2`'s 42, one numbered catalogue on
  one host (`cyb-portal.com/CP-0NN`), return 42 distinct bodies at a mean pairwise Jaccard
  of 0.027, and `dailyv2ry`'s 71 one-shot pastes at `bin.mudfish.net/r/<id>` return 71
  distinct bodies at 0.054, 4322 distinct endpoints out of 8789 — separate drops, not
  re-snapshots of one feed. Host counts do not rescue the guess either: `proxytglte`'s
  29 URLs are 8 hosts and 29 distinct paths, 20 of them on `raw.githubusercontent.com`.
  What a URL does settle is sameness of service: 49 names spread over `hiddifycode`,
  `v2raytunsub` and `o00000000i` are ONE host on ONE path, separated only by a
  `?payload=` credential on `is.wepogp.gay/bypass-hwid-lock-<id>` — 49 per-user accounts
  on a single panel. So the name promises exactly one thing and no more. Anything that
  ranks or budgets per NAME ranks per LINK — never per channel, and never per panel, where
  a single operator can hold 49 of the rows. Those six are `feed` values, taken from the
  file before the cutover stripped the prefix off every one of them.
- **The name → URL mapping exists in exactly one place: the `name:`/`url:` pair in
  `config/private.yaml`.** One line recovers the link behind a row in Grafana or a node in
  the published list: `grep -A3 'name: seyedng-3631' config/private.yaml` — three lines of
  context because the entry now carries `managed:` and `feed:` beside its `url:`, and those
  two are the answer to "may I delete this" and "which channel is it". Repair the
  token first, because every wrong form misses SILENTLY instead of failing. A dashboard `source`
  column is already the verbatim name and needs no repair, and panel 20's `feed` column greps
  too now that it is a field: `grep -c 'feed: seyedng$' config/private.yaml` counts that
  channel's entries, which is the row the panel folded. Off
  the published list, drop the
  trailing numeric index `Merge` appends (`internal/stable/merge.go:88-91`; `appendPad3` pads to a
  minimum of three digits, not to a width, so the index can be longer). Then grep both
  overlays rather than reasoning about which file owns the name: `private.yaml` holds the
  managed ones AND any hand-added entry such as `commsub`, `config/sources.yaml` the curated
  rest. Names are unique across both (`validateSources`, `internal/config/config.go:1478-1479`).
- **`<10 hex>` is the other managed shape and carries no channel at all.** It is not
  merely historical: `unattributedNameRe` matches it and the fallback mint still EMITS it — but only
  ever for a URL the crawler has never named, since an entry that already carries a name gets it
  back first (`crawl.go:700-701`). For such a URL the fallback now fires for exactly ONE reason:
  the slug was unusable, leaving no stem to build on (`crawl.go:704`). There is no probabilistic
  arm left. The ordinal is unbounded and every value it rejects is one distinct name already in
  `used`, so a usable slug always reaches a free candidate inside `len(used)+1` tries and can no
  longer fall through (`crawl.go:714-718`) — the `sha6` collision at ~2^-24 and the operator
  holding both attributed candidates by hand, which this bullet offered until 2026-08-19, reach
  the bare hash no more.
  `sourceName` upgrades a hash-only name to the attributed form the first time the URL turns
  up in a channel, and that is the only rewrite it ever performs: an already-attributed
  name is kept VERBATIM forever, because a rename churns `private.yaml` and relabels every
  published node. That is also why the migration is not an exception to it: adoption rewrote
  the prefix away once, under its own rule, and left every tail alone. It does NOT restart
  the stable worker: `Controller.Apply` reconfigures a
  live checker instead of cancelling it, and only shutdown calls `Stop`
  (`docs/guides/design.md:102`). Prod carried one hash-only name on 2026-08-15, then
  `tg-96c4d7c7a7` and `96c4d7c7a7` after adoption. The crawler's own inline harvest is a
  third shape, the fixed `inline`, which carries a `body:` and not a `url:`, so there is
  nothing behind it to look up.

## Retiring a source

Three reasons to retire a managed entry exist, and none of them is served by a
hand-maintained deny list any more: `config/channels.yaml` carries only
`channels:` since 2026-08-25, when its `blocked:` field and everything listed
under it were deleted.

- **A curated duplicate is withheld by CONFIG, for as long as it is listed, and
  no state records it.** A URL that appears VERBATIM as a
  `subscriptions.sources[].url` in any `CRAWL_CURATED` file (`curatedURLs`) is
  refused by `mergeManaged`: the crawler never manages it as a mirror — the
  curated entry itself is what the worker fetches — and the refusal re-applies
  on every rediscovery, so deleting the managed copy by hand is unnecessary.
  The match is verbatim only: a different query spelling or a dropped fragment
  is a different string and a different source. Because the mirror copies sit in
  `config/private.yaml` until something removes them, the FIRST cycle after a
  curated set broadens over URLs the crawler already mirrors drops every one of
  those managed entries, and it does so in ONE write: a withholding is
  deliberately NOT counted as a deletion (`mergeManaged`), so the bulk-prune
  floor never rules on it. That floor refuses a write only when it drops BOTH
  more than `bulkPruneMinDrop` (10) entries AND more than 30% of the corpus, and
  it exists to catch the crawler deleting sources by mistake, not to throttle
  deliberate curation — broaden the curated list in stages if you want the
  removal staged.
- **A dead panel retires itself into crawler state.** A DEFINITIVE not-live
  verdict — HTTP 404/410/451 (`StatusError.Gone()`) or an origin-advertised
  expiry — records the URL in the `Dead` map persisted in
  `.crawler-state.json` (`state.Dead`, written by `recordDead`, capped at
  `maxDead` records evicting the stamps closest to expiry). A remembered URL is
  not classified again when a channel re-advertises it: the discovery pass skips
  it without spending a request, counted as `dead` in the per-cycle reject
  summary and deliberately given no log line of its own. An entry still in the
  managed corpus IS still rechecked — `recheckManaged` passes no skip set — and
  that is the one route by which a URL that came back clears its record
  (`clearDead`) before the TTL runs out. Every renewed definitive verdict
  re-stamps it, `pruneDead` drops stamps past `CRAWL_DEAD_TTL`
  (`Options.DeadTTL`, whose ZERO VALUE disables the memory outright; the 720h
  default is `main`'s, from the env var), and after expiry the URL is classified
  afresh on its next rediscovery — revival needs no edit. A cycle that saw no
  live answer anywhere records nothing at all: without one, nothing proved the
  egress works. Transient statuses (403/408/425/429/5xx, transport errors)
  and nodeless-2xx placeholder bodies never create a record: a WAF answering for
  a live payload and a panel whose death reads nodeless-2xx stay in the ordinary
  classify/recheck flow.
- **A live-but-unwanted source has NO hold mechanism.** Owner decision,
  2026-08-25: sources rejected at curation — the aggregators measured to add
  nothing (`github.com/zieng2/wl`, all 15 files of `igareck/vpn-configs-for-russia`)
  and the wepogp panel endpoints serving real payloads — lost their old
  `blocked:` hold, and they MAY re-enter as managed mirrors whenever their
  channel reposts the link. That list stood between those URLs and the corpus
  against a harvest that rediscovers hourly, and keeping it current cost a human
  decision per URL forever. Their yield is absorbed downstream by the same gates
  every candidate passes — geo, ASN, probe — the three-gates doctrine above
  applied to retirement instead of admission.

Checking any retired URL by hand MUST use the service User-Agent:
`is.wepogp.gay` serves the real payload to `mihomo-geofeed-preprocessor/0.1` and
a 116-byte stub to a browser UA, so a browser check condemns live sources.
