# sub-preprocessor

An HTTP preprocessor for Mihomo / Clash.Meta proxy subscriptions.

It takes raw proxy subscription lists (public collectors, Telegram channels,
your own sources), filters nodes by the country their IP resolves to or by
membership of a downloaded IP allow-list, probes them for liveness, latency,
bandwidth, and real-world reachability of geo-fenced services, and serves
clean Mihomo-compatible output. The goal is to feed a router's Mihomo instance
a subscription that only contains nodes worth routing through — dead, slow,
and geo-blocked nodes removed.

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

Two instances of the service run side by side (see [Deployment](#deployment)),
so every endpoint above exists twice, on a different port and against a
different config directory.

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
(`504` when it expires), and a body of more than 50 000 parseable nodes is
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
   the `cloudflare` provider — nothing is dropped here,
10. builds each node's tags **once**, from the address that survived the
    pipeline (the traced egress when there is one), and atomically publishes
    the result,
11. writes the published list to `subscriptions.snapshot_path`, if set.

`GET /stable.txt` serves the current list as `text/plain` (or
`503 stable list not ready` while there is no list at all) with an
`X-Stable-Stats` header
(`updated=<RFC3339> sources=<ok>/<total> merged=<n> tested=<n> kept=<n>`).
A failed cycle keeps the last good list, so the router never gets an empty
response.

**Across a restart** the list survives too, when `subscriptions.snapshot_path`
is set: startup reloads the file into the same holder, so the endpoint answers
with the previous run's list from the first request instead of `503` for a
whole cycle — measured at 58 minutes on a 68266-node pool. The restored list
keeps its original `updated=`, so its age is visible rather than reset by the
restart, and there is no expiry: the in-memory rule already serves the last
good list through failing cycles. A missing, unreadable or malformed file is a
warning and nothing more — the endpoint then behaves exactly as it did before
the file existed. The write is atomic (temp file in the same directory, then
rename), and a write that fails warns without touching the published list.

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
  alone — and only when a `country` entry is present at all. Nothing is built
  for a filter type the list does not name, so a config without one leaves
  `GET /` still requiring a `countries` / `groups` / `exclude_*` parameter
  (the server answers `400` without one) that no filter then reads.
- `asn` — drop nodes whose AS name matches `deny_patterns` (regexps), and
  nodes whose Cymru-resolved country is not allowed.
- `cidr` — an IPv4 **allow-list** downloaded from `urls`: a node survives only
  when at least one of its resolved addresses falls inside one of the ranges.
  Unlike `country` it reads nothing from the request, so `/` and the worker
  reach the same verdict and no query param can widen it. The lists are merged
  into sorted, disjoint ranges once per refresh, so the per-node cost is one
  binary search — which is why this entry belongs first in `filters:`, ahead of
  anything that makes a network call. Drops are counted as `cidr_drop=` in
  `X-Preprocessor-Stats` and as `stable_source_dropped_nodes{reason="cidr"}`.

  ```yaml
  filters:
    - type: cidr
      urls:
        - https://raw.githubusercontent.com/hxehex/russia-mobile-internet-whitelist/main/cidrwhitelist.txt
      file_type: raw        # raw | gzip, default raw
      refresh_interval: 24h # unset or an explicit 0 => 24h; negative is rejected
  ```

  Several `urls` are unioned into one list, and exactly one `cidr` entry is
  allowed per config: the union is what `urls:` already expresses, so a second
  entry could only mean intersection. **The startup load is fail-closed.** If
  no source yields a range the service refuses to start — a URL answering 404
  exits 1 with `no cidr ranges loaded (1 source(s) failed)`, and a body that
  parses to an empty set is refused by the same rule — because an allow-list
  that failed to download is not a milder filter but the opposite one: it
  would drop every node and publish an empty subscription. A background
  refresh keeps the last good list on failure, and its shrink guard compares
  **addresses covered**, not the number of merged ranges: the same upstream
  also publishes `ipwhitelist.txt`, which renders the identical space as 141664
  single addresses, so a range count can grow ninefold while real coverage
  collapses to 0.39%.

  Worked example, measured 2026-08-09 against that whitelist: the file's 30228
  lines merge into 15649 ranges covering 36265984 addresses. Do not "correct"
  that range count against a CIDR tool. Python's
  `ipaddress.collapse_addresses()` reports 30222 CIDRs over the identical file
  because it merges only ALIGNED prefix pairs, where `cidrset` merges any two
  ranges that TOUCH and so counts contiguous intervals. Both numbers are right
  about different things; the 15649 has been filed as stale over that gap once
  already, and the finding was retracted.

  **This list is not a Russian ACL, whatever its upstream repository is
  named.** Measured 2026-08-10 across the same 15649 intervals: AS749 DNIC (US
  Department of Defense) holds 20.97% of the covered addresses and AS0 —
  unrouted, rentable from nobody — a further 6.52%, so roughly 27% of the
  allow-list is space no node can sit in. By country it is 28.32% US against
  10.60% RU. It behaves like a worldwide `0.0.0.0/0` scan artifact, not an
  operator ACL. Correctness is untouched by this — nothing is hosted in DoD
  space, so no node is falsely kept — but **list size is not useful size**:
  anyone sizing a deployment from 15649 ranges will be wrong, and the only
  figure that means anything is a measured node count.

  Across the 51 sources in `config/sources.yaml` — 59558 nodes on 19330
  distinct servers — 5023 nodes (8.43%) sat on a server that resolves into it.
  That measurement seeded the second compose instance and no longer describes
  it: `config-vassago/sources.yaml` now carries 54 sources selected against the
  whitelist directly, most of them not in `config/sources.yaml` at all, and its
  header records their own measurement. What survives from a live run of that
  instance is not a count but an identity — `total` equals `kept` plus every
  drop reason, e.g. a source answering with 586 nodes booking 25 kept, 533
  `cidr_drop` and 28 `geo_drop` under `countries=RU`, which is 586 = 25 + 533 +
  28. Counts themselves never reproduce: these sources are live and rotate
  within the hour.

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

Filter order is honoured, and it is load-bearing rather than cosmetic in both
stages. The chain stops at the first filter that leaves a node with no
addresses, so a cheap local gate belongs ahead of one that makes a network call
per IP: put `cidr` before `asn` (and before `{type: country, provider: asn}`),
or a Team Cymru round trip is spent on every IP the allow-list was going to
discard one step later — 54535 of the 59558 nodes measured across the shipped
sources. In the through-node stage the same reasoning puts the expensive one
(`bandwidth`) last, so it runs on the fewest nodes. Nothing validates the
order: a validator would have to encode a cost model that does not belong in
the config loader. `cloudflare` is not in this list at all — it is an annotate
provider, not a gate: see [Annotation](#annotation).

### Annotation

The ordered `annotate:` list controls the tags prepended to node names on both
endpoints. `GEO` (`[GEO:XX]`) is the only tag it accepts — `IP` and `ASN` were
both retired, and naming either now fails the load. The entry takes
`providers:` — an **ordered lookup chain** (the shipped one is
`providers: [cloudflare, geofeed, dbip, registry]`): the first provider that
resolves the IP wins, and when every provider misses the tag renders as
`[GEO:??]`. The list still earns its shape: entries render in order and may
repeat (two `GEO` entries with different chains publish two tags, and the
country filter below consults both chains, so repetition changes the tag and
not the verdict), and an empty `annotate` list disables annotation (original
names pass through).
Rewriting is scheme-aware: vmess folds tags into
the base64 `ps` field, `ssr` into the base64 `remarks` query value, every other
scheme into the `#fragment`. For `ssr` the fragment is not merely unused but
corrupting — mihomo base64-decodes everything after `ssr://`, an appended
`#name` included — so a payload neither rewriter can decode is published
verbatim: unannotated beats mangled. Known stale tags from upstream are
stripped first.

Available providers:

| Provider | Source | Character |
|---|---|---|
| `cloudflare` | Cloudflare's geo-IP database, asked through the node itself via `/cdn-cgi/trace` (`geo.cloudflare.*`) | the only one asked about the EXIT address; worker-only |
| `geofeed` | RFC 8805 CSV feeds (`geo.geofeed.sources`) | precise, low coverage |
| `dbip` | DB-IP Country Lite — monthly gzip CSV; the `{yyyy-mm}` URL placeholder expands to the current UTC month, with one previous-month retry on a 404 right after rollover | broad coverage, in-memory |
| `registry` | the five RIR delegated-extended files (APNIC's is read from LACNIC's mirror — see below) | *registration* country of the allocated block, not necessarily where it routes |
| `asn` | Team Cymru DNS | accepted, but NOT in the shipped chain — see below |

The `dbip`/`registry` databases are downloaded and indexed in memory only when
an annotate chain actually references them.

`asn` is accepted here and deliberately unused. It is Team Cymru's DNS
redistribution of the same RIR delegation data `registry` already holds in
memory, so it behaves as a registry lookup over the network rather than as a
geolocation source: measured over every address behind every configured
subscription it was a strict SUBSET of a complete `registry` — no country it
placed that `registry` did not, and no disagreement where both answered — and
placed behind this chain its marginal contribution was zero hits, because
`dbip` alone answers all but a handful of addresses and that handful is
unroutable — RFC 2544 / RFC 5737 space (198.18.0.0/15, 192.0.2.0/24) returned
by DNS-poisoning sinkholes, and sources publishing 127.0.0.1 or
255.255.255.255 outright — which Cymru correctly has no record for either. It
remains reachable as `{type: country, provider: asn}` and as the `{type: asn}`
deny-pattern filter, whose AS *names* no local database carries. Re-measure
before putting it back in a chain.

`registry` reads APNIC from `ftp.lacnic.net`, not `ftp.apnic.net`: APNIC's own
host answers the TLS ClientHello without echoing the legacy session ID that
RFC 8446 requires, which Go's `crypto/tls` rejects outright, so that RIR
silently dropped out of the build and cost roughly two and a half points of
coverage. RIPE and LACNIC both mirror the file byte-identically under APNIC's
own published checksum, so the choice between them is about blast radius, not
fidelity — and blast radius is measured in RANGES behind the worst single host,
not in hosts. The five URLs sit on four hosts either way (`ftp.lacnic.net`
serves APNIC's mirror and LACNIC's own file), so a host count cannot tell the
candidates apart and the weights are the whole argument: `ripencc` already
comes from `ftp.ripe.net`, so parking APNIC there would put 201810 of the
330937 loaded ranges (61.0%) behind one outage, where `ftp.lacnic.net` leaves
the worst host at ripencc's own 126845 (38.3%) — the floor no mirror choice can
beat — with `ftp.lacnic.net` itself only second at 108784 (32.9%). The same
arithmetic rules out `ftp.arin.net`
(163160, 49.3%), which a keep-them-on-separate-hosts reading would have waved
through; `assertRegistryHostConcentration` in `internal/config` enforces it as
a rule over the range weights, so re-pointing any of these URLs has to argue
with the numbers. A mirror is also a copy on someone else's schedule, so
`LoadRegistry` logs each file's own header serial — an observable for lag, not a
gate. Its form is the publishing registry's business (`20260804` from APNIC, a
unix timestamp from RIPE, unix ms from ARIN), so read it against the same
source's previous value, not across sources.

`cloudflare` is the odd one out, but not in the way the old name suggested. It
is a geo-IP database like the others — the tag carries the `loc=` line it
answers with, Cloudflare's own lookup — and what differs is the ADDRESS it is
asked about. Every other provider looks the node's *resolved* address up, and
41% of the named hosts measured in the pool sit in Cloudflare's shared anycast
ranges, which terminate in many countries at once — so a node tagged `CA` was
in fact exiting in Germany. Asking the node where its traffic leaves from costs
one request through it, which only the `/stable.txt` worker's post-probe stage
can spend: on `GET /` there is nothing to ask, so `cloudflare` always misses
there and the chain falls through to the offline providers. Naming it in a
chain is what arms that probe; leaving it out means no cycle pays for it.

Its `timeout`/`concurrency` are `geo.cloudflare.*`. There is deliberately no
`endpoint` key: the parser encodes Cloudflare's documented reserved `loc`
values (`XX`, `T1`) and its uppercase convention, so no other VENDOR's endpoint
can satisfy it, and a key whose every cross-vendor value silently yields "no
answer" would be a false affordance. The one substitution that does parse is
intra-vendor — Cloudflare serves `/cdn-cgi/trace` on every domain it proxies,
so any Cloudflare-fronted host answers the same shape — and that belongs in
`stable.cloudflareTraceURL`, not in a config key.

The country **filter** (`provider: geofeed`) judges nodes with that same chain,
in the order the `annotate:` list gives it: it consults every local database
every `GEO` entry names, concatenated in written order and de-duplicated by
first occurrence. A node only DB-IP can place is therefore dropped by an
`exclude_countries` naming that country and kept by a `countries` allow-list
naming it — the filter's verdict and the `[GEO:...]` tag agree for every LOCAL
provider any `GEO` entry names, so splitting one chain across entries changes
what is RENDERED (`[GEO:??][GEO:DE]` instead of `[GEO:DE]`) and never the
verdict. Three asymmetries remain:

- `cloudflare` is skipped by the filter, and cannot be otherwise: the address
  it is asked about does not exist yet. The filter runs in preprocess, before
  any probe exists to ask. The tag can name the egress the filter never saw.
- `asn` is skipped by the filter whenever a chain does name it (the shipped one
  does not): it is a per-IP Cymru round trip, not a local table. A node only
  Cymru can place counts as unplaceable for the filter while its tag names the
  country. Operators who want that lookup in the filter too configure it
  explicitly as `{type: country, provider: asn}`.
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
| stable snapshot (`subscriptions.snapshot_path`, empty disables) | one JSON file, rewritten atomically once per published cycle | the published `/stable.txt` list, reloaded at startup so a restart does not answer `503` for a whole cycle. No TTL. Shipped at `/config/.stable-snapshot.json` — inside the only writable host bind mount, so it outlives a redeploy and a host reboot alike, the same guarantee `.geoblock.db` beside it already has |
| DNS cache (`resolver.cache_ttl` / `cache_negative_ttl`) | in-memory TTL map, capped | node hostname resolution across cycles |
| ASN cache (`geo.asn.cache_ttl`, default 24h; 5m negative) | in-memory TTL map, capped | Team Cymru lookups |
| geofeed data (`geo.geofeed.refresh_interval`, default 24h; explicit `0` = never refresh) | in-memory, refreshed in background | IP→country entries from configured CSV sources |
| dbip (`geo.dbip.refresh_interval`, default 24h) | in-memory range index (~700k ranges), refreshed in background | DB-IP Country Lite IP→country database for the `dbip` annotate provider |
| registry (`geo.registry.refresh_interval`, default 24h) | in-memory range index (~330k ranges), refreshed in background | RIR delegated-extended registration countries for the `registry` annotate provider |
| cidr allow-list (`filters[].refresh_interval`, default 24h) | in-memory merged range list, refreshed in background | the IPv4 ranges a `cidr` filter admits; built only when such a filter is configured |

The two downloadable geo databases are built only when an `annotate` chain
references them. A failed startup download logs a warning and starts empty —
the provider chain degrades to the next provider and the next
request-triggered refresh retries; a failed background refresh keeps the
stale data. Loaded databases are carried across hot reloads when their config
block is unchanged, together with the retry schedule of a download that is
currently failing, so a reload neither re-downloads a healthy database nor
resets a failing one's backoff. The cidr allow-list rides that same machinery
with three deliberate differences: it is built when a `cidr` filter is
configured rather than when a provider is named, an empty startup load is
fatal rather than a warning, and its refresh guard measures the swap in
covered ADDRESSES where the geo databases count ranges — the last two are
spelled out with the filter above.

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

Every candidate that fails a gate or does not classify live gets one log line
saying which channel it came from, which `tg-<slug>-<sha6>` source it would have
been, and why — `noise-host`, `invalid-url`, `bad-status` (with the code),
`fetch-failed`, `nodeless-2xx` or `expired` — followed by one per-cycle summary
counting each reason. These lines carry the host and nothing more of the URL:
the credential lives in the query on some panels (`?payload=…`) and in the path
on others (Marzban's `/sub/<token>`, 3x-ui's `/<subPath>/<subId>`, neither with
a query at all), so only the `sha6` identifies which subscription it was. They
are capped at 200 per cycle and the cycle reports how many it withheld; the
summary counts stay complete regardless. Dedupe is what keeps them complete, and
it is bounded in turn: past 20,000 distinct rejected URLs in one cycle the
summary stops tracking and reports the overflow as `untracked=<n>` instead.
`untracked` is deliberately *not* folded into the total — past the bound a repeat
cannot be told from a new candidate — so the per-reason counts still sum exactly
to `rejected`, and `untracked` is the rejections beside them that went
unaccounted for.

A line's `error` field is guarded by two rules, and each has its own replacement
text. The candidate's URL is substituted out of the message, and if any URL
survives that (a refused redirect names a second one) the whole message is
dropped for `redacted: error names the candidate url`. An error naming only the
path or query slips that rule — no `://` is left to see — so a second check
replaces it with `redacted: error names the candidate url path or query`. That
second one is deliberately over-eager and matches substrings, so **read it as a
false positive first**: a candidate whose path is `/o` matches the `i/o` in
`dial tcp: i/o timeout`, and `net/http: TLS handshake timeout` has a slash too,
so the commonest failures can lose their diagnosis to a short path. Only when
the candidate's own path and query cannot collide with the expected message does
it mean what it says.

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
`server.listen`, `server.metrics_listen`, `geoblock.db_path`/`ttl`,
`deadcache.ttl`, or `subscriptions.snapshot_path` requires a restart (logged as
a warning; listeners and stores are built once at startup). Everything else — filters, annotate, groups,
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

The repo ships **two** config directories, one per compose instance:
`config/` for `sub-preprocessor` and `config-vassago/` for
`sub-preprocessor-vassago`. Each is bind-mounted at `/config` inside its own
container, so everything below is relative to whichever directory an instance
runs on, and the strict-decode, overlay and hot-reload rules are identical for
both. `config-vassago/` ships `config.yaml` + `sources.yaml` and no
`private.yaml`: the crawler writes into `config/` only, so that instance's 54
sources are curated by hand. Read that file's header before editing it — it
carries the measurement the list was selected on, and the three gates a
candidate has to pass: the repo is FRESH, the body is FETCHABLE inside
`fetch.timeout` AND fits under the worker's 10 MiB body cap, and its
contribution is MARGINAL — `server:port` no already-accepted source carries,
while the whole ADDED BLOCK stays under a per-cycle budget of resolved hosts.
Cost here is distinct hosts, not node lines.

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
  `{yyyy-mm}` monthly URL, the five RIR delegated-extended files with APNIC
  taken from LACNIC's mirror, 24h refresh).
- `geo.cloudflare.timeout` / `geo.cloudflare.concurrency` (default 15s, 8) —
  the `/cdn-cgi/trace` probe behind the `cloudflare` ANNOTATE provider. Two
  keys and no `endpoint`: only Cloudflare's own body parses, so the URL is a
  compile-time constant. There is no `{type: cloudflare}` filter entry either;
  naming `cloudflare` in an `annotate` chain is what turns the probe on.
- `resolver.timeout` / `cache_ttl` / `cache_negative_ttl`, and `resolver.address`
  — the upstream DNS server as `host:port` (a portless value is rejected at
  load: it dials nothing, so every node would be dropped as a DNS failure).
- `filters` — the ordered filter list described above. The `cidr` entry's
  `urls` / `file_type` / `refresh_interval` hot-reload like everything else in
  the list: editing one of them re-downloads the allow-list (and refuses the
  reload, keeping the running processor, if the new one comes back empty),
  while an edit anywhere else in the file carries the loaded list over instead
  of paying for it again.
- `annotate` — the ordered tag list described above; every entry is a `GEO`
  entry and takes a `providers:` chain. The retired singular `provider:` key is
  rejected as an unknown key by the strict decode instead of being silently
  dropped.
- `geoblock` — store path/TTL plus `gemini.*`, `claude.*`, `chatgpt.*` and
  `tidal.*` base params (endpoint, model, marker, key, timeout, concurrency)
  for the through-node filters. Every tenant here is a gate; the trace probe
  used to sit alongside them and moved to `geo.cloudflare` because it is not.
- `deadcache.ttl`, `fetch.timeout` (per-subscription fetch deadline).
- `groups` — named country sets referenced by requests and `exclude_groups`.
- `subscriptions` — `interval`, `sources[]` (`name` + `url` *or* inline
  `body`), `check.*`: URL-test prober params only (`rounds`, `timeout`,
  `max_fail`, `max_avg_ms`, `concurrency`, `source_timeout`, `test_url`,
  `expected_status` in mihomo IntRanges syntax), and `snapshot_path` — where
  the published list is persisted so a restart serves it instead of `503`
  (empty disables; restart-only, see above).

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
per-source and per-filter in/kept/dropped-by-reason counters, two kept-node
histograms — `stable_kept_speed_mbps` and `stable_kept_latency_ms`, each with
`_min`/`_max` gauges beside it — cycle duration, success timestamp, and
cycle/failure totals. The latency histogram is the one that closes a loop:
`check.max_avg_ms` both admits a node and orders the published list, so
without it the single threshold deciding how long that list is had no
observable to tune against. That only works while the gate is visible on the
axis, so **`latencyBuckets` must carry a bound equal to every
`check.max_avg_ms` any shipped config sets.** A threshold landing between two
bounds hides the very edge the panel exists to show. Both instances publish
this metric name under different Prometheus jobs and their thresholds are free
to differ, so the ladder carries all of them and moving any one of them adds
its bucket in the same commit. The
IP-stage drop reasons are `dns`, `geo`, `cidr`, `asn`, `geoblock`, `ipv6` and
`unsupported`; adding `cidr` cost no new metric name and no panel or query
change, because the reasons are label values on `stable_source_dropped_nodes`
and the panel sums by `reason` instead of enumerating them. The dashboard was
still edited: the "IP-stage drops by reason" panel's DESCRIPTION enumerates the
reasons in prose, so it names `cidr` and says what gates it.

A gate that verified nothing is reported separately from a gate that dropped
nothing. The gemini check needs a working credential to see a location verdict
at all, so `stable_gemini_gate_checks` / `stable_gemini_gate_unverified_checks`
publish how many API responses it classified and how many of those arrived
before the location check (401/403/404/429, or a 400 `API_KEY_INVALID`).
Those nodes are **kept and published unverified**, so the pair is deliberately
outside the per-filter drop counters. `stable_gemini_gate_enabled` separates
the third state: 0 means the gate is configured but has no usable key, so it
checked nothing at all. All three are absent when the gate did not run, which
is not the same as "not configured": nothing has been scraped, no cycle has
published yet, no `gemini` filter is in `filters`, or one is and never reached
its check (the prober has no Gemini support, or parsing the survivors into
proxies failed and the whole through-node chain was skipped).

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

Both compose instances are scraped, each under its own Prometheus job name
(`sub-preprocessor`, `sub-preprocessor-vassago`), and the dashboard picks
between them with an **Instance** variable —
`label_values(stable_cycles_total, job)`, with every panel expression scoped
`{job="$job"}`. Two jobs rather than two targets in one job is what makes them
selectable at all: the picker's values come from the `job` label.

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

Docker Compose runs two instances of the service plus the crawler sidecar, all
from one image:

```bash
docker compose up -d --build   # or: make dc-up
```

- `sub-preprocessor` — the general instance on `./config`: the HTTP service
  (published on `:7008`) and the stable worker. Metrics are published
  loopback-only (`127.0.0.1:9091:9090`) for the host Prometheus — keep them
  non-public. The Gemini API key is read from an agenix-decrypted secret
  mounted at `/run/agenix/litellm-env`.
- `sub-preprocessor-vassago` — the same image on a second config
  (`./config-vassago`), published on `:7009` with metrics on
  `127.0.0.1:9092:9090`. Its `filters:` are `cidr`, `country` and `bandwidth`: a
  node is kept only when its entry IP is inside the allow-list that `cidr`
  entry downloads — upstream `hxehex/russia-mobile-internet-whitelist`, which
  despite the name is a worldwide scan artifact rather than a Russian ACL (see
  the `cidr` filter above; ~27% of it is US DoD and unrouted space). The
  `country` entry carries no `exclude_*`, so it is inert on
  `/stable.txt` — a full allow set with an empty deny set makes `GeofeedFilter` a
  no-op — and exists for `GET /`, where it is what makes the `countries=` /
  `groups=` parameters actually gate: without an entry of that type the server
  still demands one of those parameters and then no filter reads it. No
  through-node geo gate runs at all — these nodes are meant to be used UNDER that
  whitelist, so an egress-geo gate answers a question nobody asked and would cost
  a request per survivor to do it. It mounts no agenix secret
  (there is no Gemini key to read) and carries no `build:` section, so
  `sub-preprocessor` is what produces the shared image — hence the
  `depends_on`.
- `tg-sub-crawler` — the crawler (`command: ["crawl"]`), sharing the
  `./config` volume so its `private.yaml` writes hot-reload the service. It
  feeds the first instance only; `./config-vassago` has no crawler and is
  curated by hand.
- Shutdown is graceful and bounded: the server drains for 15 s (long enough for
  the 5 s DNS/ASN lookup an in-flight request may be blocked in) and the stable
  worker is joined for another 5 s, hence `stop_grace_period: 30s`. Expiring
  that budget is logged as a warning, not a failed exit.

## Package map

See [`routes.md`](./routes.md) for a per-package reference (types, functions,
dependency graph). Agent-facing conventions live in [`AGENTS.md`](./AGENTS.md).
