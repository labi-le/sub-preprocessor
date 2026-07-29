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
- `shutdownTimeout = 15s` — server drain budget; above the 5s DNS/ASN lookup an in-flight request can be blocked in, so a routine SIGTERM does not exit non-zero. Expiry is a warning, not an error
- `controllerStopTimeout = 5s` — bound on joining the stable worker; `shutdownTimeout + controllerStopTimeout` must stay under the supervisor's `stop_grace_period`

**Internal functions:**
- `startMetrics(addr, m, logger) (*http.Server, error)` — binds the metrics listener with `net.Listen` BEFORE returning, so a port conflict is a startup error instead of a detached goroutine's log line; `srv.Addr` reports the bound address. Callers skip an empty addr
- `stopController(ctl, logger)` — cancel + join the stable worker under `controllerStopTimeout`, degrading a non-cooperative stage to a warning instead of a hung shutdown
- `gracefulShutdown(ctx, srv, logger) error` — drains the server within `shutdownTimeout`; a deadline expiry is logged at warn and returns nil, anything else is wrapped as an error

**Wiring (inside `Run`):**
- Builds `server.Holder` seeded with the startup `Snapshot`
- Creates `server.New(logger, listen, holder)` (no longer passes `svc`/`groupsMap` directly)
- Creates `reload.NewReloader` seeded with startup `cfg` + `svc`
- Creates `reload.NewWatcher` with `reloader.Reload` as the `onChange` callback
- Runs watcher in a goroutine under a derived cancellable context; a deferred cancel+join (`<-watcherDone`) runs before the `stopController(ctl)`/`gbStore.Close()` defers on EVERY return path (incl. listen error), so an in-flight reload can never race teardown

**Uses:** `config`, `geoblock`, `log`, `preprocess`, `reload`, `server`, `stable`
**Tags:** `bootstrap`, `wire`, `shutdown`, `lifecycle`, `hot-reload`

---

## `internal/config`

`./internal/config/config.go`

YAML config loading and validation. Uses `gopkg.in/yaml.v3` with **`KnownFields(true)`**: `config.yaml` and both overlays decode strictly, so a misspelled key fails the load naming the key instead of silently falling back to the default (an empty or comment-only overlay still loads — the decoder's `io.EOF` is not an error). `Load` merges sibling overlays when present — `sources.yaml` (curated subscription sources) and `private.yaml` (crawler-managed sources) — appending their `subscriptions.sources`; a read error other than not-exist fails the load. Also provides diff helpers used by the reloader to decide what changed.

**Key types:**
- `Config` — root config struct (`log`, `server`, `geo`, `resolver`, `filters`, `annotate`, `groups`, `subscriptions`, `geoblock`, `deadcache`, `fetch`)
- `GeoConfig` — `geo.geofeed` (`GeofeedConfig`) + `geo.dbip` (`DBIPConfig`) + `geo.registry` (`RegistryConfig`) + `geo.asn` (`ASNConfig`); the shared geo providers used by the country/asn filters and by annotation. Provider name constants: `ProviderGeofeed`/`ProviderDBIP`/`ProviderRegistry`/`ProviderASN`.
- `GeofeedConfig` — `sources` + `refresh_interval *time.Duration` with `Validate() error` method. Unset `refresh_interval` defaults to 24h; explicit `0` means "load once, never refresh". The dbip/registry siblings read `0` differently — see below
- `ASNConfig` — `timeout` + `cache_ttl` (ASN deny patterns now live on an `{type: asn}` filter entry, not here)
- `DBIPConfig` — `url` + `refresh_interval *time.Duration` for the DB-IP Country Lite download (annotate provider `dbip`); defaults built in (monthly `{yyyy-mm}`-templated gzip-CSV URL, 24h). Unlike `GeofeedConfig`, an explicit `0` is coerced to the 24h default, which is what it meant before the field became a pointer: freezing a month-stamped mirror for the process lifetime has no legitimate use (refresh rarely with a long interval instead), and a non-positive interval also makes `preprocess.geoDB.staleLocked` return before it consults `retryAt`, so a failed initial download could never retry. A negative value is still rejected by `Validate`. The literal `{yyyy-mm}` expands to the current UTC month at fetch time; `validateDownloadURL` requires an absolute https URL (placeholder substituted before parsing)
- `RegistryConfig` — `urls` + `refresh_interval *time.Duration` for the RIR delegated-extended downloads (annotate provider `registry`), one URL per RIR; defaults built in (the five `delegated-*-extended-latest` files, 24h), and `refresh_interval` carries `DBIPConfig`'s explicit-`0`-means-default semantics
- `Groups` — `map[string][]string` with `Validate() error` method
- `LogConfig` — `level` (`yaml:"level"`, default `"info"`)
- `FilterConfig` — one entry in the unified `filters:` list. `type` selects the filter (`country`/`asn`/`gemini`/`claude`/`chatgpt`/`tidal`/`bandwidth`); type-specific fields: country → `provider` (`geofeed`|`asn`, default `geofeed`), `exclude_groups`, `exclude_countries`; asn → `deny_patterns`; bandwidth → `min_mbps *int`, `test_url`, `timeout`, `concurrency`; gemini/claude/chatgpt/tidal → optional overrides (`marker`/`model`/`endpoint`/`key_file`/`key_var`/`api_key`/`timeout`/`concurrency`/`version`) merged over `geoblock.{gemini,claude,chatgpt,tidal}`. **`exclude_groups`/`exclude_countries` are worker-only**: their single consumer is `Config.DeniedCountries()` (`/stable.txt`); preprocess never receives them, so on `GET /` the country constraint comes from the query params alone
- `AnnotateSpec` — one entry in the ordered `annotate:` list: `tag` (`GEO`/`IP`/`ASN`) + `providers` (ordered lookup chain over `geofeed|dbip|registry|asn`, first provider that answers wins; required for GEO/ASN — defaulted to `[geofeed]` / `[asn]` — and forbidden for IP; unknown and duplicate providers rejected). The retired singular `provider` key needs no field: the strict decode rejects it by name, as it does every future rename
- `IPFilterSpec` / `NodeFilterSpec` — parsed views of `filters` consumed by the two builders: `IPFilterSpecs()` returns country/asn specs (preprocess; `Type`/`Provider`/`DenyPatterns` only), `NodeFilterSpecs()` returns gemini/claude/chatgpt/tidal/bandwidth specs (stable, the API specs merged over geoblock, bandwidth carrying entry params)
- `GeoBlockConfig` — `db_path` + `ttl` + `Gemini GeminiConfig` + `Claude ClaudeConfig` + `ChatGPT ChatGPTConfig` + `Tidal TidalConfig` (per-node geo-block store plus the base params of every through-node API check; tidal's live here for uniformity although the tidal filter never writes to the store); own `validate()` rejects negative ttl/timeouts/concurrency
- `GeminiConfig` — `endpoint`/`model`/`marker`/`api_key`/`key_file`/`key_var`/`timeout`/`concurrency` (base params for the `gemini` filter); `APIKeyResolved()` reads the key inline or from `key_file` (agenix `KEY=VALUE`). The `gemini` filter is enabled by listing `{type: gemini}` in `filters`.
- `ClaudeConfig` — keyless counterpart for the `claude` filter (`endpoint`/`marker`/`version`/`timeout`/`concurrency`)
- `ChatGPTConfig` — keyless params for the `chatgpt` filter (`endpoint`/`marker`/`timeout`/`concurrency`); defaults `https://api.openai.com` + `unsupported_country`, the code OpenAI's `/compliance/cookie_requirements` returns (403) for an egress it refuses
- `TidalConfig` — keyless params for the `tidal` filter (`endpoint`/`timeout`/`concurrency`); default `https://api.tidal.com`. `GET /v1/country` answers 200 `{"countryCode":"XX"}` for an egress Tidal accepts; where it refuses one, the request never reaches the API (CloudFront 403 + HTML, measured from a RU egress). The gate is **fail-closed**: kept only on 2xx with a parseable code. The code itself is NOT compared against Tidal's 61 markets — that list gates where a subscription can be bought, while an existing subscriber streams from unsold countries too
- `BandwidthConfig` — through-node download-speed gate params (`test_url`/`min_mbps *int`/`timeout`/`concurrency`), sourced from a `{type: bandwidth}` filter entry. Unset `min_mbps` defaults to 5; explicit `0` = no floor (annotate only).
- `FetchConfig` — `timeout` (per-subscription fetch deadline, default 3s)
- `SubscriptionsConfig` / `CheckConfig` — `/stable.txt` worker settings (`interval`, `sources`, `check.*`). `check` is now URL-test (latency) prober params ONLY (no `filters`/`bandwidth`/`exclude_*` — those moved to the top-level `filters:` list). `SubscriptionsConfig.Validate` checks `interval` and the whole `check` block **unconditionally** and the source list separately (`validateSources`, re-run after each overlay merge), so a bad prober param cannot boot clean on empty overlays and then fail every reload once the crawler writes its first source. `CheckConfig.validate` parses `expected_status` with mihomo's `utils.NewUnsignedRanges` (same parser the prober uses) and requires `test_url` to be an absolute http(s) URL (the URL test egresses through the proxy node, so host-side SSRF rules don't apply)
- `SubscriptionSource` — `name` + `url` + `body` (`yaml:"body,omitempty"`). A source carries **either** a fetched `url` **or** an inline `body` (base64/raw newline-joined node URIs). `Subscriptions.Validate` requires a valid `name` (`sourceNameRe`) for both; when `body` is set the URL check is skipped (URL may be empty), otherwise `fetch.ValidatePublicHTTPSURL(url)` is enforced — a source with neither is rejected. `body` is used by the crawler's inline-node harvest (`tg-inline`).
- `DeadCacheConfig` — `ttl` (in-memory short-TTL cache of probe-dead nodes; skips re-probing them; default 2h)

**Key functions:**
- `Load(path) (Config, error)` — read + strict unmarshal (`decodeStrict`) + apply defaults + call `cfg.Validate()`
- `(*Config).Validate() error` — validates `resolver.address` (must be `host:port`; a portless value dials nothing and drops every node as a DNS failure), geo.geofeed sources, geo.dbip/geo.registry download URLs (absolute https), groups, the `filters`/`annotate` lists (unknown types/tags, country provider + exclude groups/countries, asn deny-pattern regexps, api timeout/concurrency, bandwidth knobs, annotate provider chains), subscriptions/check, geoblock, log level (`zerolog.ParseLevel`), and rejects negative durations across all sections. `validateCountryList`/`validateCountryCode` are the shared ISO-code validators (the error is prefixed with the caller's path, e.g. `filters[0].exclude_countries`)
- `(*Config).IPFilterSpecs() []IPFilterSpec` / `(*Config).NodeFilterSpecs() []NodeFilterSpec` — split the unified `filters` list into the IP-stage (country/asn) and through-node (gemini/claude/chatgpt/tidal/bandwidth) specs the two builders consume
- `(*Config).DeniedCountries() filter.CountrySet` — the stable worker's country deny-set, expanded from every `country` filter entry's `exclude_countries` + `exclude_groups` (via `Groups`). The only reader of those two keys; a deny-set rather than the complement allow-set so a node no geo source can place is kept, not dropped
- `(*GeofeedConfig).Validate() error` — validates sources are non-empty with valid types
- `(Groups).Validate() error` — validates group names and 2-letter country codes
- `Equal(a, b Config) bool` — deep equality check via `reflect.DeepEqual`; used by reloader to skip no-op reloads
- `GeofeedSourcesChanged(old, newCfg Config) bool` — true when `geo.geofeed.sources` differ; reloader uses this to decide whether to carry over the existing lookup
- `DBIPChanged` / `RegistryChanged (old, newCfg Config) bool` — true when the `geo.dbip` / `geo.registry` block differs; the reloader carries the loaded lookup over when they don't, avoiding a multi-MB re-download per reload
- `ResolverChanged` / `ASNChanged (old, newCfg Config) bool` — true when the whole `resolver` / `geo.asn` block differs; the reloader carries the live resolver (and its warm DNS/Cymru cache) over when they don't, so the configured `cache_ttl` is reachable instead of being reset by every reload. Whole-block granularity is deliberate: the timeouts and TTLs are baked into the resolver at construction
- `ListenChanged` / `MetricsListenChanged(old, newCfg Config) bool` — true when `server.listen` / `server.metrics_listen` changed; reloader logs a warning and ignores the change (both listeners start once; restart required)
- `SubscriptionsChanged` / `GroupsChanged` / `FiltersChanged` / `ProberChanged` / `AnnotateChanged (old, newCfg Config) bool` — reloader hands the new config to the stable worker (`Controller.Apply` -> `Checker.Reconfigure`; the worker is never restarted) when any is true; `FiltersChanged` diffs the `filters` list, `AnnotateChanged` diffs the `annotate` list, `ProberChanged` compares only the geoblock gemini/claude/chatgpt/tidal sub-configs (store-only geoblock fields belong to `StoresChanged`)
- `StoresChanged(old, newCfg Config) bool` — true when `geoblock.db_path`/`geoblock.ttl`/`deadcache.ttl` changed; stores are built once at startup, so the reloader logs a restart-required warning
- `internal/reload/reload_coverage_test.go` — one classification table for every yaml leaf of `Config`, enforced twice: `TestReloadCoverageComplete` walks the struct (pointers dereferenced, slice/map element structs followed) so no key can ship unclassified, and `TestReloadClassificationMatchesBehaviour` mutates each key alone and asserts the declared class (live-processor / live-worker / live-both / live-other / restart-warned) is the path `Reload` actually takes. It lives in `internal/reload` because the behavioural half needs `OptionsFromConfig`

**Uses:** `fetch`, `filter`, `geofeed`, `mihomo/common/utils`, `zerolog`
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
- `ValidatePublicHTTPSURL(url SubscriptionURL) error` — `ValidateHTTPSURL` + reject a literal non-public-IP host (SSRF guard for the `/` endpoint and subscription sources). The non-public set covers the IPv4 reserved ranges plus the IPv6 special-purpose prefixes that embed an IPv4 target (`::/96`, `64:ff9b::/96`, `64:ff9b:1::/48`, `100::/64`, `2001::/32`, `2002::/16`, `fec0::/10`), which `Unmap` does not normalise
- `NewSafeHTTPClient() *http.Client` — guarded transport: non-public resolved IPs refused at dial time
- `NewUnrestrictedHTTPClient() *http.Client` — https-only, no proxy, **no IP guard** (crawler only)
- `MaybeDecode(resp, fileType) (io.ReadCloser, error)` — wrap gzip if needed; the gzip path is guarded against decompression bombs (the read fails once output passes the size floor AND exceeds a fixed ratio of the wire bytes consumed, so a few-KB archive can no longer inflate to the caller's whole byte limit)
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
- `SizedLookup` — `CountryLookup` + `Len() int`; implemented by the indexed lookups so the preprocess reload guards can compare a freshly loaded database against the live one

**Key functions:**
- `LoadAll(ctx, sources, logger) ([]Entry, int, error)` — fetch + parse geofeeds; skips a source that fails to fetch/parse (logs a warning) and errors only when NO source yields entries, so one flaky feed can't crash-loop startup. The middle return is the number of skipped sources: a partial load is still `err == nil`, so callers holding a good database (the preprocess background reload) use the count to refuse the swap
- `Parse(body) ([]Entry, error)` — parse geofeed CSV body (`parseLine`: one `ioutil.UnsafeString` alloc per line, then `strings.Cut`; `parsePrefixOrAddr` uses `addr.BitLen()` instead of hardcoded bit widths)
- `NewLookup(entries) CountryLookup` — indexed lookup from CIDR entries (prefixes convert to ranges)
- `NewRangeLookup(ranges []Range) CountryLookup` — indexed lookup from raw ranges (DB-IP / registry)
- `LookupCountry(lookup, ip) CountryCode` — helper forwarding to the configured lookup
- `ExpandMonthURL(url, now) string` — replaces the literal `{yyyy-mm}` with now's UTC month (DB-IP publishes per UTC calendar month)
- `LoadDBIP(ctx, url, logger) ([]Range, error)` — fetch + parse the DB-IP Country Lite gzip CSV; on a 404 for the current month (not yet published right after rollover) retries once with the previous month (`errors.As` on `*fetch.StatusError`); errors when both fail or nothing parses — the caller (preprocess `geoDB`) degrades to an empty lookup instead of failing startup
- `ParseDBIP(body) []Range` — `start_ip,end_ip,CC` lines, v4/v6 mixed; per-line tolerant (malformed, mixed-family, unordered, and `ZZ` lines skipped)
- `LoadRegistry(ctx, urls, logger) ([]Range, int, error)` — fetches the RIR delegated-extended files; skips (warns) any single failing RIR so one registry outage can't take down startup, errors only when NO ranges load, and reports the skipped-RIR count (mirrors `LoadAll`)
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
- `(*CountrySet).Add(part string) bool` — add a single country code (whitespace trimmed, case normalized); reports whether the token was a 2-letter code, so callers validating user input can reject the rest
- `(*CountrySet).Exclude(other CountrySet)` — remove one set from another
- `All() CountrySet` — return a set with all 676 2-letter codes set, and only those: the 28 bits above `ZZ` stay clear so a fully-excluded set compares empty
- `ParseAllowed(parts ...string) CountrySet` — parse `"DE,US,  nl  "` or `"DE", "US", "nl"` into bitset (uses `strings.SplitSeq`); unparseable tokens are ignored, use `Add` to detect them
- `Permits(allowed, denied CountrySet, country CountryCode) bool` — the country decision for one IP. `allowed` constrains only when it is not full; `denied` is matched positively
- `Permitted(lookup, ips, allowed, denied) []netip.Addr` — apply `Permits` to each IP's geofeed country, compacting the input slice in place
- `IsFull(s CountrySet) bool` — true when `s` contains every code (the `All()` set); as an allow-list it constrains nothing
- `IsEmpty(s CountrySet) bool` — true when `s` contains no code at all

The allow-list and the deny-list are deliberately asymmetric about the zero `CountryCode` that `geofeed.LookupCountry` returns for an IP no source covers. A non-full allow-list drops it (it is not in the list); the deny-list keeps it (it is in no excluded country). That is what makes `exclude_countries`/`exclude_groups` a deny-list rather than "all countries minus the exclusions": excluding RU must not drop every IP the geofeed cannot place.

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

DNS resolver for subscription node hostnames. Uses system DNS or custom address. Deduplicates IPv4 results. Process-wide TTL cache (RWMutex map): positive hits cached for `resolver.cache_ttl`, failed/empty lookups negative-cached for `resolver.cache_negative_ttl` (returned as empty result without error); zero TTLs disable caching entirely. Cache keys are cloned (`strings.Clone`) so they never pin the fetched subscription body backing array. Cache is capped at 16384 entries — on overflow expired entries are evicted, and if that frees nothing a bounded 1/8 sample of live entries is dropped (never a full wipe, which would take the hit rate to zero exactly when the working set is largest). The whole resolver, cache included, is carried across a config reload unless the `resolver` block changed — otherwise the crawler's hourly `private.yaml` rewrite would reset it long before `cache_ttl` elapsed. preprocess still isolates results once per request/hostname via a pooled resolved-map. When a custom `resolver.address` is configured, `PreferGo: true` is set on the `net.Resolver` so the custom `Dial` function is actually used (the cgo resolver ignores `Dial`).

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

ASN resolver using Team Cymru DNS (`origin.asn.cymru.com` + `asn.cymru.com`). Results are cached in memory with a configurable TTL (`asn.cache_ttl`, default 24h; a zero/negative value falls back to `defaultCacheTTL`); failures are negative-cached for 5m (`negativeCacheTTL`, cancellation errors excluded) so an unreachable Cymru doesn't serialize per-node timeouts. The cache is capped at 16384 entries with evict-expired-then-sample-on-insert (same pattern as `internal/resolver`; the 24h TTL means nothing expires within a day, so a full map would otherwise wipe on every insert), and the resolver is carried across a config reload unless the `geo.asn` block changed. `CacheLen()` exposes the size. Currently IPv4-only.

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
- `Parse(body, yield) (rejected int)` — iterate lines via `ioutil.Lines`, parse URIs containing `://`; the return is how many URI-shaped lines `parseNode` refused (they never reach `yield`), which `preprocess.processBody` books as `Stats.Unsupported`
- `Normalize(body) []byte` — trim + strip ASCII whitespace + base64 decode + URI detection
- `parseNode(line) (Node, bool)` — scheme → authority → host:port → fragment; the fragment is the FIRST `#` after the authority (later `#`s stay in the name); bracketed IPv6 hosts are returned without brackets, unbracketed multi-colon authorities are treated as a portless IPv6 host. The text before `://` must have the RFC 3986 scheme shape (`validScheme`: ALPHA then alnum/`+`/`-`/`.`) — the parser stays scheme-generic, but without that check an HTML error page or Clash YAML document parses as a healthy node list (`<a href="https` was a valid scheme)
- `parseVmess(line, schemeEnd) (Node, bool)` — base64 JSON payload (`add`/`port`/`ps`); when `ps` is absent the display name falls back to the URI fragment, then to the server host

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

Persistent per-host geo-block list: node hosts that failed a through-node API reachability check (Gemini/Claude/ChatGPT), each with a TTL (default 30d). Backed by SQLite via the pure-Go `modernc.org/sqlite` driver (works under `CGO_ENABLED=0`/distroless). Reads are served from an in-memory cache (the filter hot path); the DB file is touched only on write/prune/load. Keys are **lowercased hosts** — a host is a DNS name, so two sources spelling the same node in different case must fold onto one entry, or the mixed-case duplicate of a blocked host is silently re-admitted every cycle. The `tidal` filter deliberately does NOT write here — see `internal/stable`.

**Key types:**
- `Store` — `Open(path, ttl)`, `Blocked(host) bool`, `Block(host) error`, `Prune() error`, `Count() int`, `Close() error`. `Block`/`Blocked` fold `host` to lower case, and so does `load`, which merges two legacy mixed-case rows for one host onto the later expiry (row-order independent); a stale mixed-case row is never refreshed again and ages out under the TTL. `Prune` is called once per stable cycle through `stable.Blocklist` (see `internal/stable`); `Open` also prunes, but that alone left expired rows and map entries in place for the whole uptime of a `restart: always` container.

**Uses:** `modernc.org/sqlite`
**Tags:** `geoblock`, `sqlite`, `ttl`, `blocklist`, `gemini`, `claude`, `chatgpt`

---

## `internal/preprocess`

`./internal/preprocess/processor.go`, `filters.go`

Core processing. Orchestrates subscription loading, DNS resolution, geofeed/ASN filtering, and output rewriting per node.

**Key types:**
- `Processor` — geofeed country lookup (async background reload via `TryLock`) + optional dbip/registry `geoDB`s (lazily built, see below) + DNS resolver + sequential filter pipeline (no country cache, no groups map). The background swap is guarded: `loadEntries` (a field defaulting to `geofeed.LoadAll`, injectable in tests) reports how many sources failed, and `swapRefusal` keeps the live database when any source failed or the new set came back below `minSwapRatio` (half) of the current one — a truncated-but-200 body reports no error, so relative size is the only signal. A failed or refused reload leaves `loadedAt` on the last GOOD data and arms `retryAt` instead (`retryDelay`: 5 min doubling per consecutive failure, clamped to the refresh interval), so a transient outage is retried within minutes rather than after the full `refresh_interval` (24h in production), while a permanently dead source degrades to the normal cadence. `retryAt`/`reloadFailures` cross a config reload with the data they belong to (`GeoState`); without that the crawler's hourly `private.yaml` rewrite would reset the backoff before it ever grew.
- `Stats` — `Total` / `Kept` / `DNSDrop` / `GeoDrop` / `ASNDrop` / `GeoBlockDrop` / `IPv6Drop` / `Unsupported`. `Kept` plus every drop reason sums back to `Total`; `IPv6Drop` counts nodes whose server is a non-IPv4 literal (refused before any lookup — they used to be mis-booked as `DNSDrop`). `Unsupported` sits OUTSIDE that identity: it counts `://`-bearing input lines `subscription.Parse` refused, which never became nodes, so a body of pure junk still leaves `Total == 0` and fails the call
- `PipelineContext` — request-scoped state shared across filters (`Buffer`, `Lookup`, `Allowed`, `Denied`, `Resolved`, `Scratch`, `Stats`, `IsFirstNode`); `Scratch` is the per-node IP slice handed to filters (they compact in place), keeping the `Resolved` cache pristine across nodes sharing a server. `Lookup` is `Processor.countryChain`: the in-memory country databases the configured `GEO` annotate chain names, walked in that order, first answer wins
- `Filter` — interface for one IP-stage filter; `Process(ctx, ips, pctx)`
- `GeofeedFilter` — keeps IPs whose country `filter.Permits(pctx.Allowed, pctx.Denied, …)` accepts; a full allow set with an empty deny set makes it a no-op. An IP no database can place survives an exclusion and is dropped only by an allow-list. The name is historical: it judges against the whole `pctx.Lookup` chain, the same databases the GEO annotation resolves against, so a node is never dropped as unplaceable while carrying a tag that places it
- `ASNFilter` — drops IPs matching ASN deny patterns AND IPs whose Cymru-resolved country the same `filter.Permits` pair rejects (so country filtering works via the ASN provider without a geofeed stage)
- `annotator` (`annotator.go`) — builds the ordered `[GEO][IP][ASN]` tag prefix for the chosen IP from the `annotate` specs, then writes the relabeled node via `rewrite.NodeName`; nil when no tags are configured (annotation disabled, raw node emitted). GEO/ASN tags carry an ordered provider chain (`annotTag.providers`, resolved against the map of providers the processor actually built — a referenced-but-missing name is logged and skipped, not panicked): the first provider that answers wins; all-miss renders `[GEO:??]` / `[ASN:??]`.
- `geoDB` — one downloadable in-memory IP→country database (dbip or registry) under the processor's mutex discipline (`mu` guards lookup/loadedAt/retryAt, `reloadMu` serializes refreshes). Startup: a preloaded `GeoState` (reload carry-over) is adopted whole — lookup, load time AND retry schedule, so a mirror that is currently failing keeps its backoff instead of being re-downloaded on the first request after every reload; otherwise the initial load runs inline but a failure only WARNs and starts with an empty lookup — startup must never depend on a third-party database mirror, the annotate chain just degrades. A failed or partial initial load arms `retryAt` so the next attempt lands in minutes instead of a whole interval. Refresh: opportunistic background reload on the request path (`maybeRefreshGeoDBs` → `TryLock`, same trigger point as the geofeed refresh), running the same `swapRefusal` + `retryDelay` guards as the geofeed path, so a dead RIR can neither shrink the live database nor pin it for a day.
- `Blocklist` — interface `Blocked(host string) bool` (satisfied by `*geoblock.Store`); when set, `processNode` drops nodes whose `Server` is currently geo-blocked (`GeoBlockDrop`) before DNS resolution, on both `/` and the worker. Casing is the store's problem, not this call's: it folds the host to lower case, so a source spelling a blocked node differently is still dropped
- `Options` — configuration struct for `NewProcessor` (`GeofeedSources`, `RefreshInterval`, `DNSTimeout`, `DNSAddress`, `DNSCacheTTL`, `DNSCacheNegativeTTL`, `FetchTimeout`, `ASNTimeout`, `ASNCacheTTL`, `IPFilters []config.IPFilterSpec`, `Annotate []config.AnnotateSpec`, `DBIP config.DBIPConfig`, `Registry config.RegistryConfig`, `Blocklist`, plus the `Preloaded*` carry-over fields). `IPFilters` drives the filter chain; `Annotate` builds the annotator (empty → the node's original name passes through). `providerNeeds` decides which geo backends are lazily built: the ASN resolver when any IP filter or annotate chain needs it (asn filter, `country` provider `asn`, or `asn` in a chain), the dbip/registry `geoDB`s only when an annotate chain names them.
- `FilterRequest` — request struct for `Filter` (`SubscriptionURL fetch.SubscriptionURL`, `AllowedCountries filter.CountrySet`, `DeniedCountries filter.CountrySet`, `Body []byte`). `AllowedCountries` must be non-empty (`All()` when the caller asked for no allow-list) or `Filter` errors; `DeniedCountries` carries `exclude_countries`/`exclude_groups`. When `Body` is non-empty the payload is filtered directly — normalized with the same `subscription.Normalize` (base64-tolerant) used for fetched bodies, **skipping `subscription.Load`/HTTP fetch** — and takes precedence over `SubscriptionURL`; the log context labels it `inline`. URL-source behavior is unchanged.

**Key functions:**
- `NewProcessor(ctx, logger, opts Options) (*Processor, error)` — load geofeed (or use `opts.PreloadedGeofeed` when set; a geofeed load failure IS fatal, unlike the geoDBs), build the geoDBs and ASN resolver per `providerNeeds` (reusing `opts.PreloadedASN` / `opts.PreloadedResolver` when the reloader carried them), build filter chain, then hand the annotator only the providers actually built
- `(*Processor).Filter(ctx, b, req FilterRequest) (Stats, error)` — main pipeline writing into caller-owned `bytes.Buffer`; a cancelled request returns `ctx.Err()` instead of a truncated list. Inline (`req.Body`) requests skip the fetch entirely and normalize the body in-process.
- `(*Processor).GeofeedState() GeoState` — returns the lookup, its load time and the retry schedule in flight as one snapshot under the read lock; used by the reloader to carry geofeed state across config reloads when sources are unchanged (the underlying `loadedAt`/`retryAt`/`refreshInterval` fields are unexported; `shouldReloadGeofeedLocked` requires `p.mu`)
- `(*Processor).DBIPState() / RegistryState() GeoState` — same carry-over accessors for the downloadable databases (the zero `GeoState` when the provider was not built in this processor)
- `(*Processor).ResolverState() *resolver.Resolver` / `(*Processor).ASNState() *asn.Resolver` — hand the live resolvers, and with them their warm caches, to the next processor across a reload; `ASNState` is nil when no filter or annotate chain needed ASN data
- `(*Processor).countryChain(ctx) geofeed.CountryLookup` — the lookup the country filter judges with: the local databases the configured `GEO` annotate chain names, in the order it names them (`chainLookup`, first non-zero answer wins; `countryChainOrder` derives the order once in `NewProcessor`). The filter used to see the geofeed alone while the GEO tag resolved through the whole annotate chain, so a node dbip places in DE was geo-dropped as unplaceable while its tag said `[GEO:DE]`; hardcoding geofeed → dbip → registry then reintroduced the same divergence for any operator who ordered the chain differently. The `asn` provider stays out — it is a per-IP Cymru round trip, and `{type: country, provider: asn}` already exposes it explicitly. A geofeed-only chain, an asn-only chain and a config with no `GEO` entry all collapse to the geofeed lookup alone
- `(*Processor).resolveNode(ctx, server, pctx) (ips []netip.Addr, supported bool)` — resolve once per request/hostname and copy resolver results into request-local storage. `supported=false` means the server is an IP literal that is not IPv4; the caller books `IPv6Drop`, not `DNSDrop`, because nothing was ever looked up
- `buildFilters(specs []config.IPFilterSpec, asnR) ([]Filter, error)` — construct the IP-stage chain in config order from the parsed specs (country→geofeed/ASN provider, asn→compiled deny patterns + ASN-country). No implicit geofeed force-append: country filtering only happens when a `country` filter is configured. Enforcement of the per-request `AllowedCountries`/`DeniedCountries` (from `countries`/`groups` and `exclude_*` on `/`, or `All()` + the config excludes for the worker) happens inside the country filter.
- `FormatStats(stats) string` — `done: total=N kept=N …`
- `(*Processor).processBody(ctx, body, pctx) error` — parse + per-node pipeline. Adds `subscription.Parse`'s rejected-line count to `Stats.Unsupported`. Rejects a body over `maxSubscriptionNodes` (20 000) with `ErrTooManyNodes` BEFORE the first lookup: resolution is serial with a 5s per-hostname timeout, so a 10 MiB body of ~380k minimal URIs would otherwise be hours of DNS. `tooManyNodes` bounds the count by newlines first (one vectorized pass) and only a body clearing that bound pays for the exact `countNodes` parse, keeping the check off the hot path. Callers on `/` map the sentinel to `413`

**Options fields added for hot-reload:**
- `GeoState{Lookup, LoadedAt, RetryAt, Failures}` — the carry-over unit for one country database. The four values are one type because they are only correct together: carrying the data while dropping the retry schedule makes a source whose last reload failed read as permanently stale (`LoadedAt` marks the last GOOD data), so the first request after every reload re-downloads it — hourly, given the crawler's `private.yaml` rewrites
- `PreloadedGeofeed GeoState` — when `Lookup` is non-nil, `NewProcessor` skips the initial geofeed fetch and adopts the whole state, retry schedule included
- `PreloadedDBIP` / `PreloadedRegistry GeoState` — same pattern for the dbip/registry databases; used only when the matching provider is referenced by `Annotate`
- `PreloadedResolver *resolver.Resolver` / `PreloadedASN *asn.Resolver` — carry the DNS and Cymru caches across a reload instead of rebuilding them cold. The reloader sets them only when the whole `resolver` / `geo.asn` block is unchanged, because both bake their timeouts and TTLs in at construction; a carried `PreloadedASN` is dropped when nothing references ASN data

**Uses:** `asn`, `config`, `filter`, `geo`, `geofeed`, `log`, `resolver`, `rewrite`, `subscription`
**Tags:** `orchestrator`, `pipeline`, `filter`, `geo`, `asn`, `annotate`, `stats`, `hot-reload`

---

## `internal/server`

`./internal/server/server.go`, `holder.go`

HTTP layer using Fiber (`ReadTimeout` 30s, keepalive disabled). Routes: `GET /healthz` → `ok`, `GET /` → preprocess subscription, `GET /stable.txt` → latest stability-tested node list. The active `Processor` and groups map are held in an atomic `Holder` so the reloader can swap them without restarting the server.

The root handler now accepts:
- `subscription_url` (required)
- `countries` / `groups` — additive allowed countries
- `exclude_countries` / `exclude_groups` — a deny-list of countries to drop

`countries`/`groups` and `exclude_*` are enforced separately: the first is an allow-list, the second a deny-list applied to the resolved country. An IP no geo source can place passes the deny-list (it is in no excluded country) but fails a non-empty allow-list. Any token the server cannot resolve — an unknown group name, or a country that is not a 2-letter code — is a `400` naming the offending token, never a silently narrowed filter. If `countries`/`groups` are present but produce an empty set, or the exclusions cover every allowed country, the request also returns `400`.

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
- `newIndexHandler(holder *Holder) fiber.Handler` — root handler: loads snapshot, validates URL, builds the allowed/denied sets (rejecting unresolvable tokens with `400`), then calls `Filterer` under an explicit `indexRequestTimeout` (60s) deadline derived from `c.Context()` — fasthttp's request context has no deadline and never cancels on client disconnect. `preprocess.ErrTooManyNodes` maps to `413`, a deadline to `504`, anything else to a generic `502`
- `newStableHandler(holder *stable.Holder) fiber.Handler` — serves the stable payload or `503` before the first cycle
- `buildCountrySet(rawCountries, rawGroups, groupsMap) (CountrySet, []string)` — HTTP-layer group expansion (used for both the allowed and the denied set); the second return lists tokens it could not resolve, which the handler turns into a `400`
- `newRecoveryMiddleware(logger) fiber.Handler` — recovers a handler panic into a `500` (panic value + stack logged at error level, never sent to the client). Neither fiber nor fasthttp recovers, and a panic would take the in-memory stable list down with the process. Registered inside the logging middleware so the request still gets an access-log line
- `redactSubscriptionURL(raw) string` — `host#<12 hex of sha256>` for the access log: subscription links are capability URLs, so the verbatim value would put a bearer token in `docker logs`
- `(*Server).Listen() error`
- `(*Server).Shutdown(ctx) error`
- `(*Server).TestApp() *fiber.App` — for test usage

**Uses:** `fetch`, `filter`, `preprocess`, `stable`, `fiber`
**Tags:** `http`, `fiber`, `api`, `handler`, `server`, `healthz`, `atomic-swap`, `hot-reload`

---

## `internal/stable`

`./internal/stable/stable.go`, `merge.go`, `select.go`, `report.go`, `prober.go`, `prober_api.go`, `prober_gemini.go`, `prober_claude.go`, `prober_chatgpt.go`, `prober_tidal.go`, `prober_bandwidth.go`, `nodefilter.go`, `checker.go`, `controller.go`, `progress.go`, `deadset.go`

Background worker that produces a stability-tested subscription list. Every `subscriptions.interval` it fetches each configured source through the geo/ASN pipeline (`Filterer`), merges the results into one deduplicated relabeled URI list, probes every node with the embedded mihomo library (`URLTest` HEAD requests, `check.rounds` rounds), keeps only nodes within `check.max_fail`/`check.max_avg_ms`, then runs the configured **through-node gates** (Gemini/Claude/ChatGPT reachability, Tidal availability, bandwidth) on each surviving node (a real API `GET` whose body is classified — the check mihomo's HEAD-only `URLTest` cannot do), records geo-blocked node hosts in the `geoblock` store (TTL) and drops them, and atomically publishes the rest for `GET /stable.txt`. Every failure mode (all sources down, zero parsable nodes, prober error, zero survivors) keeps the previous snapshot.

**Key types:**
- `Stats` — `SourcesOK/SourcesTotal/Merged/Tested/Kept` counters for the `X-Stable-Stats` header
- `Snapshot` — immutable `Payload []byte` + `UpdatedAt` + `Stats`
- `Holder` — `atomic.Pointer[Snapshot]`; `Load()` returns nil before the first successful cycle
- `SourceBody` / `Entry` — merge input (source name + fetched body) and output. `Entry.Raw` is the clean `<source>-NNN` name used for probing; `Entry.Tagged` is the published name (`Raw` plus the `[GEO][IP]` annotation carried over from the filter pass, when present); `Addr` is the server:port dead-cache key; `Country` is the `[GEO:xx]` code, `"??"` for an unresolved one and `""` when annotation is off or the tag held something that is not two ASCII letters. `tagCountry` both validates and `strings.Clone`s it — the tag string is a zero-copy view into the cycle's source body, and `Country` outlives the cycle inside the metrics snapshot; the validation is what keeps a source-authored tag out of a Prometheus label value. `BuildPayload` emits `Tagged`.
- `ProbeResult` / `Survivor` — per-node probe aggregate and selected node with mean delay; `Survivor.Mbps` holds the bandwidth filter's measured speed (0 when the filter is off)
- `Filterer` — local copy of `server.Filterer` (avoids an import cycle); satisfied by `*preprocess.Processor`
- `Prober` — `Probe(ctx, payload) (map[string]ProbeResult, error)`; implemented by `MihomoProber`
- `Blocklist` — `Block(host)` + `Prune()`, the through-node geo-block store (`*geoblock.Store`, SQLite/30d, keyed by lowercased host). `DeadCache` — `Blocked(key)/Block(key)/Prune()`, the dead-node cache; satisfied by `*DeadSet` (in-memory, not persisted — dead nodes are cheap to re-probe after a restart). Both carry `Prune` because the `Checker` sweeps every TTL cache it wrote to once per cycle (`pruneCaches`); the geoblock store used to be swept only inside `geoblock.Open`, so on a long-lived container it grew for the whole uptime.
- `APIOutcome` — per-node through-node API result (`Server`/`Reachable`/`Blocked`), shared by the gemini/claude/chatgpt/tidal checks
- `Checker` — the periodic worker loop, holding the injected `Blocklist`/`DeadCache` and nothing else cache-shaped. `filterDead` skips a merged node for exactly one reason: the dead cache holds a recent failed probe for it. A through-node verdict is NOT remembered here — it used to be, in a 6h per-filter "reject cache", which the round-4 rule that only a store-backed check may seed it made redundant: the checks still allowed to seed it (gemini/claude/chatgpt) write `store.Block(o.Server)` on the same line, and that entry is strictly stronger in every dimension — `preprocess.processNode` drops the host as `GeoBlockDrop` a whole stage earlier (before `Merge` even builds the `Entry`), for 720h instead of 6h, keyed by host rather than `host:port`, and across restarts. The reject branch could therefore essentially never fire, so it, `rejectKey`, `recordRejects`, `NodeFilter.active()`, `NodeFilterParams` and `DeadSet.Reset` are all gone; the one case the cache still caught — a second source spelling the host in different case — is now closed inside `geoblock` by folding the key to lower case.
- `CheckerSpec` — the config-derived half of a `Checker` (`Sources`, `Denied`, `Interval`, `Rounds`, `MaxFail`, `MaxAvgMs`, `SourceTimeout`, `Prober`, `Filters`), held in an `atomic.Pointer` and read once per cycle so a reload configures the NEXT cycle instead of rewriting the running one. `Denied` is the config-level country deny-set; the worker never imposes an allow-list, so it sends `filter.All()` as `AllowedCountries`. Nothing here needs to be diffed on reload: `Reconfigure` just swaps the pointer, because no cycle-crossing state is derived from these params any more.
- `Controller` — owns the single `Checker`: starts it on first enable, reconfigures it in place on every later reload, stops it only on shutdown or an emptied source list
- `CycleReport` / `SourceReport` / `FilterReport` — per-cycle accounting (aggregate counts + per-source preprocess drops + per-filter in/kept/dropped-by-reason + kept-node speeds + duration), assembled by `RunOnce` from data otherwise only logged. `SourceReport` mirrors `preprocess.Stats` field for field (`DNSDrop`/`GeoDrop`/`ASNDrop`/`GeoBlockDrop`/`IPv6Drop`/`Unsupported`)
- `Reporter` — nil-safe metrics sink: `Observe(CycleReport)` on a published cycle, `ObserveError()` on any abort/no-op; `*metrics.Metrics` implements it

**Key functions:**
- `Merge(bodies []SourceBody) []Entry` — dedupe by lowercased `server:port` first-wins in source order (`Entry.Addr` shares the lowercased key); relabel fragments to `<source>-NNN`
- `SelectSurvivors(entries, results, rounds, maxFail, maxAvgMs) []Survivor` — keep `rounds-successes <= maxFail && mean <= maxAvgMs`, sort by mean ascending
- `BuildPayload(survivors) []byte` — newline-joined URI list
- `NewMihomoProber(cfg config.CheckConfig, bandwidth config.BandwidthConfig, geo config.GeoBlockConfig, geminiKey string, logger) (*MihomoProber, error)` — latency `Probe` (HEAD `URLTest`) plus through-node API checks: `GeminiCheck` + `GeminiEnabled()` (needs a key), keyless `ClaudeCheck`, `ChatGPTCheck` and `TidalCheck`, and `BandwidthCheck`/`BandwidthMinMbps` (from the injected `bandwidth` config). The whole geoblock block travels as one param so a new API check adds a `GeoBlockConfig` field, not another parameter; `geminiKey` stays separate because only Gemini needs a credential. API checks run through the shared `apiCheck` fan-out (mihomo `DialContext` + fixed-conn `http.Transport`, `GET` via `apiProbeOne`, which returns `(reachable, status, body)`; concurrency is clamped to >=1 because an unbuffered semaphore would deadlock the fan-out) and classify with one of two predicates: `markerBlocked` (body only, **fail-open** — Gemini: location marker; Claude: 403 `Request not allowed`; ChatGPT: 403 `unsupported_country` from `/compliance/cookie_requirements`) or `tidalBlocked` (status + body, **fail-closed** — keeps a node only on 2xx whose body yields a `parseCountryCode`; any non-2xx is the refusal, which is how Tidal's CloudFront 403 is caught, and a 200 without a code proves the answer did not come from Tidal).
- `NodeFilter` — through-node check applied after the IP-filters + latency probe, routing THROUGH each surviving node (worker-only, so it shapes `/stable.txt`, not `/`); selected from the unified `filters` list via `cfg.NodeFilterSpecs()` (types `gemini`/`claude`/`chatgpt`/`tidal`/`bandwidth`). The interface is one method: `apply(ctx, survivors, proxies) (kept, report)`. A drop lasts exactly this cycle — the checker remembers nothing a filter decides; a verdict worth outliving the cycle goes to the geoblock store, which the filter writes itself and which `preprocess` then honours before the node is ever merged. `buildNodeFilters(names, prober, store, annotate, logger)` constructs them; one generic `apiFilter{filterName, enabled, check, store}` implements the interface for gemini (key-gated) and claude/chatgpt/tidal (keyless), each keeping API-reachable survivors. A nil `enabled` func means always active; a disabled filter logs and keeps every survivor. gemini/claude/chatgpt record blocked hosts in the geoblock store; **tidal is built with a nil store on purpose** — its verdict is a bare status code, weaker than the AI checks' explicit refusal markers, and the store is host-keyed for its whole TTL, so one CDN hiccup would evict the node from every endpoint. With no store nothing outlives the cycle, so a rate-limited batch costs tidal's drop exactly one cycle.
- The `bandwidth` `NodeFilter` (`bandwidthFilter`) downloads the bandwidth filter's `test_url` through each survivor (`BandwidthCheck` → `bandwidthProbeOne` → `measure`, `Accept-Encoding: identity` + body-transfer timing; `computeMbps` guards divide-by-zero), drops nodes below `min_mbps` and unreachable ones, records `Survivor.Mbps`, and — when annotation is enabled (`len(cfg.Annotate) > 0`) — prepends `[SPD:<n>M]` to the published name via the vmess-aware `relabelNode`. `measure` reports a sample ONLY for a 2xx response read to completion, or one our own deadline cut short (a legitimate sample of a slow node): a non-2xx, an unfollowed redirect, or a body the peer truncated is a few fast kilobytes that `computeMbps` would inflate into a large fabricated rate, so it is reported not-reachable instead. It persists nothing: the concurrent downloads share one host uplink, so a dip on our side puts the whole batch under the floor, and next cycle re-measures.
- `NewChecker(spec CheckerSpec, filterer func() Filterer, store Blocklist, dead DeadCache, holder *Holder, logger, reporter Reporter)` / `(*Checker).Reconfigure(spec)` / `(*Checker).Run(ctx)` — immediate first cycle, then ticker (`Run` resets the ticker when a reload changed `Interval`); `Reconfigure` atomically swaps the spec and the cycle in flight keeps the one it started with. The `store` is held ONLY to prune it; the writes still happen inside each `apiFilter`. `RunOnce(ctx) error` is one cycle and loads the spec ONCE up front, threading it into `fetchSources`/`applyFilters` so a mid-cycle reload cannot half-rewrite it: fetch sources concurrently (results kept in config order so first-source-wins is deterministic), drop dead-cached nodes before probing, probe the rest, record no-success nodes as dead (short TTL), `SelectSurvivors`, apply the configured `NodeFilter`s, then `pruneCaches` (dead cache + geoblock store). A cancelled/failed probe aborts the cycle: the previous snapshot is kept and nothing is recorded dead (a shutdown mid-probe can't poison the dead cache; a reload no longer cancels a cycle at all). `Probe` shares ONE semaphore across rounds — built by the same `fanoutSem` as the API and bandwidth fan-outs, so `check.concurrency` caps total in-flight URL tests and a hand-built `Concurrency: 0` degrades to serial instead of deadlocking. `fetchSources` builds each `preprocess.FilterRequest` per source: when `src.Body != ""` it passes `Body: []byte(src.Body)` with an empty `SubscriptionURL` (inline path, no fetch), otherwise the usual `SubscriptionURL: fetch.SubscriptionURL(src.URL)`; the local `Filterer` interface stays a single `Filter(...)` method.
- `NewController(ctx, holder, filterer func() Filterer, store Blocklist, dead DeadCache, logger, reporter Reporter)` / `(*Controller).Apply(cfg) error` / `(*Controller).Stop()` — `Apply` builds the denied `CountrySet` via `cfg.DeniedCountries()`, folds each through-node spec's per-entry overrides back into a `GeoBlockConfig` copy, resolves the Gemini key, and builds the prober + `NodeFilterSpecs`-selected filters BEFORE touching the worker (a failed construction leaves the previous spec in force), then hands the resulting `CheckerSpec` to the running `Checker` via `Reconfigure` — or starts one when none is running and `subscriptions.sources` is non-empty. A reload NEVER restarts a live worker: the cycle in flight runs to publication instead of being cancelled and losing its whole probe pass. A reload whose merged source list is **empty** is refused rather than obeyed — with a worker running it logs a warning and keeps the previous spec live (every source comes from an overlay, so an empty list is nearly always a missing `sources.yaml`/`private.yaml`, and stopping would cancel the cycle in flight and freeze `/stable.txt` with nothing behind it); with none running it stays a no-op. `Stop` is idempotent, clears the checker, and is now reached only on shutdown. The `reporter` (nil-safe) is threaded into every `Checker` it builds.

**Uses:** `config`, `filter`, `fetch`, `preprocess`, `subscription`, `mihomo` (adapter, common/convert, common/utils, constant)
**Tags:** `stable`, `probe`, `url-test`, `gemini`, `claude`, `chatgpt`, `tidal`, `bandwidth`, `speed`, `geoblock`, `delay`, `worker`, `mihomo`, `atomic-swap`

---

## `internal/metrics`

`./internal/metrics/metrics.go`

Renders the stable worker's per-cycle stats as Prometheus text exposition (hand-rolled — no `client_golang`, avoiding the `google.golang.org/protobuf => metacubex/protobuf-go` replace). Served on an internal listener (`server.metrics_listen`, default `:9090`) that docker-compose publishes loopback-only (`127.0.0.1:9091:9090`); the server's NixOS Prometheus scrapes it and a provisioned Grafana dashboard renders the funnel + trends.

**Key exports:**
- `Metrics` — holds the last `stable.CycleReport` + lifetime counters (RWMutex); `New()` constructs it
- `(*Metrics).Observe(stable.CycleReport)` / `ObserveError()` — satisfies `stable.Reporter`; both count toward `stable_cycles_total`, ObserveError also bumps `stable_cycle_failures_total`
- `(*Metrics).Handler() http.Handler` — renders `/metrics` into a buffer under a read lock, so a slow scrape never blocks `Observe`
- `escapeLabelValue(s)` — a label value can originate outside this service (with `annotate: []` the pipeline republishes upstream node names verbatim and `stable` derives the `country` label from a `[GEO:xx]` tag the SOURCE wrote), and one invalid byte makes the whole scrape unparseable, so an invalid-UTF-8 value is replaced with `invalid_utf8` rather than escaped, and `\r` is escaped alongside the format-reserved `\`, `"`, `\n`

**Metrics:** `stable_kept_nodes`, `stable_merged_nodes`, `stable_probed_nodes`, `stable_dead_skipped_nodes`, `stable_sources_ok`/`_total`, `stable_cycle_duration_seconds`, `stable_last_success_timestamp_seconds`, `stable_cycles_total`/`_failures_total`, `stable_filter_{in,kept,dropped}_nodes{filter,reason}`, `stable_source_{nodes_total,kept_nodes,dropped_nodes}{source,reason}`, `stable_kept_speed_mbps` (histogram). `stable_dead_skipped_nodes` counts every merged node the cycle did not probe — all of them dead-cached, the only pre-probe skip left — so the funnel closes: `merged = dead_skipped + probed`. `stable_source_dropped_nodes` reasons: `dns`, `geo`, `asn`, `geoblock`, `ipv6`, `unsupported` — `unsupported` counts unparseable INPUT LINES and is therefore not part of `stable_source_nodes_total`.

**Uses:** `stable`
**Tags:** `metrics`, `prometheus`, `grafana`, `exposition`, `observability`

---

## `internal/reload`

`./internal/reload/reloader.go`, `watcher.go`, `options.go`

Config hot-reload. Watches the config file **and its `private.yaml` / `sources.yaml` overlay siblings** for changes (via fsnotify on the parent directory), debounces bursts, and atomically swaps the active `Processor` + groups into the server `Holder`. On any error the previous settings are kept unchanged. The overlays matter because the crawler writes `private.yaml` and `sources.yaml` carries curated sources, and a change to either must reach the stable worker, which is reconfigured in place rather than restarted.

**Key types:**
- `Reloader` — holds `path`, `*server.Holder`, `zerolog.Logger`, and the last-applied `config.Config` + `*preprocess.Processor` for diffing
- `Applier` — interface `Apply(config.Config) error`; satisfied by `*stable.Controller` (enables fake controllers in tests)
- `Watcher` — wraps `*fsnotify.Watcher`; watches the config file's parent directory to survive atomic-rename writes; fires `onChange` for events on `config.yaml` or its `private.yaml` / `sources.yaml` siblings; debounces events with a 200 ms window

**Key functions:**
- `NewReloader(path string, holder *server.Holder, logger zerolog.Logger, cfg config.Config, proc *preprocess.Processor, ctl Applier, blocklist preprocess.Blocklist) *Reloader` — seed with startup state so the first reload can diff against it; injects the shared geo-block store into every rebuilt `Processor`
- `(*Reloader).Reload(ctx context.Context)` — load config → skip only if `Equal` against BOTH `currentCfg` and `currentProcCfg` (they diverge after a failed `Apply`, and keying the fast path on `currentCfg` alone made reverting the file a silent no-op that left the holder serving the rejected config) → build `OptionsFromConfig` (+ inject `Blocklist`) → carry the geofeed `GeoState` if `!GeofeedSourcesChanged`, the dbip/registry ones if `!DBIPChanged` / `!RegistryChanged`, and the live DNS/ASN resolvers (with their warm caches) if `!ResolverChanged` / `!ASNChanged` (all diffed against the config that BUILT the current processor, so a failed-Apply divergence can't carry data across the wrong source set; a zero state — provider not built — simply leaves the preload unset). Each `GeoState` moves as one value, so the retry schedule of a failing source can never be dropped while its data is kept → `NewProcessor` → `SetLevel` → warn if `ListenChanged` or `StoresChanged` (restart required) → `holder.Store` new snapshot → `ctl.Apply(newCfg)` when `SubscriptionsChanged || GroupsChanged || FiltersChanged || ProberChanged || AnnotateChanged`. On a failed `Apply`, `currentCfg` is NOT committed, so re-saving the file retries instead of hitting the `Equal` fast path (the worker keeps running on its previous spec — Apply builds the prober/filters before swapping it in, and never stops the worker).
- `NewWatcher(configPath string, onChange func(context.Context), logger zerolog.Logger) (*Watcher, error)` — register fsnotify watch on parent directory; return error if watcher or directory watch fails
- `(*Watcher).Run(ctx context.Context) error` — event loop: debounce matching events, call `onChange` once per burst; close fsnotify watcher on ctx cancellation and return nil (callers use the return as a join point)
- `OptionsFromConfig(cfg config.Config) preprocess.Options` — single source of truth for mapping `config.Config` to `preprocess.Options` (incl. `geo.dbip` → `DBIP`, `geo.registry` → `Registry`); leaves every `Preloaded*` field unset (callers decide whether to carry over geofeed/dbip/registry state and the DNS/ASN resolvers)

**Uses:** `config`, `geofeed`, `log`, `preprocess`, `server`, `stable`, `fsnotify`
**Tags:** `reload`, `fsnotify`, `hot-reload`, `watch`, `atomic-swap`, `debounce`

---

## `internal/classify`

`./internal/classify/classify.go`

Decides whether a URL serves a usable Mihomo-compatible subscription, reusing the project's HTTP client (the caller supplies it — the crawler an unrestricted client, the `classify` CLI a guarded one) and the same normalizer/parser the preprocessor uses. Used by the `crawl` subcommand and the `classify` CLI subcommand.

**Key types:**
- `Result{Nodes int, Expired bool}` — `(Result).Live()` reports `Nodes > 0 && !Expired`
- `StatusError{Code int, Status string}` — a non-2xx answer; `(*StatusError).Gone()` reports the **definitive** codes only (404/410/451). Every other status (403 WAF/geo-block, 408/425/429 back-pressure, all 5xx) is transient, so a caller that deletes on a dead verdict must not delete on those

**Key functions:**
- `Body(body []byte, subUserinfo string, now int64) Result` — pure classifier: base64-normalizes the body, counts only **proxy-scheme** nodes (`vless`/`vmess`/`ss`/`ssr`/`trojan`/`tuic`/`hysteria`/`hysteria2`/`hy2`/`anytls` — so HTML pages full of `https://` links are rejected), and marks expired from a `subscription-userinfo: expire=` header
- `URL(ctx, client *http.Client, rawURL fetch.SubscriptionURL) (Result, error)` — scheme-validate + fetch + `Body`; a non-2xx answer returns `*StatusError` (check `Gone()` before acting on it); the IP/SSRF policy comes from the passed client

**Uses:** `fetch`, `subscription`
**Tags:** `classify`, `subscription`, `ssrf`, `crawler`

---

## `internal/crawl`

`./internal/crawl/crawl.go`, `discover.go`, `state.go`, `channels.go`

Format-agnostic, recursive subscription crawler. Scrapes public Telegram channel web previews (`t.me/s/<channel>` + `?before=` pagination), treats **every** https link as a candidate, keeps the ones that `classify` as a live subscription, and writes them to the `private.yaml` overlay as `tg-<channel-slug>-<sha6>` sources — the discovering channel (first-wins in BFS order, so seeds beat discovered channels) plus a 6-hex URL hash; `channelSlug` folds the Telegram slug into the config name alphabet (`^[a-z0-9-]+$`, `_`→`-`, 24-byte cap). An already-attributed name is kept verbatim across cycles (renames would churn private.yaml and relabel published nodes); a legacy pure-hash `tg-<sha10>` name upgrades the first time its URL is rediscovered in a channel; a name collision or unknown origin falls back to the `tg-<sha10>` form (`sourceName`). Matches the artifact (a URL that returns a subscription), not any channel-specific wrapper pattern. Runs as the `crawl` subcommand in the same image as the service. One **unrestricted** `http.Client` (no IP guard — it reaches `t.me` through the host's local fake-ip tunnel and follows links scraped from channel content) is held on the `Crawler` and reused for pages + classify batches. Cycle hygiene: a cancelled or budget-exhausted ctx aborts before any state/private.yaml write, and `Run`/`RunDaily` bound each cycle to `cycleBudget` (¾ of the schedule interval, capped at 2h) so a growing crawl cannot outrun its own tick.

**Key types:**
- `Options{Channels []string, ChannelsPath string, PrivatePath string, Pages int, Prune bool, MaxDepth int, MaxChannels int, StatePath string, StateTTL time.Duration, InlineEnabled bool, InlineMax int}`
- `Crawler` — `New(opts, logger)`; `RunOnce(ctx)` one cycle, `Run(ctx, interval)` loop

**Behavior:** `scan` (in `discover.go`) does a **relevance-gated BFS** over the channel repost graph — seeds are crawled unconditionally, a discovered channel (`extractChannels`: forwarded-from/@mention `t.me/<slug>` links, excluding self/reserved/bot `?start=`) is expanded only if it itself yielded a live subscription; the subscription yield is the thematic signal (a VPN channel forwards VPN channels; a news channel yields nothing and its branch stops). Depth is bounded by `MaxDepth`; `MaxChannels` caps discovered (non-seed) visits per cycle (`0` = the built-in `defaultMaxDiscovered`, 200 — never unlimited, since every discovery can promote itself to a seed). Only operator-configured seeds get the full `Pages` budget; remembered seeds and discovered channels get `discoveredPages` (3), which is what stops each cycle from costing more than the last. A per-cycle `visited` set means every channel is fetched at most once and a repost loop (A→B→A) can never re-enter an explored channel. Page fetches are sequential (rate-limit friendly). Validates candidate URLs via `fetch.ValidatePublicHTTPSURL` — the same rule `config.Load` applies, so a harvested literal-private-IP URL can never make the whole config unloadable — and skips Telegram/CDN noise hosts before fetching; classifies concurrently. **Pruning is evidence-based:** a managed source is dropped only on a definitive verdict — an expiry the origin itself advertised (`Result.Expired`), or a `classify.StatusError` whose `Gone()` is true (404/410/451). A transport failure, a transient status, **or a 2xx carrying no proxy-scheme node at all** retains it: since `Body` counts only real node schemes, a captive portal, a Cloudflare interstitial and a panel login page all come back nodeless, and that is "alive but told us nothing", not proof of death. A cycle that discovered nothing *and* revived nothing prunes nothing (`recheckResult.dark` — that shape is a crawler-side egress fault, not mass death), and `allowShrink` refuses any write dropping both >10 managed sources and >30% of them until a later cycle re-proposes substantially the same deletion (see Persistence). Hand-added private sources are preserved; `writePrivate` re-validates the whole file against the consumer's rules (name alphabet, uniqueness, public https URL) and refuses to author a config the service cannot load; only rewrites `private.yaml` (atomic temp+rename) when the managed set changes, so unchanged cycles trigger no reload.

**Persistence (`state.go`):** channels that yield a live subscription are recorded in a JSON state file (`StatePath`, default `/config/.crawler-state.json`) and become depth-0 seeds on future cycles — always crawled and always expanded — so a proven-productive channel keeps growing the graph even on days its recent pages carry no live sub. Entries with no live sub for `StateTTL` (default 30d) are pruned, and the memory is then capped at `maxProductive` (200) most-recently-productive channels, bounding the seed set that drives every cycle's cost. The file also carries the refused bulk prune as `bulk_prune_at` + `bulk_prune_urls` — when `allowShrink` refused it, and the sorted set of managed URLs it condemned. A later cycle carries it out only if it re-proposes at least `bulkPruneOverlap` (80%) of that set (symmetric, so the new proposal may be neither much smaller nor much larger), at least `bulkPruneConfirmAfter` (6h) but no more than `bulkPruneRecordTTL` (24h) after the refusal; honouring it consumes the record. Any other proposal — a different set, or one arriving past the TTL — replaces the record and starts its own wait, and any cycle whose merge proposes no bulk deletion at all withdraws the record. The URL set is what makes the record consent to one specific deletion instead of to the next one that happens along. Empty `StatePath` disables persistence — and therefore refuses every bulk prune, the safe direction. A read or unmarshal failure is logged at ERROR and marks the stand-in state `loadFailed`, which makes `saveState` a no-op for the rest of the cycle: an EACCES or a truncated file costs one degraded cycle instead of silently overwriting up to 30 days of channel memory with an empty file. A genuinely corrupt file must be deleted by hand.

**Inline-node harvest (`InlineEnabled`, default on):** alongside https subscription links, each scraped page is scanned by `extractInlineNodes` for **raw proxy URIs pasted directly in messages** (`vless|vmess|ss|ssr|trojan|tuic|hysteria|hysteria2|hy2|anytls://…`, HTML-unescaped, trailing punctuation trimmed). Per cycle the URIs are accumulated across all pages, parsed with `subscription.Parse`, deduped by lowercased `server:port` (first-wins, mirroring `stable.Merge`), and capped to `InlineMax` (default 500, first N kept). When ≥1 node survives, `buildInlineSource` packs the kept node URIs into a single base64 `Body` under a managed `tg-inline` source appended to `private.yaml` (empty-URL source → the stable checker filters `Body` directly, no fetch); a cycle with 0 inline nodes omits the source. `mergeManaged` skips existing `Body` sources so `tg-inline` is regenerated fresh each cycle, and `sameSources` includes `Body`, so a changed inline set triggers a private.yaml rewrite + reload. Env: `CRAWL_INLINE` (default `true`) toggles the harvest; `CRAWL_INLINE_MAX` (default `500`) sets the cap.

**Seed config (`channels.go`):** `config/channels.yaml` (`ChannelsPath`) carries two lists: `channels: [slug|@handle|t.me-url]` (depth-0 seeds) and `blocked: [url]` (subscription URLs the crawler must never manage). `loadChannels` returns the parsed `channelsFile` and is best-effort (missing/malformed → empty, never fatal), re-read every cycle, so both lists hot-reload without a restart. Effective seeds = `channels.yaml` ∪ `CRAWL_CHANNELS` env ∪ remembered productive channels. `blocked` is applied in `mergeManaged`, the single funnel every candidate URL passes through, so a retired source cannot re-enter from the re-loaded file, from rediscovery in a channel, or from `recheckManaged` reviving it — hand-deleting it from `private.yaml` alone does not stick.

**Pagination health:** `scrapeChannel` returns `cursorLost` when a fetched page carried no `?before=` cursor while the page budget still had room — the signature of the one undocumented `data-post` attribute (`cursorRe`) being renamed. `scan` aggregates the per-channel outcomes into `cursorStats` and `reportCursors` warns once per cycle when every cursor-relevant channel (at least `cursorAlarmMin`, 5) lost its cursor. Per channel the signal is noise (short channels legitimately have one page); fleet-wide it is the selector breaking and silently cutting `CRAWL_PAGES` to 1.

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
 │   │   ├─ config     (Load, Equal, GeofeedSourcesChanged, DBIPChanged, RegistryChanged, ResolverChanged, ASNChanged, ListenChanged, SubscriptionsChanged, GroupsChanged, FiltersChanged, ProberChanged, AnnotateChanged)
 │   │   ├─ log        (SetLevel)
 │   │   ├─ preprocess (NewProcessor, Options, GeofeedState, DBIPState, RegistryState, ResolverState, ASNState)
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
| `stable`, `probe`, `url-test`, `gemini`, `claude`, `chatgpt`, `tidal`, `bandwidth`, `worker`, `mihomo` | `stable` |
| `metrics`, `prometheus`, `grafana`, `observability` | `metrics` |
| `geoblock`, `sqlite`, `ttl`, `blocklist` | `geoblock` |
