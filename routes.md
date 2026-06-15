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

Geofeed CSV parsing, lookup, and data source management. Entries are sorted by prefix length (most specific first). Default country lookup uses an internal indexed IPv4 structure with IPv6 linear fallback.

**Key types:**
- `Entry` — `Prefix` + `Country` + `Region` + `City`
- `Source` — `URL` + `Type` (also used in config.yaml via yaml tags)
- `CountryLookup` — interface with `LookupCountry(ip) string`
- `LinearLookup` — reference linear-scan implementation
- `IndexedLookup` — built-in indexed IPv4 lookup + IPv6 fallback

**Key functions:**
- `LoadAll(ctx, sources) ([]Entry, error)` — fetch + parse + sort by prefix length
- `Parse(body) ([]Entry, error)` — parse CSV body
- `NewLookup(entries) CountryLookup` — default indexed lookup builder
- `NewLinearLookup(entries) *LinearLookup` — explicit linear implementation
- `NewIndexedLookup(entries) *IndexedLookup` — indexed implementation
- `LookupCountry(lookup, ip) string` — helper forwarding to the configured lookup
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
- `ForEachLine(body, fn)` — convenience wrapper over Lines + callback

**Tags:** `util`, `iterator`, `unsafe`, `string`, `utility`, `shared`, `dry`

---

## `internal/filter`

`./internal/filter/filter.go`

Country filtering using a compact bitset (`[11]uint64`) for O(1) lookup of 2-letter country codes.

**Key types:**
- `CountrySet [11]uint64` — bitset for AA–ZZ (676 codes)

**Key functions:**
- `(*CountrySet).Has(country) bool` — O(1) check
- `ParseAllowCountries(raw) CountrySet` — parse `"DE,US,  nl  "` into bitset (uses `strings.SplitSeq`)
- `ParseAllowed(rawCountries, rawGroups, groupsMap) CountrySet` — parse countries + groups, expanding groups into countries
- `FirstAllowed(lookup, ips, allowed, strict) (ip, country, ok)` — first match in allowed set; strict = all must match
- `AllAllowed(lookup, ips, allowed) []netip.Addr` — filter IPs by allowed countries

**Uses:** `geofeed`
**Tags:** `filter`, `country`, `bitset`, `geo`, `permit`

---

## `internal/resolver`

`./internal/resolver/resolver.go`

DNS resolver for subscription node hostnames. Uses system DNS or custom address. Deduplicates IPv4 results. Pooled resolved-map for per-request reuse.

**Key types:**
- `Resolver` — `timeout` + `dialer` + `sync.Pool` for resolved maps

**Key functions:**
- `New(timeout, address) *Resolver`
- `(*Resolver).Resolve(ctx, host) ([]netip.Addr, error)` — resolve IPv4 with timeout
- `(*Resolver).GetResolvedMap() map[string][]netip.Addr` — get pooled map
- `(*Resolver).PutResolvedMap(m)` — return map to pool

**Tags:** `dns`, `resolve`, `hostname`, `ip`, `pool`, `dedup`

---

## `internal/asn`

`./internal/asn/resolver.go`

ASN resolver using Team Cymru DNS (`origin.asn.cymru.com` + `asn.cymru.com`). Caches results in `sync.Map`. Currently IPv4-only.

**Key types:**
- `Result` — `ASN` + `Country` + `Name`
- `Resolver` — `cache sync.Map` + `timeout`

**Key functions:**
- `New(timeout) *Resolver`
- `(*Resolver).Resolve(ctx, ip) (Result, error)` — lookup + cache
- `(*Resolver).Preload(ctx, ips)` — concurrent preload with semaphore (concurrency = 10)

**Uses:** `net` (stdlib, not internal resolver)
**Tags:** `asn`, `cymru`, `dns`, `ip`, `carrier`, `deny`, `name`

---

## `internal/subscription`

`./internal/subscription/subscription.go`

Subscription fetch, normalize (base64 → raw), and URI parsing. Lightweight node parser avoids `url.Parse` heap allocations.

**Key types:**
- `Node` — `Raw` + `Scheme` + `Name` + `Server` + `Port` + `FragmentIdx`

**Key functions:**
- `Load(ctx, url fetch.SubscriptionURL) ([]byte, error)` — fetch + normalize
- `Parse(body, yield)` — iterate lines via `ioutil.Lines`, parse URIs containing `://`
- `Normalize(body) []byte` — strip whitespace + base64 decode + URI detection
- `parseNode(line) (Node, bool)` — scheme → authority → host:port → fragment

**Uses:** `fetch`, `ioutil`
**Tags:** `subscription`, `uri`, `parse`, `node`, `normalize`, `base64`, `vless`, `trojan`

---

## `internal/rewrite`

`./internal/rewrite/rewrite.go`

Node output rewriting. Prepends `[GEO:XX][IP:x.x.x.x]` tags before node name. Strips existing known tags. Alloc-free IPv4 octet writing.

**Key functions:**
- `NodeName(b, node, country, ip)` — write rewritten URI fragment into a reusable `bytes.Buffer`
- `StripKnownTags(s) string` — remove `[GEO:…]`, `[IP:…]`, `[OK]`, `[BAD]`, `[JUR:…]`
- `writeOctet(b, n)` — 1–3 digit IPv4 octet without `fmt.Sprintf`

**Uses:** `subscription`
**Tags:** `rewrite`, `output`, `fragment`, `tag`, `geo-tag`, `ip-tag`

---

## `internal/preprocess`

`./internal/preprocess/processor.go`, `filters.go`

Core processing. Orchestrates subscription loading, DNS resolution, geofeed/ASN filtering, and output rewriting per node.

**Key types:**
- `Processor` — geofeed entries + country lookup + DNS resolver + sequential filter pipeline + country cache + groups map
- `Stats` — `Total` / `Kept` / `DNSDrop` / `GeoDrop` / `ASNDrop` / `Unsupported`
- `Filter` — interface for workflow stages
- `GeofeedFilter` — returns IPs in allowed geofeed countries
- `ASNFilter` — drops IPs matching ASN deny patterns
- `Options` — configuration struct for `NewProcessor` (`GeofeedSources`, `RefreshInterval`, `DNSTimeout`, `DNSAddress`, `ASNTimeout`, `ASNDenyPatterns`, `WorkflowStages`, `GroupsMap`)
- `FilterRequest` — request struct for `Filter` (`SubscriptionURL fetch.SubscriptionURL`, `RawCountries string`, `RawGroups string`)

**Key functions:**
- `NewProcessor(ctx, logger, opts Options) (*Processor, error)` — load geofeed, build filter chain
- `(*Processor).Filter(ctx, b, req FilterRequest) (Stats, error)` — main pipeline writing into caller-owned `bytes.Buffer`
- `buildFilters(stages, asnR, patterns) []Filter` — construct sequential filter pipeline from config
- `FormatStats(stats) string` — `done: total=N kept=N …`

**Uses:** `asn`, `config`, `filter`, `geofeed`, `resolver`, `rewrite`, `subscription`
**Tags:** `orchestrator`, `pipeline`, `filter`, `geo`, `asn`, `stats`, `workflow`

---

## `internal/server`

`./internal/server/server.go`

HTTP layer using Fiber. Routes: `GET /healthz` → `ok`, `GET /` → preprocess subscription.

**Key types:**
- `Filterer` — interface `Filter(ctx, b, req preprocess.FilterRequest) (Stats, error)`
- `Server` — `listen` + `fiber.App`

**Key functions:**
- `New(listen, svc) *Server`
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
