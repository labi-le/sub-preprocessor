# sub-preprocessor

An HTTP preprocessor for Mihomo / Clash.Meta proxy subscriptions.

It takes raw proxy subscription lists (from public collectors or your own),
filters the nodes by the country their IP resolves to, and serves clean
Mihomo-compatible output. The goal is to feed a router's Mihomo instance a
subscription that only contains nodes from countries you actually want to exit
through — with dead and geo-blocked nodes removed.

## Why

Free proxy subscriptions are noisy: they mix exit countries, carry unreachable
nodes, and change constantly. A router pointing Mihomo directly at such a list
gets unpredictable routing. This service sits between the raw sources and the
router and does the filtering once, centrally, so the router just fetches a
ready-to-use list over HTTP.

## Two ways to use it

### 1. On-demand filter — `GET /`

Filter a single subscription URL at request time by exit country.

```bash
curl "http://127.0.0.1:8080/?subscription_url=https://example.com/sub&countries=FI,EE,SE,DE,NL"
curl "http://127.0.0.1:8080/?subscription_url=https://example.com/sub&groups=nordics"
```

Query params:

- `subscription_url` (required) — the upstream list to fetch (https only, SSRF-protected).
- `countries` — comma-separated allow-list of exit countries, or
- `groups` — comma-separated names referencing `groups` in `config.yaml`, and
- `exclude_countries` / `exclude_groups` — subtract from the allow-list.

The response is `text/plain` Mihomo-compatible text; each node name is rewritten
to `[GEO:XX][IP:a.b.c.d] <original name>`. Stats come back in the
`X-Preprocessor-Stats` header.

### 2. Stable subscriptions worker — `GET /stable.txt`

A background worker maintains one curated "stable" list from many configured
sources. On each cycle it:

1. fetches every source in `subscriptions.sources`,
2. geo-filters each through the same pipeline as `/` (dropping excluded countries),
3. merges and dedupes nodes by `server:port` (first source wins),
4. relabels each kept node to `<source>-NNN`,
5. probes every node with an embedded Mihomo URL test (real latency check),
6. keeps only nodes that pass all rounds under the latency threshold,
7. atomically swaps in the new list.

`GET /stable.txt` serves the current list as `text/plain` (or `503` until the
first cycle completes) with an `X-Stable-Stats` header
(`updated=… sources=ok/total merged=… tested=… kept=…`). It keeps the last good
list if a cycle fails, so the router never gets an empty response.

Supported node schemes include `vless`, `vmess`, `trojan`, `ss` (SIP002),
`hysteria2`, and `tuic`.

`GET /healthz` returns `ok`.

## Configuration

Everything is driven by `config/config.yaml`, hot-reloaded on change:

- `server.listen` — HTTP listen address.
- `geofeed.sources[]` + `refresh_interval` — IP→country data (in-memory, refreshed).
- `resolver.timeout`, `resolver.cache_ttl`, `resolver.cache_negative_ttl` — DNS
  resolution and its TTL cache (so cycles don't hammer the upstream resolver).
- `asn.deny_patterns` — ASN name patterns whose nodes are dropped.
- `workflow.stages` — order of filter stages (`asn`, `geofeed`).
- `groups` — named country sets referenced by requests and `exclude_groups`.
- `subscriptions` — the stable worker: `interval`, `exclude_groups`,
  `sources[]`, and `check` knobs (`rounds`, `timeout`, `max_fail`, `max_avg_ms`,
  `test_url`, `concurrency`).

## Security

`subscription_url` is untrusted input. The fetcher enforces https-only, rejects
URL userinfo, rejects private/loopback targets, and disables env proxies —
guarding against SSRF. Do not reintroduce implicit proxy support without
redesigning that validation.

## Running

The toolchain is pinned in `shell.nix`; run everything through `nix-shell`:

```bash
nix-shell --run "make"       # build + run
nix-shell --run "make test"
nix-shell --run "make race"
nix-shell --run "make fmt"
nix-shell --run "make lint"
```

## Deployment

Docker Compose runs the service plus the crawler sidecar:

```bash
docker compose up -d --build
```

- `sub-preprocessor` — the HTTP service (`:7008`) and the stable worker. The
  worker probes nodes and, via the `gemini` node-filter (`subscriptions.check.filters`),
  keeps only nodes that can reach the Gemini API through the node, recording
  geo-blocked hosts in a SQLite TTL store (`geoblock`). The Gemini API key is
  read from an agenix-decrypted secret mounted at `/run/agenix/litellm-env`.
- `tg-sub-crawler` — a sidecar that nightly crawls Telegram channels
  (`config/channels.yaml`) and appends discovered live subscriptions to
  `config/private.yaml`, which the service hot-reloads.

## Package map

See [`routes.md`](./routes.md) for a per-package reference (types, functions,
dependency graph). Agent-facing conventions live in [`AGENTS.md`](./AGENTS.md).
