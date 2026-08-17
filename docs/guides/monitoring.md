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
  it loopback-only once per instance — `127.0.0.1:9091:9090` for `sub-preprocessor`,
  `127.0.0.1:9092:9090` for `sub-preprocessor-vassago` — keep both non-public.
- Data flows via the nil-safe `stable.Reporter`: `RunOnce` hands a `CycleReport`
  (per-source stage counts AND drops, per-filter in/kept/dropped-by-reason, kept
  speeds AND kept mean latencies, cycle aggregate + duration, the six `Phases` and
  the probed set split by `ProbeStages`) to `metrics.Metrics.Observe` on a
  published cycle, and
  `ObserveError()` on any abort. **Adding/renaming a metric? Update
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
  `stable_probe_outcome_nodes{stage="condemned"}` — the stage count alone reads 0 whenever the
  pre-check's breaker DISCARDED its verdict, and `stable_precheck_trusted` is what tells those two
  apart. A cycle that ABORTS publishes no
  phase numbers at all, so both families keep describing the last cycle that PUBLISHED.
- **Two figures were retracted mid-wave; do not re-derive them from an older
  transcript.** (1) A "~664 MB transient allocation burst during fetch/parse" was a
  `GOGC=off` run, where RSS approximates CUMULATIVE allocation and belongs to no
  phase; measured over the real 163-source corpus that phase allocated 40.69 MB
  before the wave and 24.57 MB after. (2) "`check.concurrency: 16` is pinned" is
  instance-specific: `config/config.yaml` ships 16, `config-vassago/config.yaml`
  ships 32, each with its own measured rationale in its own comment. A constant in
  the prober that is tighter than either instance's `check.timeout` deletes nodes
  that instance is tuned to keep, which is why `precheckDialBudget` derives from
  `check.timeout` and a test reads BOTH shipped configs to pin it.
- **A histogram bound that marks a gate is a CONTRACT with the config, not a default.**
  `latencyBuckets` MUST carry a bound equal to every `check.max_avg_ms` that any shipped
  config sets, because `SelectSurvivors` admits on exactly that value and a threshold
  landing between two bounds hides the one edge `stable_kept_latency_ms` was added to make
  visible. So moving a `max_avg_ms` ONTO a bound the ladder already carries is a one-file
  edit — that is what the tail past every gate is for — and moving it anywhere else adds
  its bound in the same commit. Never append a bound the ladder already has:
  `writeHistogram` emits one line per element with no dedupe, so a duplicate renders two
  identical `le=` series and Prometheus rejects the whole scrape.
  The two instances scrape this metric name under different jobs and may disagree on the
  threshold, so the ladder carries EVERY shipped value, not one of them. All three halves
  are enforced rather than merely written here: `TestLatencyBucketsCoverShippedGates`
  reads both shipped configs through `config.Load`,
  `TestLatencyBucketsCoverTheDefaultGate` covers the gate neither of them exercises by
  loading a shipped config with the key stripped, and
  `TestLatencyBucketsAreStrictlyIncreasing` catches the duplicate.
- **A count of nodes the pipeline KEPT never rides `FilterReport.Dropped`.** That map renders as
  `stable_filter_dropped_nodes{filter,reason}`, which the dashboard titles "drops by reason"; the trace's
  `corrected`/`unanswered` shipped through it for a filter that drops nothing — shipped by `b545d0a`,
  corrected in `e554307`, which moved them out of `Dropped` (into a `FilterReport.Notes` map that `abf452b`
  then retired along with the whole filter, geotrace now being the annotate stage behind `TraceReport`). The
  gemini gate's `stable_gemini_gate_{enabled,checks,unverified_checks}` are the worked example of the right
  shape: a stage-specific report on `CycleReport` (like `TraceReport`), rendered by its own `writeGemini`.
  They also fix the three states apart, because a gate that is OFF must not read like a gate that is FINE —
  `enabled 0` = configured but no usable key so it checked nothing, `enabled 1` = it ran, and nothing
  rendered = the gate did not run, which is FOUR causes rather than "gemini not configured" alone: no
  scrape, no cycle published yet, no `gemini` in `filters`, or a configured one that never reached its check
  (`buildNodeFilters` skipped it with only a WARN for want of Gemini support on the prober, or
  `filterAndMeasureEgress` returned before the chain when `ParseProxies` failed — which publishes every
  survivor UNFILTERED). Metric names are a wire format from the moment they ship, exactly like the
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
  `stable_source_{nodes_total,valid_nodes,tested_nodes,published_nodes}{source}`: yielded, survived
  the IP stage, survived the URL test, reached the published payload — with
  `stable_source_dropped_nodes{source,reason}` splitting the first gap by reason, `unsupported`
  aside (it counts unparseable input LINES, which never entered `nodes_total`). `valid_nodes` was
  `stable_source_kept_nodes` until the two post-merge counts joined it and the name had to say
  WHICH kept. The gap readers get wrong is the first one, and it is not the probe's failure
  rate: `valid_nodes` is counted per source in `fetchSources`, BEFORE `Merge`, while
  `tested_nodes` is counted after it, and SIX stages sit between them — `Merge` drops a node
  whose line will not re-parse, one `PlaceholderNode` names, one whose lowercased `server:port`
  an earlier source already claimed, and one `relabelNode` fails; then `filterDead` skips every
  merged node the dead cache holds, before `SelectSurvivors` ever sees it; and only the
  remainder is URL-tested. The biggest term is the dedupe, not the dead-node
  skip: measured 2026-08-15, `valid_nodes` summed over sources less `stable_merged_nodes` was
  145553 on `sub-preprocessor` (`:9091`) and 64308 on `sub-preprocessor-vassago` (`:9092`),
  against a dead skip of 34282 and 32284 and a probe loss of 3102 and 2972. Reach for the dead
  skip second, as the largest term no per-source series shows: merged 37760 vs probed 3478 on
  the first instance and 36109 vs 3825 on the second, so nine merged nodes in ten never reach
  the URL test at all and probe failure can account for at most the ~9% that did — read
  `stable_dead_skipped_nodes` against `stable_merged_nodes` before blaming a source's probe
  results. The "Top sources" tables — panel 8 (`Top sources: crawler-managed (last cycle)`) and
  panel 22 (`Top sources: hand-curated (last cycle)`) beside it — rename the four columns
  `yielded`/`valid`/`kept`/`filtered`, so their `kept` column is `stable_source_tested_nodes` and
  their `filtered` column is `stable_source_published_nodes`, while the GLOBAL `stable_kept_nodes`
  means PUBLISHED. That collision is known and left standing: the global name is a wire format
  that panels and alerts already read, and renaming it to tidy a column label is a bigger break
  than the ambiguity.
- **"Top sources" is a PAIR of tables split by owner, and the split is not an invitation to read
  them side by side.** Panel 8 (x=0 y=20) takes `source=~"tg-.*"`, panel 22 (x=12 y=20, the new
  one) takes `source!~"tg-.*"`, and each ranks `topk(25, ...)` on `valid` WITHIN its own set;
  panel 21 sums both sides full-width beneath them. That regex is the only discriminator that
  exists, because the four per-source counters the tables read carry `source` and nothing else
  (`labelSource`, `internal/metrics/metrics.go:44`, sampled at :287, :291, :295, :299; only
  `stable_source_dropped_nodes` adds a second label at :312, and `reason` is a drop cause, not
  an owner) — no owner, origin or file field ever reaches Prometheus.
  Panel 8 hides the prefix with an outermost `label_replace(<expr>, "source", "$1", "source",
  "tg-(.*)")` in all four targets, and the two matchers around it see DIFFERENT names. The `and
  on(source)` join matches the ORIGINAL prefixed name, because it sits inside the `label_replace`.
  The `joinByField` transformation matches the STRIPPED name: Prometheus returns frames whose
  `source` label is already rewritten, so the original name never reaches Grafana at all. The join
  is still correct because all four targets rewrite identically, and the strip is injective over
  every name that can reach the table — so the stripped key is as unique as the original. That is
  display-only PromQL and it changes NO source name: the prefix is WRITE AUTHORITY over
  `config/private.yaml` — `crawl.go:367` rechecks only prefixed sources, `:419` preserves
  non-prefixed entries verbatim, and `:546` counts only prefixed ones so bulk-prune can never
  delete a hand-added source. It costs no information to hide only because every row in that table
  is crawler-managed already; panel 22 strips nothing. Row counts across the two are NOT
  comparable evidence of anything: measured on `:9091`, job `sub-preprocessor`, 2026-08-16
  12:45 +03:00, 439 of the sources reporting series were crawler-managed against 45 hand-curated,
  so panel 8's 25 rows are a 5.7% window on its side while panel 22's 25 cover 55.6% of its own.
  The crawler side grows hourly — the same count read 327 a day earlier — so take the live
  denominator from panel 3 (Sources OK / total), not a stamp here.
  **The comparison a reader reaches unaided is false, and it is false BY CONSTRUCTION.**
  `config.Load` merges the curated `config/sources.yaml` overlay at
  `internal/config/config.go:985` but appends the crawler's `private.yaml` only at :1000, and
  `Merge` dedupes FIRST-WINS on lowercased `server:port` (`internal/stable/merge.go:79`).
  Concurrency does not reshuffle that: `fetchSources` writes `results[i]` by index
  (`internal/stable/checker.go:605`) and reassembles them `for i, r := range results` (:615), so
  config order survives into `Merge` intact. Every curated source therefore claims each shared
  node BEFORE any quality judgement, and panel 22's `kept` and `filtered` are inflated against
  panel 8's. Measured floor on the cycle behind those numbers (prod
  `config/.stable-snapshot.json`, `updated_at` 2026-08-16 09:16:16Z, whose 83/231 published split
  matches Prometheus exactly): decoding `tg-inline`'s inline body (500 node URIs) and diffing
  lowercased `server:port` against the published payload found 16 published nodes attributed to a
  curated source that `tg-inline` ALSO yields — `fifthidea` 6, `mehrtat` 5, `bahemmat` 4,
  `yafeisun` 1, at merged-slice config index 10, 2, 5 and 14 against `tg-inline` at 492 of 493,
  so the curated index is below the crawler one in all 16. Read `server:port` off BOTH sides with
  `subscription.Parse` — the call `Merge` uses to build `Entry.Addr` — and never off the URI text:
  `vmess` and legacy `ss`/`ssr` keep server and port inside a base64 payload, so a regex or a
  URL-shaped `server:port` extraction finds 15 of these 16 and silently skips the `vmess` one
  (`bahemmat-481` at `165.140.216.142:443`, on both sides). A FLOOR, not the total: only 1 of the
  447 `private.yaml` entries carries an inline body (file at 2026-08-16 12:15 +03:00), the other
  446 being URLs the measurement did not fetch.
  Two more things the pair will not tell you. On `sub-preprocessor-vassago` there are no `tg-*`
  sources at all (52 configured, every one hand-curated, same instant), so panel 8 there is
  EMPTY: all four targets return no series and Grafana renders "No data" — not rows of zeros.
  And the gap between `stable_sources_total` 493 and `stable_sources_ok` 484 at that instant is 9
  sources that FAILED TO FETCH that cycle, where `fetchSources` warns and `continue`s
  (`checker.go:618`); they emit no per-source series and appear in neither table. They are not
  dead, not pruned, not dropped from config, and the next cycle may fetch them fine.
- **The channel breakdown (panel 20, "Top source groups") is a query, not a metric, and its rows
  are MIXED.** The exporter emits `source` and nothing else — Prometheus knows no `channel`
  label, `label_values(..., channel)` comes back empty and there are no recording rules — so
  panel 20 derives it inline, `label_replace` stripping the `-<sha6>` suffix off `source`.
  Nothing ships on the exporter side and no series cardinality is added: the dashboard JSON is
  the whole change. The cost lands on anyone writing their own query or alert, because a
  per-source series thresholds ONE URL — an alert that means a channel must carry the same
  `label_replace` pair itself, and both of its calls are load-bearing. The inner call copies
  `source` into `channel` verbatim; the outer overwrites that copy for names matching the slug
  form. Drop the inner copy and every unmatched name falls into one empty-`channel` bucket
  instead of standing alone (verified against `:9091`, 2026-08-15). Because the inner call keeps
  unmatched names, the groups are not channels-only, and the figure below counts BUCKETS, not
  rows: all four targets are gated on `topk(25, ...)`, so the table draws 25 rows out of however
  many groups reported. Measured on `:3020`, host clock 2026-08-17 04:48 +03:00, the query
  bucketed 493 reporting sources into 108 buckets: 61 folded channels; `tg-inline`
  (`crawl.go:771`) and the legacy `tg-96c4d7c7a7` (`legacyNameRe`, `crawl.go:65`), neither of
  which folds anything nor is a channel; and 45 names on the curated side of `source!~"tg-.*"`,
  all of them `config/sources.yaml` entries because `commsub` reported no series on that check.
  The 25 it drew held 18 channels beside 7 curated names. That is why this table keeps the `tg-`
  prefix where panel 8 strips it: panel 8's query fixes the owner, so there the prefix is
  redundant, and panel 22 has nothing to strip because its rows never carry the prefix, whereas
  here the prefix is the ONLY thing separating a folded channel from a curated name. The column
  is therefore headed `group`, display-only via `renameByName`; the field name and the PromQL
  label both stay `channel`, which is why `indexByName` still keys on `channel` and a query
  copied out of this panel works verbatim. For how many sources are configured, read
  `stable_sources_total` — panel 3, top of the same dashboard — rather than counting a config
  file; it is published per cycle, so it trails a config edit by a cycle.
- **The crawler-managed vs hand-curated split is a NAME PREFIX, and its `published` column
  measures ATTRIBUTION, not contribution.** The discriminator is `source=~"tg-.*"` for
  crawler-managed and `source!~"tg-.*"` for hand-curated, and it is the only one that exists:
  `source` is the SOLE label on every per-source series (`labelSource`,
  `internal/metrics/metrics.go:44`, sampled at :287, :291, :295, :299 and — with `reason` — :312),
  so no origin, channel or url field ever reaches Prometheus and a query written against one
  matches nothing. The prefix is `managedPrefix` (`internal/crawl/crawl.go:36`), whose comment
  fixes the axis as OWNERSHIP rather than provenance: "marks sources this crawler owns. Sources
  without it (hand-added private subscriptions) are never touched." So do not read it as
  "telegram": `tg-inline` (`crawl.go:771`) is crawler-managed but is harvested raw node URIs
  rather than a channel, and the legacy `^tg-[0-9a-f]{10}$` form (`legacyNameRe`, `crawl.go:65`)
  is managed too — measured on `:9091` 2026-08-16 12:45 +03:00, the managed side was 439 names
  reporting series: 437 channel-attributed, 1 `tg-inline` and 1 legacy (`tg-96c4d7c7a7`). Nor is
  it a file boundary: `private.yaml` held 447 entries at 2026-08-16 12:15 +03:00, one of them
  (`commsub`) hand-added without the prefix, and it lands on the curated side.
  Read the funnel across that split and the crawler looks worthless — 439 managed sources against
  45 curated, `valid` 227878 vs 57121, `published` 83 vs 231 (`:9091`, 2026-08-16 12:45 +03:00).
  **That conclusion does not follow**, for the attribution reason the panel 8 / 22 bullet above
  measures: the curated overlay is merged first (`config.go:985` before :1000) and `Merge` dedupes
  first-wins (`merge.go:79`), so a curated source claims every shared node before any quality
  judgement, and the floor measured on that cycle is 16 published nodes held by a curated source
  that `tg-inline` also yields. Breadth says it from the other side: 29 managed sources published
  at least one node against 18 curated ones at the same instant, so the curated lead is volume per
  source, not reach. `tested` inverts one column earlier too (139 managed vs 493 curated), for the
  same reason — it is post-merge, so it is not the probe killing crawler nodes either.
  Therefore these series CANNOT answer "what would we lose without the crawler". They are
  post-attribution by construction, while the question is about nodes NO curated source yields —
  which nothing exported distinguishes from nodes a curated source merely claimed first.
  Answering it needs the counterfactual: a cycle merged with the crawler overlay absent, or a
  per-node record of every source that also yielded each published node. Neither exists today;
  name that gap rather than squeezing an answer out of these numbers.
  Three traps when querying the split. `count()` over a per-source series counts sources that
  RETURNED A BODY, not sources configured — 439 + 45 = 484 = `stable_sources_ok`, against
  `stable_sources_total` 493 (`:9091`, 2026-08-16 12:45 +03:00), so the 9 fetch failures of that
  cycle sit on NEITHER side and their ownership is unknowable from Prometheus.
  `job="sub-preprocessor-vassago"` has no managed series at all (52 sources, all hand-curated),
  so there `source=~"tg-.*"` is EMPTY rather than 0 and a panel scoped to it reads "No data"
  correctly. And never divide the two columns into a survival rate: `valid` is counted per source
  BEFORE `Merge` and double-counts a node two sources both yield, whereas `published` is
  post-attribution. Panel 21 (Ownership split: crawler-managed vs hand-curated (last cycle)) sits
  full-width at the row below panels 8 and 22, summarising the pair it follows, and renders as a
  table because its five numeric columns spanned 45 to 263500 at that instant and a shared axis
  would flatten `filtered` to zero. It quotes no digits of its own, derives `owner` with the same
  nested `label_replace` pair panel 20 uses for `channel` — so both of those calls are
  load-bearing here too — and keeps panels 8, 22 and 20's column vocabulary, in which `filtered`
  IS `stable_source_published_nodes`.
- `flake.nix` output `nixosModules.monitoring` (`deploy/monitoring.nix`) = the
  Prometheus scrape jobs + the Grafana dashboard provider
  (`deploy/grafana/sub-preprocessor.json`; datasource picked via a template
  variable, so no fixed uid). `nixosModules.default` is the separate systemd-service
  module — leave it.
- **Two instances, two scrape JOBS — not two targets in one job.** `sub-preprocessor`
  (`127.0.0.1:9091`) and `sub-preprocessor-vassago` (`127.0.0.1:9092`) each carry their
  own `job_name`, because the dashboard's Instance picker is
  `label_values(stable_cycles_total, job)` and every panel expression is scoped
  `{job="$job"}`. Sharing a job leaves the two deployments unselectable AND silently
  sums their funnels. Two consequences bind any dashboard edit: a panel added later
  MUST carry that selector, and a panel description MUST NOT name one config file as
  the authority for what ran — which filters exist depends on which instance `$job`
  selects.

**Editing the dashboard** — source of truth is `deploy/grafana/sub-preprocessor.json`
(provisioned `editable: false`; validate with `jq`, ideally render against a throwaway
Grafana+Prometheus first):

```bash
# here:
$EDITOR deploy/grafana/sub-preprocessor.json && git commit -am '...' && git push
# in the nixos repo (server imports inputs.sub-preprocessor.nixosModules.monitoring):
nix flake update sub-preprocessor && make switch
```
