# Design decisions and API behaviour

> **When to read this:** Read before changing pipeline behaviour, adding a scheme or decoder, touching the probe/filter order, or answering "why is it built this way". Includes what the endpoints guarantee.

## What this project is

This is a small HTTP preprocessor for Mihomo-compatible subscription content.
It exposes two modes.

**On-demand filter (`GET /`)** — filter one subscription URL at request time:

1. accepts `subscription_url` + `countries` (or `groups` referencing `config.groups`) via HTTP query params
2. downloads subscription text
3. parses generic URI-style nodes (not VLESS-only; `vmess://` base64-JSON is decoded too)
4. resolves node hostnames
5. geofilters by IP country from geofeed sources
6. rewrites node fragment/name with the configured `annotate` tags (shipped config: `[GEO:XX] ...`)
7. returns raw Mihomo-compatible text/plain subscription body

**Stable subscriptions worker (`GET /stable.txt`)** — a background worker keeps
one curated list built from all `subscriptions.sources`. Each cycle fetches every
source, runs it through the same IP-stage filter chain (geo/ASN/cidr), merges and dedupes by
`server:port` (first source wins), relabels kept nodes to `<source>-NNN`, probes
every node with an embedded Mihomo URL test, keeps only those that pass all
rounds under the latency threshold, then runs the configured through-node
filters (`gemini`/`claude`/`chatgpt`/`tidal`/`bandwidth`) on the survivors. The
egress stage consumes the very adapters the probe built (see the
retained-adapters decision below); it never re-parses the survivor set. Only
then are the published names built — once, from what the probes learned, with
the egress each survivor reports about itself through `cdn-cgi/trace` when the
`annotate` chain names the `cloudflare` provider. The result is
swapped in atomically; the last good list is kept if a cycle fails, and with
`subscriptions.snapshot_path` set it is also written to disk and reloaded at
startup — when subscriptions are enabled, so the deliberate empty-source-list
disable does not serve last run's list forever — leaving `503` for a genuinely
cold start rather than every restart.

**One instance of this binary runs from the image** (`docker-compose.yaml`):
`sub-preprocessor` on `:7008` reading `./config`, with the `tg-sub-crawler`
sidecar writing its `private.yaml` overlay into that same directory, so "the
shipped config" is one directory and one `filters:` chain. A second instance on
a second config directory was retired 2026-08-26. The `cidr` allow-list on the
ENTRY address that had given it its purpose, holding
`hxehex/russia-mobile-internet-whitelist`, was disabled 2026-08-14 after it
published 1 node per cycle for 43 cycles; the filter type is still a feature and
that verdict now sits with it, commented out, in `config/config.yaml`.
The shipped `country` entry carries no `exclude_*`, so it is inert on
`/stable.txt` (`GeofeedFilter` early-returns on a full allow set with an empty
deny set) and is there for `GET /`: without an entry of that type — or a
`{type: asn}` one — no filter can honour the request's country parameters, so
the server refuses every country-gated request with a `400` naming the missing
filter instead of demanding a parameter that nothing would then read (see the
API section).

**Do not read that upstream repository name as a description of the data.** Measured
2026-08-10 over the whitelist's 15649 intervals: AS749 DNIC (US DoD) is 20.97% of the
covered addresses, AS0 (unrouted) a further 6.52%, and by country it is 28.32% US against
10.60% RU — a worldwide `0.0.0.0/0` scan artifact, not an operator ACL. Nothing is kept
falsely by this, since no node is hosted in DoD space, but list size is NOT useful size:
no deployment may be sized off the 15649, only off a measured node count.

## Important current design decisions

- **The project is in Go because mihomo is, and that is the root of every rule below
  that starts "mirrors mihomo".** `internal/stable/prober.go` imports
  `mihomo/adapter`, `mihomo/common/convert`, `mihomo/common/utils` and
  `mihomo/constant` (lines 14-17) to turn a subscription payload into LIVE proxies
  (`parseLive`, :410) and dial through each one — the `/stable.txt` worker's
  entire purpose. The pre-check that decides which nodes are worth an adapter
  object reads the raw mapping instead (`probeNodes`, :318), and a successful
  probe keeps the adapters it built alive for the egress stage (see the
  retained-adapters decision below). Nothing else needs
  the language: outside the prober the dependency is type constants
  (`mihomo/constant` in `internal/stable/{label,nodefilter,checker}.go`) and one
  helper (`mihomo/common/utils` in `internal/config/config.go:23`), and the rest of
  the pipeline is HTTP, YAML and parsing. The hand-rolled metrics renderer is a
  second-order cost of the same dependency (see
  [`monitoring.md`](./monitoring.md)).
- **"Rewrite it in another language" is a SETTLED question — do not reopen it as a
  performance change.** A line-by-line Rust port was written, measured against this
  tree and dropped. It reached parity on every mihomo-free package and could not
  cross the prober: replacing the language means reimplementing mihomo's adapter set
  (vless/vmess/ss/ssr/trojan/hysteria2/tuic/mieru) *and* its parse quirks, and
  without that step the worker probes nobody and publishes nothing. **Cite the
  mechanism, not a microbenchmark number**: a `/stable.txt` cycle is 20-55 min on a
  68k-node pool, nearly all of it DNS resolution and one URL-test probe per node, so
  microbenchmark seconds are not the cycle. What the exercise actually bought is the
  three commits sitting on top of
  `cd21b6b` — the `inet_aton` SSRF gate, one shared `net.Resolver`, and a single
  unescape per crawled page. Writing the pipeline a second time is what found them;
  keeping a second implementation was not what paid.
- Parsing is **generic URI parsing**, not hardcoded to `vless://` only, and that stays the policy: there is deliberately **no whitelist** of mihomo-known schemes, so a scheme with no concrete reason to be rejected keeps the generic `scheme://[userinfo@]host[:port][?query][#fragment]` walk. Four schemes need more than that walk, because the fields the pipeline needs are not where it looks, and each decoder mirrors mihomo's so that a node we keep is a node the prober can convert:
  - `vmess://` — base64 JSON (`add`/`port`/`ps`).
  - `ss://` — the PORT picks the form, as it does for mihomo: a portful SIP002 link takes the untouched generic path, and a portless authority is the legacy form, base64 of `method:pass@host:port`, decoded with `base64.RawStdEncoding` ONLY. Narrower than our tolerant vmess decoder on purpose: the shadowsocks spec writes that form WITHOUT padding and mihomo decodes it with that alphabet alone, so a padded or url-safe payload is refused at parse (the authority never decodes) where a tolerant decoder — the vmess habit — would have kept it, and a line we kept but mihomo cannot decode is a node that merges, spends a probe, converts to nothing and parks in the dead cache. Branching on the `@` instead would keep `ss://<b64userinfo>@host` — SIP002-shaped but portless, and a line mihomo drops — and the blanket 443 default the parser once applied would have published it under a port it does not have; the parser now refuses the line outright too, because a portless ss authority must decode as legacy base64 (`decodeSSLegacy`), an alphabet holding no `@`.
  - `ssr://` — host, port AND display name all live inside one base64 payload, so nothing is readable from the URI. Accepted only on mihomo's own terms: a `/?` split, exactly 6 colon-separated head fields, a query `url.ParseQuery` accepts, and a port that is a bare decimal in 1..65535. `adapter.ParseProxy` decodes that port itself, so a non-numeric one is a node that converts and then fails to build; the range is stricter than that decode deliberately, since `NewShadowSocksR` has no range check of its own and nothing can dial `0`, `-1` or `70000`. Its name is relabeled through `subscription.RewriteSSRName` (the ssr twin of `RewriteVmessName`), which emits a **fragment-free** link — mihomo base64-decodes everything after `ssr://`, a `#fragment` included, and refuses the link if one is there.
  - `mierus://` — the port list lives in the query. We key the node on the first `port` value mihomo's adapter would actually serve (1..65535, or a range of two such), not merely the first one written down.
- A **portless line is refused for every scheme except `hysteria2`/`hy2`**, which receive the 443 mihomo itself supplies (`converter.go:85-89`): mihomo drops a portless vless (and the Xray VMessAEAD form) fail-closed on `url.Port() == ""` (convert/v.go:20-22), the empty trojan/tuic/hysteria port fails its structure decoder, and an unknown scheme has no converter case — so defaulting the port (as the parser once did for every scheme) published a probe slot and a dead-cache entry under a port the node does not have. The PORTFUL form is still a valid node under the generic walk — which is why `internal/classify`'s proxy-scheme whitelist and the crawler's inline-node scheme list (`isInlineScheme`) still leave the plain-web schemes out: a `https://example.com:8443/docs` in a README must not make the page read as a live subscription, and the portless half of that (`https://t.me/somechannel`, an `http://` panel notice) dies at parse for every scheme, not only those five.
- The scheme is **lowercased once at the top of `parseNode`** before any dispatch, mirroring mihomo (convert/converter.go:35): a byte-exact compare would let `SS://`, `VMESS://` or `HTTPS://` bypass the dedicated decoders and the portless reject above, and `Node.Scheme` stays lowercased so rewrite's and merge's switches fire too. The common case allocates nothing — `strings.ToLower` returns an already-lowercase string untouched.
- A subscription URL may also answer with an **Xray JSON config** instead of a URI list — panel software (Hiddify) does. `subscription.Normalize` converts such a document's outbounds into share links, so nothing downstream (`Parse`, `classify`, the geo pipeline, `Merge`, `rewrite`) knows about JSON. The asymmetry with the line above is deliberate, not an oversight: URI parsing is scheme-generic, the JSON conversion covers **vless and hysteria2 only** — 158 of the 160 proxy outbounds in the first measured corpus were vless, and one of the two shadowsocks entries carried the literal address `sdfsdf`, so shadowsocks is still out. Add a protocol when data justifies it: hysteria2 is what that looked like, and it converts at version 2 ONLY, mihomo reading v1 under its own `hysteria://` scheme with a different parameter set.
- IP-stage filters see only a node's resolved IPs (`Filter.Process`, `internal/preprocess/filters.go`), never its name or protocol: country from the local databases, AS name/country, `cidr` membership.
- Output rewriting is still **scheme-aware/safe**: it only rewrites parsed URI nodes.
- The on-demand `/` path does no liveness probing. The `/stable.txt` worker is the only place that probes nodes (embedded Mihomo URL test).
- In the `/stable.txt` worker a node is identified by its `Entry.Label` (`<source>-NNN`), never by the mihomo proxy name — because the two differ for `mierus://`, which mihomo expands into ONE proxy PER configured port, named `<label>:<port>/<protocol>`. `entryLabel` (`internal/stable/label.go`) folds that back; without it a healthy mieru node matched nothing, was never selected, went into the dead cache and counted `unreachable` in every through-node filter. The latency probe and both through-node outcome maps then fold a label's duplicates **best-of-ports** (the entry survives if any of its ports passed — best-of, never a sum, or `Successes` could exceed `check.rounds`), while the filters' proxy subset keeps EVERY port, so a port dead on our egress cannot mask a live sibling.
- The resolver keeps an in-memory DNS TTL cache (`resolver.cache_ttl` / `resolver.cache_negative_ttl`) so repeated stable cycles don't hammer the upstream DNS.
- Geofeed sources are explicit in YAML via `geofeed.sources[].url` + `geofeed.sources[].type`.
- File type is explicit only: `raw` or `gzip`. There is no auto-detection/legacy mode.
- Geofeed data is cached in memory and reloaded by `geofeed.refresh_interval` (unset → 24h; explicit `0` disables the refresh).
- A config reload NEVER restarts the `/stable.txt` worker. `stable.Controller.Apply` swaps a `CheckerSpec` the next cycle reads, so the crawler's hourly `private.yaml` rewrite cannot cancel a 20–55 min cycle in flight and burn a full probe pass. Nor does it stop the worker: a reload whose merged source list came out EMPTY is refused with a warning and the previous spec stays live, because every source comes from an overlay and an empty list is nearly always a missing file. At STARTUP the same predicate reads differently: an empty source list is the deliberate disable, so no worker is started and no snapshot is restored — a disabled worker must not serve last run's list forever. Only shutdown calls `Stop`.
- The `cidr` allow-list **fails closed at BUILD**, unlike every downloadable database beside it. Two gates, both fatal to `NewProcessor`: `cidrset.Load` errors when no URL yielded a range, and `newCIDRStore` refuses an empty set (`errEmptyCIDRSet`) even where a load reported success. A warning would be wrong here, because an allow-list that failed to download is not a degraded filter but the INVERTED one — it drops every node while every counter reads healthy, and the instance publishes an empty subscription. Verified against the real config: pointing `filters[].urls` at a 404 exits 1 with `no cidr ranges loaded (1 source(s) failed)`. A hot reload hitting the same error keeps the previous processor, so the running list survives a typo.
- **The allow-list's swap guard counts COVERED ADDRESSES, never merged ranges.** `swapRefusalSize` is shared with the geo databases, whose unit is ranges, and the cidr store hands it `Set.Covered()` instead. The two quantities move in opposite directions, and the upstream itself is the proof: `cidrwhitelist.txt` is 30228 lines -> 15649 merged ranges -> 36265984 addresses, while the SAME repo's `ipwhitelist.txt` publishes that identical space as 141664 lines — every one of them the `.1` of a /24 already covered, 0 outside. Configure both URLs and lose the CIDR file, and a range-count guard reads 15649 -> 141664 (+805%) as healthy growth while swapping in a set covering 0.39% of the space, whose only survivors are nodes resolving to a literal `.1`. Coverage refuses that swap; `Len()` applauds it. That is why `cidrset.Len`'s doc comment says what it must not be used for.
- **That 15649 is an INTERVAL count and no CIDR tool reproduces it.** `cidrset.newSet` coalesces any two ranges that TOUCH (`next.lo <= ranges[last].hi+1`), where `ipaddress.collapse_addresses()` merges only ALIGNED prefix pairs and answers 30222 CIDRs over the same `cidrwhitelist.txt`. Both are right about different things: check a range count against the merge rule that produced it, never against a tool with a different one.
- **At most ONE `cidr` entry per config**, rejected at load rather than merged. `urls:` already expresses the union, so a second entry could only mean intersection — a shape nothing here wants — and one entry means one live store, which is what lets the reload carry-over stay a single `CIDRState` instead of a keyed collection. The same singleton rule now covers the five through-node types — see config.md's `filters` entry for why two entries of one gate would break the metrics, not the config.
- **The egress stage consumes the probe's retained adapters; it never re-parses the survivor set.** `MihomoProber.Probe` keeps the adapter objects it built for the whole live set past the probe (`m.probed`), and the checker takes them over for `filterAndMeasureEgress` (`TakeProbedAdapters`, the optional `probedAdapterSource` capability) — the through-node filters and the cdn-cgi/trace stage look their proxies up by label from that set, which is byte-identical to what parsing the survivors' raw again would build. Every adapter is closed exactly once on every path: a failed or cancelled probe closes what it retained before returning, `RunOnce` defers the close of what it took, and the next `Probe` releases anything a previous call left untaken. A prober without the capability falls back to parsing the survivor set inside `filterAndMeasureEgress`, owning that parse's close — the two paths are why the old parse-everything-twice cost is gone for the shipped prober while the base `Prober` contract is unchanged.
- **A verdict that condemns the whole survivor batch is disbelieved before any drop, any geoblock-store write or any dead-cache write.** `apiFilter.apply` and `bandwidthFilter.apply` count their batch outcome first and trip the same plausibility breaker the pre-check uses (`breakerTrips`: 100% refusal at any sample size — one refused endpoint tells the same story as ten thousand — or ≥95% once 100+ were judged): every node dials the SAME endpoint over the SAME uplink, so a wholesale unreachable/blocked verdict is the endpoint's or our egress's story, not the nodes' — the batch is kept, warned about, and re-checked next cycle, and nothing is persisted (a disbelieved verdict would evict every survivor host from the geoblock store for its whole TTL). The dead-cache WRITE carries the same guard (`recordDead`), so a cycle where nearly every probed node failed leaves the cache unchanged. What an operator can and cannot see of these states is [`monitoring.md`](./monitoring.md)'s subject; the honest limit, stated there too, is that a tripped breaker renders exactly like a clean pass on the drop series (present zeros) — only the gate's warn log distinguishes them.

## API behavior to remember

- `GET /healthz` returns `ok`
- `GET /` requires:
  - `subscription_url`
  - `countries` (comma-separated) OR `groups` (comma-separated, referencing `config.groups`) — or, alone, either `exclude_*` parameter below: an exclusion-only request is accepted, but only if it actually names a country: a value made of blank tokens (`exclude_countries=,,`) carries no exclusion and answers the same `400` as no parameter at all, where it once answered 200 with the FULL unfiltered subscription
  - optional `exclude_countries` / `exclude_groups` — a true **deny-list**, not a subtraction from the allow-list. A node is dropped only when its IP resolves to an excluded country; an IP no geo provider can place SURVIVES an exclusion-only request. Under an explicit `countries`/`groups` allow-list an unplaceable IP is still dropped — that is what an allow-list means. Unknown group names and non-alpha-2 codes are rejected with `400`, not silently ignored
  - a country-capable IP filter must exist in the loaded config — a `{type: country}` entry (either provider) or `{type: asn}`. With neither (an empty list or cidr-only) every countries/groups/exclude_* request answers `400` `no country-capable filter configured (filters[].type: country or asn)` instead of 200 with a list the parameters never narrowed (`countryPolicy` in the handler; `config.CountryFilterConfigured`)
  - each query key is single-valued: a repeated `countries=`/`exclude_countries=`/… answers `400` `repeated query parameter: <key>` rather than silently serving the first value (a dropped second exclusion could serve exactly the jurisdictions the caller asked to exclude)
- `GET /` does not publish a portless line as a node: a bare `https://t.me/somechannel` in a source body counts in `Stats.Unsupported` (`unsupported=` in `X-Preprocessor-Stats`), as does any other scheme's portless form. A portful one (`https://example.com:8443`) is still a node
- `GET /` bounds one request: a 60s deadline (`504` on expiry, since fasthttp's request context has neither a deadline nor client-disconnect cancellation) and a 50k node ceiling (`413`). The request's own deadline is also the on-demand DOWNLOAD's budget: `preprocess.Filter` passes no sub-budget, so the subscription fetch inherits the request context — `fetch.timeout` is the worker's fail-fast knob and is NOT applied here (see config.md's `fetch.timeout` entry). The byte ceiling answers the same `413`: a body over the 10 MiB cap is refused by the fetch layer (`response too large: over …`) and mapped to the documented status, so oversize stays distinguishable from an upstream fault (`502`). The node ceiling is a DoS bound shared with the worker's per-source load, not a quality filter — at 20k it dropped a configured 36421-node aggregator source outright
- `GET /stable.txt` serves the worker's current list; `503` until there is one — the first completed cycle, or the snapshot restored at startup when `subscriptions.snapshot_path` is set AND `subscriptions.sources` is non-empty (an empty source list is the deliberate disable, and restoring then would serve a stale list with no worker ever to replace it — `restoreStableList`, `app.go:193`). A restored list keeps its original `updated=`, so its age shows. Stats are returned in `X-Stable-Stats` (`updated=… sources=ok/total merged=… tested=… kept=…`)
- Response is `text/plain; charset=utf-8`
- `/` stats are returned in `X-Preprocessor-Stats`

Example:

```bash
curl "http://127.0.0.1:8080/?subscription_url=https://raw.githubusercontent.com/flaafix/AetrisVPN-black-list/refs/heads/main/configs.txt&countries=FI,EE,LV,LT,SE,PL,DE,NL"
curl "http://127.0.0.1:8080/?subscription_url=https://raw.githubusercontent.com/flaafix/AetrisVPN-black-list/refs/heads/main/configs.txt&groups=nordics,euronorth"
```
