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
`trojan`, `hysteria2`, `tuic`, …) — there is deliberately no whitelist of
mihomo-known schemes. Four schemes need more than that generic walk, because
the fields the pipeline needs are not in the URI: `vmess` hides server, port
and name in a base64 payload, the legacy `ss` form hides server and port there
(its name stays in the `#fragment`), `ssr` hides all three — its display name
being a base64 `remarks` query value — and `mierus` carries its port list in
the query. Each decoder mirrors mihomo's own accept rule, so a
node kept here is a node the prober can convert. Portless `http`, `https`,
`socks`, `socks5` and `socks5h` lines are the one outright rejection: such a
proxy is `host:port` by definition and mihomo refuses it, so a bare
`https://t.me/somechannel` in a source body is now counted in `unsupported=`
(see `X-Preprocessor-Stats` below) instead of being published as a node. The
portful form is still a node — which is why the crawler's classifier keeps its
own fixed proxy-scheme list, to reject pages full of ordinary `https://` links.

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
- `exclude_countries` / `exclude_groups` — a deny-list of exit countries to drop.

`countries`/`groups` and the `exclude_*` params are enforced separately: the
first is an allow-list, the second a deny-list. A node whose exit IP no geo
source can place is in no excluded country, so an exclusion-only request keeps
it; an allow-list request drops it, because it is not in the list either. An
unknown group name or a country that is not a 2-letter code fails the request
with `400` naming the offending token. So does a request whose exclusions cover
every allowed country.

The response is `text/plain` Mihomo-compatible text; node names are annotated
according to the `annotate` config (shipped config: `[GEO:XX] <name>`).
Stats come back in the `X-Preprocessor-Stats` header. This path does **no**
liveness probing — only IP-stage filtering (see below).

One request is bounded on purpose. fasthttp cannot cancel a handler when the
client disconnects, so the pipeline runs under an explicit 60 s deadline
(`504` when it expires), and a body of more than 20 000 parseable nodes is
refused with `413` before a single DNS lookup — resolution is serial, so an
unbounded node list would otherwise occupy a goroutine for hours after the
caller left. A panic in the request path is recovered as a `500` (logged with
its stack) instead of taking the process down with the in-memory stable list.
The access log records the subscription URL as `host#<digest>`, never verbatim:
these links are capability URLs, and the token would otherwise land in
`docker logs`.

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
5. skips nodes a recent probe already proved dead (in-memory dead cache,
   `deadcache.ttl`),
6. probes the rest with an embedded **Mihomo URL test** (HEAD requests through
   each node, `check.rounds` rounds, one shared concurrency semaphore),
7. keeps nodes within `check.max_fail` / `check.max_avg_ms`, sorted by mean
   latency; nodes with zero successful rounds are recorded in the dead cache,
8. runs the configured **through-node filters** (`gemini` / `claude` /
   `chatgpt` / `tidal` / `bandwidth`) on the survivors, all of them gates; a
   `gemini`/`claude`/`chatgpt` geo-block writes the node's host to the geoblock
   store, so step 2 drops it on every later cycle. Every other drop lasts this
   cycle only,
9. asks the final survivor set where its traffic actually leaves from
   (Cloudflare's `/cdn-cgi/trace`), but only when the `annotate` chain names
   the `geotrace` provider — nothing is dropped here,
10. builds each node's tags **once**, from the address that survived the
    pipeline (the traced egress when there is one), and atomically publishes
    the result.

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

- `country` — keep nodes whose IP's country is allowed, drop those whose
  country is denied. `provider: geofeed` judges against the same in-memory
  database chain the `GEO` annotation resolves through (see
  [Annotation](#annotation)); `provider: asn` judges against a Team Cymru
  lookup instead. An IP no source can place has no country, so only an
  allow-list can drop it. `exclude_groups` / `exclude_countries` are
  **worker-only**: they build the `/stable.txt` deny-set and never reach the
  `/` chain, where the allowed and denied sets come from the query params
  alone.
- `asn` — drop nodes whose AS name matches `deny_patterns` (regexps), and
  nodes whose Cymru-resolved country is not allowed.

Before any of that, nodes whose host is in the **geoblock store** (see below)
are dropped outright — on both endpoints, before DNS even runs.

**Through-node filters** — run only in the stable worker, after the latency
probe, routing real requests *through* each surviving node. All but the last
are gates that drop:

- `gemini` — GET the Gemini API through the node and inspect the response
  body for the location-block marker (a check a HEAD-only URL test cannot
  do). Blocked hosts are recorded in the geoblock store and dropped.
  Requires an API key (`geoblock.gemini.key_file` in agenix `KEY=VALUE`
  format, `key_var`, or inline `api_key`); without a key the filter is
  skipped — and it cannot be made keyless. Measured from a geo-blocked
  egress, the API answers in this order: caller identity (403 `Method
  doesn't allow unregistered callers` when no key is sent), key validity
  (400 `API_KEY_INVALID` for a junk key), and only then the location
  precondition (400 `FAILED_PRECONDITION` + the marker) — so the verdict
  this gate reads is invisible to anything but a working credential. For
  the same reason a response that never reached the location check — key
  rotated or restricted, wrong `model` (404), quota (429) — is not read as
  "not blocked": those nodes are counted, warned about, and kept
  unverified.
- `claude` — same idea, keyless: the Anthropic endpoint answers 403
  `Request not allowed` from blocked regions. Also feeds the geoblock store.
- `chatgpt` — keyless too: OpenAI's compliance endpoint answers 403
  `unsupported_country` for an egress it refuses. Also feeds the geoblock
  store.
- `tidal` — keyless as well, and the only **fail-closed** gate: a node is kept
  only when `GET api.tidal.com/v1/country` came back `2xx`. Where Tidal refuses
  an egress the request never reaches the API — measured from a Russian egress,
  CloudFront answers `403` with an HTML error page — so there is no refusal
  marker to match and the status is the whole verdict (redirects are not
  followed, so a `3xx` interstitial is a refusal too). The response body is not
  read at all: the country it carries only says where Tidal *sells*
  subscriptions, not where an existing one streams. The tradeoff of judging by
  status alone: a node whose upstream answers `2xx` from something that is not
  Tidal (ISP block page, captive portal) counts as passed.
  It deliberately does **not** feed the geoblock store either: a bare status
  code is a weaker signal than the other checks' refusal markers, and the store
  is host-keyed for its whole TTL, so one CDN hiccup would evict the node from
  every endpoint.
- `bandwidth` — download `test_url` through the node and measure Mbps. Nodes
  below `min_mbps` (default 5; explicit `0` = no floor, annotate only) are
  dropped; a kept node's Mbps is recorded, and the `[SPD:<n>M]` tag it earns is
  rendered later, with every other tag, at publication.
  Results are never cached — measured fresh each cycle.

Filter order is honoured; putting the expensive one (`bandwidth`) last means it
runs on the fewest nodes. `geotrace` is not in this list — it is an annotate
provider, not a gate: see [Annotation](#annotation).

### Annotation

The ordered `annotate:` list controls the tags prepended to node names on both
endpoints: `GEO` (`[GEO:XX]`), `IP` (`[IP:1.2.3.4]`), `ASN` (`[ASN:...]`).
GEO and ASN entries take `providers:` — an **ordered lookup chain** (e.g.
`providers: [geotrace, geofeed, dbip, registry, asn]`): the first provider that
resolves the IP wins, and when every provider misses the tag renders as
`[GEO:??]` / `[ASN:??]`. An empty `annotate` list disables annotation
(original names pass through). Rewriting is scheme-aware: vmess folds tags into
the base64 `ps` field, `ssr` into the base64 `remarks` query value, every other
scheme into the `#fragment`. For `ssr` the fragment is not merely unused but
corrupting — mihomo base64-decodes everything after `ssr://`, an appended
`#name` included — so a payload neither rewriter can decode is published
verbatim: unannotated beats mangled. Known stale tags from upstream are
stripped first.

Available providers:

| Provider | Source | Character |
|---|---|---|
| `geotrace` | the node itself, via Cloudflare's `/cdn-cgi/trace` (`geoblock.geotrace.*`) | the only provider that reports the EXIT; worker-only |
| `geofeed` | RFC 8805 CSV feeds (`geo.geofeed.sources`) | precise, low coverage |
| `dbip` | DB-IP Country Lite — monthly gzip CSV; the `{yyyy-mm}` URL placeholder expands to the current UTC month, with one previous-month retry on a 404 right after rollover | broad coverage, in-memory |
| `registry` | the five RIR delegated-extended files | *registration* country of the allocated block, not necessarily where it routes |
| `asn` | Team Cymru DNS | cached last resort |

The `dbip`/`registry` databases are downloaded and indexed in memory only when
an annotate chain actually references them.

`geotrace` is the odd one out. Every other provider looks the node's *resolved*
address up in a table, and 41% of the named hosts measured in the pool sit in
Cloudflare's shared anycast ranges, which terminate in many countries at once —
so a node tagged `CA` was in fact exiting in Germany. Asking the node where its
traffic leaves from costs one request through it, which only the `/stable.txt`
worker's post-probe stage can spend: on `GET /` there is nothing to ask, so
`geotrace` always misses there and the chain falls through to the offline
providers. Naming it in a chain is what arms that probe; leaving it out means
no cycle pays for it. Only the address the endpoint saw is a fact — the country
beside it is still a geo-IP lookup, just one made about the right address.

The country **filter** (`provider: geofeed`) judges nodes with that same chain,
in the order the `GEO` entry's `providers:` list gives it: it consults every
local database that list names. A node only DB-IP can place is therefore
dropped by an `exclude_countries` naming that country and kept by a `countries`
allow-list naming it — the filter's verdict and the `[GEO:...]` tag can no
longer disagree. Three asymmetries remain:

- `geotrace` is skipped by the filter, and cannot be otherwise: the filter runs
  in preprocess, before any probe exists to ask. The tag can name the egress
  the filter never saw.
- `asn` is skipped by the filter: it is a per-IP Cymru round trip, not a local
  table. A node only Cymru can place counts as unplaceable for the filter while
  its tag names the country. Operators who want that lookup in the filter too
  configure it explicitly as `{type: country, provider: asn}`.
- with no `GEO` annotate entry (or one naming only `asn`), the filter falls
  back to the geofeed alone — the one database every process loads.

The DB-IP data is the free Country Lite edition, licensed CC BY 4.0 —
[IP Geolocation by DB-IP](https://db-ip.com) (this link is the required
attribution).

## Caches and stores

| Store | Kind | Purpose |
|---|---|---|
| geoblock (`geoblock.db_path`, `geoblock.ttl`, default 720h) | SQLite (pure-Go driver, `CGO_ENABLED=0`-safe), reads served from an in-memory cache | hosts that failed a through-node API reachability check (Gemini/Claude/ChatGPT — `tidal` deliberately does not feed it); dropped pre-DNS on both endpoints. Keys are lowercased, so a source spelling a blocked host in different case does not slip past. Expired entries are swept once per worker cycle, not only at startup |
| dead cache (`deadcache.ttl`, default 2h) | in-memory, not persisted | `server:port` of nodes with zero successful probe rounds; skipped before probing |
| DNS cache (`resolver.cache_ttl` / `cache_negative_ttl`) | in-memory TTL map, capped | node hostname resolution across cycles |
| ASN cache (`geo.asn.cache_ttl`, default 24h; 5m negative) | in-memory TTL map, capped | Team Cymru lookups |
| geofeed data (`geo.geofeed.refresh_interval`, default 24h; explicit `0` = never refresh) | in-memory, refreshed in background | IP→country entries from configured CSV sources |
| dbip (`geo.dbip.refresh_interval`, default 24h) | in-memory range index (~700k ranges), refreshed in background | DB-IP Country Lite IP→country database for the `dbip` annotate provider |
| registry (`geo.registry.refresh_interval`, default 24h) | in-memory range index (~330k ranges), refreshed in background | RIR delegated-extended registration countries for the `registry` annotate provider |

The two downloadable databases are built only when an `annotate` chain
references them. A failed startup download logs a warning and starts empty —
the provider chain degrades to the next provider and the next
request-triggered refresh retries; a failed background refresh keeps the
stale data. Loaded databases are carried across hot reloads when their config
block is unchanged, together with the retry schedule of a download that is
currently failing, so a reload neither re-downloads a healthy database nor
resets a failing one's backoff.

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
  future cycles (pruned after `CRAWL_STATE_TTL` without a live sub, then capped
  at the 200 most recently productive so cycle cost stays bounded),
- prunes conservatively: a harvested source is dropped only when the origin
  proves it gone (404/410/451) or serves no nodes — a 403/429/5xx or a network
  error keeps it — and a cycle that would delete a large share of the list at
  once refuses to write until a later cycle confirms the loss,
- additionally harvests **raw proxy URIs pasted directly in messages**
  (`vless://…` etc.), dedupes them, and packs them into a single inline
  `tg-inline` source with a base64 `body`,
- writes results to `config/private.yaml` as `tg-<channel>-<sha6>` sources
  (the discovering channel's slug plus a short URL hash, so the origin of
  every subscription is visible — including in `/stable.txt` node labels;
  pre-attribution `tg-<sha10>` names upgrade on the next rediscovery) — an
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
three are parsed **strictly** — an unknown or misspelled key fails the load
naming the key, because a silently dropped key means a silently restored
default (an empty or comment-only overlay is still fine). All
three are watched and **hot-reloaded** on
change; on any reload error the previous settings stay active. Changing
`server.listen`, `server.metrics_listen`, `geoblock.db_path`/`ttl`, or
`deadcache.ttl` requires a restart (logged as a warning; listeners and stores
are built once at startup). Everything else — filters, annotate, groups,
sources, prober knobs, log level — applies live; worker-side keys (sources,
prober knobs, through-node filters) take effect on the worker's next cycle. A
reflection test (`TestReloadCoverageComplete`) classifies every config key's
reload path, so a new key cannot ship without one, and a companion test
(`TestReloadClassificationMatchesBehaviour`) mutates each key to prove the
declared path is the one the reloader actually takes. A reload never restarts the
stable worker: when its inputs actually changed it is reconfigured in place, so
the cycle already in flight (20–55 min) runs to publication under the settings
it started with instead of being cancelled and losing its whole probe pass. A
reload that comes out with **zero** subscription sources is refused rather than
obeyed — the running worker keeps its previous sources and logs a warning,
since an empty list is nearly always a missing overlay file.

Key sections:

- `log.level` — zerolog level, hot-reloadable.
- `server.listen` / `server.metrics_listen` — public HTTP and internal
  Prometheus listeners.
- `geo.geofeed.sources[]` (`url` + explicit `type: raw|gzip`) +
  `refresh_interval` (unset → 24h, explicit `0` → load once and never
  refresh); `geo.asn.timeout` / `cache_ttl` — shared geo providers.
- `geo.dbip.url` / `geo.dbip.refresh_interval` and `geo.registry.urls[]` /
  `geo.registry.refresh_interval` — optional blocks for the downloadable
  IP→country databases; defaults are built in (the DB-IP Country Lite
  `{yyyy-mm}` monthly URL, the five RIR delegated-extended files, 24h
  refresh).
- `resolver.timeout` / `cache_ttl` / `cache_negative_ttl`, and `resolver.address`
  — the upstream DNS server as `host:port` (a portless value is rejected at
  load: it dials nothing, so every node would be dropped as a DNS failure).
- `filters` — the ordered filter list described above.
- `annotate` — the ordered tag list described above; GEO/ASN entries take a
  `providers:` chain. The retired singular `provider:` key is rejected as an
  unknown key by the strict decode instead of being silently dropped.
- `geoblock` — store path/TTL plus `gemini.*`, `claude.*`, `chatgpt.*` and
  `tidal.*` base params (endpoint, model, marker, key, timeout, concurrency)
  for the through-node filters, plus `geotrace.*` — `endpoint`/`timeout`/
  `concurrency` only, defaulting to `https://cloudflare.com/cdn-cgi/trace`,
  15s, 8 — which configures the `geotrace` ANNOTATE provider's probe, not a
  filter. There is no `{type: geotrace}` filter entry; naming `geotrace` in an
  `annotate` chain is what turns the probe on.
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
`stable_kept_nodes`, `stable_dead_skipped_nodes` — the last counts every node
skipped before probing, all of them dead-cached, so the funnel closes),
per-source and per-filter in/kept/dropped-by-reason counters, kept-node speed
histogram, cycle duration, success timestamp, and cycle/failure totals.

The metrics listener is bound synchronously at startup, so a port conflict is a
startup failure like any other rather than a silently missing monitoring
surface — the service's stable-list health is only observable through these
metrics.

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
- Shutdown is graceful and bounded: the server drains for 15 s (long enough for
  the 5 s DNS/ASN lookup an in-flight request may be blocked in) and the stable
  worker is joined for another 5 s, hence `stop_grace_period: 30s`. Expiring
  that budget is logged as a warning, not a failed exit.

## Package map

See [`routes.md`](./routes.md) for a per-package reference (types, functions,
dependency graph). Agent-facing conventions live in [`AGENTS.md`](./AGENTS.md).
