# Package Map & Tags

LLM-oriented reference. Each package described with purpose, key exports, and search tags.

---

## `main`

`./main.go`

Entry point. Creates `context.Context` with `SIGINT/SIGTERM` cancellation, calls `app.Run()`.

**Tags:** `entrypoint`, `root`, `signal`, `main`

---

## `internal/app`

`./internal/app/app.go`, `pprof.go`

Application bootstrap: loads config, creates `Processor`, starts HTTP server, handles graceful shutdown.

**Key exports:**
- `Run(ctx) error` — main lifecycle

**Tags:** `bootstrap`, `wire`, `shutdown`, `lifecycle`

---

## `internal/config`

`./internal/config/config.go`

YAML config loading and validation. Uses `gopkg.in/yaml.v3`. Defines the full config schema.

**Key types:**
- `Config` — root config struct (`log`, `server`, `geofeed`, `resolver`, `workflow`, `asn`, `groups`)
- `GeofeedConfig` — `sources` + `refresh_interval` with `Validate() error` method
- `Groups` — `map[string][]string` with `Validate() error` method
- `LogConfig` — `level` (`yaml:"level"`, default `"info"`)
- `WorkflowConfig` — `stages` (sequential pipeline order; known names: `geofeed`, `asn`)
- `ASNConfig` — `deny_patterns` + `timeout`

**Key functions:**
- `Load(path) (Config, error)` — read + unmarshal + apply defaults + call `cfg.Validate()`
- `(*Config).Validate() error` — validates geofeed sources and groups
- `(*GeofeedConfig).Validate() error` — validates sources are non-empty with valid types
- `(Groups).Validate() error` — validates group names and 2-letter country codes

**Uses:** `fetch`, `geofeed`
**Tags:** `config`, `yaml`, `validation`, `startup`, `defaults`

---

## `internal/fetch`

`./internal/fetch/fetch.go`

Safe HTTP fetching with SSRF protection. Only `https` URLs, no userinfo, no private IPs, no proxy.

**Key types:**
- `FileType` — `"raw"` | `"gzip"`
- `SubscriptionURL` — lightweight `string` type for subscription URLs

**Key functions:**
- `BytesWithType(ctx, url SubscriptionURL, limit, fileType) ([]byte, error)` — fetch + decode body
- `ValidatePublicHTTPSURL(url SubscriptionURL) error` — SSRF guard
- `NewSafeHTTPClient() *http.Client` — transport with private-IP blocking
- `MaybeDecode(resp, fileType) (io.ReadCloser, error)` — wrap gzip if needed
- `ValidateFileType(fileType) error` — must be `raw` or `gzip`

**Tags:** `http`, `fetch`, `ssrf`, `security`, `gzip`, `download`, `client`, `redirect`

---

## `internal/geofeed`

`./internal/geofeed/geofeed.go`, `lookup.go`

Geofeed CSV parsing, lookup, and data source management. Default country lookup uses a flat sorted slice with binary search for IPv4 and linear scan for IPv6.

**Key types:**
- `CountryCode` — strict 2-byte ISO country code (`[2]byte`) with `String()`
- `Entry` — `Prefix` + `Country` (`Country` is `CountryCode`)
- `Source` — `URL` + `Type` (also used in config.yaml via yaml tags)
- `CountryLookup` — interface with `LookupCountry(ip) CountryCode`

**Key functions:**
- `LoadAll(ctx, sources) ([]Entry, error)` — fetch + parse (sorting handled by lookup constructor)
- `parsePrefixOrAddr` uses `addr.BitLen()` instead of hardcoded `bitsV4`/`bitsV6`
- `Parse(body) ([]Entry, error)` — parse CSV body
- `NewLookup(entries) CountryLookup` — default indexed lookup builder
- `LookupCountry(lookup, ip) CountryCode` — helper forwarding to the configured lookup
- `parseLine(line) (Entry, bool)` — one `ioutil.UnsafeString` alloc per line, then `strings.Cut` for CSV fields

**Uses:** `fetch`, `ioutil`
**Tags:** `geofeed`, `csv`, `geoip`, `prefix`, `lookup`, `ip-country`

---

## `internal/log`

`./internal/log/log.go`, `ctxlog.go`

Logging package using `github.com/rs/zerolog`. Sets up console logging with timestamps, caller info (short `file:line`), and configurable log level.

**Key functions:**
- `InitDefault()` — configure the global `zerolog.Logger` with default `info` level (called from `main()`)
- `InitLogger(level str) zerolog.Logger` — override global level from config, return logger
- `Op(logger, op) zerolog.Logger` — create child logger with `"op"` field (contextual)

**Tags:** `log`, `zerolog`, `logging`, `structured-log`, `contextual`

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

When no `countries`/`groups` are provided, the server can start with `All()` and subtract `exclude_countries`/`exclude_groups` to implement an inverted filter.

**Uses:** `geofeed`
**Tags:** `filter`, `country`, `bitset`, `geo`, `permit`

---

## `internal/resolver`

`./internal/resolver/resolver.go`

DNS resolver for subscription node hostnames. Uses system DNS or custom address. Deduplicates IPv4 results. Global `sync.Map`-based DNS cache with TTL expiry per `Resolver` instance. Returns shared cached slices, while preprocess isolates them once per request/hostname via a pooled resolved-map.

**Key types:**
- `Resolver` — `timeout` + `cache` (`sync.Map`) + `cacheTTL` + `dialer` + `sync.Pool` for resolved maps

**Key functions:**
- `New(timeout, address, ttl) *Resolver` — TTL defaults to 5 min if ≤ 0
- `(*Resolver).Resolve(ctx, host) ([]netip.Addr, error)` — cache-first (skip for bare IPs), fallback to real DNS; cached host results are returned without per-call copying
- `(*Resolver).GetResolvedMap() map[string][]netip.Addr` — get pooled per-request hostname map
- `(*Resolver).PutResolvedMap(m)` — return map to pool

**Tags:** `dns`, `resolve`, `hostname`, `ip`, `pool`, `dedup`, `cache`, `ttl`

---

## `internal/asn`

`./internal/asn/resolver.go`

ASN resolver using Team Cymru DNS (`origin.asn.cymru.com` + `asn.cymru.com`). Caches results in `sync.Map`. Currently IPv4-only.

**Key types:**
- `Result` — `Country` (`geofeed.CountryCode`) + `Name`
- `Resolver` — `cache sync.Map` + `timeout`

**Key functions:**
- `New(timeout) *Resolver`
- `(*Resolver).Resolve(ctx, ip) (Result, error)` — lookup + cache

**Uses:** `net` (stdlib, not internal resolver)
**Tags:** `asn`, `cymru`, `dns`, `ip`, `carrier`, `deny`, `name`

---

## `internal/subscription`

`./internal/subscription/subscription.go`

Subscription fetch, normalize (base64 → raw), and URI parsing. Lightweight node parser avoids `url.Parse` heap allocations. `Normalize` trims, uses a fast-path single-pass ASCII whitespace stripper, then attempts base64 decode.

**Key types:**
- `Scheme` — strict URI scheme type alias
- `Node` — `Raw` + `Scheme` + `Name` + `Server` + `Port` + `FragmentIdx`

**Key functions:**
- `Load(ctx, url fetch.SubscriptionURL) ([]byte, error)` — fetch + normalize
- `Parse(body, yield)` — iterate lines via `ioutil.Lines`, parse URIs containing `://`
- `Normalize(body) []byte` — trim + strip ASCII whitespace + base64 decode + URI detection
- `parseNode(line) (Node, bool)` — scheme → authority → host:port → fragment

**Uses:** `fetch`, `ioutil`
**Tags:** `subscription`, `uri`, `parse`, `node`, `normalize`, `base64`, `vless`, `trojan`

---

## `internal/rewrite`

`./internal/rewrite/rewrite.go`

Node output rewriting. Prepends `[GEO:XX][IP:x.x.x.x]` tags before node name. Strips existing known tags. Alloc-free IPv4 octet writing.

**Key functions:**
- `NodeName(b, node, country CountryCode, ip)` — write rewritten URI fragment into a reusable `bytes.Buffer`
- `StripKnownTags(s) string` — remove `[GEO:…]`, `[IP:…]`, `[OK]`, `[BAD]`, `[JUR:…]`
- `writeOctet(b, n)` — 1–3 digit IPv4 octet without `fmt.Sprintf`

**Uses:** `subscription`
**Tags:** `rewrite`, `output`, `fragment`, `tag`, `geo-tag`, `ip-tag`

---

## `internal/preprocess`

`./internal/preprocess/processor.go`, `filters.go`

Core processing. Orchestrates subscription loading, DNS resolution, geofeed/ASN filtering, and output rewriting per node.

**Key types:**
- `Processor` — country lookup (with async background reload via `TryLock`) + DNS resolver + sequential filter pipeline (no country cache, no groups map)
- `Stats` — `Total` / `Kept` / `DNSDrop` / `GeoDrop` / `ASNDrop` / `Unsupported`
- `PipelineContext` — request-scoped state shared across filters (`Buffer`, `Lookup`, `Allowed`, `Resolved`, `Stats`, `IsFirstNode`)
- `Filter` — interface for workflow stages; `Process(ctx, ips, pctx)`
- `GeofeedFilter` — returns IPs in allowed geofeed countries
- `ASNFilter` — drops IPs matching ASN deny patterns AND IPs whose Cymru-resolved country is not in `AllowedCountries` (so country filtering works without a geofeed stage)
- `Options` — configuration struct for `NewProcessor` (`GeofeedSources`, `RefreshInterval`, `DNSTimeout`, `DNSAddress`, `ASNTimeout`, `ASNDenyPatterns`, `WorkflowStages`)
- `FilterRequest` — request struct for `Filter` (`SubscriptionURL fetch.SubscriptionURL`, `AllowedCountries filter.CountrySet`)

**Key functions:**
- `NewProcessor(ctx, logger, opts Options) (*Processor, error)` — load geofeed, build filter chain
- `(*Processor).Filter(ctx, b, req FilterRequest) (Stats, error)` — main pipeline writing into caller-owned `bytes.Buffer`
- `(*Processor).resolveNode(ctx, server, resolved) []netip.Addr` — resolve once per request/hostname and copy shared resolver results into request-local storage
- `buildFilters(stages, asnR, patterns) []Filter` — construct filter pipeline; always appends a `GeofeedFilter` last even when `"geofeed"` is not explicitly listed, so that `AllowedCountries` (from `countries`/`groups`/`exclude_*`) is always enforced
- `FormatStats(stats) string` — `done: total=N kept=N …`

**Uses:** `asn`, `config`, `filter`, `geofeed`, `resolver`, `rewrite`, `subscription`
**Tags:** `orchestrator`, `pipeline`, `filter`, `geo`, `asn`, `stats`, `workflow`

---

## `internal/server`

`./internal/server/server.go`

HTTP layer using Fiber. Routes: `GET /healthz` → `ok`, `GET /` → preprocess subscription.

The root handler now accepts:
- `subscription_url` (required)
- `countries` / `groups` — additive allowed countries
- `exclude_countries` / `exclude_groups` — countries to remove from the allowed set

If only exclusion params are provided (i.e. `countries` and `groups` are both absent), the allowed set starts from `filter.All()` (every country) minus the exclusions. If `countries`/`groups` are present but produce an empty set, the fallback to `All()` is not applied, so the request fails with `400` when nothing remains. If exclusions remove every allowed country, the request also returns `400`.

**Key types:**
- `Filterer` — interface `Filter(ctx, b, req preprocess.FilterRequest) (Stats, error)`
- `Server` — `listen` + `fiber.App`

**Key functions:**
- `New(logger, listen, svc, groupsMap) *Server` — wires Fiber, logging, and the filter handler
- `newIndexHandler(svc, groupsMap) fiber.Handler` — root handler implementation: validates URL, builds allowed/excluded sets, and calls `Filterer`
- `buildCountrySet(rawCountries, rawGroups, groupsMap) CountrySet` — HTTP-layer group expansion (used for both allowed and excluded sets)
- `isEmpty(set) bool` — checks whether a `CountrySet` has any country set
- `(*Server).Listen() error`
- `(*Server).Shutdown(ctx) error`
- `(*Server).TestApp() *fiber.App` — for test usage

**Uses:** `fetch`, `preprocess`, `fiber`
**Tags:** `http`, `fiber`, `api`, `handler`, `server`, `healthz`

---

## Dependency Graph

```
main
 └─ app
     ├─ config ─── fetch, geofeed
     ├─ log        (zerolog initialization)
     ├─ preprocess
     │   ├─ asn        (Team Cymru DNS)
     │   ├─ config     (workflow constants)
     │   ├─ filter ─── geofeed (lookup)
     │   ├─ geofeed ── fetch, ioutil
     │   ├─ log        (ctxlog.Op helper)
     │   ├─ resolver   (hostname DNS)
     │   ├─ rewrite ── subscription
     │   └─ subscription ── fetch, ioutil
     └─ server ─── fetch, preprocess, log
```

## Quick Tag Index

| Tag | Package |
|---|---|
| `ssrf`, `http-client` | `fetch` |
| `geoip`, `csv`, `prefix` | `geofeed` |
| `bitset`, `country-filter` | `filter` |
| `dns`, `hostname-resolve` | `resolver` |
| `asn`, `cymru`, `carrier-deny` | `asn` |
| `uri-parse`, `node`, `base64` | `subscription` |
| `geo-tag`, `output-rewrite` | `rewrite` |
| `pipeline`, `orchestrator` | `preprocess` |
| `fiber`, `http-handler` | `server` |
| `config`, `yaml`, `defaults` | `config` |
| `bootstrap`, `wire` | `app` |
| `log`, `zerolog`, `structured-log` | `log` |
| `shared-iterator`, `unsafe-string` | `ioutil` |
