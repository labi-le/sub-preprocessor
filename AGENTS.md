# Agent Instructions

## Running commands

Always run project commands via `nix-shell` — the toolchain (Go version, linter, etc.)
is defined in `shell.nix`. Running tools directly may use different versions or fail.

Prefer Makefile targets for common flows:

```bash
nix-shell --run "make"
nix-shell --run "make test"
nix-shell --run "make fmt"
nix-shell --run "make race"
nix-shell --run "make bench"
```

## Code conventions

- **Comments earn their place.** Write one only for what the code cannot say itself: the *why* (rationale, tradeoffs), non-obvious invariants, ordering/locking/concurrency rules, units, edge-case semantics, gotchas, or external behavior (mihomo quirks, SSRF, protocol details).
- **Never restate the code.** Delete doc blocks that only echo the name or signature (`// name returns the name`, `// NewX creates a new X`, `// Close closes it`) or narrate the next line. Self-explanatory code gets no comment.
- **A stale comment is worse than none.** Changing behavior means updating the comment or deleting it — never leave it describing the old world.
- Removing an obvious doc comment is lint-safe: `.golangci.yml` excludes revive's "exported … should have comment", and `godot` needs no trailing period. Comments that remain must still start with the symbol's name (`godoc`/`staticcheck`).

## Project overview for LLM agents

Before making any changes, read `./routes.md` — it describes every package in the
project, its key types/functions, tags, and the dependency graph. This gives an
LLM a complete orientation without browsing the full source.

After adding, removing, or significantly restructuring a package — or changing
a package's public API (key types, constructors, interfaces) — update
`./routes.md` to reflect the new state.

## What this project is

This is a small HTTP preprocessor for Mihomo-compatible subscription content.
It exposes two modes.

**On-demand filter (`GET /`)** — filter one subscription URL at request time:

1. accepts `subscription_url` + `countries` (or `groups` referencing `config.groups`) via HTTP query params
2. downloads subscription text
3. parses generic URI-style nodes (not VLESS-only; `vmess://` base64-JSON is decoded too)
4. resolves node hostnames
5. geofilters by IP country from geofeed sources
6. rewrites node fragment/name with `[GEO:XX][IP:x.x.x.x] ...`
7. returns raw Mihomo-compatible text/plain subscription body

**Stable subscriptions worker (`GET /stable.txt`)** — a background worker keeps
one curated list built from all `subscriptions.sources`. Each cycle fetches every
source, runs it through the same geo/ASN filter, merges and dedupes by
`server:port` (first source wins), relabels kept nodes to `<source>-NNN`, probes
every node with an embedded Mihomo URL test, and keeps only those that pass all
rounds under the latency threshold. The result is swapped in atomically; the last
good list is kept if a cycle fails (`503` only until the first cycle completes).

## Important current design decisions

- Parsing is **generic URI parsing**, not hardcoded to `vless://` only.
- Filtering logic only cares about hostname/IP and final geofeed country.
- Output rewriting is still **scheme-aware/safe**: it only rewrites parsed URI nodes.
- The on-demand `/` path does no liveness probing; it only geo/ASN-filters. The `/stable.txt` worker is the only place that probes nodes (embedded Mihomo URL test).
- The resolver keeps an in-memory DNS TTL cache (`resolver.cache_ttl` / `resolver.cache_negative_ttl`) so repeated stable cycles don't hammer the upstream DNS.
- Geofeed sources are explicit in YAML via `geofeed.sources[].url` + `geofeed.sources[].type`.
- File type is explicit only: `raw` or `gzip`. There is no auto-detection/legacy mode.
- Geofeed data is cached in memory and reloaded by `geofeed.refresh_interval`.

## Important security / correctness notes

- `subscription_url` is user input and must stay protected against SSRF.
- Fetching uses a safe HTTP client:
  - only `https` URLs are allowed
  - userinfo in URL is rejected
  - private/local targets are rejected
  - env proxy usage is disabled (`Transport.Proxy = nil`) to avoid SSRF bypass via proxy
- Do not reintroduce implicit proxy support unless SSRF validation is redesigned.
- Request context is passed explicitly through the stack. Prefer `ctx context.Context` as the first argument.
- Root `main.go` is the only normal place where `context.Background()` should be introduced.

## Current config shape

`config/config.yaml` is hot-reloaded on change, together with its overlay
siblings `config/sources.yaml` (curated sources) and `config/private.yaml`
(crawler-managed sources) — both append to `subscriptions.sources`. The `/`
request still takes its `subscription_url` and allowed countries from HTTP
query params; the `subscriptions` block only configures the `/stable.txt`
worker.

Important keys:

- `server.listen`
- `server.metrics_listen` — internal Prometheus `/metrics` endpoint (default `:9090`; docker-compose publishes it loopback-only on `127.0.0.1:9091`, never public)
- `geo.geofeed.refresh_interval` / `geo.geofeed.sources[].url` / `geo.geofeed.sources[].type` (`raw` or `gzip`)
- `geo.dbip.url` / `geo.dbip.refresh_interval` — DB-IP Country Lite monthly gzip CSV (`{yyyy-mm}`-templated URL, default built-in, 24h refresh); in-memory IP→country DB for the `dbip` annotate provider, built only when an annotate chain references it
- `geo.registry.urls[]` / `geo.registry.refresh_interval` — the five RIR delegated-extended files (defaults built-in, 24h refresh); in-memory registration-country DB for the `registry` annotate provider, built only when referenced
- `geo.asn.timeout` / `geo.asn.cache_ttl` — Team-Cymru ASN lookups, in-memory TTL cache (default 24h)
- `resolver.timeout`
- `resolver.cache_ttl` / `resolver.cache_negative_ttl` (DNS TTL cache)
- `filters` — ONE ordered list for both stages. IP-stage entries (`type: country` with `provider: geofeed|asn` + `exclude_groups`/`exclude_countries`; `type: asn` with `deny_patterns`) run per node in preprocess on both `/` and the worker; through-node entries (`type: gemini`/`claude`/`bandwidth`) run post-probe in the stable worker only.
- `annotate` — ordered tag list (`tag: GEO|IP|ASN`; GEO/ASN take `providers:`, an ordered chain of `geofeed|dbip|registry|asn` — first provider that resolves wins, all-miss renders `??`; IP takes no providers) prepended to node names on both `/` and `/stable.txt`; empty list disables annotation. The retired singular `provider:` key is rejected at load with a rename error.
- `geoblock.db_path` / `geoblock.ttl` (SQLite per-host geo-block list; default TTL 720h)
- `geoblock.gemini.*` / `geoblock.claude.*` (`endpoint`, `model`, `marker`, `api_key`/`key_file`/`key_var`, `timeout`, `concurrency`) — base params for the `gemini`/`claude` node-filters; enabled by listing `{type: gemini}` / `{type: claude}` in `filters` (a filter entry may override these per-field)
- `deadcache.ttl` (in-memory cache of probe-dead nodes keyed by `server:port`; default 2h; skips re-probing; not persisted)
- `groups.<name>` (country sets referenced by requests and `exclude_groups`)
- `subscriptions.interval`
- `subscriptions.sources[].name` + `url` *or* inline `body` (base64/raw node URIs; used by the crawler's `tg-inline` harvest)
- `subscriptions.check.*` (`rounds`, `timeout`, `max_fail`, `max_avg_ms`, `test_url`, `expected_status`, `concurrency`, `source_timeout`) — URL-test (latency) prober params ONLY; through-node filters and exclusions live in the top-level `filters` list
- `fetch.timeout` — per-subscription fetch deadline (default 3s)
- `log.level` — zerolog level, hot-reloadable

## Important package map

- `main.go` — root entrypoint
- `internal/app` — app bootstrap, config load, service construction, server start
- `internal/config` — YAML config parsing/validation
- `internal/fetch` — safe HTTP fetching, file-type decoding, SSRF protections
- `internal/geofeed` — geofeed download/parse/lookup
- `internal/resolver` — DNS resolution with an in-memory TTL cache
- `internal/asn` — ASN lookup (Team Cymru) for the ASN name/country filter
- `internal/filter` — country allow/deny bitset
- `internal/subscription` — subscription fetch/normalize/parse (incl. `vmess://` decode)
- `internal/rewrite` — node name/fragment rewrite (`[GEO][IP]`, vmess `ps` rewrite)
- `internal/preprocess` — the core per-node filter pipeline
- `internal/geoblock` — SQLite TTL list of node hosts that failed the Gemini reachability check
- `internal/stable` — `/stable.txt` worker: merge/dedupe/relabel, dead-node cache skip (pre-probe), Mihomo prober + through-node Gemini reachability gate, checker loop, holder
- `internal/reload` — config file watcher + hot-reload
- `internal/server` — Fiber HTTP layer
- `internal/metrics` — renders stable-cycle stats as hand-rolled Prometheus text exposition; served on `server.metrics_listen`

## API behavior to remember

- `GET /healthz` returns `ok`
- `GET /` requires:
  - `subscription_url`
  - `countries` (comma-separated) OR `groups` (comma-separated, referencing `config.groups`)
  - optional `exclude_countries` / `exclude_groups` subtract from the allow-list
- `GET /stable.txt` serves the worker's current list; `503` until the first cycle completes. Stats are returned in `X-Stable-Stats` (`updated=… sources=ok/total merged=… tested=… kept=…`)
- Response is `text/plain; charset=utf-8`
- `/` stats are returned in `X-Preprocessor-Stats`

Example:

```bash
curl "http://127.0.0.1:8080/?subscription_url=https://mifa.world/vless&countries=FI,EE,LV,LT,SE,PL,DE,NL"
curl "http://127.0.0.1:8080/?subscription_url=https://mifa.world/vless&groups=nordics,euronorth"
```

## Monitoring / metrics (Prometheus + Grafana)

The stable worker exports per-cycle stats as Prometheus metrics, visualized by a
Grafana dashboard. **The dashboard AND its NixOS wiring live in this repo** so they
track the metric names; the NixOS host pulls them in as a flake input — do NOT
vendor the dashboard into the nixos repo.

- `internal/metrics` renders `stable.CycleReport` as hand-rolled Prometheus text
  exposition (deliberately no `client_golang` — the `google.golang.org/protobuf =>
  metacubex/protobuf-go` replace in `go.mod` makes it risky). Served on an internal
  listener (`server.metrics_listen`, default `:9090`); `docker-compose.yaml` publishes
  it loopback-only (`127.0.0.1:9091:9090`) — keep it non-public.
- Data flows via the nil-safe `stable.Reporter`: `RunOnce` hands a `CycleReport`
  (per-source drops, per-filter in/kept/dropped-by-reason, kept speeds, cycle
  aggregate + duration) to `metrics.Metrics.Observe` on a published cycle, and
  `ObserveError()` on any abort. **Adding/renaming a metric? Update
  `deploy/grafana/sub-preprocessor.json` in the same commit.**
- `flake.nix` output `nixosModules.monitoring` (`deploy/monitoring.nix`) = the
  Prometheus scrape job (`127.0.0.1:9091`) + the Grafana dashboard provider
  (`deploy/grafana/sub-preprocessor.json`; datasource picked via a template
  variable, so no fixed uid). `nixosModules.default` is the separate systemd-service
  module — leave it.

**Editing the dashboard** — source of truth is `deploy/grafana/sub-preprocessor.json`
(provisioned `editable: false`; validate with `jq`, ideally render against a throwaway
Grafana+Prometheus first):

```bash
# here:
$EDITOR deploy/grafana/sub-preprocessor.json && git commit -am '...' && git push
# in the nixos repo (server imports inputs.sub-preprocessor.nixosModules.monitoring):
nix flake update sub-preprocessor && make switch
```

## Bench / performance notes

- Benchmark results are stored in `./benchmarks/bench-<UTC timestamp>.txt`
- Baseline/optimization notes live in `BENCHMARK_OPTIMIZATION_PLAN.md`
- Recent optimization work improved:
  - geofeed parsing allocations
  - fragment rewrite allocations
  - inner filter hot-path allocations
  - skipping non-URI lines during subscription parse
- Still-hot areas worth revisiting before large refactors:
  - `internal/subscription.Parse`
  - `internal/filter.ParseAllowed`
  - IPv6 support in resolver/filter path is still incomplete / not yet generalized

## Project layout

- `main.go` — entry point
- `config.yaml` — application configuration
- `Makefile` — common targets (`run`, `test`, `fmt`, `race`, `bench`)
- `.golangci.yml` — linter configuration
- `internal/` — internal packages
- `benchmarks/` — timestamped benchmark output snapshots
