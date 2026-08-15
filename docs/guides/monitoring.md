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
  results. The "Top sources" table (panel 8) renames the four columns
  `yielded`/`valid`/`kept`/`filtered`, so its `kept` column is `stable_source_tested_nodes` and
  its `filtered` column is `stable_source_published_nodes`, while the GLOBAL `stable_kept_nodes`
  means PUBLISHED. That collision is known and left standing: the global name is a wire format
  that panels and alerts already read, and renaming it to tidy a column label is a bigger break
  than the ambiguity.
- **The channel breakdown (panel 20) is a query, not a metric.** The exporter emits `source` and
  nothing else — Prometheus knows no `channel` label, `label_values(..., channel)` comes back
  empty and there are no recording rules — so panel 20 derives it inline, `label_replace`
  stripping the `-<sha6>` suffix off `source`. Nothing ships on the exporter side and no series
  cardinality is added: the dashboard JSON is the whole change. The cost lands on anyone writing
  their own query or alert, because a per-source series thresholds ONE URL — an alert that means
  a channel must carry the same `label_replace` pair itself, and both of its calls are
  load-bearing. The inner call copies `source` into `channel` verbatim; the outer overwrites that
  copy for names matching the slug form. Drop the inner copy and every unmatched name falls into
  one empty-`channel` bucket instead of standing alone (verified against `:9091`, 2026-08-15).
  For how many sources are configured, read `stable_sources_total` — panel 3, top of the same
  dashboard — rather than counting a config file; it is published per cycle, so it trails a
  config edit by a cycle.
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
