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
- Runs watcher in a goroutine; joins it via `watcherDone` channel before returning from shutdown

**Uses:** `config`, `geoblock`, `log`, `preprocess`, `reload`, `server`, `stable`
**Tags:** `bootstrap`, `wire`, `shutdown`, `lifecycle`, `hot-reload`

---

## `internal/config`

`./internal/config/config.go`

YAML config loading and validation. Uses `gopkg.in/yaml.v3`. Defines the full config schema. Also provides diff helpers used by the reloader to decide what changed.

**Key types:**
- `Config` — root config struct (`log`, `server`, `geofeed`, `resolver`, `workflow`, `asn`, `groups`)
- `GeofeedConfig` — `sources` + `refresh_interval` with `Validate() error` method
- `Groups` — `map[string][]string` with `Validate() error` method
- `LogConfig` — `level` (`yaml:"level"`, default `"info"`)
- `WorkflowConfig` — `stages` (sequential pipeline order; known names: `geofeed`, `asn`)
- `ASNConfig` — `deny_patterns` + `timeout`
- `GeoBlockConfig` — `db_path` + `ttl` + `Gemini GeminiConfig` (per-node Gemini geo-block list)
- `GeminiConfig` — `enabled`/`endpoint`/`model`/`marker`/`api_key`/`key_file`/`key_var`/`timeout`/`concurrency`; `APIKeyResolved()` reads the key inline or from `key_file` (agenix `KEY=VALUE` env file)

**Key functions:**
- `Load(path) (Config, error)` — read + unmarshal + apply defaults + call `cfg.Validate()`
- `(*Config).Validate() error` — validates geofeed sources and groups
- `(*GeofeedConfig).Validate() error` — validates sources are non-empty with valid types
- `(Groups).Validate() error` — validates group names and 2-letter country codes
- `Equal(a, b Config) bool` — deep equality check via `reflect.DeepEqual`; used by reloader to skip no-op reloads
- `GeofeedSourcesChanged(old, newCfg Config) bool` — true when `geofeed.sources` differ; reloader uses this to decide whether to carry over the existing lookup
- `ListenChanged(old, newCfg Config) bool` — true when `server.listen` changed; reloader logs a warning and ignores the change (restart required)

**Uses:** `fetch`, `geofeed`
**Tags:** `config`, `yaml`, `validation`, `startup`, `defaults`, `diff`, `reload`

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

When no `countries`/`groups` are provided, the server can start with `All()` and subtract `exclude_countries`/`exclude_groups` to implement an inverted filter.

**Uses:** `geofeed`
**Tags:** `filter`, `country`, `bitset`, `geo`, `permit`

---

## `internal/resolver`

`./internal/resolver/resolver.go`

DNS resolver for subscription node hostnames. Uses system DNS or custom address. Deduplicates IPv4 results. Process-wide TTL cache (RWMutex map): positive hits cached for `resolver.cache_ttl`, failed/empty lookups negative-cached for `resolver.cache_negative_ttl` (returned as empty result without error); zero TTLs disable caching entirely. Cache is capped at 16384 entries — on overflow expired entries are evicted, or the map is reset when everything is still fresh. preprocess still isolates results once per request/hostname via a pooled resolved-map. When a custom `resolver.address` is configured, `PreferGo: true` is set on the `net.Resolver` so the custom `Dial` function is actually used (the cgo resolver ignores `Dial`).

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

ASN resolver using Team Cymru DNS (`origin.asn.cymru.com` + `asn.cymru.com`). Results are cached in memory with a 6h TTL (`cacheTTL`), so repeated IPs across nodes/cycles skip the lookup; `CacheLen()` exposes the size. Currently IPv4-only.

**Key types:**
- `Result` — `Country` (`geofeed.CountryCode`) + `Name`
- `Resolver` — `timeout`

**Key functions:**
- `New(timeout) *Resolver`
- `(*Resolver).Resolve(ctx, ip) (Result, error)` — fresh Cymru lookup (IPv6 rejected)

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
- `Processor` — country lookup (with async background reload via `TryLock`) + DNS resolver + sequential filter pipeline (no country cache, no groups map)
- `Stats` — `Total` / `Kept` / `DNSDrop` / `GeoDrop` / `ASNDrop` / `GeoBlockDrop` / `Unsupported`
- `PipelineContext` — request-scoped state shared across filters (`Buffer`, `Lookup`, `Allowed`, `Resolved`, `Stats`, `IsFirstNode`)
- `Filter` — interface for workflow stages; `Process(ctx, ips, pctx)`
- `GeofeedFilter` — returns IPs in allowed geofeed countries
- `ASNFilter` — drops IPs matching ASN deny patterns AND IPs whose Cymru-resolved country is not in `AllowedCountries` (so country filtering works without a geofeed stage)
- `Blocklist` — interface `Blocked(host string) bool` (satisfied by `*geoblock.Store`); when set, `processNode` drops nodes whose `Server` is currently geo-blocked (`GeoBlockDrop`) before DNS resolution, on both `/` and the worker
- `Options` — configuration struct for `NewProcessor` (`GeofeedSources`, `RefreshInterval`, `DNSTimeout`, `DNSAddress`, `ASNTimeout`, `ASNDenyPatterns`, `WorkflowStages`, `Blocklist`, `PreloadedGeofeed`, `PreloadedLoadedAt`). The ASN resolver is now built whenever the `asn` stage is active (not only when `deny_patterns` is non-empty), so country filtering survives an empty deny list.
- `FilterRequest` — request struct for `Filter` (`SubscriptionURL fetch.SubscriptionURL`, `AllowedCountries filter.CountrySet`)

**Key functions:**
- `NewProcessor(ctx, logger, opts Options) (*Processor, error)` — load geofeed (or use `opts.PreloadedGeofeed` when set), build filter chain
- `(*Processor).Filter(ctx, b, req FilterRequest) (Stats, error)` — main pipeline writing into caller-owned `bytes.Buffer`
- `(*Processor).GeofeedState() (geofeed.CountryLookup, time.Time)` — returns the current lookup and `LoadedAt` under read lock; used by the reloader to carry geofeed state across config reloads when sources are unchanged
- `(*Processor).resolveNode(ctx, server, resolved) []netip.Addr` — resolve once per request/hostname and copy resolver results into request-local storage
- `buildFilters(stages, asnR, patterns) []Filter` — construct filter pipeline; always appends a `GeofeedFilter` last even when `"geofeed"` is not explicitly listed, so that `AllowedCountries` (from `countries`/`groups`/`exclude_*`) is always enforced
- `FormatStats(stats) string` — `done: total=N kept=N …`

**Options fields added for hot-reload:**
- `PreloadedGeofeed geofeed.CountryLookup` — when non-nil, `NewProcessor` skips the initial geofeed fetch and uses this lookup directly
- `PreloadedLoadedAt time.Time` — paired with `PreloadedGeofeed`; sets `Processor.LoadedAt` so the background refresh timer is not reset unnecessarily

**Uses:** `asn`, `config`, `filter`, `geofeed`, `log`, `resolver`, `rewrite`, `subscription`
**Tags:** `orchestrator`, `pipeline`, `filter`, `geo`, `asn`, `stats`, `workflow`, `hot-reload`

---

## `internal/server`

`./internal/server/server.go`, `holder.go`

HTTP layer using Fiber. Routes: `GET /healthz` → `ok`, `GET /` → preprocess subscription, `GET /stable.txt` → latest stability-tested node list. The active `Processor` and groups map are held in an atomic `Holder` so the reloader can swap them without restarting the server.

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

`./internal/stable/stable.go`, `merge.go`, `select.go`, `prober.go`, `prober_gemini.go`, `checker.go`, `controller.go`

Background worker that produces a stability-tested subscription list. Every `subscriptions.interval` it fetches each configured source through the geo/ASN pipeline (`Filterer`), merges the results into one deduplicated relabeled URI list, probes every node with the embedded mihomo library (`URLTest` HEAD requests, `check.rounds` rounds), keeps only nodes within `check.max_fail`/`check.max_avg_ms`, then runs a **Gemini reachability gate** through each surviving node (a real API `GET`, body-inspected for the geo-block marker — the check mihomo's HEAD-only `URLTest` cannot do), records geo-blocked node hosts in the `geoblock` store (TTL) and drops them, and atomically publishes the rest for `GET /stable.txt`. Every failure mode (all sources down, zero parsable nodes, prober error, zero survivors) keeps the previous snapshot.

**Key types:**
- `Stats` — `SourcesOK/SourcesTotal/Merged/Tested/Kept` counters for the `X-Stable-Stats` header
- `Snapshot` — immutable `Payload []byte` + `UpdatedAt` + `Stats`
- `Holder` — `atomic.Pointer[Snapshot]`; `Load()` returns nil before the first successful cycle
- `SourceBody` / `Entry` — merge input (source name + fetched body) and output (label + relabeled raw URI)
- `ProbeResult` / `Survivor` — per-node probe aggregate and selected node with mean delay
- `Filterer` — local copy of `server.Filterer` (avoids an import cycle); satisfied by `*preprocess.Processor`
- `Prober` — `Probe(ctx, payload) (map[string]ProbeResult, error)`; implemented by `MihomoProber`
- `GeminiOutcome` — per-node through-node Gemini result (`Server`/`Reachable`/`Blocked`)
- `Blocklist` — interface `Block(host string) error` (satisfied by `*geoblock.Store`); nil disables persistence
- `Checker` — the periodic worker loop
- `Controller` — start/stop lifecycle around `Checker`, driven by config (re)loads

**Key functions:**
- `Merge(bodies []SourceBody) []Entry` — dedupe by `Server:Port` first-wins in source order; relabel fragments to `<source>-NNN`
- `SelectSurvivors(entries, results, rounds, maxFail, maxAvgMs) []Survivor` — keep `rounds-successes <= maxFail && mean <= maxAvgMs`, sort by mean ascending
- `BuildPayload(survivors) []byte` — newline-joined URI list
- `NewMihomoProber(cfg config.CheckConfig, gemini config.GeminiConfig, geminiKey string, logger) (*MihomoProber, error)` — latency `Probe` (HEAD `URLTest`) plus `GeminiCheck(ctx, payload) map[label]GeminiOutcome` + `GeminiEnabled()`; `GeminiCheck` dials the Gemini API through each node (mihomo `DialContext` + fixed-conn `http.Transport`, `GET`) and scans the body for the geo-block marker
- `NewChecker(...)` / `(*Checker).Run(ctx)` — immediate first cycle, then ticker; `RunOnce(ctx)` is one cycle; after `SelectSurvivors`, `geminiGate` keeps only Gemini-reachable survivors and writes geo-blocked hosts to the store (TTL)
- `NewController(ctx, holder, filterer func() Filterer, store Blocklist, logger)` / `(*Controller).Apply(cfg) error` / `(*Controller).Stop()` — `Apply` resolves the Gemini key (`cfg.GeoBlock.Gemini.APIKeyResolved()`), builds the prober + checker (with the geo-block store), stops the old worker and starts a new one when `subscriptions.sources` is non-empty; allowed set = `filter.All()` minus `exclude_countries`/`exclude_groups`; `Stop` is idempotent

**Uses:** `config`, `filter`, `fetch`, `preprocess`, `subscription`, `mihomo` (adapter, common/convert, common/utils, constant)
**Tags:** `stable`, `probe`, `url-test`, `gemini`, `geoblock`, `delay`, `worker`, `mihomo`, `atomic-swap`

---

## `internal/reload`

`./internal/reload/reloader.go`, `watcher.go`, `options.go`

Config hot-reload. Watches the config file **and its `private.yaml` overlay sibling** for changes (via fsnotify on the parent directory), debounces bursts, and atomically swaps the active `Processor` + groups into the server `Holder`. On any error the previous settings are kept unchanged. The private overlay matters because the crawler writes it, and a change there must restart the stable worker.

**Key types:**
- `Reloader` — holds `path`, `*server.Holder`, `zerolog.Logger`, and the last-applied `config.Config` + `*preprocess.Processor` for diffing
- `Watcher` — wraps `*fsnotify.Watcher`; watches the config file's parent directory to survive atomic-rename writes; fires `onChange` for events on either `config.yaml` or its `private.yaml` sibling; debounces events with a 200 ms window

**Key functions:**
- `NewReloader(path string, holder *server.Holder, logger zerolog.Logger, cfg config.Config, proc *preprocess.Processor, ctl *stable.Controller, blocklist preprocess.Blocklist) *Reloader` — seed with startup state so the first reload can diff against it; injects the shared geo-block store into every rebuilt `Processor`
- `(*Reloader).Reload(ctx context.Context)` — load config → skip if `Equal` → build `OptionsFromConfig` (+ inject `Blocklist`) → carry geofeed state if `!GeofeedSourcesChanged` → `NewProcessor` → `SetLevel` → warn if `ListenChanged` → `holder.Store` new snapshot → `ctl.Apply(newCfg)` only when `SubscriptionsChanged || GroupsChanged`
- `NewWatcher(configPath string, onChange func(context.Context), logger zerolog.Logger) (*Watcher, error)` — register fsnotify watch on parent directory; return error if watcher or directory watch fails
- `(*Watcher).Run(ctx context.Context) error` — event loop: debounce matching events, call `onChange` once per burst; close fsnotify watcher on ctx cancellation and return nil (callers use the return as a join point)
- `OptionsFromConfig(cfg config.Config) preprocess.Options` — single source of truth for mapping `config.Config` to `preprocess.Options`; leaves `PreloadedGeofeed`/`PreloadedLoadedAt` unset (callers decide whether to carry over geofeed state)

**Uses:** `config`, `geofeed`, `log`, `preprocess`, `server`, `stable`, `fsnotify`
**Tags:** `reload`, `fsnotify`, `hot-reload`, `watch`, `atomic-swap`, `debounce`

---

## `internal/classify`

`./internal/classify/classify.go`

Decides whether a URL serves a usable Mihomo-compatible subscription, reusing the SSRF-safe client and the same normalizer/parser the preprocessor uses. Used by the `crawl` subcommand and the `classify` CLI subcommand.

**Key types:**
- `Result{Nodes int, Expired bool}` — `(Result).Live()` reports `Nodes > 0 && !Expired`

**Key functions:**
- `Body(body []byte, subUserinfo string, now int64) Result` — pure classifier: base64-normalizes the body, counts only **proxy-scheme** nodes (`vless`/`vmess`/`ss`/`ssr`/`trojan`/`tuic`/`hysteria`/`hysteria2`/`hy2`/`anytls` — so HTML pages full of `https://` links are rejected), and marks expired from a `subscription-userinfo: expire=` header
- `URL(ctx, client *http.Client, rawURL fetch.SubscriptionURL) (Result, error)` — SSRF-gate + fetch + `Body`

**Uses:** `fetch`, `subscription`
**Tags:** `classify`, `subscription`, `ssrf`, `crawler`

---

## `internal/crawl`

`./internal/crawl/crawl.go`, `discover.go`, `state.go`, `channels.go`

Format-agnostic, recursive subscription crawler. Scrapes public Telegram channel web previews (`t.me/s/<channel>` + `?before=` pagination), treats **every** https link as a candidate, keeps the ones that `classify` as a live subscription, and writes them to the `private.yaml` overlay as `tg-<sha10>` sources. Matches the artifact (a URL that returns a subscription), not any channel-specific wrapper pattern. Runs as the `crawl` subcommand in the same image as the service.

**Key types:**
- `Options{Channels []string, ChannelsPath string, PrivatePath string, Pages int, Prune bool, MaxDepth int, MaxChannels int, StatePath string, StateTTL time.Duration}`
- `Crawler` — `New(opts, logger)`; `RunOnce(ctx)` one cycle, `Run(ctx, interval)` loop

**Behavior:** `scan` (in `discover.go`) does a **relevance-gated BFS** over the channel repost graph — seeds are crawled unconditionally, a discovered channel (`extractChannels`: forwarded-from/@mention `t.me/<slug>` links, excluding self/reserved/bot `?start=`) is expanded only if it itself yielded a live subscription; the subscription yield is the thematic signal (a VPN channel forwards VPN channels; a news channel yields nothing and its branch stops). Depth is bounded by `MaxDepth`; `MaxChannels` caps discovered (non-seed) visits per cycle (`0` = unlimited). A per-cycle `visited` set means every channel is fetched at most once and a repost loop (A→B→A) can never re-enter an explored channel. Page fetches are sequential (rate-limit friendly). SSRF-gates candidates via `fetch.ValidatePublicHTTPSURL` and skips Telegram/CDN noise hosts before fetching; classifies concurrently; managed (`tg-`) sources are fully derived from currently-live URLs (implicit prune when `Prune`), hand-added private sources are preserved; only rewrites `private.yaml` (atomic temp+rename) when the managed set changes, so unchanged cycles trigger no reload.

**Persistence (`state.go`):** channels that yield a live subscription are recorded in a JSON state file (`StatePath`, default `/config/.crawler-state.json`) and become **permanent depth-0 seeds** on future cycles — always crawled and always expanded — so a proven-productive channel keeps growing the graph even on days its recent pages carry no live sub. Entries with no live sub for `StateTTL` (default 30d) are pruned; empty `StatePath` disables persistence.

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
 │   │   ├─ config     (workflow constants)
 │   │   ├─ filter ─── geofeed (lookup)
 │   │   ├─ geofeed ── fetch, ioutil
 │   │   ├─ log        (ctxlog.Op helper)
 │   │   ├─ resolver   (hostname DNS)
 │   │   ├─ rewrite ── subscription
 │   │   └─ subscription ── fetch, ioutil
 │   ├─ reload
 │   │   ├─ config     (Load, Equal, GeofeedSourcesChanged, ListenChanged, SubscriptionsChanged, GroupsChanged)
 │   │   ├─ log        (SetLevel)
 │   │   ├─ preprocess (NewProcessor, Options, GeofeedState)
 │   │   ├─ server     (Holder, Snapshot)
 │   │   └─ stable     (Controller.Apply on subscriptions/groups change)
 │   ├─ stable
 │   │   ├─ config     (SubscriptionsConfig, CheckConfig)
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
| `geoip`, `csv`, `prefix` | `geofeed` |
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
| `geoblock`, `sqlite`, `ttl`, `blocklist` | `geoblock` |
