# GitHub source discovery

> **Load this guide when** adding, retuning or auditing the GitHub side of the crawler
> (`internal/ghfind`, `internal/crawl/github.go`, the `GITHUB_*` env keys), or when asking
> why a repository was accepted, skipped or dropped.

The Telegram side of the crawler reads chats; this side reads GitHub. Both feed the same
mint in `internal/crawl` and the same `private.yaml`, so there is one corpus owner, one
retirement rule and one state file. What differs is how a candidate URL is found — and
that a GitHub source is minted on novelty alone, which is why this phase alone withdraws
a source the service's probe never validates (probation, below) instead of waiting for it
to go not-live. The production measurement, the gate it refuted and the probation that
replaced the gate come next, because they are why the phase is built the way it is.

## Why GitHub at all — the measurement that bought this code

Taken 2026-09-07 against the shipped corpus of that day (1058 configured source URLs,
73 774 distinct `server:port`):

| step | cost | result |
|---|---|---|
| code search, 6 queries x 2 pages | 12 calls | 956 file hits, 477 distinct repositories |
| repo search, 8 queries | 8 calls | 226 repositories, only 16 already seen by code search |
| `git/trees?recursive=1` for all 687 | 687 core calls | 975 069 tree entries, 5 truncated |
| candidate filter | free | 110 041 candidate files in 578 repositories |
| fetch top 6 per repository | 2655 GETs, 40 s | 1351 files carrying nodes in 315 repositories |
| endpoint census | free | 208 304 distinct endpoints, **155 394 (75%) absent from the corpus** |
| greedy cover over repositories | free | 119 repositories account for 154 980 of those |

For comparison, a good Telegram forum topic measured on 2026-09-05 carried 82 novel
endpoints, and the richest single one 118. GitHub is two orders of magnitude wider, which is
why the gate here is not "is it live" but "does it add anything", and why the number of
sources it is allowed to mint is capped.

Volume alone would prove nothing — a list of dead endpoints is easy to find. So the same
sample was probed: 500 random novel GitHub endpoints and 500 random endpoints the corpus
already carries, one TCP connect each, 3 s timeout. **44% of the GitHub endpoints accepted a
connection against 40% of the corpus's own.** The nodes this phase brings in were no deader
at the TCP layer than what the service already serves — which is what made the novelty
count worth taking to production. It did not make it a survival guarantee: production's
probe answered differently, the section below records that reading and the dial gate it
refuted, and the crawler's job still ends at "these endpoints are new and the file that
serves them is live" — whether the ENDPOINTS then survive the probe is the service's own
measure, which the crawler now reads back through probation.

## What production said once the phase shipped

The measurement above prices what GitHub can OFFER; production answered the question that
pays — what the service PUBLISHES — and the two diverged. Three cycles on prod,
2026-09-08: 34 then 16 files accepted, 63 903 then 5814 novel endpoints, zero API errors,
12 rate-limit sleeps (pacing, not breaches), 53 GitHub sources minted. Those sources make
104 135 endpoints reachable, **62 821 of them reachable through no other source**, and
they lifted the merged pool from 66 409 to 121 591 nodes. Of the 77 nodes the service
publishes, 13 are served by a GitHub source and **exactly one is served only by GitHub**;
per-source attribution (`stable_source_tested_nodes`) credits GitHub with 0 survivors in
the last cycle and 1 in the one before. Rate per endpoint: GitHub-exclusive endpoints
produced 1 published node out of 62 821; everything else produced 76 out of 73 197.
**GitHub-exclusive endpoints are ~65x less likely to reach the published set.**

That is the reading the 155 394-novel-endpoint figure must carry: endpoint novelty is what
the crawler can measure BEFORE minting, and whether those endpoints ever pass the probe is
a different question, measured AFTER, by the service. The 44% TCP acceptance above is not
contradicted — it just does not reach the published set. Novelty is still why the phase
exists (GitHub served 13 of the 77 published nodes and is the only source of one of them);
what production refuted is that any mint-time signal separates the endpoints that will
survive from the 62 821 that will not. The first candidate separator was measured and
struck before it was built, and the record of that strike is next.

## The dial gate the measurement refuted

A TCP-reachability gate at mint time was proposed: dial a sample of a candidate file's
endpoints and refuse a file whose accept rate is too low. It was measured BEFORE being
built, as this repo's workflow demands: per-source TCP accept rate over 120 sampled
endpoints, 53 GitHub sources against 45 others — **GitHub median 0.42, others median
0.37**. TCP reachability does not separate the two populations, so the gate would have
rejected nothing while costing thousands of dials a cycle. Struck, never implemented.

What does separate them is only visible after the probe: a working proxy handshake with
the advertised credentials, inside the latency and speed envelope. The crawler cannot
cheaply predict that at mint time; the service already measures it per source, every
cycle, and reports it. That report is what probation reads.

## Probation — withdrawal on the service's verdict

Intake keeps its gates and gains no predictor: nothing measured at mint time predicts
survival before the probe, so the phase bounds the DAMAGE instead. A GitHub-minted source
is minted on novelty and withdrawn again when the service's probe never validates it.

- **The reading.** A minted source enters probation at mint; once per cycle the crawler
  fetches the service's own per-source outcome — `stable_source_tested_nodes` off its
  metrics endpoint, `GITHUB_OUTCOMES`
  (default `http://sub-preprocessor:9090/metrics`, the service's own listener, reachable
  at that name on the compose network) — and folds it into the source's probation record
  on the state file, keyed by source NAME (`probation` in `.crawler-state.json`).
- **Service cycles, not crawler cycles.** A fold counts only when the service's
  `stable_cycles_total` has moved since the last reading: six reads of one snapshot are
  one observation, and a source the service never probed accumulates nothing. Probation
  mirrors the corpus's 6-cycle not-live retirement rule, on the service's clock.
- **The loop fails safe.** An unreachable metrics endpoint, a parse failure, an empty
  reading or a reading whose cycle has not advanced withdraws NOTHING and folds NOTHING.
  A source is never condemned on missing evidence — only on positive evidence of a
  survivor-free service cycle.
- **Withdrawal.** After `GITHUB_PROBATION` (default 6) consecutive survivor-free service
  cycles, the source's URL joins the deny set `RunOnce` already builds from the curated
  URLs, so `mintRetained` drops it from `private.yaml` and logs it, and the same URL is
  dead-stamped with `recordDead` for `CRAWL_DEAD_TTL` (default 720h), so the next cycle
  does not re-mint it. The dead stamp expiring is when the URL may be tried again.
- Each withdrawal increments the crawler's own `stable_crawl_github_withdrawn_total`
  (operator reading and its panel: `docs/guides/monitoring.md`).

Because probation bounds how long a bad batch stays, intake volume is bounded too:
`GITHUB_MAX_SOURCES` dropped 150 -> 60. The cap is the standing cost the corpus pays
while a batch serves its probation — the two knobs are one decision, and the table below
states both.

## The signals, measured rather than assumed

Live rate of a sampled candidate file, same measurement (n = 2655):

- extension: `.txt` 78% of 1588, no extension 38% of 309, `.yml` 2% of 114, `.yaml` 0% of
  124, `.json` 0% of 433, `.conf` 0% of 63, `.list` 0% of 24. Clash YAML is not a
  subscription this pipeline can read (`subscription.Normalize` handles URI lists, base64
  and xray JSON only), so `.yaml`/`.yml` are excluded outright. `.json` stays admissible at
  a low score because the Python probe that produced this table could not see xray JSON
  while `classify.Body` can.
- path/name token score (see `internal/ghfind/filter.go`): score >= 7 is 85-90% live,
  4-6 is 37-69%, <= 3 is at most 13%.
- blob size: over 200 KB 77% live, 20-200 KB 68%, 2-20 KB 31%, 200 B - 2 KB 9%. Hence the
  2 KB floor, waived only for a high-scoring name.
- where the repository came from: repo search with a `pushed:` filter 70% live, code search
  42%. Code search still earns its calls — it found 477 repositories to repo search's 226,
  and the two overlap by 16.
- inside one repository, 1-3 files carry 97% of its endpoint union. Repositories that
  publish 30 per-country splits are the norm, not the exception, so per-repository
  selection is a greedy set cover with a minimum gain, capped at 4 files.

## What one real cycle costs — the two runs that priced it

2026-09-07 22:17-22:22, real token, real GitHub, defaults everywhere, against the 1096-URL
corpus of that moment. Whole cycle 4 min 38 s, of which:

- census: 1096 URLs fetched, 74 344 distinct endpoints, 98 s, file 594 768 B (`16 + 74344*8`).
- searches: 14 (8 code + 6 repo), 12 rate-limit sleeps — the 6 s code-search pacing, not a
  limit breach.
- repositories: 120 admitted and treed. Candidates probed: 555, of which 386 carried nodes
  (70% — the filter's measured precision, against 51% for the same filter judged by a
  Python probe blind to xray JSON).
- accepted: 31 files from 17 repositories, 54 727 novel endpoints. **Acceptance, not
  liveness, is what the other 355 live files failed**, in two places: the per-repository
  cover keeps at most `GITHUB_ACCEPT_PER_REPO` union-maximal picks and drops the rest before
  any global measurement, and the survivors then have to clear `GITHUB_MIN_NOVEL` against the
  census. The logged `offered` count is the post-cover number, so the split between those two
  is not in the log line.
- an independent audit 20 minutes later — separate fetch of all 1096 corpus URLs and all 31
  minted URLs, endpoints diffed outside this codebase — put the novelty at 54 181, i.e. the
  run's own claim reproduces within 1%.

So a census-rebuild cycle costs about 1900 HTTP requests and a steady-state one about 800:
254 to the API (14 searches + 120 repository-metadata calls, which code-search hits need
because search results carry no `pushed_at` + 120 trees), 555 raw candidate fetches, plus the
1096 census fetches on the cycles that rebuild it — and, on either kind of cycle since
probation shipped, one further GET for the reading of the service's metrics endpoint
(probation below). Those are the numbers to watch when
retuning: raising `GITHUB_MAX_REPOS` buys candidates linearly in trees AND raw fetches,
lowering `GITHUB_CENSUS_TTL` buys baseline accuracy at 1096 requests a rebuild, and a
repository remembered productive is revisited every cycle outside the repository budget, so a
good harvest raises the steady-state cost for the next 30 days.

The next cycle, 35 minutes later, is what steady state looks like: census reused from the
file, 3 min 45 s, the same 14 searches from the advanced cursor, 137 repositories (120 new
plus the 17 remembered productive, which do not consume the repository budget), 608 probed,
**554 live (91%, against the first run's 70% — a remembered repository is a repository that
already published)**, and 19 accepted for 4637 novel endpoints. The novelty collapsed by an
order of magnitude because the first run took the big fish; that decay is the marginal gate
working, not the phase running out of GitHub.

## The algorithm

One pass per crawl cycle, all of it budget-capped and resumable through a cursor in
`.crawler-state.json`:

1. **Census.** Novelty needs a baseline, and the baseline doctrine (`sources.md`) is the
   WHOLE shipped corpus, not the curated part. Every configured source URL is fetched once
   and its endpoints folded into a 64-bit hash set, persisted beside the state file and
   rebuilt when older than `GITHUB_CENSUS_TTL`. A census that fails to build is not an
   excuse to mint blind: with no census the phase refuses to accept anything new.
2. **Search, rotating.** A built-in grid of code-search and repo-search queries is walked a
   few queries per cycle from the persisted cursor. Rotation is what beats the 1000-result
   cap per query and keeps each cycle inside the 10 req/min code-search limit.
3. **Repository admission.** Archived repositories are refused; a repository must have been
   pushed within `GITHUB_FRESH`; forks need a push within a quarter of that window.
   Repositories already remembered productive are always revisited, and they do not consume
   the new-repository budget.
4. **Tree.** One `git/trees?recursive=1` call per repository yields every path with its blob
   size, so candidate selection costs nothing further. A truncated tree is used as far as it
   goes.
5. **Candidates.** Filter by directory, name, extension and size; score by name and path
   tokens; keep the top `GITHUB_FILES_PER_REPO` by score then size.
6. **Verdict.** Each candidate body is fetched once, through the same unrestricted client
   and rotating User-Agent the Telegram side uses, normalized once, and judged by
   `classify.Body` — the same verdict function the worker fetch would reach, so a mint cannot
   disagree with a probe. The endpoints come out of that same normalized buffer, so a
   candidate is fetched once, decoded once and parsed twice at most.
7. **Selection.** Per repository, greedy cover with a minimum gain of
   `max(20, 2% of the repository union)`, at most `GITHUB_ACCEPT_PER_REPO` files. Then
   globally, a file is accepted only if it still adds `GITHUB_MIN_NOVEL` endpoints the
   census and the already-accepted picks do not carry.
8. **Mint.** Accepted URLs join the same `live` map the Telegram scan fills, so they are
   minted, aged, retired and pruned by the machinery that already owns `private.yaml`.
   Names are `gh-<owner>-<repo>`, plus an ordinal for a repository's second file — and the
   stem is capped at 40 bytes, so a very long owner/repository pair is truncated and two
   repositories sharing the first 40 bytes are separated by that ordinal rather than by the
   name. The `feed: gh:<owner>/<repo>` carries the full identity either way, and it is what
   the source cap counts.

The whole pass is bounded by half the cycle's remaining budget, capped at 20 minutes: the
channel graph is the work a cycle cannot resume and the mint is what a cycle exists for, so
a GitHub overrun — a census against slow hosts, a secondary-limit sleep storm — ends the
phase and not the cycle. The channel scan's state is persisted before the phase starts, for
the same reason.

## Rate limits

Authenticated REST, measured 2026-09-07: core 5000/hour, `search/*` 30/minute,
`search/code` 10/minute. The limiter honours `x-ratelimit-remaining`/`reset` per bucket and
sleeps on `retry-after`; a secondary-limit 403 is a sleep, never a retry storm. The token is
read from `GITHUB_TOKEN`, or from the file named by `GITHUB_TOKEN_FILE` (the agenix pattern
`geoblock.gemini.key_file` already uses). Without a token the phase is disabled rather than
degraded: unauthenticated code search does not exist.

## Knobs

Every key is an env var of the `crawl` subcommand, read in `main.go` beside the `CRAWL_*`
block. Defaults live there too; the table states them so a compose file can be read without
the source.

|Env|Default|What it bounds|
|---|---|---|
|`GITHUB_ENABLED`|`true`|the whole phase; false skips it even with a token|
|`GITHUB_TOKEN` / `GITHUB_TOKEN_FILE`|empty|the credential; empty disables the phase|
|`GITHUB_SEARCH_CODE`|`8`|code-search calls per cycle (10/min limit)|
|`GITHUB_SEARCH_REPO`|`6`|repo-search calls per cycle|
|`GITHUB_MAX_REPOS`|`120`|new repositories treed per cycle|
|`GITHUB_FILES_PER_REPO`|`8`|candidate bodies fetched per repository|
|`GITHUB_ACCEPT_PER_REPO`|`4`|files one repository may contribute|
|`GITHUB_MIN_NOVEL`|`100`|endpoints a file must add to be minted|
|`GITHUB_FRESH`|`504h`|how recently a repository must have been pushed|
|`GITHUB_MAX_SOURCES`|`60`|GitHub-minted sources allowed to exist at once (150 until probation shipped)|
|`GITHUB_OUTCOMES`|`http://sub-preprocessor:9090/metrics`|the service metrics endpoint probation reads `stable_source_tested_nodes` from, once per cycle|
|`GITHUB_PROBATION`|`6`|consecutive survivor-free SERVICE cycles before a GitHub-minted source is withdrawn (mirrors the 6-cycle not-live retirement rule)|
|`GITHUB_CENSUS_TTL`|`6h`|how long a census is reused before rebuilding|
|`GITHUB_CENSUS`|`/config/.crawler-census.bin`|census file path|
|`GITHUB_CONCURRENCY`|`8`|parallel candidate fetches|

## Gotchas

- `raw.githubusercontent.com/<owner>/<repo>/HEAD/<path>` resolves (verified 2026-09-07,
  2655/2655 responses were 200), but the mint writes the default branch by name: `HEAD`
  would silently follow a branch rename and a renamed default branch is exactly the case
  where a source should die instead.
- A repository can publish the same payload under several git-ref spellings; the endpoint
  cover collapses them, the URL string alone would not.
- Search results are not stable between pages; the cursor is a budget device, not a
  guarantee of exhaustive coverage. Coverage comes from repetition across cycles.
- The tree call returns blob sizes but not commit dates. Per-file freshness would cost one
  call per path, so the repository's `pushed_at` is the freshness signal and the node census
  is what catches a stale file: a file whose endpoints are all known adds nothing and is not
  minted, whether it is stale or merely redundant.
