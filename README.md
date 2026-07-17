# sub-preprocessor

An HTTP preprocessor for Mihomo / Clash.Meta proxy subscriptions.

It takes raw proxy subscription lists (public collectors, Telegram channels,
your own sources), filters nodes by the country their IP resolves to, probes
them for liveness, latency, bandwidth, and real-world reachability of
geo-fenced services, and serves clean Mihomo-compatible output. The goal is to
feed a router's Mihomo instance a subscription that only contains nodes worth
routing through — dead, slow, and geo-blocked nodes removed.

## Why

Free proxy subscriptions are noisy: they mix exit countries, carry unreachable
nodes, and change constantly. A router pointing Mihomo directly at such a list
gets unpredictable routing. This service sits between the raw sources and the
router and does the filtering once, centrally, so the router just fetches a
ready-to-use list over HTTP.

## Endpoints

| Endpoint | What it does |
|---|---|
| `GET /` | On-demand filter: fetch one subscription URL, geo-filter it, return the result |
| `GET /stable.txt` | The curated stable list maintained by the background worker |
| `GET /healthz` | Returns `ok` |
| `GET /metrics` | Prometheus exposition, on a **separate internal listener** (`server.metrics_listen`, default `:9090`) |

Node parsing is scheme-generic: any `scheme://` URI line is parsed (`vless`,
`vmess`, `trojan`, `ss`, `hysteria2`, `tuic`, …), with `vmess` base64-JSON
additionally decoded so its name (`ps`) can be rewritten. Only the crawler's
classifier restricts itself to a fixed proxy-scheme list, to reject pages
full of ordinary `https://` links.

### 1. On-demand filter — `GET /`

Filter a single subscription URL at request time by exit country:

```bash
curl "http://127.0.0.1:8080/?subscription_url=https://example.com/sub&countries=FI,EE,SE,DE,NL"
curl "http://127.0.0.1:8080/?subscription_url=https://example.com/sub&groups=nordics"
curl "http://127.0.0.1:8080/?subscription_url=https://example.com/sub&exclude_groups=geo_blocked"
```

Query params:

- `subscription_url` (required) — the upstream list to fetch (https only, SSRF-protected).
- `countries` — comma-separated allow-list of exit countries, and/or
- `groups` — comma-separated names referencing `groups` in `config.yaml`;
- `exclude_countries` / `exclude_groups` — subtract from the allow-list.

If only exclusion params are given, the allowed set starts from *all*
countries minus the exclusions. If the resulting set is empty, the request
fails with `400`.

The response is `text/plain` Mihomo-compatible text; node names are annotated
according to the `annotate` config (default `[GEO:XX][IP:a.b.c.d] <name>`).
Stats come back in the `X-Preprocessor-Stats` header. This path does **no**
liveness probing — only IP-stage filtering (see below).

### 2. Stable subscriptions worker — `GET /stable.txt`

A background worker maintains one curated list from all configured sources.
Every `subscriptions.interval` it:

1. fetches every source in `subscriptions.sources` concurrently (a source is
   either a `url` or an inline base64 `body`, e.g. the crawler's `tg-inline`
   harvest),
2. runs each through the same IP-stage filter pipeline as `/`,
3. merges and dedupes nodes by lowercased `server:port` (first source wins,
   config order),
4. relabels each kept node to `<source>-NNN`,
5. skips nodes recently proven dead (in-memory dead cache, `deadcache.ttl`),
6. probes the rest with an embedded **Mihomo URL test** (HEAD requests through
   each node, `check.rounds` rounds, one shared concurrency semaphore),
7. keeps nodes within `check.max_fail` / `check.max_avg_ms`, sorted by mean
   latency; nodes with zero successful rounds are recorded in the dead cache,
8. runs the configured **through-node filters** (`gemini` / `claude` /
   `bandwidth`) on the survivors,
9. atomically publishes the result.

`GET /stable.txt` serves the current list as `text/plain` (or
`503 stable list not ready` until the first cycle completes) with an
`X-Stable-Stats` header
(`updated=<RFC3339> sources=<ok>/<total> merged=<n> tested=<n> kept=<n>`).
A failed cycle keeps the last good list, so the router never gets an empty
response.

## The filter pipeline

All filtering is configured as one ordered `filters:` list. Entries fall into
two stages:

**IP-stage filters** — run per node on **both** `/` and the stable worker,
after DNS resolution, before any probing:

- `country` — keep nodes whose IP's country is in the allowed set. The
  IP→country source is selectable per filter: `provider: geofeed` (CSV
  geofeed sources, in-memory indexed lookup) or `provider: asn` (Team Cymru
  DNS). `exclude_groups` / `exclude_countries` shrink the worker's allowed
  set; on `/` the allowed set comes from query params.
- `asn` — drop nodes whose AS name matches `deny_patterns` (regexps), and
  nodes whose Cymru-resolved country is not allowed.

Before any of that, nodes whose host is in the **geoblock store** (see below)
are dropped outright — on both endpoints, before DNS even runs.

**Through-node filters** — run only in the stable worker, after the latency
probe, routing real requests *through* each surviving node:

- `gemini` — GET the Gemini API through the node and inspect the response
  body for the location-block marker (a check a HEAD-only URL test cannot
  do). Blocked hosts are recorded in the geoblock store and dropped.
  Requires an API key (`geoblock.gemini.key_file` in agenix `KEY=VALUE`
  format, `key_var`, or inline `api_key`); without a key the filter is
  skipped.
- `claude` — same idea, keyless: the Anthropic endpoint answers 403
  `Request not allowed` from blocked regions. Also feeds the geoblock store.
- `bandwidth` — download `test_url` through the node and measure Mbps. Nodes
  below `min_mbps` (default 5; explicit `0` = no floor, annotate only) are
  dropped; kept nodes get a `[SPD:<n>M]` tag when annotation is enabled.
  Results are never cached — measured fresh each cycle.

Filter order within each stage is honoured; putting `bandwidth` last means it
runs on the fewest nodes.

### Annotation

The ordered `annotate:` list controls the tags prepended to node names on both
endpoints: `GEO` (`[GEO:XX]`, `[GEO:??]` when unknown), `IP`
(`[IP:1.2.3.4]`), `ASN` (`[ASN:...]`), each with a selectable provider
(`geofeed`/`asn`). An empty list disables annotation (original names pass
through). Rewriting is scheme-aware: URI schemes fold tags into the
`#fragment`, vmess into the base64 `ps` field. Known stale tags from upstream
are stripped first.

## Caches and stores

| Store | Kind | Purpose |
|---|---|---|
| geoblock (`geoblock.db_path`, `geoblock.ttl`, default 720h) | SQLite (pure-Go driver, `CGO_ENABLED=0`-safe), reads served from an in-memory cache | hosts that failed the Gemini/Claude reachability check; dropped pre-DNS on both endpoints |
| dead cache (`deadcache.ttl`, default 2h) | in-memory, not persisted | `server:port` of nodes with zero successful probe rounds; skipped before probing |
| DNS cache (`resolver.cache_ttl` / `cache_negative_ttl`) | in-memory TTL map, capped | node hostname resolution across cycles |
| ASN cache (`geo.asn.cache_ttl`, default 24h; 5m negative) | in-memory TTL map, capped | Team Cymru lookups |
| geofeed data (`geo.geofeed.refresh_interval`) | in-memory, refreshed in background | IP→country entries from configured CSV sources |

## Telegram crawler

The same binary has a `crawl` subcommand (run as the `tg-sub-crawler`
compose sidecar) that discovers new sources automatically:

- scrapes public Telegram channel web previews (`t.me/s/<channel>`, paginated),
- treats every https link as a candidate and keeps those that **classify** as
  a live subscription (proxy-scheme node count > 0, not expired),
- walks the channel repost graph (relevance-gated BFS: a discovered channel is
  expanded only if it itself yielded a live subscription; `CRAWL_DEPTH`
  bounds recursion),
- remembers productive channels in a JSON state file and re-seeds them on
  future cycles (pruned after `CRAWL_STATE_TTL` without a live sub),
- additionally harvests **raw proxy URIs pasted directly in messages**
  (`vless://…` etc.), dedupes them, and packs them into a single inline
  `tg-inline` source with a base64 `body`,
- writes results to `config/private.yaml` as `tg-<sha10>` sources — an
  overlay the service merges into `subscriptions.sources` and **hot-reloads**
  on change.

Seed channels live in `config/channels.yaml` (re-read every cycle). Schedule:
`CRAWL_INTERVAL` (default 30m) or daily `CRAWL_AT=HH:MM`; `CRAWL_RUN_ONCE=1`
for a single cycle; optional `CRAWL_HTTP` on-demand trigger listener.

There is also a one-shot `classify` subcommand:

```bash
sub-preprocessor classify https://example.com/sub   # exit 0 = live (prints node count), 1 = not, 2 = usage
```

## Configuration

Everything is driven by `config/config.yaml` plus two overlay siblings merged
into it on load: `config/sources.yaml` (curated subscription sources kept out
of the main file) and `config/private.yaml` (crawler-managed sources). All
three are watched and **hot-reloaded** on
change; on any reload error the previous settings stay active. Changing
`server.listen`, `geoblock.db_path`/`ttl`, or `deadcache.ttl` requires a
restart (logged as a warning); a `server.metrics_listen` change is silently
ignored on reload (the metrics server starts once). Everything else —
filters, annotate, groups,
sources, prober knobs, log level — applies live. The stable worker is
restarted only when its inputs actually changed, and the new worker is built
*before* the old one stops.

Key sections:

- `log.level` — zerolog level, hot-reloadable.
- `server.listen` / `server.metrics_listen` — public HTTP and internal
  Prometheus listeners.
- `geo.geofeed.sources[]` (`url` + explicit `type: raw|gzip`) +
  `refresh_interval`; `geo.asn.timeout` / `cache_ttl` — shared geo providers.
- `resolver.timeout` / `cache_ttl` / `cache_negative_ttl`.
- `filters` — the ordered filter list described above.
- `annotate` — the ordered tag list described above.
- `geoblock` — store path/TTL plus `gemini.*` and `claude.*` base params
  (endpoint, model, marker, key, timeout, concurrency) for the through-node
  filters.
- `deadcache.ttl`, `fetch.timeout` (per-subscription fetch deadline).
- `groups` — named country sets referenced by requests and `exclude_groups`.
- `subscriptions` — `interval`, `sources[]` (`name` + `url` *or* inline
  `body`), and `check.*`: URL-test prober params only (`rounds`, `timeout`,
  `max_fail`, `max_avg_ms`, `concurrency`, `source_timeout`, `test_url`,
  `expected_status` in mihomo IntRanges syntax).

## Security

`subscription_url` is untrusted input. The fetcher enforces https-only,
rejects URL userinfo, and disables env proxies; the SSRF IP policy lives in
the HTTP client's **dialer** — resolved non-public IPs (private, loopback,
link-local, CGN, benchmarking, class-E) are refused at dial time, so DNS
tricks can't bypass the check. Do not reintroduce implicit proxy support
without redesigning that validation. The only unrestricted client belongs to
the crawler (blind SSRF: nothing is reflected to a user, and it needs a local
fake-ip tunnel to reach `t.me`). The through-node probe URLs egress through
the proxy nodes, so host-side SSRF rules deliberately don't apply to them.

## Observability

The stable worker reports every cycle to `internal/metrics`, which renders
hand-rolled Prometheus text exposition (no `client_golang` — the
`protobuf => metacubex/protobuf-go` replace in `go.mod` makes it risky):
cycle funnel (`stable_merged_nodes`, `stable_probed_nodes`,
`stable_kept_nodes`, `stable_dead_skipped_nodes`), per-source and per-filter
in/kept/dropped-by-reason counters, kept-node speed histogram, cycle duration,
success timestamp, and cycle/failure totals.

`deploy/grafana/sub-preprocessor.json` is the provisioned Grafana dashboard;
`flake.nix` exports `nixosModules.monitoring` (Prometheus scrape job +
dashboard provisioning) for the NixOS host to import, and
`nixosModules.default` (systemd service module). The dashboard lives in this
repo so it tracks the metric names — change a metric, update the dashboard in
the same commit.

## Running

The toolchain is pinned in `shell.nix`; run everything through `nix-shell`:

```bash
nix-shell --run "make"       # build + run
nix-shell --run "make test"
nix-shell --run "make race"
nix-shell --run "make fmt"
nix-shell --run "make lint"
nix-shell --run "make bench" # saves output to ./benchmarks/
```

## Deployment

Docker Compose runs the service plus the crawler sidecar from one image:

```bash
docker compose up -d --build   # or: make dc-up
```

- `sub-preprocessor` — the HTTP service (published on `:7008`) and the stable
  worker. Metrics are published loopback-only (`127.0.0.1:9091:9090`) for the
  host Prometheus — keep them non-public. The Gemini API key is read from an
  agenix-decrypted secret mounted at `/run/agenix/litellm-env`.
- `tg-sub-crawler` — the crawler (`command: ["crawl"]`), sharing the
  `./config` volume so its `private.yaml` writes hot-reload the service.

## Package map

See [`routes.md`](./routes.md) for a per-package reference (types, functions,
dependency graph). Agent-facing conventions live in [`AGENTS.md`](./AGENTS.md).
