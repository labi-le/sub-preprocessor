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

`config.yaml` is hot-reloaded on change. The `/` request still takes its
`subscription_url` and allowed countries from HTTP query params; the
`subscriptions` block only configures the `/stable.txt` worker.

Important keys:

- `server.listen`
- `geofeed.refresh_interval`
- `geofeed.sources[].url`
- `geofeed.sources[].type` (`raw` or `gzip`)
- `resolver.timeout`
- `resolver.cache_ttl` / `resolver.cache_negative_ttl` (DNS TTL cache)
- `workflow.stages` (order of IP-filter stages: `asn`, `geofeed`) + `workflow.annotate` (default true — add `[GEO:XX][IP:]` to node names on both `/` and `/stable.txt`)
- `asn.deny_patterns` (+ `asn.timeout`, `asn.cache_ttl`) — usually empty now; the per-host `geoblock` list replaces ASN-name denial. The ASN stage still does country filtering, backed by an in-memory Cymru TTL cache (`asn.cache_ttl`, default 24h).
- `geoblock.db_path` / `geoblock.ttl` (SQLite per-host geo-block list; default TTL 720h)
- `geoblock.gemini.*` (`model`, `marker`, `key_file`, `key_var`, `timeout`, `concurrency`) — params for the `gemini` node-filter; enabled by listing `gemini` in `subscriptions.check.filters`
- `deadcache.ttl` (in-memory cache of probe-dead nodes keyed by `server:port`; default 2h; skips re-probing; not persisted)
- `groups.<name>` (country sets referenced by requests and `exclude_groups`)
- `subscriptions.interval`
- `subscriptions.exclude_groups`
- `subscriptions.sources[].name` / `subscriptions.sources[].url`
- `subscriptions.check.*` (`rounds`, `timeout`, `max_fail`, `max_avg_ms`, `test_url`, `concurrency`, `filters`) — `filters` lists Layer-2 through-node node-filters (e.g. `gemini`) run after latency selection

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
