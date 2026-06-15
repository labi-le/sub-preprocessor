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

Current behavior in one line:

1. accepts `subscription_url` + `countries` (or `groups` referencing `config.groups`) via HTTP query params
2. downloads subscription text
3. parses generic URI-style nodes (not VLESS-only anymore)
4. resolves node hostnames
5. geofilters by IP country from geofeed sources
6. rewrites node fragment/name with `[GEO:XX][IP:x.x.x.x] ...`
7. returns raw Mihomo-compatible text/plain subscription body

## Important current design decisions

- Parsing is **generic URI parsing**, not hardcoded to `vless://` only.
- Filtering logic only cares about hostname/IP and final geofeed country.
- Output rewriting is still **scheme-aware/safe**: it only rewrites parsed URI nodes.
- TCP health-check and old dead-IP cache were intentionally removed from the current design.
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

`config.yaml` currently contains only server/geofeed/resolver settings.

Subscription URL and allowed countries are **not** stored in YAML; they come from HTTP query params.

Important keys:

- `server.listen`
- `geofeed.refresh_interval`
- `geofeed.sources[].url`
- `geofeed.sources[].type` (`raw` or `gzip`)
- `resolver.timeout`
- `resolver.strict_dns`

## Important package map

- `main.go` — root entrypoint
- `internal/app` — app bootstrap, config load, service construction, server start
- `internal/config` — YAML config parsing/validation
- `internal/fetch` — safe HTTP fetching, file-type decoding, SSRF protections
- `internal/geofeed` — geofeed download/parse/lookup
- `internal/subscription` — subscription fetch/normalize/parse
- `internal/preprocess` — core filtering and fragment rewrite logic
- `internal/server` — Fiber HTTP layer

## API behavior to remember

- `GET /healthz` returns `ok`
- `GET /` requires:
  - `subscription_url`
  - `countries` (comma-separated) OR `groups` (comma-separated, referencing `config.groups`)
- Response is `text/plain; charset=utf-8`
- Stats are returned in `X-Preprocessor-Stats`

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
