# Metrics and the Grafana dashboard

> **When to read this:** Read before adding, renaming or rendering a metric, or before editing `deploy/grafana/sub-preprocessor.json`. A metric name is a wire format from the moment it ships.

## Monitoring / metrics (Prometheus + Grafana)

The stable worker exports per-cycle stats as Prometheus metrics, visualized by a
Grafana dashboard. **The dashboard AND its NixOS wiring live in this repo** so they
track the metric names; the NixOS host pulls them in as a flake input — do NOT
vendor the dashboard into the nixos repo.

- `internal/metrics` renders `stable.CycleReport` as hand-rolled Prometheus text
  exposition (deliberately no `client_golang` — the `google.golang.org/protobuf =>
  metacubex/protobuf-go` replace in `go.mod` makes it risky). Served on an internal
  listener (`server.metrics_listen`, default `:9090`); `docker-compose.yaml` publishes
  it loopback-only — `127.0.0.1:9091:9090` for `sub-preprocessor`
  (`docker-compose.yaml:12`) — keep it non-public. One published port per deployment,
  and each deployment gets its own scrape job (last bullet); the second instance that
  carried one was retired 2026-08-26.
- Data flows via the nil-safe `stable.Reporter`: `RunOnce` hands a `CycleReport` to
  `metrics.Metrics.Observe` on a published cycle and `ObserveError()` on any abort.
  **Adding/renaming a metric? Update
  `deploy/grafana/sub-preprocessor.json` in the same commit.**
- **The cycle is timed per PHASE, and the probe phase is not what its name suggests.**
  `stable_cycle_phase_duration_seconds{phase}` carries `fetch`/`merge`/`dead_filter`/
  `probe`/`egress`/`publish`; they sum to slightly LESS than
  `stable_cycle_duration_seconds` because the steps between them (dead-cache write,
  survivor selection, cache prune, report assembly) belong to no phase, and the panel
  draws the total over the stack so the residue is visible rather than hidden.
  `phase="probe"` is the whole `Prober.Probe` call — payload parsing, then the TCP
  reachability pre-check at its own concurrency OUTSIDE `check.concurrency`, then the
  URL-test rounds — so `check.timeout * check.rounds` bounds only its last part. The
  pre-check is deliberately not its own phase (that needs a per-cycle path back out of
  `Probe`, and `Prober` gained a per-node `ProbeResult.Stage` instead of a second
  return value); read its share off `stable_precheck_dialled_endpoints` and
  `stable_probe_outcome_nodes{stage="condemned"}`, against `stable_precheck_trusted` (see the
  pre-check bullet below). A cycle that ABORTS publishes no
  phase numbers at all, so both families keep describing the last cycle that PUBLISHED.
- **An RSS reading from a `GOGC=off` run is not a per-phase allocation figure.** With the
  collector off, RSS approximates CUMULATIVE allocation, so the number belongs to no phase and
  cannot be attributed to one — the same attribution the
  `stable_cycle_phase_duration_seconds{phase}` bullet above exists to police. Measure with the
  collector on: over the real 163-source corpus the fetch/parse phase allocated 40.69 MB before
  the wave and 24.57 MB after.
- **`check.concurrency` is per-deployment; nothing may pin one value.**
  `config/config.yaml` ships 16 against its own `max_avg_ms: 800`
  (`config/config.yaml:262`, `:261`), with the measured rationale in its comment. The
  second compose instance, retired 2026-08-26, shipped 32 against `max_avg_ms: 4000`
  and `check.timeout: 5000ms`: 16 was the knee measured against an 800ms gate, where
  the +107ms that conc=64 adds is 13% of the budget and only 2.7% of a 4000ms one.
  That spread is the argument for DERIVING `precheckDialBudget` from `check.timeout`
  instead of hardcoding it, and one config does not weaken it — the constant it
  replaced, 500ms, was 8x tighter than the retired instance's gate and deleted nodes
  that instance was tuned to keep, and nothing stops the next deployment from shipping
  that gate again. What the retirement did change is that the derivation is now harder
  to SEE than to keep: the shipped `check.timeout: 1000ms` (`config/config.yaml:259`)
  over `precheckAttempts` 2 is exactly the 500ms constant. So the test that still tells
  derived from hardcoded is `TestPrecheckBudgetTracksTheDefaultTimeout`
  (`internal/stable/prober_test.go:774`), which strips that one line before loading,
  while `TestPrecheckBudgetCoversShippedLatencyGates` (`:753`) reads the shipped config
  as it stands.
- **A histogram bound that marks a gate is a CONTRACT with the config, not a default.**
  `latencyBuckets` MUST carry a bound equal to every `check.max_avg_ms` that any shipped
  config sets, because `SelectSurvivors` admits on exactly that value and a threshold
  landing between two bounds hides the one edge `stable_kept_latency_ms` was added to make
  visible. So moving a `max_avg_ms` ONTO a bound the ladder already carries is a one-file
  edit — that is what the tail past every gate is for — and moving it anywhere else adds
  its bound in the same commit. Never append a bound the ladder already has:
  `writeHistogram` emits one line per element with no dedupe, so a duplicate renders two
  identical `le=` series and Prometheus rejects the whole scrape.
  The ladder carries EVERY shipped value rather than the one this deployment runs,
  because a second deployment scrapes this same metric name under its own job and may
  gate elsewhere: 4000 sits on the ladder (`internal/metrics/metrics.go:38`) as the
  retired second instance's gate and now rides the tail that every bound past the live
  gate is already in, so keeping it costs nothing and re-adding it would cost a commit.
  All three halves are enforced rather than merely written here:
  `TestLatencyBucketsCoverShippedGates` reads the shipped config through `config.Load`
  (`shippedConfigDirs`, `internal/metrics/ladder_test.go:52`),
  `TestLatencyBucketsCoverTheDefaultGate` covers the gate it does not exercise by
  loading it with the key stripped, and
  `TestLatencyBucketsAreStrictlyIncreasing` catches the duplicate.
- **A count of nodes the pipeline KEPT never rides `FilterReport.Dropped`.** That map renders as
  `stable_filter_dropped_nodes{filter,reason}`, which the dashboard titles "drops by reason", so a
  kept count carried through it reads as a drop. The
  gemini gate's `stable_gemini_gate_{enabled,checks,unverified_checks}` are the worked example of the right
  shape: a stage-specific report on `CycleReport` (like `TraceReport`), rendered by its own `writeGemini`.
  They also fix the three states apart, because a gate that is OFF must not read like a gate that is FINE —
  `enabled 1` = it ran last cycle, `enabled 0` = a configured gate that was skipped for want of a usable
  key so it checked nothing, and nothing rendered = no gemini report at all. The 2026-09 config round
  moved the second state out of the config's reach: the LOAD refuses an armed gemini entry with no key
  material and Apply refuses one whose declared key_file cannot be resolved (see config.md), so a
  deployed `enabled 0` means a spec wired around those gates — direct construction, a test, a wiring
  bug — not a live gate that booted without a key. Nothing rendered keeps its FOUR causes rather than
  "gemini not configured" alone: no
  scrape, no cycle published yet, no `gemini` in `filters`, or a configured one that never reached its check
  (`buildNodeFilters` skipped it with only a WARN for want of Gemini support on the prober, or
  `filterAndMeasureEgress` returned before the chain — which publishes every survivor UNFILTERED;
  the old `ParseProxies`-failure cause for that last skip now exists only on the no-retention prober
  fallback, since production's prober hands its probe-built adapters to the egress stage and there is
  no egress parse to fail). The three families have had their own tiles since the same round: panels
  23-25 (`Gemini gate enabled`/`checks`/`unverified checks`), and the other two families the
  same-commit rule then caught up — `stable_kept_speed_min_mbps` and `stable_kept_latency_min_ms` —
  sit in panels 26 and 27. The trace families got the same absent-gating on the same round:
  `writeTrace` renders `stable_trace_{answered,unanswered,moved}_nodes` only for a cycle whose egress
  stage reached the trace (`TraceReport.State`, set only in `applyTrace`), so a gap reads "no trace
  ran" instead of the old byte-identical "trace ran and nobody answered". Metric names are a wire format from the moment they ship, exactly like the
  drop-reason strings.
  The reachability pre-check rides that same shape, for the same reason: `PrecheckReport` on
  `CycleReport`, rendered by its own `writePrecheck` as
  `stable_precheck_{trusted,dialled_endpoints,refused_endpoints,unresolved_endpoints}`, with the
  three states apart — `trusted 1` = the verdict was used, `trusted 0` = its breaker rejected the
  verdict as implausible so every node was probed, nothing rendered = no pre-check ran. A tripped
  breaker condemns nobody, so `stage="condemned"` reads 0 exactly as it does for a pre-check that
  found every server reachable. The condemned count stays OUT of `Dropped` too: the pre-check is
  not a filter, and its counts are ENDPOINTS where every filter series counts NODES.
- **`kept` means two different things, one per scope, and the per-source funnel is where that
  bites.** Each source carries four falling counts —
  `stable_source_{nodes_total,valid_nodes,tested_nodes,published_nodes}{source,feed,owner}`:
  yielded, survived the IP stage, survived the URL test, reached the published payload — with
  `stable_source_dropped_nodes{source,reason}` splitting the first gap by reason, `unsupported`
  aside (it counts unparseable input LINES, which never entered `nodes_total`).
  `dropped_nodes` alone carries no `feed` and no `owner`, by decision: it is 3507 of
  the 5511 per-source samples the exporter renders — 7 of every 11 per source, exact whatever the
  build — and no panel or rule asks for drops by owner, so per-source drops are the one question
  still answered through `source`. The gap readers get wrong is the first one, and it is not the probe's failure
  rate: `valid_nodes` is counted per source in `fetchSources`, BEFORE `Merge`, while
  `tested_nodes` is counted after it, and SIX stages sit between them — `Merge` drops a node
  whose line will not re-parse, one `PlaceholderNode` names, one whose lowercased `server:port`
  an earlier source already claimed, and one `relabelNode` fails; then `filterDead` skips every
  merged node the dead cache holds, before `SelectSurvivors` ever sees it; and only the
  remainder is URL-tested. The biggest term is the dedupe, not the dead-node
  skip: measured 2026-08-15, `valid_nodes` summed over sources less `stable_merged_nodes` was
  145553 on `sub-preprocessor` (`:9091`) and 64308 on the second instance (retired
  2026-08-26), against a dead skip of 34282 and 32284 and a probe loss of 3102 and 2972.
  Both readings stay: the ordering of the three terms is the finding, and it held on two
  differently curated source lists. Reach for the dead skip second, as the largest term no
  per-source series shows: merged 37760 vs probed 3478 on `sub-preprocessor` and 36109 vs
  3825 on the retired instance, so nine merged nodes in ten never reach
  the URL test at all and probe failure can account for at most the ~9% that did — read
  `stable_dead_skipped_nodes` against `stable_merged_nodes` before blaming a source's probe
  results. The per-name source tables — panel 8 (`Crawler sources: top 25 (last cycle)`) and
  panel 22 (`Curated sources: top 25 (last cycle)`) beside it — rename the four columns
  `yielded`/`valid`/`kept`/`filtered`, so their `kept` column is `stable_source_tested_nodes` and
  their `filtered` column is `stable_source_published_nodes`, while the GLOBAL `stable_kept_nodes`
  means PUBLISHED. That collision is known and left standing: the global name is a wire format
  that panels and alerts already read, and renaming it to tidy a column label is a bigger break
  than the ambiguity.
  The same word carries a UNIT trap one metric over: `stable_kept_latency_ms` observes one value
  per survivor, and that value is the node's MEAN over its rounds rather than a single probe
  sample (`keptLatencies`, `internal/stable/checker.go:384-390`; the `Survivor.MeanMs` it reads is the
  exact quantity `SelectSurvivors` admits on, `internal/stable/select.go:84-87`) — so
  its p90 is a percentile over per-node means, which compress the tail.
- **The per-name tables are a PAIR split by owner, and the split is not an invitation to read
  them side by side.** Panel 8 (x=0 y=20) takes `{owner="crawler"}`, panel 22 (x=12 y=20) takes
  `{owner="curated"}`, and each ranks `topk(25, ...)` on `valid` WITHIN its own set;
  panel 21 sums both sides full-width beneath them. `owner` is an EXPORTER label, and behind it a
  FIELD rather than a regex over the name: the four per-source counters carry `feed` and `owner`
  beside `source` (`writeSources`, `internal/metrics/metrics.go:342`), read once per source off
  `SourceReport.Managed` (`:350-352`) and `.Feed` (`:354`, via `sourceFeed` at `:413`) — the
  entry's own fields, carried through `internal/stable/report.go:236-244` (`SourceReport`'s
  `Managed`/`Feed` fields) from the config entry (`checker.go:739-744`) and written by the
  crawler at mint, so a panel, an ad-hoc query and an
  alert cannot disagree about who owns a source and nothing anywhere parses a name to decide.
  Both tables show `source` VERBATIM and no query rewrites a label,
  so the `and on(source)` gate and the `joinByField` transformation see the SAME string, and that
  string is the `private.yaml` / `sources.yaml` key. `feed` and `owner` DO reach these two tables,
  as ordinary columns and once per joined frame, so both panels drop them in `organize`'s
  `excludeByName` exactly as they already drop `job`, `instance` and `__name__` — remove that
  exclusion and the table grows `feed 1 | owner 1` through `owner 4` behind its five real columns.
  That exclusion is the ONLY guard: the `labelsToFields` step ahead of it is inert, because with
  `format: "table"` the Prometheus datasource hands Grafana every label as a plain string FIELD
  and that transformation passes through any field carrying no labels of its own (measured against
  `@grafana/data` 10.4.19 on the shipped transformation arrays, 2026-08-18). `managed: true` is
  WRITE AUTHORITY over `config/private.yaml` — `recheckManaged` (`internal/crawl/crawl.go:540`)
  re-classifies only marked entries (`:548`, testing the field never the name, and skipping a
  `Body` source outright, `:551-554`), `mergeManaged`'s ownership switch (`:648`) passes an
  unmarked entry through verbatim (`:665-670`), and `managedCount` (`:877`) counts only marked
  URL sources so bulk-prune can never delete a hand-added source — and the recheck's liveness
  fetch now carries each managed entry's `hwid` per URL (`:563-568`), judging a source the way
  the worker fetches it — and `owner="crawler"` is that same predicate, read off the
  exposition instead of re-derived per query. Each of those three reads the field for one stated
  reason: a name says nothing about ownership, so a name test would put hand-added entries under
  the crawler's prune and shelter its own behind a rename, and at
  `managedCount` a zero denominator collapses the prune floor to its absolute arm.
  Row counts across the two are NOT comparable evidence of anything: measured on `:9091`, job
  `sub-preprocessor`, 2026-08-16 12:45 +03:00, 439 of the sources reporting series were
  crawler-managed against 45 hand-curated,
  so panel 8's 25 rows are a 5.7% window on its side while panel 22's 25 cover 55.6% of its own.
  The crawler side grows hourly — the same count read 327 a day earlier — so take the live
  denominator from panel 3 (Sources OK / total), not a stamp here.
  **The comparison a reader reaches unaided is false, and it is false BY CONSTRUCTION.**
  `config.Load` merges the curated `config/sources.yaml` overlay at
  `internal/config/config.go:1098` (inside `mergeSourcesOverlay`, `:1166`) but appends the
  crawler's `private.yaml` only at :1115, and
  `Merge` dedupes FIRST-WINS on lowercased `server:port` (`internal/stable/merge.go:81-88`).
  Concurrency does not reshuffle that: `fetchSources` writes `results[i]` by index
  (`internal/stable/checker.go:720`, `:723`) and reassembles them `for i, r := range results`
  (:733), so config order survives into `Merge` intact. Every curated source therefore claims
  each shared node BEFORE any quality judgement, and panel 22's `kept` and `filtered` are
  inflated against panel 8's. Measured floor on the cycle behind those numbers (prod
  `config/.stable-snapshot.json`, `updated_at` 2026-08-16 09:16:16Z, whose 83/231 published split
  matches Prometheus exactly): decoding the inline harvest's body (500 node URIs; that source was
  named `tg-inline` on the date measured and is named `inline` after the cutover) and diffing
  lowercased `server:port` against the published payload found 16 published nodes attributed to a
  curated source that the inline source ALSO yields — `fifthidea` 6, `mehrtat` 5, `bahemmat` 4,
  `yafeisun` 1, at merged-slice config index 10, 2, 5 and 14 against it at 492 of 493,
  so the curated index is below the crawler one in all 16. Read `server:port` off BOTH sides with
  `subscription.Parse` — the call `Merge` uses to build `Entry.Addr` — and never off the URI text:
  `vmess` and legacy `ss`/`ssr` keep server and port inside a base64 payload, so a regex or a
  URL-shaped `server:port` extraction finds 15 of these 16 and silently skips the `vmess` one
  (`bahemmat-481` at `165.140.216.142:443`, on both sides). A FLOOR, not the total: only 1 of the
  447 `private.yaml` entries carries an inline body (file at 2026-08-16 12:15 +03:00), the other
  446 being URLs the measurement did not fetch.
- **The feed breakdown (panel 20, "Feeds (both owners): top 25 (last cycle)") is two exporter LABELS, both of
  them fields, and its rows are still MIXED.** `feed` and `owner` ride the four per-source
  counters, so panel 20 is `sum by (feed, owner) (<metric>{job="$job"})` and nothing is derived at
  query time: `label_values(..., feed)` answers, and an alert that means a feed writes that same
  `sum by (feed, owner)` instead of carrying a `label_replace` pair of its own. Neither label is
  computed: `owner` is `SubscriptionSource.Managed` and `feed` is `SubscriptionSource.Feed`
  (`internal/config/config.go:639,644`), which the crawler writes at mint, so every URL of one
  channel shares that channel's row and a curated entry, which normally sets no `feed`, falls back
  to naming itself (`sourceFeed`, `metrics.go:413`). Group by the PAIR, never `by
  (feed)` alone: a curated name equal to some channel's slug would otherwise merge two
  owners into one row.
  Attribution is recorded, not recovered, because no fold over the NAME survives the corpus: one
  trailing strip leaves the collision form `seyedng-3631-1444c8` on its post rather than its
  channel, and a greedy strip mangles any slug ending in a digit run — `channelSlug` keeps digits
  and folds `_` to `-`, and the corpus carries `file-vpn-2`, whose `file-vpn-2-1444c8` would strip
  to `file-vpn` while `file-vpn-2-3631` must strip to `file-vpn-2`. Curated names share the minted
  shape too, and in the shipped overlay: `kort0881-vless-042` and `-041`
  (`config/sources.yaml:164`, `:166` — salvaged from the retired second instance's list
  2026-08-26) would collapse onto a `kort0881-vless` row no channel produced, and
  `goida26-1` (`:137`) is exactly what the mint writes for the first postless URL of a
  channel slugging to `goida26` (`sourceName`, `internal/crawl/crawl.go:966-969`).
  The rows stay mixed, for a reason `owner` makes legible: the inline
  harvest (`inline`, `crawl.go:1155`) records no channel and neither does a bare-hash name
  (`unattributedNameRe`, `crawl.go:68`), so each falls back to naming itself, yet each is an
  `owner="crawler"` feed standing alone.
  The figure to read here counts BUCKETS, not rows: all four targets are gated on `topk(25, sum
  by (feed, owner) (stable_source_valid_nodes{job="$job"}))`, so the table draws 25 rows out of
  however many `(feed, owner)` pairs reported. Measured on `:3020`, host clock 2026-08-18 00:58
  +0300, 501 reporting sources landed on 110 buckets: 66 crawler feeds and 44 curated names.
  The columns are `feed | owner | yielded | valid | kept | filtered`, and the four targets are
  stitched by `filterFieldsByName` → `merge` → `organize` — `labelsToFields` sits ahead of them
  and does nothing here either — NOT by the `joinByField` panels 8, 22 and 21 use: that
  transformation takes a SINGLE key (`byField`) and so cannot join on a pair, whereas `merge` keys
  on every field name the frames share — exactly `feed` and `owner` once the filter has dropped
  `Time`, the one exclusion doing work here, the aggregation having already dropped `__name__`,
  `job` and `instance`. Each column IS its label, so `organize` renames only `Value #A..D` and a
  query copied out of this panel works verbatim.
  The two labels add no series — the same per-source samples carry them — but they
  RE-IDENTIFY every series they touch, so a range crossing the deploy that shipped them shows a
  break. That break was accepted because nothing else consumed the old identities: 0 of 0 alert
  and recording rules citing `stable_source_` (measured 2026-08-18 +0300). Re-run that count
  before any further label change on these four families. For how many sources
  are configured, read `stable_sources_total` — panel 3, top of the same dashboard — rather than
  counting a config file; it is published per cycle, so it trails a config edit by a cycle.
- **The crawler-managed vs hand-curated split is an exporter LABEL over a FIELD, and its
  `published` column measures ATTRIBUTION, not contribution.** The discriminator is
  `owner="crawler"` / `owner="curated"`, rendered once per source rather than matched per query
  (see the panel 8 / 22 bullet above). Underneath it is
  `SubscriptionSource.Managed` (`internal/config/config.go:639`), whose comment fixes the axis as
  OWNERSHIP rather than provenance: it "marks an entry the crawler minted and may therefore
  prune. Absent means hand-added, so forgetting the field shelters a source instead of exposing
  it." The crawler sets that field on every entry it mints and tests it on every entry it may
  touch, so the label cannot drift from the write rule it reports. Absent-means-sheltered is the
  direction that matters, and the mark is refused on a git-tracked entry by a per-file gate so a
  curated file cannot claim it by accident: Load's pre-merge pass refuses it in config.yaml's own
  list naming the file (`config.go:1093`), `mergeSourcesOverlay` (`config.go:1166`) refuses it in
  `sources.yaml`'s entries as it loads them (the check at `:1182-1187`), and `validateSources`
  (`config.go:1661`) over the merged list stays as the second gate for any other statement order —
  which is what also covers `config.yaml`.
  So do not read it as "telegram": the inline harvest (`inline`, `crawl.go:1155`) is
  crawler-managed but is harvested raw node URIs rather than a channel, and a bare-hash name
  (`unattributedNameRe`, `crawl.go:68`)
  is managed too — measured on `:9091` 2026-08-16 12:45 +03:00, before the cutover, the managed
  side was 439 names reporting series: 437 channel-attributed, 1 inline and 1 bare hash. Nor is
  it a file boundary: `private.yaml` held 447 entries at 2026-08-16 12:15 +03:00, one of them
  (`commsub`) hand-added, and it lands on the curated side by carrying no `managed:` line.
  Read the funnel across that split and the crawler looks worthless — 439 managed sources against
  45 curated, `valid` 227878 vs 57121, `published` 83 vs 231 (`:9091`, 2026-08-16 12:45 +03:00).
  **That conclusion does not follow**, for the attribution reason the panel 8 / 22 bullet above
  measures — a curated source claims every shared node before any quality judgement, with a
  measured floor of 16 published nodes on this very cycle. Breadth says it from the other side: 29 managed sources
  published
  at least one node against 18 curated ones at the same instant, so the curated lead is volume per
  source, not reach. `tested` inverts one column earlier too (139 managed vs 493 curated), for the
  same reason — it is post-merge, so it is not the probe killing crawler nodes either.
  Therefore these series CANNOT answer "what would we lose without the crawler". They are
  post-attribution by construction, while the question is about nodes NO curated source yields —
  which nothing exported distinguishes from nodes a curated source merely claimed first.
  Answering it needs the counterfactual: a cycle merged with the crawler overlay absent, or a
  per-node record of every source that also yielded each published node. Neither exists today;
  name that gap rather than squeezing an answer out of these numbers.
  Three traps when querying the split. `count by (owner)` over a per-source series — the shape
  panel 21's sources column uses — counts sources that RETURNED A BODY, not sources configured:
  439 + 45 = 484 = `stable_sources_ok`, against `stable_sources_total` 493 (`:9091`, 2026-08-16
  12:45 +03:00), so the 9 fetch failures of that cycle sit on NEITHER side and their ownership is
  unknowable from Prometheus: `fetchSources` warns and `continue`s (`checker.go:735-736`), so they
  emit no per-source series and appear in neither table — not dead, not pruned, not dropped from
  config, and the next cycle may fetch them fine.
  A label VALUE with no series reads EMPTY, not 0, and `owner` is where that bites: a
  deployment with no managed entry loaded — a fresh `./config` before the crawler's first
  `private.yaml` write, or a preprocessor deployed without `tg-sub-crawler` beside it —
  makes `{owner="crawler"}` match nothing, so panel 8 renders "No data" from all four of
  its targets rather than rows of zeros, and panel 21's crawler row goes missing instead of
  reading 0. Stopping the crawler is NOT that case: `private.yaml` stays on disk and every
  entry in it keeps reporting `owner="crawler"`, so the managed side reads stale, not empty.
  The measured case was the second instance: 52 hand-curated sources, no `private.yaml` and
  no crawler writing into its directory, panel 8 "No data" on all four targets for its whole
  life (retired 2026-08-26).
  And never divide the two columns into a survival rate: `valid` is counted per source
  BEFORE `Merge` and double-counts a node two sources both yield, whereas `published` is
  post-attribution. Panel 21 (Ownership split: crawler vs curated (last cycle)) sits
  full-width at the row below panels 8 and 22, summarising the pair it follows, and renders as a
  table because its five numeric columns spanned 45 to 263500 at that instant and a shared axis
  would flatten `filtered` to zero. It quotes no digits of its own, aggregates `sum by (owner)`
  over one target per column (`count by (owner)` for the sources column), and keeps panels 8, 22
  and 20's column vocabulary, in which `filtered` IS `stable_source_published_nodes`.
- `flake.nix` output `nixosModules.monitoring` (`deploy/monitoring.nix`) adds
  the `sub-preprocessor` Prometheus scrape job and the Grafana dashboard
  provider (`deploy/grafana/sub-preprocessor.json`). It assumes the host
  already owns Prometheus, Grafana, and the datasource they use; the dashboard
  itself picks that datasource through a template variable, so no fixed uid is
  needed here. `nixosModules.default` is the separate systemd-service module —
  leave it.
- **Every IP-stage drop reason is emitted every cycle, so a zero is an answer on four of the
  seven.** `writeSources` builds a FIXED seven-entry table per source, never reading `filters:`
  (`internal/metrics/metrics.go:384-400`, reason names at :340): `dns` (nothing resolved), `ipv6`
  (a non-IPv4 literal, unlooked-up), `geo` (country policy), `asn` (ASN deny-patterns), `cidr`
  (outside the allow-list), `geoblock` (host in the geoblock store), `unsupported`. A zero on the
  middle three is ambiguous — no filter, nothing to exclude, or broken — and each gate differs:
  `geo` a `country` entry with non-empty `exclude_*` (`internal/preprocess/filters.go:44-47`
  no-ops on the worker's full allow set) or an `asn` entry whose ASN-resolved country the policy
  rejects, `asn` that same asn entry's `deny_patterns` (`internal/config/config.go:274`, copied
  only under `case FilterASN` at :376-380), `cidr` a `cidr` entry (`:381-388`). The other four
  ignore `filters:` —
  `geoblock` the separate `geoblock:` block (`internal/preprocess/processor.go:822-824`), `ipv6`
  and `dns` to `resolveNode` in `processNode` (`:829`, `:833`), `unsupported` the parser — so
  their zero is real. Seven holds for the current build only: `ipv6` joined the table in `1638523`
  (2026-07-29) and `cidr` in `82e8ef3` (2026-08-09), so a range reaching back past either plots
  fewer series, and an absence there is not a zero. Worker cycles only: the on-demand `GET /`
  path runs the same IP-stage chain but samples nothing, every `stable_source_*` series coming
  out of the cycle report (`internal/metrics/metrics.go:138`), so preprocessing done for an HTTP
  request is invisible here.
- **The through-node drops panel (panel 7, `Through-node filter drops by reason`) has three
  states, not two — and the batch breaker is a fourth that the drop series cannot show: it
  reads on its own family, `stable_filter_trusted{filter}`, which panel 7 draws per gate as
  a "verdict trusted" line.** `apiFilter.apply` and `bandwidthFilter.apply` each
  assign a full `Dropped` map on the completing path — `{blocked, unreachable}` at
  `internal/stable/nodefilter.go:174`, `{slow, unreachable}` at :311, both keys either way — so a
  gate that ran clean emits a PRESENT ZERO. The early returns keep the empty map created at
  :103/:254 (`apiFilter` disabled-for-no-key at :104-107 and cancelled context at :111-116;
  `bandwidthFilter` cancelled at :257-260; the old `outcomes == nil` guard is gone — it was
  unreachable and untested, so an empty outcome map is no longer a skip), and `writeFilters`
  iterates only the keys present (`internal/metrics/metrics.go:160`), so an inert gate writes NO
  drop series though its `filter_in`/`filter_kept` still land. Third: a gate dropped from
  `filters:` adds nothing, yet its old rows stop rather than vanish, so a long range still draws
  lines ending at that sample; the legend reduces with `last`, not `lastNotNull`, so its cell
  reads empty rather than frozen.
  The whole-batch breaker is the round's fourth state, and it is the one the drop series CANNOT
  carry: a gate whose batch verdict tripped `breakerTrips` keeps every survivor and
  writes `Dropped[blocked]=0`/`Dropped[unreachable]=0` (`nodefilter.go:140-141`, bandwidth at
  `:283-284`) — byte-identical to the ran-clean present-zero case above. The disbelief rides
  the report instead: both filters mark `FilterReport.State` (`FilterTripped` on that path,
  `FilterRan` on the believed one), and `writeFilters` renders it per filter on the pre-check's
  pattern —
  `stable_filter_trusted{filter="..."}` 1 = the verdict was believed and its drops enacted
  last cycle, 0 = the breaker rejected the batch verdict so every survivor was kept and is
  re-checked next cycle, and no sample = the filter reached no verdict (the keyless gemini
  skip; a cancelled check never publishes), where `filter_in`/`filter_kept` alone would read
  like a clean pass. "Ran and dropped everything" and "ran and dropped nothing" therefore no
  longer render alike — the drop series stays identical and only this gauge moves, and it
  moves ON THE DASHBOARD, not just in a query: panel 7 draws one right-axis "verdict
  trusted" line per gate (legendFormat `{{filter}} verdict trusted`), whose legend cell reads
  0 or 1 under the panel's `last` calc, so a tripped gate shows as its line stepping from 1
  to 0 while its drop rows stay at zero — visible without opening Prometheus. The gate's WARN
  log ("gate refused every survivor; treating the batch verdict as the endpoint's or our
  egress's ... and keeping all survivors") still fires alongside the 0. What IS
  distinguishable on the drop series themselves: a gate that genuinely ran (drop series
  present, in/kept landing) against one that never reached the chain (no report at all — no
  filter configured, zero survivors entering egress, or the no-retention prober's `ParseProxies`
  skip), because the never-ran case also has no `filter_in`/`filter_kept` sample to anchor a
  reading.
- **The dead cache turns one condemnation into several cycles of missing funnel.** A node
  answering no round folds to `Successes: 0`, which `recordDead` blocks on (`internal/stable/checker.go:794`),
  keyed by the endpoint's `server:port` AND the address the IP stage resolved for it that cycle
  (a hostname re-pointed to a new address is a new server, not the old one's corpse; `deadKey`,
  `internal/stable/deadset.go:39-41`); `filterDead` skips it for the TTL (the skip at
  `checker.go:772-775`). The write is guarded by the same plausibility breaker as the pre-check
  and the gates: a cycle where nearly every probed node failed leaves the cache unchanged
  (`breakerTrips` at `checker.go:806-815`) — committing that verdict would freeze the list for
  the whole TTL after the network recovered. The shipped config sets
  `deadcache.ttl: 3h` against a 1h `subscriptions.interval` (`config/config.yaml:174` and
  `:227`; the retired second instance shipped that same pair), and `jitteredTTL` stretches
  it by a uniform [1, 1.5)
  so the graveyard does not expire as one batch (`internal/stable/deadset.go:64-74`): [3h, 4.5h),
  three to four cycles. Stages (`internal/stable/select.go:22-52`): `passed` = a round answered,
  `connect` = no tunnel, `fetch` = tunnel up, GET failed, `condemned` = the pre-check refused the
  server. `unknown` is no mis-assignment: `probeStages` walks the PROBED ENTRIES
  (`checker.go:413-419`), so a label the prober never named reads as the zero `ProbeStage`, not
  as an absence; a non-zero `unknown` COUNT is the payload's lines `adapter.ParseProxy` refused
  (`parseLive`'s failure count, `prober.go:410-433`, the parse at :414). The pre-check runs
  BEFORE that parse, so a line it condemns is never
  parsed and reads `condemned` even when mihomo would also have refused it — the verdict is
  about the endpoint, which the raw mapping carries in full, and both stages mean the same
  thing to `recordDead` and to selection.
- **The breaker trips on a share of what the pre-check JUDGED, over a floor — and a total
  refusal trips at any sample size.** `filterReachable` (`internal/stable/prober.go:598`) dials
  each distinct `server:port` the payload NAMES — parsable or not, since the parse comes
  after — and a position whose adapter would reach its server over UDP — hysteria2, tuic, mieru,
  vless xhttp-over-QUIC — is not dialled at all (`dialsServerOverTCP`, `prober.go:539`), which
  is why these counts are endpoints (see above). The percentage arm needs both halves to hold:
  at least 100 judged endpoints, at least 95% of them refused (`precheckBreakerMin` at :483,
  `precheckBreakerPercent` at :479, the decided test at :655-657); the total-refusal arm fires on
  one refused endpoint as surely as on ten thousand (`breakerTrips`, :494-497, shared with
  `recordDead` and the through-node gates). Judged is dialled minus
  unresolved: an unresolvable name is judged by nobody, so no resolver outage can fire the
  breaker, and every verdict but `verdictRefused` falls through to `live` (the loop at
  :670-681).
- **Two counters render BEFORE the cycle report exists, and they are all a no-data page has.**
  `writeMetrics` emits `stable_cycles_total` and `stable_cycle_failures_total`
  (`internal/metrics/metrics.go:109`, :110) BEFORE returning on a nil `m.last` (:112), so a worker
  yet to publish exports those two alone — the cold-start twin of the abort case above, and every
  other panel's no-data state. `stable_last_success_timestamp_seconds` (:135) reads `m.lastAt`,
  which only `Observe` sets (:76; `ObserveError` moves the counters alone, :82-87), so a no-data
  publish-age tile means restart-before-first-publish, dead scrape or dead worker — only cycles/h
  parts them. `CyclePhases` (`internal/stable/report.go:67`) fixes the phase contents: the
  per-node IP stage (DNS, geo/asn/cidr) inside `fetch`, the through-node filters and the
  cdn-cgi/trace measurement inside `egress`, and payload build, the moved recount, the swap,
  snapshot write and log inside `publish`. That recount re-runs the chain over traced nodes alone
  (`movedCount`, `internal/stable/checker.go:452`), so `stable_trace_moved_nodes` counts how often the GEO tag
  WOULD have been wrong: the trace CORRECTS the chain, it rejects nobody. Two lead-tag rules
  govern the geo bookkeeping beside it. `writeTrace` renders the trace families only for a cycle
  whose egress stage actually ran (`TraceReport.State`, set solely in `applyTrace`,
  `checker.go:658-676`): a gap reads "no trace ran", not the old byte-identical "trace ran and
  nobody answered". And the country gauges are booked by the FIRST rendered `[GEO:xx]` tag:
  `stable_geo_unknown_nodes` counts published nodes whose lead tag is `[GEO:??]`, and a later GEO
  entry resolving the node does not clear it — such a node is absent from
  `stable_kept_country_nodes` as well, because `Annotate` returns the lead tag's country and
  `BuildPayload` writes that into `Survivor.Country` (annotator.go's `tookLead`;
  `internal/stable/select.go:99-131`).
- **A panel declaring no colour is not neutral: Grafana merges in green-base/red-from-80, so an
  ABSENT series paints green.** A no-value takes the BASE threshold step's colour, which is why
  the stat tiles declare an explicit red base. A continuous palette fails the other way: it
  interpolates on percent alone, ignoring value and thresholds, so the merged default is inert
  and a no-value's percent 0 is the FIRST palette colour. Panel 12 therefore uses
  `continuous-BlPu`, whose low end asserts nothing; a green-yellow-red palette would paint an
  empty panel green.
- **The speed ladder is fixed, and only its average survives a cycle that measured nothing.**
  `speedBuckets` is seven bounds, 5 to 500 Mbps (`internal/metrics/metrics.go:24`), so a quantile
  at the top bound means faster than 500, not a plateau. `keptSpeeds` skips a zero Mbps
  (`internal/stable/checker.go:371`), so an unmeasured cycle passes an empty slice and
  `writeHistogram` still emits a zero `_sum` and `_count` (`internal/metrics/metrics.go:139`,
  :431-432): the avg target's `clamp_min(...,1)` evaluates 0/1 into a FLAT ZERO, while p50/p90 go
  NaN over all-zero buckets and the guarded max (:142) vanishes. `stable_kept_speed_min_mbps` is
  exported (:141) and since the 2026-09 round has its own tile — panel 26, "Kept-node speed min
  (last cycle)" — whose no-data means nothing was measured that cycle, exactly like the max line;
  the raw `stable_kept_speed_mbps_bucket{le="5"}` histogram line itself stays unplotted, the tile
  answering the floor question directly. `keptLatencies` keeps every survivor
  (`internal/stable/checker.go:384`), so `stable_kept_latency_ms_count` always equals
  `stable_kept_nodes` and that panel has no such trap; its min twin sits in panel 27, "Kept-node
  latency min (last cycle)", where no-data means no published nodes yet rather than a missing
  measurement.
- **A source you cannot find in a `top 25` table is under the cutoff, and a feed's `valid` is an
  upper bound.** On the 2026-08-18 +0300 reading above (110 buckets, 25 drawn), 85 buckets went
  undrawn. Skew, not fan-out, decides placement — individually small URLs place no per-URL row
  yet rank high summed. Summing `valid` over a feed's own URLs double-counts every node several
  carry: the inflation above applies WITHIN a feed too. `filtered` does not, `Merge`
  giving each node exactly one `<source>-NNN` label (`internal/stable/merge.go:88-97`) and
  `countSourceStages` crediting one source per published node (`internal/stable/checker.go:625`),
  so `sum(stable_source_published_nodes)` equals `stable_kept_nodes` unless `countSourceStages`
  logged unattributed survivors (`checker.go:651-654`) — the one way it can undercount. `filtered` 0
  beside a large `valid` says only that nothing reached the payload under that name: dedupe,
  dead-cache skip, probe failure and a gate read alike.
- **One deployment, one scrape JOB — a second one gets its OWN job, never a second target
  in the first.** `deploy/monitoring.nix:10-17` scrapes `sub-preprocessor` at
  `127.0.0.1:9091` under that `job_name`, and nothing else today: the second instance's job
  was removed with it on 2026-08-26. The mechanism outlives the count, because the
  dashboard's Instance picker is `label_values(stable_cycles_total, job)` and every panel
  expression is scoped `{job="$job"}` — two targets sharing a job are unselectable AND
  silently summed into one funnel. So the picker stays (it enumerates one value today,
  which is valid) and two consequences still bind any dashboard edit: a panel added later
  MUST carry that selector, and a panel description MUST NOT name a config file as the
  authority for what ran — which filters exist depends on which deployment `$job` selects.
- **The crawler's five counters are label-less lifetime int64s with no dashboard panel, and
  they are readable where nothing scrapes.** Rendered by `internal/metrics`' exposition helpers
  and served by the CRAWLER process — not the preprocessor listener described above — at
  `GET /metrics` on the optional `CRAWL_HTTP` trigger listener; a deployment without `CRAWL_HTTP`
  reads the same five numbers off a per-cycle structured log line that every cycle which crawls emits. No Grafana
  panel or Prometheus rule consumes them yet — documentation only, by decision. Semantics:
  `stable_crawl_topic_pages_total` counts successful topic embed fetches (the denominator);
  `stable_crawl_topic_live_total` those yielding at least one live subscription (the numerator);
  `stable_crawl_topic_empty_total` embed pages that answered with a reachable body but zero
  message wraps — a gone/private topic or an embed markup change;
  `stable_crawl_topic_discovered_total` same-group carve-out edges admitted into the crawl queue;
  `stable_crawl_group_empty_total` bare discovered groups whose `/s/` listing was reached and
  empty with no topic hint available — the counted dead end.
- **One automated alarm and one operator check ship with the five counters; both are fleet-shaped,
  never per-topic.** (1) The empty ratio pinned near 1 while fetches keep rising ⇒ the embed markup
  changed and every topic read is coming back silently empty — fired as a warn from the same per-cycle
  log line once at least five topic pages were fetched in a cycle with zero live yields, the same
  fleet-shaped warn `reportCursors` uses for lost listing cursors. (2) `stable_crawl_topic_discovered_total`
  stuck near zero over days ⇒ the same-group carve-out is misfiring and intra-forum recursion is
  effectively dead — operator guidance rather than an automated alarm: nothing fires on it, so check
  that before concluding the forums themselves dried up.

**Editing the dashboard** — source of truth is `deploy/grafana/sub-preprocessor.json`
(provisioned `editable: false`; validate with `jq`, ideally render against a throwaway
Grafana+Prometheus first):

```bash
# here:
$EDITOR deploy/grafana/sub-preprocessor.json && git commit -am '...' && git push
# in the nixos repo (server imports inputs.sub-preprocessor.nixosModules.monitoring):
nix flake update sub-preprocessor && make switch
```
