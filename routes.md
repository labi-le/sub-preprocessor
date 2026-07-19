# Package Map & Tags

LLM-oriented reference. Each package described with purpose, key exports, and search tags.

---

## `main`

`./main.go`

Entry point. With no args, creates `context.Context` with `SIGINT/SIGTERM` cancellation and calls `app.Run()` (the HTTP service). Two subcommands share the binary: `crawl` runs the Telegram subscription crawler loop (`internal/crawl`, configured via `CRAWL_*` env), `classify <url>` classifies one URL and exits 0 (live subscription) / 1 (not) / 2 (usage).

**Tags:** `entrypoint`, `root`, `signal`, `main`, `subcommand`, `crawl`, `classify`

---

## `internal/app`

`./internal/app/app.go`, `pprof.go`

Application bootstrap: loads config, creates `Processor`, wires the config watcher and reloader, starts HTTP server, handles graceful shutdown.

**Key exports:**
- `Run(ctx) error` — main lifecycle

**Constants:**
- `defaultConfigPath = "./config/config.yaml"` — path passed to `config.Load`, `reload.NewReloader`, and `reload.NewWatcher`

**Wiring (inside `Run`):**
- Builds `server.Holder` seeded with the startup `Snapshot`
- Creates `server.New(logger, listen, holder)` (no longer passes `svc`/`groupsMap` directly)
- Creates `reload.NewReloader` seeded with startup `cfg` + `svc`
- Creates `reload.NewWatcher` with `reloader.Reload` as the `onChange` callback
- Runs watcher in a goroutine under a derived cancellable context; a deferred cancel+join (`<-watcherDone`) runs before the `ctl.Stop()`/`gbStore.Close()` defers on EVERY return path (incl. listen error), so an in-flight reload can never race teardown

**Uses:** `config`, `geoblock`, `log`, `preprocess`, `reload`, `server`, `stable`
**Tags:** `bootstrap`, `wire`, `shutdown`, `lifecycle`, `hot-reload`

---

## `internal/config`

`./internal/config/config.go`

YAML config loading and validation. Uses `gopkg.in/yaml.v3`. Defines the full config schema. `Load` merges sibling overlays when present — `sources.yaml` (curated subscription sources) and `private.yaml` (crawler-managed sources) — appending their `subscriptions.sources`; a read error other than not-exist fails the load. Also provides diff helpers used by the reloader to decide what changed.

**Key types:**
- `Config` — root config struct (`log`, `server`, `geo`, `resolver`, `filters`, `annotate`, `groups`, `subscriptions`, `geoblock`, `deadcache`, `fetch`)
- `GeoConfig` — `geo.geofeed` (`GeofeedConfig`) + `geo.dbip` (`DBIPConfig`) + `geo.registry` (`RegistryConfig`) + `geo.asn` (`ASNConfig`); the shared geo providers used by the country/asn filters and by annotation. Provider name constants: `ProviderGeofeed`/`ProviderDBIP`/`ProviderRegistry`/`ProviderASN`.
- `GeofeedConfig` — `sources` + `refresh_interval` with `Validate() error` method
- `ASNConfig` — `timeout` + `cache_ttl` (ASN deny patterns now live on an `{type: asn}` filter entry, not here)
- `DBIPConfig` — `url` + `refresh_interval` for the DB-IP Country Lite download (annotate provider `dbip`); defaults built in (monthly `{yyyy-mm}`-templated gzip-CSV URL, 24h). The literal `{yyyy-mm}` expands to the current UTC month at fetch time; `validateDownloadURL` requires an absolute https URL (placeholder substituted before parsing)
- `RegistryConfig` — `urls` + `refresh_interval` for the RIR delegated-extended downloads (annotate provider `registry`), one URL per RIR; defaults built in (the five `delegated-*-extended-latest` files, 24h)
- `Groups` — `map[string][]string` with `Validate() error` method
- `LogConfig` — `level` (`yaml:"level"`, default `"info"`)
- `FilterConfig` — one entry in the unified `filters:` list. `type` selects the filter (`country`/`asn`/`gemini`/`claude`/`bandwidth`); type-specific fields: country → `provider` (`geofeed`|`asn`, default `geofeed`), `exclude_groups`, `exclude_countries`; asn → `deny_patterns`; bandwidth → `min_mbps *int`, `test_url`, `timeout`, `concurrency`; gemini/claude → optional overrides (`marker`/`model`/`endpoint`/`key_file`/`key_var`/`api_key`/`timeout`/`concurrency`/`version`) merged over `geoblock.{gemini,claude}`
- `AnnotateSpec` — one entry in the ordered `annotate:` list: `tag` (`GEO`/`IP`/`ASN`) + `providers` (ordered lookup chain over `geofeed|dbip|registry|asn`, first provider that answers wins; required for GEO/ASN — defaulted to `[geofeed]` / `[asn]` — and forbidden for IP; unknown and duplicate providers rejected). The retired singular `provider` key is kept in the struct ONLY so a stale config fails loudly (`annotate[i]: "provider" was renamed to "providers" (ordered list)`) instead of the non-strict yaml load silently dropping it
- `IPFilterSpec` / `NodeFilterSpec` — parsed views of `filters` consumed by the two builders: `IPFilterSpecs()` returns country/asn specs (preprocess), `NodeFilterSpecs()` returns gemini/claude/bandwidth specs (stable, gemini/claude merged over geoblock, bandwidth carrying entry params)
- `GeoBlockConfig` — `db_path` + `ttl` + `Gemini GeminiConfig` + `Claude ClaudeConfig` (per-node geo-block list); own `validate()` rejects negative ttl/timeouts/concurrency
- `GeminiConfig` — `endpoint`/`model`/`marker`/`api_key`/`key_file`/`key_var`/`timeout`/`concurrency` (base params for the `gemini` filter); `APIKeyResolved()` reads the key inline or from `key_file` (agenix `KEY=VALUE`). The `gemini` filter is enabled by listing `{type: gemini}` in `filters`.
- `ClaudeConfig` — keyless counterpart for the `claude` filter (`endpoint`/`marker`/`version`/`timeout`/`concurrency`)
- `BandwidthConfig` — through-node download-speed gate params (`test_url`/`min_mbps *int`/`timeout`/`concurrency`), sourced from a `{type: bandwidth}` filter entry. Unset `min_mbps` defaults to 5; explicit `0` = no floor (annotate only).
- `FetchConfig` — `timeout` (per-subscription fetch deadline, default 3s)
- `SubscriptionsConfig` / `CheckConfig` — `/stable.txt` worker settings (`interval`, `sources`, `check.*`). `check` is now URL-test (latency) prober params ONLY (no `filters`/`bandwidth`/`exclude_*` — those moved to the top-level `filters:` list). `CheckConfig.validate` parses `expected_status` with mihomo's `utils.NewUnsignedRanges` (same parser the prober uses) and requires `test_url` to be an absolute http(s) URL (the URL test egresses through the proxy node, so host-side SSRF rules don't apply)
- `SubscriptionSource` — `name` + `url` + `body` (`yaml:"body,omitempty"`). A source carries **either** a fetched `url` **or** an inline `body` (base64/raw newline-joined node URIs). `Subscriptions.Validate` requires a valid `name` (`sourceNameRe`) for both; when `body` is set the URL check is skipped (URL may be empty), otherwise `fetch.ValidatePublicHTTPSURL(url)` is enforced — a source with neither is rejected. `body` is used by the crawler's inline-node harvest (`tg-inline`).
- `DeadCacheConfig` — `ttl` (in-memory short-TTL cache of probe-dead nodes; skips re-probing them; default 2h)

**Key functions:**
- `Load(path) (Config, error)` — read + unmarshal + apply defaults + call `cfg.Validate()`
- `(*Config).Validate() error` — validates geo.geofeed sources, geo.dbip/geo.registry download URLs (absolute https), groups, the `filters`/`annotate` lists (unknown types/tags, country provider + exclude groups/countries, asn deny-pattern regexps, bandwidth knobs, annotate provider chains incl. rejecting the renamed `provider` key), subscriptions/check, geoblock, log level (`zerolog.ParseLevel`), and rejects negative durations across all sections
- `(*Config).IPFilterSpecs() []IPFilterSpec` / `(*Config).NodeFilterSpecs() []NodeFilterSpec` — split the unified `filters` list into the IP-stage (country/asn) and through-node (gemini/claude/bandwidth) specs the two builders consume
- `(*GeofeedConfig).Validate() error` — validates sources are non-empty with valid types
- `(Groups).Validate() error` — validates group names and 2-letter country codes
- `Equal(a, b Config) bool` — deep equality check via `reflect.DeepEqual`; used by reloader to skip no-op reloads
- `GeofeedSourcesChanged(old, newCfg Config) bool` — true when `geo.geofeed.sources` differ; reloader uses this to decide whether to carry over the existing lookup
- `DBIPChanged` / `RegistryChanged (old, newCfg Config) bool` — true when the `geo.dbip` / `geo.registry` block differs; the reloader carries the loaded lookup over when they don't, avoiding a multi-MB re-download per reload
- `ListenChanged` / `MetricsListenChanged(old, newCfg Config) bool` — true when `server.listen` / `server.metrics_listen` changed; reloader logs a warning and ignores the change (both listeners start once; restart required)
- `SubscriptionsChanged` / `GroupsChanged` / `FiltersChanged` / `ProberChanged` / `AnnotateChanged (old, newCfg Config) bool` — reloader re-applies the stable worker when any is true; `FiltersChanged` diffs the `filters` list, `AnnotateChanged` diffs the `annotate` list, `ProberChanged` compares only the geoblock gemini/claude sub-configs (store-only geoblock fields belong to `StoresChanged`)
- `StoresChanged(old, newCfg Config) bool` — true when `geoblock.db_path`/`geoblock.ttl`/`deadcache.ttl` changed; stores are built once at startup, so the reloader logs a restart-required warning
- `reload_coverage_test.go` — reflection walk over `Config`'s yaml leaves asserting every key is classified (live-processor / live-worker / live-other / restart-warned / validation-only); adding a config field without deciding its reload path fails `TestReloadCoverageComplete`

**Uses:** `fetch`, `geofeed`, `mihomo/common/utils`, `zerolog`
**Tags:** `config`, `yaml`, `validation`, `startup`, `defaults`, `diff`, `reload`

---

## `internal/fetch`

`./internal/fetch/fetch.go`

Safe HTTP fetching. Only `https`, no userinfo, no proxy. The **SSRF IP policy lives in the HTTP client's dialer**, not the validators: `NewSafeHTTPClient` refuses resolved non-public IPs at dial time (private/loopback/link-local + CGN/benchmarking/class-E reserved ranges) and backs the shared client for user/content URLs; `NewUnrestrictedHTTPClient` keeps https-only + no-proxy but does **not** restrict IPs — used only by the crawler, which reaches `t.me` through a local fake-ip tunnel and follows scraped links (blind SSRF, no response reflected to any user).

**Key types:**
- `FileType` — `"raw"` | `"gzip"`
- `SubscriptionURL` — lightweight `string` type for subscription URLs
- `StatusError` — typed non-2xx response error (`Code int`, message `bad status: <code> <text>`); callers branch on the code via `errors.As` instead of parsing error text (the dbip previous-month fallback checks 404)

**Key functions:**
- `BytesWithType(ctx, url SubscriptionURL, limit, fileType) ([]byte, error)` — fetch + decode body (uses the shared guarded client)
- `ValidateHTTPSURL(url SubscriptionURL) error` — scheme/host/userinfo check only; no IP restriction
- `ValidatePublicHTTPSURL(url SubscriptionURL) error` — `ValidateHTTPSURL` + reject a literal non-public-IP host (SSRF guard for the `/` endpoint and subscription sources)
- `NewSafeHTTPClient() *http.Client` — guarded transport: non-public resolved IPs refused at dial time
- `NewUnrestrictedHTTPClient() *http.Client` — https-only, no proxy, **no IP guard** (crawler only)
- `MaybeDecode(resp, fileType) (io.ReadCloser, error)` — wrap gzip if needed
- `ValidateFileType(fileType) error` — must be `raw` or `gzip`

**Constants:**
- `UserAgent` — sent on every outbound fetch; exported so `classify` presents the same identity a worker fetch would

**Tags:** `http`, `fetch`, `ssrf`, `security`, `gzip`, `download`, `client`, `redirect`

---

## `internal/geofeed`

`./internal/geofeed/geofeed.go`, `lookup.go`, `dbip.go`, `registry.go`

IP→country data sources and lookup: RFC 8805 geofeed CSV, the DB-IP Country Lite database, and the RIR delegated-extended registry files all parse into the same in-memory indexed lookup. Both address families now use a flat sorted slice with binary search plus a prefix-max array of range ends bounding the most-specific backward walk — the v6 linear scan is gone; v6 mirrors the v4 path over big-endian `uint128` words. Most-specific = smallest span among covering ranges; equal spans resolve to earliest input order.

**Key types:**
- `CountryCode` — strict 2-byte ISO country code (`[2]byte`) with `String()`
- `Entry` — `Prefix` + `Country` (`Country` is `CountryCode`)
- `Range` — inclusive `Start`/`End` (`netip.Addr`, same family) + `Country`; addr pairs rather than `netip.Prefix` because DB-IP and RIR v4 ranges are not CIDR-aligned
- `Source` — `URL` + `Type` (also used in config.yaml via yaml tags)
- `CountryLookup` — interface with `LookupCountry(ip) CountryCode`

**Key functions:**
- `LoadAll(ctx, sources, logger) ([]Entry, error)` — fetch + parse geofeeds; skips a source that fails to fetch/parse (logs a warning) and errors only when NO source yields entries, so one flaky feed can't crash-loop startup
- `Parse(body) ([]Entry, error)` — parse geofeed CSV body (`parseLine`: one `ioutil.UnsafeString` alloc per line, then `strings.Cut`; `parsePrefixOrAddr` uses `addr.BitLen()` instead of hardcoded bit widths)
- `NewLookup(entries) CountryLookup` — indexed lookup from CIDR entries (prefixes convert to ranges)
- `NewRangeLookup(ranges []Range) CountryLookup` — indexed lookup from raw ranges (DB-IP / registry)
- `LookupCountry(lookup, ip) CountryCode` — helper forwarding to the configured lookup
- `ExpandMonthURL(url, now) string` — replaces the literal `{yyyy-mm}` with now's UTC month (DB-IP publishes per UTC calendar month)
- `LoadDBIP(ctx, url, logger) ([]Range, error)` — fetch + parse the DB-IP Country Lite gzip CSV; on a 404 for the current month (not yet published right after rollover) retries once with the previous month (`errors.As` on `*fetch.StatusError`); errors when both fail or nothing parses — the caller (preprocess `geoDB`) degrades to an empty lookup instead of failing startup
- `ParseDBIP(body) []Range` — `start_ip,end_ip,CC` lines, v4/v6 mixed; per-line tolerant (malformed, mixed-family, unordered, and `ZZ` lines skipped)
- `LoadRegistry(ctx, urls, logger) ([]Range, error)` — fetches the RIR delegated-extended files; skips (warns) any single failing RIR so one registry outage can't take down startup, errors only when NO ranges load (mirrors `LoadAll`)
- `ParseDelegated(body) []Range` — `registry|cc|type|start|value|date|status` records; keeps only ipv4/ipv6 with status allocated/assigned and a real country (version header, summary rows, asn records, available/reserved, `ZZ`/`*`/empty countries skipped)
- `fetchBytes` / `timeNow` — package vars so tests stub the network fetch and pin the month templating

**Uses:** `fetch`, `ioutil`
**Tags:** `geofeed`, `dbip`, `registry`, `rir`, `csv`, `geoip`, `prefix`, `range`, `lookup`, `ip-country`

---

## `internal/log`

`./internal/log/log.go`, `ctxlog.go`

Logging package using `github.com/rs/zerolog`. Sets up console logging with timestamps, caller info (short `file:line`), and configurable log level. Supports runtime level changes without restarting.

**Key functions:**
- `InitDefault()` — configure the global `zerolog.Logger` with default `info` level (called from `main()`)
- `InitLogger(level string) zerolog.Logger` — override global level via `zerolog.SetGlobalLevel`, return logger; called after config is loaded
- `SetLevel(level string) error` — change the global zerolog level at runtime via `zerolog.SetGlobalLevel`; returns an error if the level string is unrecognised; called by the reloader when log level changes in config
- `Op(logger, op) zerolog.Logger` — create child logger with `"op"` field (contextual)

**Tags:** `log`, `zerolog`, `logging`, `structured-log`, `contextual`, `runtime-level`

`./internal/ioutil/ioutil.go`

Shared I/O utilities. Created to eliminate duplicated line-iteration and `unsafe`-string patterns across packages.

**Key types:**
- `Lines` — `remain []byte`; iterator pattern with `Next() []byte`

**Key functions:**
- `NewLines(body) Lines` — create iterator
- `(*Lines).Next() []byte` — return next trimmed non-comment line, `nil` when done
- `UnsafeString(b) string` — zero-copy `[]byte` → `string` (safe for nil/empty)

**Tags:** `util`, `iterator`, `unsafe`, `string`, `utility`, `shared`, `dry`

---

## `internal/filter`

`./internal/filter/filter.go`

Country filtering using a compact bitset (`[11]uint64`) for O(1) lookup of 2-letter country codes.

**Key types:**
- `CountrySet [11]uint64` — bitset for AA–ZZ (676 codes)

**Key functions:**
- `(*CountrySet).Has(country CountryCode) bool` — O(1) check
- `(*CountrySet).Add(part string)` — add a single country code (whitespace trimmed, case normalized)
- `(*CountrySet).Exclude(other CountrySet)` — remove one set from another
- `All() CountrySet` — return a set with all 2-letter codes set
- `ParseAllowed(parts ...string) CountrySet` — parse `"DE,US,  nl  "` or `"DE", "US", "nl"` into bitset (uses `strings.SplitSeq`)
- `AllAllowed(lookup, ips, allowed) []netip.Addr` — filter IPs by allowed countries by compacting the input slice in place
- `IsFull(s CountrySet) bool` — true when `s` allows every code (the `All()` set, i.e. no exclusions in effect); the preprocess country filter is a no-op in this case (keeps every IP, including unknown-country ones), which is how an empty `exclude_groups` drops nothing

When no `countries`/`groups` are provided, the server can start with `All()` and subtract `exclude_countries`/`exclude_groups` to implement an inverted filter.

**Uses:** `geofeed`
**Tags:** `filter`, `country`, `bitset`, `geo`, `permit`

---

## `internal/geo`

`./internal/geo/geo.go`

Shared `Provider` abstraction over the in-memory IP→country lookups (geofeed / dbip / registry) and the Team-Cymru ASN resolver, so filtering and annotation reuse the same provider instances.

**Key types:**
- `Info` — resolved geo metadata for an IP (`Country geofeed.CountryCode`, `ASN string`); zero `Country` / empty `ASN` mean unknown
- `Provider` — interface `{ Name() string; Lookup(ctx, ip) Info }`

**Key functions:**
- `NewLookupProvider(name string, current func() geofeed.CountryLookup) Provider` — country provider (replaces `NewGeofeed`) reading the lookup through the getter on each call, so it reflects the processor's background reloads instead of a captured snapshot; backs the `geofeed`, `dbip`, and `registry` providers
- `NewASN(r asnResolver) Provider` — country + AS-name provider backed by the Team-Cymru resolver (`*asn.Resolver` satisfies the local `asnResolver` interface)

**Uses:** `asn`, `geofeed`
**Tags:** `geo`, `provider`, `geofeed`, `asn`, `annotate`, `country`

---

## `internal/resolver`

`./internal/resolver/resolver.go`

DNS resolver for subscription node hostnames. Uses system DNS or custom address. Deduplicates IPv4 results. Process-wide TTL cache (RWMutex map): positive hits cached for `resolver.cache_ttl`, failed/empty lookups negative-cached for `resolver.cache_negative_ttl` (returned as empty result without error); zero TTLs disable caching entirely. Cache keys are cloned (`strings.Clone`) so they never pin the fetched subscription body backing array. Cache is capped at 16384 entries — on overflow expired entries are evicted, or the map is reset when everything is still fresh. preprocess still isolates results once per request/hostname via a pooled resolved-map. When a custom `resolver.address` is configured, `PreferGo: true` is set on the `net.Resolver` so the custom `Dial` function is actually used (the cgo resolver ignores `Dial`).

**Key types:**
- `Resolver` — `timeout` + `dialer` + TTL cache (`map[string]cacheEntry` under `sync.RWMutex`) + `sync.Pool` for resolved maps

**Key functions:**
- `New(timeout, address, cacheTTL, negativeTTL) *Resolver`
- `(*Resolver).Resolve(ctx, host) ([]netip.Addr, error)` — bare IPs returned directly, then cache, then DNS lookup
- `(*Resolver).GetResolvedMap() map[string][]netip.Addr` — get pooled per-request hostname map
- `(*Resolver).PutResolvedMap(m)` — return map to pool

**Tags:** `dns`, `resolve`, `hostname`, `ip`, `pool`, `dedup`, `cache`, `ttl`

---

## `internal/asn`

`./internal/asn/resolver.go`

ASN resolver using Team Cymru DNS (`origin.asn.cymru.com` + `asn.cymru.com`). Results are cached in memory with a configurable TTL (`asn.cache_ttl`, default 24h; a zero/negative value falls back to `defaultCacheTTL`); failures are negative-cached for 5m (`negativeCacheTTL`, cancellation errors excluded) so an unreachable Cymru doesn't serialize per-node timeouts. The cache is capped at 16384 entries with evict-expired-on-insert (same pattern as `internal/resolver`). `CacheLen()` exposes the size. Currently IPv4-only.

**Key types:**
- `Result` — `Country` (`geofeed.CountryCode`) + `Name`
- `Resolver` — `timeout` + `cacheTTL`

**Key functions:**
- `New(timeout, cacheTTL) *Resolver` — a zero/negative `cacheTTL` falls back to the 24h default
- `(*Resolver).Resolve(ctx, ip) (Result, error)` — fresh Cymru lookup (IPv6 rejected)

**Uses:** `net` (stdlib, not internal resolver)
**Tags:** `asn`, `cymru`, `dns`, `ip`, `carrier`, `deny`, `name`

---

## `internal/subscription`

`./internal/subscription/subscription.go`

Subscription fetch, normalize (base64 → raw), and URI parsing. Lightweight node parser avoids `url.Parse` heap allocations. `Normalize` trims, uses a fast-path single-pass ASCII whitespace stripper, then attempts a tolerant base64 decode (all four alphabets, shared with the vmess decoder).

**Key types:**
- `Scheme` — strict URI scheme type alias
- `Node` — `Raw` + `Scheme` + `Name` + `Server` + `Port` + `FragmentIdx`

**Key functions:**
- `Load(ctx, url fetch.SubscriptionURL) ([]byte, error)` — fetch + normalize
- `Parse(body, yield)` — iterate lines via `ioutil.Lines`, parse URIs containing `://`
- `Normalize(body) []byte` — trim + strip ASCII whitespace + base64 decode + URI detection
- `parseNode(line) (Node, bool)` — scheme → authority → host:port → fragment; the fragment is the FIRST `#` after the authority (later `#`s stay in the name); bracketed IPv6 hosts are returned without brackets, unbracketed multi-colon authorities are treated as a portless IPv6 host

**Uses:** `fetch`, `ioutil`
**Tags:** `subscription`, `uri`, `parse`, `node`, `normalize`, `base64`, `vless`, `trojan`

---

## `internal/rewrite`

`./internal/rewrite/rewrite.go`

Node output rewriting. Prepends `[GEO:XX][IP:x.x.x.x]` tags before node name. Strips existing known tags. Alloc-free IPv4 octet writing.

**Key functions:**
- `NodeName(b, node, tags string)` — write the node into a reusable `bytes.Buffer` with the already-formatted tag prefix (e.g. `"[GEO:NL][IP:1.2.3.4]"`) folded into its name; empty `tags` reduces to a clean relabel; vmess folds into the base64 `ps`, URI schemes into the `#fragment`. The tag string is assembled by the preprocess annotator.
- `StripKnownTags(s) string` / `LeadingTags(s) string` — remove / return the leading `[GEO:…]`, `[IP:…]`, `[ASN:…]`, `[SPD:…]`, `[OK]`, `[BAD]`, `[JUR:…]` tags

**Uses:** `subscription`
**Tags:** `rewrite`, `output`, `fragment`, `tag`, `geo-tag`, `ip-tag`

---

## `internal/geoblock`

`./internal/geoblock/store.go`

Persistent per-host geo-block list: node hosts that failed the through-node Gemini reachability check, each with a TTL (default 30d). Backed by SQLite via the pure-Go `modernc.org/sqlite` driver (works under `CGO_ENABLED=0`/distroless). Reads are served from an in-memory cache (the filter hot path); the DB file is touched only on write/prune/load.

**Key types:**
- `Store` — `Open(path, ttl)`, `Blocked(host) bool`, `Block(host) error`, `Prune() error`, `Count() int`, `Close() error`

**Uses:** `modernc.org/sqlite`
**Tags:** `geoblock`, `sqlite`, `ttl`, `blocklist`, `gemini`

---

## `internal/preprocess`

`./internal/preprocess/processor.go`, `filters.go`

Core processing. Orchestrates subscription loading, DNS resolution, geofeed/ASN filtering, and output rewriting per node.

**Key types:**
- `Processor` — geofeed country lookup (async background reload via `TryLock`) + optional dbip/registry `geoDB`s (lazily built, see below) + DNS resolver + sequential filter pipeline (no country cache, no groups map)
- `Stats` — `Total` / `Kept` / `DNSDrop` / `GeoDrop` / `ASNDrop` / `GeoBlockDrop` / `Unsupported`
- `PipelineContext` — request-scoped state shared across filters (`Buffer`, `Lookup`, `Allowed`, `Resolved`, `Scratch`, `Stats`, `IsFirstNode`); `Scratch` is the per-node IP slice handed to filters (they compact in place), keeping the `Resolved` cache pristine across nodes sharing a server
- `Filter` — interface for one IP-stage filter; `Process(ctx, ips, pctx)`
- `GeofeedFilter` — keeps IPs whose geofeed country is in `pctx.Allowed`; a full allow set (`filter.IsFull`) makes it a no-op (keeps everything, incl. unknown country)
- `ASNFilter` — drops IPs matching ASN deny patterns AND IPs whose Cymru-resolved country is not in `AllowedCountries` (so country filtering works via the ASN provider without a geofeed stage)
- `annotator` (`annotator.go`) — builds the ordered `[GEO][IP][ASN]` tag prefix for the chosen IP from the `annotate` specs, then writes the relabeled node via `rewrite.NodeName`; nil when no tags are configured (annotation disabled, raw node emitted). GEO/ASN tags carry an ordered provider chain (`annotTag.providers`, resolved against the map of providers the processor actually built — a referenced-but-missing name is logged and skipped, not panicked): the first provider that answers wins; all-miss renders `[GEO:??]` / `[ASN:??]`.
- `geoDB` — one downloadable in-memory IP→country database (dbip or registry) under the processor's mutex discipline (`mu` guards lookup/loadedAt, `reloadMu` serializes refreshes). Startup: preloaded lookup used as-is; otherwise the initial load runs inline but a failure only WARNs and starts with an empty lookup + zero `loadedAt` (always stale → next request-triggered refresh retries) — startup must never depend on a third-party database mirror, the annotate chain just degrades. Refresh: opportunistic background reload on the request path (`maybeRefreshGeoDBs` → `TryLock`, same trigger point as the geofeed refresh); a failed reload keeps the stale data and still stamps `loadedAt`, so a broken mirror is retried once per interval, not per request.
- `Blocklist` — interface `Blocked(host string) bool` (satisfied by `*geoblock.Store`); when set, `processNode` drops nodes whose `Server` is currently geo-blocked (`GeoBlockDrop`) before DNS resolution, on both `/` and the worker
- `Options` — configuration struct for `NewProcessor` (`GeofeedSources`, `RefreshInterval`, `DNSTimeout`, `DNSAddress`, `DNSCacheTTL`, `DNSCacheNegativeTTL`, `FetchTimeout`, `ASNTimeout`, `ASNCacheTTL`, `IPFilters []config.IPFilterSpec`, `Annotate []config.AnnotateSpec`, `DBIP config.DBIPConfig`, `Registry config.RegistryConfig`, `Blocklist`, plus the `Preloaded*` carry-over fields). `IPFilters` drives the filter chain; `Annotate` builds the annotator (empty → the node's original name passes through). `providerNeeds` decides which geo backends are lazily built: the ASN resolver when any IP filter or annotate chain needs it (asn filter, `country` provider `asn`, or `asn` in a chain), the dbip/registry `geoDB`s only when an annotate chain names them.
- `FilterRequest` — request struct for `Filter` (`SubscriptionURL fetch.SubscriptionURL`, `AllowedCountries filter.CountrySet`, `Body []byte`). When `Body` is non-empty the payload is filtered directly — normalized with the same `subscription.Normalize` (base64-tolerant) used for fetched bodies, **skipping `subscription.Load`/HTTP fetch** — and takes precedence over `SubscriptionURL`; the log context labels it `inline`. URL-source behavior is unchanged.

**Key functions:**
- `NewProcessor(ctx, logger, opts Options) (*Processor, error)` — load geofeed (or use `opts.PreloadedGeofeed` when set; a geofeed load failure IS fatal, unlike the geoDBs), build the geoDBs and ASN resolver per `providerNeeds`, build filter chain, then hand the annotator only the providers actually built
- `(*Processor).Filter(ctx, b, req FilterRequest) (Stats, error)` — main pipeline writing into caller-owned `bytes.Buffer`; a cancelled request returns `ctx.Err()` instead of a truncated list. Inline (`req.Body`) requests skip the fetch entirely and normalize the body in-process.
- `(*Processor).GeofeedState() (geofeed.CountryLookup, time.Time)` — returns the current lookup and load time under read lock; used by the reloader to carry geofeed state across config reloads when sources are unchanged (the underlying `loadedAt`/`refreshInterval` fields are unexported; `shouldReloadGeofeedLocked` requires `p.mu`)
- `(*Processor).DBIPState() / RegistryState() (geofeed.CountryLookup, time.Time)` — same carry-over accessors for the downloadable databases (nil lookup when the provider was not built in this processor)
- `(*Processor).resolveNode(ctx, server, resolved) []netip.Addr` — resolve once per request/hostname and copy resolver results into request-local storage
- `buildFilters(specs []config.IPFilterSpec, asnR) ([]Filter, error)` — construct the IP-stage chain in config order from the parsed specs (country→geofeed/ASN provider, asn→compiled deny patterns + ASN-country). No implicit geofeed force-append: country filtering only happens when a `country` filter is configured. Enforcement of the per-request `AllowedCountries` (from `countries`/`groups`/`exclude_*` on `/`, or `All()`−excludes for the worker) happens inside the country filter.
- `FormatStats(stats) string` — `done: total=N kept=N …`

**Options fields added for hot-reload:**
- `PreloadedGeofeed geofeed.CountryLookup` — when non-nil, `NewProcessor` skips the initial geofeed fetch and uses this lookup directly
- `PreloadedLoadedAt time.Time` — paired with `PreloadedGeofeed`; sets the processor's load timestamp so the background refresh timer is not reset unnecessarily
- `PreloadedDBIP` / `PreloadedDBIPLoadedAt`, `PreloadedRegistry` / `PreloadedRegistryLoadedAt` — same pattern for the dbip/registry databases; used only when the matching provider is referenced by `Annotate`

**Uses:** `asn`, `config`, `filter`, `geo`, `geofeed`, `log`, `resolver`, `rewrite`, `subscription`
**Tags:** `orchestrator`, `pipeline`, `filter`, `geo`, `asn`, `annotate`, `stats`, `hot-reload`

---

## `internal/server`

`./internal/server/server.go`, `holder.go`

HTTP layer using Fiber (`ReadTimeout` 30s, keepalive disabled). Routes: `GET /healthz` → `ok`, `GET /` → preprocess subscription, `GET /stable.txt` → latest stability-tested node list. The active `Processor` and groups map are held in an atomic `Holder` so the reloader can swap them without restarting the server.

The root handler now accepts:
- `subscription_url` (required)
- `countries` / `groups` — additive allowed countries
- `exclude_countries` / `exclude_groups` — countries to remove from the allowed set

If only exclusion params are provided (i.e. `countries` and `groups` are both absent), the allowed set starts from `filter.All()` (every country) minus the exclusions. If `countries`/`groups` are present but produce an empty set, the fallback to `All()` is not applied, so the request fails with `400` when nothing remains. If exclusions remove every allowed country, the request also returns `400`.

`GET /stable.txt` serves the newest `stable.Snapshot` payload (plain-text URI list) with an `X-Stable-Stats` header (`updated=<RFC3339> sources=<ok>/<total> merged=<n> tested=<n> kept=<n>`). Until the first successful check cycle completes it returns `503 stable list not ready`.

**Key types:**
- `Filterer` — interface `Filter(ctx, b, req preprocess.FilterRequest) (Stats, error)`
- `Snapshot` — `Svc Filterer` + `Groups map[string][]string`; the immutable value swapped atomically on reload
- `Holder` — wraps `atomic.Pointer[Snapshot]`; safe for concurrent reads and single-writer stores
- `Server` — `listen` + `fiber.App`

**Key functions:**
- `NewHolder(initial *Snapshot) *Holder` — create a Holder seeded with the startup snapshot
- `(*Holder).Load() *Snapshot` — atomic load of the current snapshot
- `(*Holder).Store(s *Snapshot)` — atomic store of a new snapshot (called by reloader)
- `New(logger zerolog.Logger, listen string, holder *Holder, stableHolder *stable.Holder) *Server` — wires Fiber, logging, and the filter handler; reads `Holder` on every request so reloads are picked up without restart
- `newIndexHandler(holder *Holder) fiber.Handler` — root handler: loads snapshot, validates URL, builds allowed/excluded sets, calls `Filterer`
- `newStableHandler(holder *stable.Holder) fiber.Handler` — serves the stable payload or `503` before the first cycle
- `buildCountrySet(rawCountries, rawGroups, groupsMap) CountrySet` — HTTP-layer group expansion (used for both allowed and excluded sets)
- `isEmpty(set) bool` — checks whether a `CountrySet` has any country set
- `(*Server).Listen() error`
- `(*Server).Shutdown(ctx) error`
- `(*Server).TestApp() *fiber.App` — for test usage

**Uses:** `fetch`, `filter`, `preprocess`, `stable`, `fiber`
**Tags:** `http`, `fiber`, `api`, `handler`, `server`, `healthz`, `atomic-swap`, `hot-reload`

---

## `internal/stable`

`./internal/stable/stable.go`, `merge.go`, `select.go`, `report.go`, `prober.go`, `prober_api.go`, `prober_gemini.go`, `prober_claude.go`, `prober_bandwidth.go`, `nodefilter.go`, `checker.go`, `controller.go`, `progress.go`, `deadset.go`

Background worker that produces a stability-tested subscription list. Every `subscriptions.interval` it fetches each configured source through the geo/ASN pipeline (`Filterer`), merges the results into one deduplicated relabeled URI list, probes every node with the embedded mihomo library (`URLTest` HEAD requests, `check.rounds` rounds), keeps only nodes within `check.max_fail`/`check.max_avg_ms`, then runs a **Gemini reachability gate** through each surviving node (a real API `GET`, body-inspected for the geo-block marker — the check mihomo's HEAD-only `URLTest` cannot do), records geo-blocked node hosts in the `geoblock` store (TTL) and drops them, and atomically publishes the rest for `GET /stable.txt`. Every failure mode (all sources down, zero parsable nodes, prober error, zero survivors) keeps the previous snapshot.

**Key types:**
- `Stats` — `SourcesOK/SourcesTotal/Merged/Tested/Kept` counters for the `X-Stable-Stats` header
- `Snapshot` — immutable `Payload []byte` + `UpdatedAt` + `Stats`
- `Holder` — `atomic.Pointer[Snapshot]`; `Load()` returns nil before the first successful cycle
- `SourceBody` / `Entry` — merge input (source name + fetched body) and output. `Entry.Raw` is the clean `<source>-NNN` name used for probing; `Entry.Tagged` is the published name (`Raw` plus the `[GEO][IP]` annotation carried over from the filter pass, when present); `Addr` is the server:port dead-cache key. `BuildPayload` emits `Tagged`.
- `ProbeResult` / `Survivor` — per-node probe aggregate and selected node with mean delay; `Survivor.Mbps` holds the bandwidth filter's measured speed (0 when the filter is off)
- `Filterer` — local copy of `server.Filterer` (avoids an import cycle); satisfied by `*preprocess.Processor`
- `Prober` — `Probe(ctx, payload) (map[string]ProbeResult, error)`; implemented by `MihomoProber`
- `Blocklist` — `Block(host)`, the gemini geo-block store (`*geoblock.Store`, SQLite/30d). `DeadCache` — `Blocked(key)/Block(key)/Prune()`, the dead-node cache; satisfied by `*DeadSet` (in-memory, not persisted — dead nodes are cheap to re-probe after a restart)
- `GeminiOutcome` — per-node through-node Gemini result (`Server`/`Reachable`/`Blocked`)
- `Checker` — the periodic worker loop
- `Controller` — start/stop lifecycle around `Checker`, driven by config (re)loads
- `CycleReport` / `SourceReport` / `FilterReport` — per-cycle accounting (aggregate counts + per-source preprocess drops + per-filter in/kept/dropped-by-reason + kept-node speeds + duration), assembled by `RunOnce` from data otherwise only logged
- `Reporter` — nil-safe metrics sink: `Observe(CycleReport)` on a published cycle, `ObserveError()` on any abort/no-op; `*metrics.Metrics` implements it

**Key functions:**
- `Merge(bodies []SourceBody) []Entry` — dedupe by lowercased `server:port` first-wins in source order (`Entry.Addr` shares the lowercased key); relabel fragments to `<source>-NNN`
- `SelectSurvivors(entries, results, rounds, maxFail, maxAvgMs) []Survivor` — keep `rounds-successes <= maxFail && mean <= maxAvgMs`, sort by mean ascending
- `BuildPayload(survivors) []byte` — newline-joined URI list
- `NewMihomoProber(cfg config.CheckConfig, bandwidth config.BandwidthConfig, gemini config.GeminiConfig, geminiKey string, claude config.ClaudeConfig, logger) (*MihomoProber, error)` — latency `Probe` (HEAD `URLTest`) plus through-node API checks: `GeminiCheck` + `GeminiEnabled()` (needs a key), keyless `ClaudeCheck`, and `BandwidthCheck`/`BandwidthMinMbps` (from the injected `bandwidth` config). API checks run through the shared `apiCheck` fan-out (mihomo `DialContext` + fixed-conn `http.Transport`, `GET` via `apiProbeOne`) and scan the body for the geo-block marker (Gemini: location marker; Claude: 403 `Request not allowed`).
- `NodeFilter` — through-node check applied after the IP-filters + latency probe, routing THROUGH each surviving node (worker-only, so it shapes `/stable.txt`, not `/`); selected from the unified `filters` list via `cfg.NodeFilterSpecs()` (types `gemini`/`claude`/`bandwidth`). `buildNodeFilters(names, prober, store, annotate, logger)` constructs them; one generic `apiFilter{name, enabled, check, store}` implements the interface for gemini (key-gated) and claude (keyless), each keeping API-reachable survivors and recording blocked hosts in the geoblock store.
- The `bandwidth` `NodeFilter` (`bandwidthFilter`) downloads the bandwidth filter's `test_url` through each survivor (`BandwidthCheck` → `bandwidthProbeOne`, `Accept-Encoding: identity` + body-transfer timing; `computeMbps` guards divide-by-zero), drops nodes below `min_mbps` and unreachable ones, records `Survivor.Mbps`, and — when annotation is enabled (`len(cfg.Annotate) > 0`) — prepends `[SPD:<n>M]` to the published name via the vmess-aware `relabelNode`. No store: results are never cached.
- `NewChecker(...)` / `(*Checker).Run(ctx)` — immediate first cycle, then ticker; `RunOnce(ctx) error` is one cycle: fetch sources concurrently (results kept in config order so first-source-wins is deterministic), drop dead-cached nodes before probing, probe the rest, record no-success nodes as dead (short TTL), `SelectSurvivors`, then apply the configured `NodeFilter`s. A cancelled/failed probe aborts the cycle: the previous snapshot is kept and nothing is recorded dead (a reload/shutdown mid-probe can't poison the dead cache). `Probe` shares ONE semaphore across rounds, so `check.concurrency` caps total in-flight URL tests. `fetchSources` builds each `preprocess.FilterRequest` per source: when `src.Body != ""` it passes `Body: []byte(src.Body)` with an empty `SubscriptionURL` (inline path, no fetch), otherwise the usual `SubscriptionURL: fetch.SubscriptionURL(src.URL)`; the local `Filterer` interface stays a single `Filter(...)` method.
- `NewController(ctx, holder, filterer func() Filterer, store Blocklist, dead DeadCache, logger, reporter Reporter)` / `(*Controller).Apply(cfg) error` / `(*Controller).Stop()` — `Apply` builds the allowed `CountrySet` from the `country` filter entries' `exclude_groups`/`exclude_countries` (via `cfg.IPFilterSpecs()` + `cfg.Groups`), resolves the Gemini key, and builds the prober + `NodeFilterSpecs`-selected filters BEFORE stopping the old worker (a failed construction leaves the previous worker running), then starts a new one when `subscriptions.sources` is non-empty; `Stop` is idempotent. The `reporter` (nil-safe) is threaded into every `Checker` it builds.

**Uses:** `config`, `filter`, `fetch`, `preprocess`, `subscription`, `mihomo` (adapter, common/convert, common/utils, constant)
**Tags:** `stable`, `probe`, `url-test`, `gemini`, `claude`, `bandwidth`, `speed`, `geoblock`, `delay`, `worker`, `mihomo`, `atomic-swap`

---

## `internal/metrics`

`./internal/metrics/metrics.go`

Renders the stable worker's per-cycle stats as Prometheus text exposition (hand-rolled — no `client_golang`, avoiding the `google.golang.org/protobuf => metacubex/protobuf-go` replace). Served on an internal listener (`server.metrics_listen`, default `:9090`) that docker-compose publishes loopback-only (`127.0.0.1:9091:9090`); the server's NixOS Prometheus scrapes it and a provisioned Grafana dashboard renders the funnel + trends.

**Key exports:**
- `Metrics` — holds the last `stable.CycleReport` + lifetime counters (RWMutex); `New()` constructs it
- `(*Metrics).Observe(stable.CycleReport)` / `ObserveError()` — satisfies `stable.Reporter`; both count toward `stable_cycles_total`, ObserveError also bumps `stable_cycle_failures_total`
- `(*Metrics).Handler() http.Handler` — renders `/metrics` into a buffer under a read lock, so a slow scrape never blocks `Observe`

**Metrics:** `stable_kept_nodes`, `stable_merged_nodes`, `stable_probed_nodes`, `stable_dead_skipped_nodes`, `stable_sources_ok`/`_total`, `stable_cycle_duration_seconds`, `stable_last_success_timestamp_seconds`, `stable_cycles_total`/`_failures_total`, `stable_filter_{in,kept,dropped}_nodes{filter,reason}`, `stable_source_{nodes_total,kept_nodes,dropped_nodes}{source,reason}`, `stable_kept_speed_mbps` (histogram).

**Uses:** `stable`
**Tags:** `metrics`, `prometheus`, `grafana`, `exposition`, `observability`

---

## `internal/reload`

`./internal/reload/reloader.go`, `watcher.go`, `options.go`

Config hot-reload. Watches the config file **and its `private.yaml` / `sources.yaml` overlay siblings** for changes (via fsnotify on the parent directory), debounces bursts, and atomically swaps the active `Processor` + groups into the server `Holder`. On any error the previous settings are kept unchanged. The overlays matter because the crawler writes `private.yaml` and `sources.yaml` carries curated sources, and a change to either must restart the stable worker.

**Key types:**
- `Reloader` — holds `path`, `*server.Holder`, `zerolog.Logger`, and the last-applied `config.Config` + `*preprocess.Processor` for diffing
- `Applier` — interface `Apply(config.Config) error`; satisfied by `*stable.Controller` (enables fake controllers in tests)
- `Watcher` — wraps `*fsnotify.Watcher`; watches the config file's parent directory to survive atomic-rename writes; fires `onChange` for events on `config.yaml` or its `private.yaml` / `sources.yaml` siblings; debounces events with a 200 ms window

**Key functions:**
- `NewReloader(path string, holder *server.Holder, logger zerolog.Logger, cfg config.Config, proc *preprocess.Processor, ctl Applier, blocklist preprocess.Blocklist) *Reloader` — seed with startup state so the first reload can diff against it; injects the shared geo-block store into every rebuilt `Processor`
- `(*Reloader).Reload(ctx context.Context)` — load config → skip if `Equal` → build `OptionsFromConfig` (+ inject `Blocklist`) → carry geofeed state if `!GeofeedSourcesChanged` and dbip/registry state if `!DBIPChanged` / `!RegistryChanged` (all diffed against the config that BUILT the current processor, so a failed-Apply divergence can't carry data across the wrong source set; a nil state — provider not built — simply leaves the preload unset) → `NewProcessor` → `SetLevel` → warn if `ListenChanged` or `StoresChanged` (restart required) → `holder.Store` new snapshot → `ctl.Apply(newCfg)` when `SubscriptionsChanged || GroupsChanged || FiltersChanged || ProberChanged || AnnotateChanged`. On a failed `Apply`, `currentCfg` is NOT committed, so re-saving the file retries instead of hitting the `Equal` fast path (the old worker keeps running — Apply builds before it stops).
- `NewWatcher(configPath string, onChange func(context.Context), logger zerolog.Logger) (*Watcher, error)` — register fsnotify watch on parent directory; return error if watcher or directory watch fails
- `(*Watcher).Run(ctx context.Context) error` — event loop: debounce matching events, call `onChange` once per burst; close fsnotify watcher on ctx cancellation and return nil (callers use the return as a join point)
- `OptionsFromConfig(cfg config.Config) preprocess.Options` — single source of truth for mapping `config.Config` to `preprocess.Options` (incl. `geo.dbip` → `DBIP`, `geo.registry` → `Registry`); leaves every `Preloaded*` field unset (callers decide whether to carry over geofeed/dbip/registry state)

**Uses:** `config`, `geofeed`, `log`, `preprocess`, `server`, `stable`, `fsnotify`
**Tags:** `reload`, `fsnotify`, `hot-reload`, `watch`, `atomic-swap`, `debounce`

---

## `internal/classify`

`./internal/classify/classify.go`

Decides whether a URL serves a usable Mihomo-compatible subscription, reusing the project's HTTP client (the caller supplies it — the crawler an unrestricted client, the `classify` CLI a guarded one) and the same normalizer/parser the preprocessor uses. Used by the `crawl` subcommand and the `classify` CLI subcommand.

**Key types:**
- `Result{Nodes int, Expired bool}` — `(Result).Live()` reports `Nodes > 0 && !Expired`

**Key functions:**
- `Body(body []byte, subUserinfo string, now int64) Result` — pure classifier: base64-normalizes the body, counts only **proxy-scheme** nodes (`vless`/`vmess`/`ss`/`ssr`/`trojan`/`tuic`/`hysteria`/`hysteria2`/`hy2`/`anytls` — so HTML pages full of `https://` links are rejected), and marks expired from a `subscription-userinfo: expire=` header
- `URL(ctx, client *http.Client, rawURL fetch.SubscriptionURL) (Result, error)` — scheme-validate + fetch + `Body`; the IP/SSRF policy comes from the passed client

**Uses:** `fetch`, `subscription`
**Tags:** `classify`, `subscription`, `ssrf`, `crawler`

---

## `internal/crawl`

`./internal/crawl/crawl.go`, `discover.go`, `state.go`, `channels.go`

Format-agnostic, recursive subscription crawler. Scrapes public Telegram channel web previews (`t.me/s/<channel>` + `?before=` pagination), treats **every** https link as a candidate, keeps the ones that `classify` as a live subscription, and writes them to the `private.yaml` overlay as `tg-<channel-slug>-<sha6>` sources — the discovering channel (first-wins in BFS order, so seeds beat discovered channels) plus a 6-hex URL hash; `channelSlug` folds the Telegram slug into the config name alphabet (`^[a-z0-9-]+$`, `_`→`-`, 24-byte cap). An already-attributed name is kept verbatim across cycles (renames would churn private.yaml and relabel published nodes); a legacy pure-hash `tg-<sha10>` name upgrades the first time its URL is rediscovered in a channel; a name collision or unknown origin falls back to the `tg-<sha10>` form (`sourceName`). Matches the artifact (a URL that returns a subscription), not any channel-specific wrapper pattern. Runs as the `crawl` subcommand in the same image as the service. One **unrestricted** `http.Client` (no IP guard — it reaches `t.me` through the host's local fake-ip tunnel and follows links scraped from channel content) is held on the `Crawler` and reused for pages + classify batches. Cycle hygiene: a cancelled ctx aborts before any state/private.yaml write.

**Key types:**
- `Options{Channels []string, ChannelsPath string, PrivatePath string, Pages int, Prune bool, MaxDepth int, MaxChannels int, StatePath string, StateTTL time.Duration, InlineEnabled bool, InlineMax int}`
- `Crawler` — `New(opts, logger)`; `RunOnce(ctx)` one cycle, `Run(ctx, interval)` loop

**Behavior:** `scan` (in `discover.go`) does a **relevance-gated BFS** over the channel repost graph — seeds are crawled unconditionally, a discovered channel (`extractChannels`: forwarded-from/@mention `t.me/<slug>` links, excluding self/reserved/bot `?start=`) is expanded only if it itself yielded a live subscription; the subscription yield is the thematic signal (a VPN channel forwards VPN channels; a news channel yields nothing and its branch stops). Depth is bounded by `MaxDepth`; `MaxChannels` caps discovered (non-seed) visits per cycle (`0` = unlimited). A per-cycle `visited` set means every channel is fetched at most once and a repost loop (A→B→A) can never re-enter an explored channel. Page fetches are sequential (rate-limit friendly). Validates candidate URLs via `fetch.ValidateHTTPSURL` (scheme only — no IP guard) and skips Telegram/CDN noise hosts before fetching; classifies concurrently; managed (`tg-`) sources are fully derived from currently-live URLs (implicit prune when `Prune`), hand-added private sources are preserved; only rewrites `private.yaml` (atomic temp+rename) when the managed set changes, so unchanged cycles trigger no reload.

**Persistence (`state.go`):** channels that yield a live subscription are recorded in a JSON state file (`StatePath`, default `/config/.crawler-state.json`) and become **permanent depth-0 seeds** on future cycles — always crawled and always expanded — so a proven-productive channel keeps growing the graph even on days its recent pages carry no live sub. Entries with no live sub for `StateTTL` (default 30d) are pruned; empty `StatePath` disables persistence.

**Inline-node harvest (`InlineEnabled`, default on):** alongside https subscription links, each scraped page is scanned by `extractInlineNodes` for **raw proxy URIs pasted directly in messages** (`vless|vmess|ss|ssr|trojan|tuic|hysteria|hysteria2|hy2|anytls://…`, HTML-unescaped, trailing punctuation trimmed). Per cycle the URIs are accumulated across all pages, parsed with `subscription.Parse`, deduped by lowercased `server:port` (first-wins, mirroring `stable.Merge`), and capped to `InlineMax` (default 500, first N kept). When ≥1 node survives, `buildInlineSource` packs the kept node URIs into a single base64 `Body` under a managed `tg-inline` source appended to `private.yaml` (empty-URL source → the stable checker filters `Body` directly, no fetch); a cycle with 0 inline nodes omits the source. `mergeManaged` skips existing `Body` sources so `tg-inline` is regenerated fresh each cycle, and `sameSources` includes `Body`, so a changed inline set triggers a private.yaml rewrite + reload. Env: `CRAWL_INLINE` (default `true`) toggles the harvest; `CRAWL_INLINE_MAX` (default `500`) sets the cap.

**Seed config (`channels.go`):** seed channels live in `config/channels.yaml` (`ChannelsPath`, `{channels: [slug|@handle|t.me-url]}`), analogous to `config.yaml`/`private.yaml`. `loadChannels` is best-effort (missing/malformed → no channels, never fatal) and re-read every cycle, so adding a channel hot-reloads on the next cycle without a restart. Effective seeds = `channels.yaml` ∪ `CRAWL_CHANNELS` env ∪ remembered productive channels.

**Uses:** `classify`, `fetch`, `subscription` (via classify), `yaml.v3`, `zerolog`
**Tags:** `crawl`, `telegram`, `subscription`, `private-overlay`, `ssrf`, `sidecar`

---
## Dependency Graph

```
main
 ├─ app
 │   ├─ config ─── fetch, geofeed
 │   ├─ log        (zerolog initialization)
 │   ├─ geoblock   (SQLite TTL geo-block list; modernc pure-Go; injected into preprocess/stable via interfaces)
 │   ├─ preprocess
 │   │   ├─ asn        (Team Cymru DNS)
 │   │   ├─ config     (IPFilterSpecs, AnnotateSpec — filter/annotate consts)
 │   │   ├─ geo        (geofeed + ASN providers, shared by filters + annotator)
 │   │   ├─ filter ─── geofeed (lookup)
 │   │   ├─ geofeed ── fetch, ioutil
 │   │   ├─ log        (ctxlog.Op helper)
 │   │   ├─ resolver   (hostname DNS)
 │   │   ├─ rewrite ── subscription
 │   │   └─ subscription ── fetch, ioutil
 │   ├─ reload
 │   │   ├─ config     (Load, Equal, GeofeedSourcesChanged, DBIPChanged, RegistryChanged, ListenChanged, SubscriptionsChanged, GroupsChanged, FiltersChanged, ProberChanged, AnnotateChanged)
 │   │   ├─ log        (SetLevel)
 │   │   ├─ preprocess (NewProcessor, Options, GeofeedState, DBIPState, RegistryState)
 │   │   ├─ server     (Holder, Snapshot)
 │   │   └─ stable     (Controller.Apply on subscriptions/groups/filters/prober/annotate change)
 │   ├─ stable
 │   │   ├─ config     (SubscriptionsConfig, CheckConfig, IPFilterSpecs, NodeFilterSpecs)
 │   │   ├─ filter     (allowed CountrySet)
 │   │   ├─ fetch      (SubscriptionURL type)
 │   │   ├─ preprocess (FilterRequest via Filterer)
 │   │   ├─ subscription (Parse for merge/dedupe)
 │   │   └─ mihomo     (adapter, convert, utils, constant)
 │   └─ server ─── fetch, filter, preprocess, stable
 ├─ crawl ─── classify, fetch, yaml.v3
 └─ classify ─── fetch, subscription
```

## Quick Tag Index

| Tag | Package |
|---|---|
| `ssrf`, `http-client` | `fetch` |
| `geoip`, `csv`, `prefix`, `dbip`, `registry` | `geofeed` |
| `bitset`, `country-filter` | `filter` |
| `dns`, `hostname-resolve` | `resolver` |
| `asn`, `cymru`, `carrier-deny` | `asn` |
| `uri-parse`, `node`, `base64` | `subscription` |
| `geo-tag`, `output-rewrite` | `rewrite` |
| `pipeline`, `orchestrator` | `preprocess` |
| `atomic-swap`, `http-handler` | `server` |
| `config`, `yaml`, `defaults`, `diff` | `config` |
| `bootstrap`, `wire`, `hot-reload` | `app` |
| `log`, `zerolog`, `structured-log`, `runtime-level` | `log` |
| `shared-iterator`, `unsafe-string` | `ioutil` |
| `fsnotify`, `watch`, `debounce`, `hot-reload` | `reload` |
| `stable`, `probe`, `url-test`, `gemini`, `mihomo` | `stable` |
| `metrics`, `prometheus`, `grafana`, `observability` | `metrics` |
| `geoblock`, `sqlite`, `ttl`, `blocklist` | `geoblock` |
