# Agent Instructions

## Running commands

Always run project commands via `nix-shell` — the toolchain (Go version, linter, etc.)
is defined in `shell.nix`. Running tools directly may use different versions or fail.

Prefer Makefile targets for common flows:

```bash
nix-shell --run "make"
nix-shell --run "make test"
nix-shell --run "make fmt"
nix-shell --run "make race"
nix-shell --run "make bench"
```

## Workflow — mandatory, not advisory

Every change to production code, to a contract (an exported API, a config key, a metric
name, a published output shape), or to a document agents act on (`AGENTS.md`, `routes.md`,
`README.md`) MUST go through this loop:

> **implement → review (performance + architecture, as SEPARATE passes) → fix → repeat**

**Write the agreement down BEFORE the first implement step** — a numbered list of the agreed
points, plus what is explicitly out of scope, so a round is not spent proposing redesigns
nobody asked for. Without that list "100% of what was agreed" is unfalsifiable in both
directions: a reviewer can assert a point was agreed, an implementer can assert a dropped
point never was, and neither can be disproved.

The loop terminates on two conditions, both required: a full round returns **zero** open
findings, and **every numbered point is either implemented or struck by explicit agreement**,
the strike recorded in the list. Not "the happy path works". Not "tests pass".

**Proportionality.** The gate is risk, not diff size. A low-risk change — docs-only,
config-only — takes ONE pass, and that pass is the ARCHITECTURE one: a document or a key
that points at the wrong seam sends the next agent there, and neither has a hot path. Never
leave which pass survives to the agent's guess. Documents agents act on are IN scope
precisely because they fail this way — this very section is a docs-only change, it was given
a review pass, and it needed one: two of its load-bearing claims were false, both taken from
a report about an attempt that was rejected and never committed, so no reader could check
them. The termination conditions NEVER relax: a one-line change with an open finding is not
done either.

**A finding may be REFUTED, not only fixed — and the loop must allow it.** A round is clean
when every finding is either fixed or disproved with evidence, and a wrong finding
implemented is a regression the process invited. This is not hypothetical here: a reviewer
demanded dropping the ssr port-range check because `adapter.ParseProxy` supposedly enforces
it — measured, `NewShadowSocksR` has no range check at all and accepts `0`, `-1`, `70000`.
Another demanded rejecting a `mierus://` link whose first `port` is empty — measured,
mihomo `continue`s its INNER per-port loop and serves a working proxy from a later port, so
rejecting would have dropped usable nodes. Verify a finding against the code before
implementing it; say so with evidence when it is wrong. When the refutation is ITSELF
disputed, neither side closes the finding unilaterally: it goes to whoever owns the numbered
list — the agent that set the scope — who rules and records the ruling in that list.

**A green suite is not evidence, and MUST NEVER be reported as a review result.** A review
earns its cost only by trying to break the change; mutation is the cheapest way and is
expected — revert the fix, confirm the new test fails, restore. The restore is not done
until an empty `git diff` against the pre-mutation tree says so, and that check MUST run
before the round is reported: the mutation deliberately leaves production code wrong, and
agents here work in worktrees off a parent whose `./config` is bind-mounted read-write into
a running container (`docker-compose.yaml:14`). Mutation has repeatedly found tests here
that could not fail at all: the mieru outcome fold shipped with three tests that looked like
coverage while neutering both production fold conditions left the package green, and
replacing `betterTraceOutcome`'s body with `return a.name < b.name` also left it green
(`364a50d`) — every ranked case sorted identically under either key.

**Why review at all when CI is green.** The `geotrace` filter once shipped completely inert,
and stayed that way from `11e5ca3` to `e554307`. `go test ./...`, `-race` and
`golangci-lint` were all clean the whole time. `swapTagValues` stopped scanning at the first
space between tags while `rewrite.LeadingTags`, its own input source, skipped that
whitespace — and because `bandwidth` ran before `geotrace` and prepended `"[SPD:<n>M] "`,
every survivor in production arrived space-separated and came back untouched. Nothing
dropped, nothing warned, and the `corrected` counter read 0, which an operator reads as "the
offline chain was right" — the opposite of the truth. No test caught it: every fixture used
the pre-bandwidth name shape. (There is no `geotrace` filter now: `abf452b` deleted
`nodefilter_trace.go` and moved it into the annotate chain.)

**Why performance is its OWN pass.** A benchmark that moved is not a regression until the
changed code is shown to be on its path. On that same change the benchmark that moved most
was `BenchmarkParse_SkipsNonURILines`, which feeds 50 lines containing no `://`
(`internal/subscription/subscription_bench_test.go:93`) — `parseNode` is never called, so it
cannot execute one changed instruction and what moved was binary layout. Telling that apart
from real cost takes a hybrid tree (new production code, old test files) and `go tool
objdump` on the suspect functions. Without that pass the branch either ships a regression or
gets rewritten to chase a phantom. The inverse is just as real, and derivable without
running anything: `SelectSurvivors` does one `make([]Survivor, 0, len(entries))`, so its
B/op is `len(entries) * unsafe.Sizeof(Survivor{})` rounded up to the allocator's 8 KiB page
multiple — at the benchmark's `n = 500` and today's 136-byte `Survivor`, 73807 B/op. That
step function has been paid twice. `4ed8009` grew `Survivor` 80 -> 88 bytes for one added
`bool` and moved it 40960 -> 49152 B/op — **+20%**. **Allocations are the binding
constraint:** an `allocs/op` or `B/op` increase is a BLOCKING FINDING the change MUST
justify and the reviewer MUST accept before the round closes. A finding, not a prohibition,
because that 20% was paid deliberately: `0f7af54` then swapped the bool for the code itself
(88 -> 96 bytes, the same 49152 B/op) and the field is now `Entry.Country`, which feeds
`keptCountries` and `stable_kept_country_nodes`. An agent reading the rule as absolute would
have blocked it. `ns/op` is noisy and means nothing without a control: benchmarks here drift
several percent run to run over code whose allocation counters do not move at all.

**Why architecture is its OWN pass.** `8d2c3a5` fixed, in ONE change, all four places where
a mihomo proxy name was matched against an `Entry.Label` — mihomo expands one `mierus://`
link into one proxy per configured port, so every bridge missed for mieru. Every site was
locally correct afterwards and slice-scoped review had nothing left to say. It still left a
seam only a whole-package reader could see: the shared proxy map stayed
`map[string]mihomo.Proxy`, so a multi-port `mierus://` survivor reached the through-node
filters as whichever port mihomo emitted LAST — undoing the best-of-N choice the latency
probe had deliberately just made. `448279b` repaired it by folding on the outcome side
instead of collapsing the proxy set. A reviewer given one slice structurally cannot see
that; someone MUST look at the whole seam, especially where parallel work merges. Five such
bridges exist today — `checker.go`, `prober.go`, `prober_api.go`, `prober_bandwidth.go`, and
`prober_trace.go`'s `TraceCheck` winner map, which was born `entryLabel`-correct after
`8d2c3a5`. (`applyFilters` is now `filterAndMeasureEgress`, renamed by `abf452b`.)

**Why "100% of what was agreed" is its own condition.** "It works" is not "it is done". All
of these passed a working-feature check and were still defects: `Entry.Country` kept its
pre-trace value, so `/stable.txt` published `[GEO:DE]` while `stable_kept_country_nodes`
still counted that node as CA; `corrected`/`unanswered` rode `FilterReport.Dropped` and so
appeared on a Grafana panel titled "drops by reason" for a filter that drops nothing (both
`e554307`); `betterTraceOutcome`'s test exercised only the tiebreak, so its primary key
could be deleted with the suite green (`364a50d`).

**Comments are a first-class review target,** not polish — and so are the documents, which
rot the same way with nothing compiling against them. The recurring defect here is the
true-when-written claim. `routes.md`'s `parseNode` entry kept pointing at
`stable.tagCountry` (`merge.go`) for the aliasing rule after `abf452b` deleted that
function, and it survived a dedicated routes.md staleness pass (`322ee2a`) that rewrote
three other lines; the commit carrying this section is what finally fixed it. One comment
said the trace endpoint answers a fixed "211 bytes" — it has no fixed length, the endpoint
echoes the request User-Agent back, 202 and 301 bytes for two of them (`8d11c57`). One
claimed mihomo cannot emit a proxy shape a leading-zero mieru port makes it emit; rare, not
impossible (`bed264c`). Per the conventions below, a stale comment is worse than none, so a
review that approves the code and ignores its comments — or the docs its change falsified —
has not finished.

## Code conventions

- **Comments earn their place.** Write one only for what the code cannot say itself: the *why* (rationale, tradeoffs), non-obvious invariants, ordering/locking/concurrency rules, units, edge-case semantics, gotchas, or external behavior (mihomo quirks, SSRF, protocol details).
- **Never restate the code.** Delete doc blocks that only echo the name or signature (`// name returns the name`, `// NewX creates a new X`, `// Close closes it`) or narrate the next line. Self-explanatory code gets no comment.
- **A stale comment is worse than none.** Changing behavior means updating the comment or deleting it — never leave it describing the old world.
- Removing an obvious doc comment is lint-safe: `.golangci.yml` excludes revive's "exported … should have comment", and `godot` needs no trailing period. Comments that remain must still start with the symbol's name (`godoc`/`staticcheck`). staticcheck's SA5011 is off in `_test.go` only — it does not model `t.Fatal` as terminating, so `if x == nil { t.Fatal(...) }` followed by using `x` reads as a nil deref; it stays on for production code.

## Project overview for LLM agents

Before making any changes, read `./routes.md` — it describes every package in the
project, its key types/functions, tags, and the dependency graph. This gives an
LLM a complete orientation without browsing the full source.

After adding, removing, or significantly restructuring a package — or changing
a package's public API (key types, constructors, interfaces) — update
`./routes.md` to reflect the new state.

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
source, runs it through the same geo/ASN filter, merges and dedupes by
`server:port` (first source wins), relabels kept nodes to `<source>-NNN`, probes
every node with an embedded Mihomo URL test, keeps only those that pass all
rounds under the latency threshold, then runs the configured through-node
filters (`gemini`/`claude`/`chatgpt`/`tidal`/`bandwidth`) on the survivors. Only
then are the published names built — once, from what the probes learned, with
the egress each survivor reports about itself through `cdn-cgi/trace` when the
`annotate` chain names the `geotrace` provider. The result is
swapped in atomically; the last good list is kept if a cycle fails (`503` only
until the first cycle completes).

## Important current design decisions

- Parsing is **generic URI parsing**, not hardcoded to `vless://` only, and that stays the policy: there is deliberately **no whitelist** of mihomo-known schemes, so a scheme with no concrete reason to be rejected keeps the generic `scheme://[userinfo@]host[:port][?query][#fragment]` walk. Four schemes need more than that walk, because the fields the pipeline needs are not where it looks, and each decoder mirrors mihomo's so that a node we keep is a node the prober can convert:
  - `vmess://` — base64 JSON (`add`/`port`/`ps`).
  - `ss://` — the PORT picks the form, as it does for mihomo: a portful SIP002 link takes the untouched generic path, and a portless authority is the legacy form, base64 of `method:pass@host:port`, decoded with `base64.RawStdEncoding` ONLY. Narrower than our tolerant vmess decoder on purpose: the shadowsocks spec writes that form WITHOUT padding and mihomo decodes it with that alphabet alone, so a padded or url-safe payload is a node that merges, spends a probe, converts to nothing and parks in the dead cache. Branching on the `@` instead would keep `ss://<b64userinfo>@host` — SIP002-shaped but portless, and a line mihomo drops — and publish it under a defaulted port 443 it does not have.
  - `ssr://` — host, port AND display name all live inside one base64 payload, so nothing is readable from the URI. Accepted only on mihomo's own terms: a `/?` split, exactly 6 colon-separated head fields, a query `url.ParseQuery` accepts, and a port that is a bare decimal in 1..65535. `adapter.ParseProxy` decodes that port itself, so a non-numeric one is a node that converts and then fails to build; the range is stricter than that decode deliberately, since `NewShadowSocksR` has no range check of its own and nothing can dial `0`, `-1` or `70000`. Its name is relabeled through `subscription.RewriteSSRName` (the ssr twin of `RewriteVmessName`), which emits a **fragment-free** link — mihomo base64-decodes everything after `ssr://`, a `#fragment` included, and refuses the link if one is there.
  - `mierus://` — the port list lives in the query. We key the node on the first `port` value mihomo's adapter would actually serve (1..65535, or a range of two such), not merely the first one written down.
- Portless `http`/`https`/`socks`/`socks5`/`socks5h` lines are **rejected** by the parser: mihomo refuses such a proxy, and accepting them published any bare web URL in a source body as a node. The PORTFUL form is still a valid node — which is why `internal/classify`'s proxy-scheme whitelist and the crawler's inline-node regex still leave those schemes out: a `https://example.com:8443/docs` in a README must not make the page read as a live subscription.
- A subscription URL may also answer with an **Xray JSON config** instead of a URI list — panel software (Hiddify) does. `subscription.Normalize` converts such a document's outbounds into share links, so nothing downstream (`Parse`, `classify`, the geo pipeline, `Merge`, `rewrite`) knows about JSON. The asymmetry with the line above is deliberate, not an oversight: URI parsing is scheme-generic, the JSON conversion is **vless-only**, because 158 of the 160 proxy outbounds in the measured corpus were vless and one of the two shadowsocks entries carried the literal address `sdfsdf`. Add a protocol when data justifies it.
- Filtering logic only cares about hostname/IP and final geofeed country.
- Output rewriting is still **scheme-aware/safe**: it only rewrites parsed URI nodes.
- The on-demand `/` path does no liveness probing; it only geo/ASN-filters. The `/stable.txt` worker is the only place that probes nodes (embedded Mihomo URL test).
- In the `/stable.txt` worker a node is identified by its `Entry.Label` (`<source>-NNN`), never by the mihomo proxy name — because the two differ for `mierus://`, which mihomo expands into ONE proxy PER configured port, named `<label>:<port>/<protocol>`. `entryLabel` (`internal/stable/label.go`) folds that back; without it a healthy mieru node matched nothing, was never selected, went into the dead cache and counted `unreachable` in every through-node filter. The latency probe and both through-node outcome maps then fold a label's duplicates **best-of-ports** (the entry survives if any of its ports passed — best-of, never a sum, or `Successes` could exceed `check.rounds`), while the filters' proxy subset keeps EVERY port, so a port dead on our egress cannot mask a live sibling.
- The resolver keeps an in-memory DNS TTL cache (`resolver.cache_ttl` / `resolver.cache_negative_ttl`) so repeated stable cycles don't hammer the upstream DNS.
- Geofeed sources are explicit in YAML via `geofeed.sources[].url` + `geofeed.sources[].type`.
- File type is explicit only: `raw` or `gzip`. There is no auto-detection/legacy mode.
- Geofeed data is cached in memory and reloaded by `geofeed.refresh_interval` (unset → 24h; explicit `0` disables the refresh).
- A config reload NEVER restarts the `/stable.txt` worker. `stable.Controller.Apply` swaps a `CheckerSpec` the next cycle reads, so the crawler's hourly `private.yaml` rewrite cannot cancel a 20–55 min cycle in flight and burn a full probe pass. Nor does it stop the worker: a reload whose merged source list came out EMPTY is refused with a warning and the previous spec stays live, because every source comes from an overlay and an empty list is nearly always a missing file. Only shutdown calls `Stop`.

## Important security / correctness notes

- `subscription_url` is user input and must stay protected against SSRF.
- Fetching uses a safe HTTP client:
  - only `https` URLs are allowed
  - userinfo in URL is rejected
  - private/local targets are rejected
  - env proxy usage is disabled (`Transport.Proxy = nil`) to avoid SSRF bypass via proxy
- Do not reintroduce implicit proxy support unless SSRF validation is redesigned.
- Request context is passed explicitly through the stack. Prefer `ctx context.Context` as the first argument.
- Root `main.go` is the only normal place where `context.Background()` should be introduced.

## Current config shape

`config/config.yaml` is hot-reloaded on change, together with its overlay
siblings `config/sources.yaml` (curated sources) and `config/private.yaml`
(crawler-managed sources) — both append to `subscriptions.sources`. The `/`
request still takes its `subscription_url` and allowed countries from HTTP
query params; the `subscriptions` block only configures the `/stable.txt`
worker.

Unknown keys are rejected: `config.yaml` and both overlays decode with
`yaml.KnownFields(true)`, so a typo fails the load naming the key instead of
silently falling back to the default. An empty or comment-only overlay still
loads.

Important keys:

- `server.listen`
- `server.metrics_listen` — internal Prometheus `/metrics` endpoint (default `:9090`; docker-compose publishes it loopback-only on `127.0.0.1:9091`, never public)
- `geo.geofeed.refresh_interval` (default 24h when unset, explicit `0` = load once and never refresh) / `geo.geofeed.sources[].url` / `geo.geofeed.sources[].type` (`raw` or `gzip`)
- `geo.dbip.url` / `geo.dbip.refresh_interval` — DB-IP Country Lite monthly gzip CSV (`{yyyy-mm}`-templated URL, default built-in, 24h refresh; unlike the geofeed sibling, an explicit `0` means that same default — a month-stamped mirror frozen for the process lifetime is never what an operator wants, and a non-positive interval also blocks the retry a failed initial download arms); in-memory IP→country DB for the `dbip` annotate provider, built only when an annotate chain references it
- `geo.registry.urls[]` / `geo.registry.refresh_interval` — the five RIR delegated-extended files (defaults built-in, 24h refresh; explicit `0` = that default, as for `geo.dbip`); in-memory registration-country DB for the `registry` annotate provider, built only when referenced
- `geo.asn.timeout` / `geo.asn.cache_ttl` — Team-Cymru ASN lookups, in-memory TTL cache (default 24h)
- `resolver.timeout`
- `resolver.address` — upstream DNS server, passed verbatim to the dialer, so it MUST be `host:port` (`1.1.1.1:53`); a portless value is rejected at load, because it dials nothing and would drop every node as a DNS failure. Empty keeps the system resolver
- `resolver.cache_ttl` / `resolver.cache_negative_ttl` (DNS TTL cache)
- `filters` — ONE ordered list for both stages. IP-stage entries (`type: country` with `provider: geofeed|asn`; `type: asn` with `deny_patterns`) run per node in preprocess on both `/` and the worker; through-node entries (`type: gemini`/`claude`/`chatgpt`/`tidal`/`bandwidth`) run post-probe in the stable worker only, and every one of them is a gate that drops. `exclude_groups`/`exclude_countries` on a `country` entry are **worker-only** — their single consumer is `config.Config.DeniedCountries()`, which feeds the `/stable.txt` worker; on `/` the country constraint comes from the query params alone.
- `annotate` — ordered tag list (`tag: GEO` is the only accepted tag; it takes `providers:`, an ordered chain of `geotrace|geofeed|dbip|registry|asn` — first provider that resolves wins, all-miss renders `??`) prepended to node names on both `/` and `/stable.txt`; empty list disables annotation. One accepted tag does not make the LIST redundant: entries render in order and may repeat, so two `GEO` entries with different chains publish two tags (`preprocess.annotator` reports the leftmost that resolved as the node's country), and the empty list is a distinct mode rather than a milder one. **Repetition reaches RENDERING only.** `preprocess.countryChainOrder` builds the country FILTER's provider order from the FIRST `GEO` entry and returns, so a second entry's chain publishes a tag the filter never consulted — measured on an IP the geofeed cannot place and DB-IP puts in DE: one entry chaining `[geofeed, dbip]` keeps the node under `countries=DE` and publishes `[GEO:DE]`, while two entries (`[geofeed]` then `[dbip]`) render `[GEO:??][GEO:DE]` and geo-drop it. Pre-existing and deliberately left alone — merging the chains would change which nodes survive — so an operator wanting a provider in the filter must put it in the FIRST entry's chain. `geotrace` is the only provider that is not an offline database: it answers what the node reported about its own egress, which exists only in the worker's post-probe stage, so on `/` it always misses and the chain falls through. `config.Config.AnnotateUsesProvider("geotrace")` is what arms that probe — an offline-only chain never spends a request on it. The retired singular `provider:` key is rejected as an unknown key by the strict decode. The `IP` and `ASN` tags are retired — both removed outright, since nothing consumed them and no shipped config selected them, so `- tag: IP` and `- tag: ASN` now fail the load as unknown tags; `rewrite.isKnownTag` still recognises `[IP:…]` and `[ASN:…]` on the STRIP side, deliberately, so an upstream-authored address or AS attribution is removed instead of riding along. Note that `asn` the PROVIDER and `{type: asn}` the FILTER are untouched and load-bearing — only the tag went.
- `geoblock.db_path` / `geoblock.ttl` (SQLite per-host geo-block list; default TTL 720h)
- `geoblock.gemini.*` / `geoblock.claude.*` / `geoblock.chatgpt.*` / `geoblock.tidal.*` (`endpoint`, `timeout`, `concurrency`; plus `marker` for gemini/claude/chatgpt, `model` + `api_key`/`key_file`/`key_var` for gemini, `version` for claude) — base params for the `gemini`/`claude`/`chatgpt`/`tidal` node-filters; enabled by listing `{type: gemini}` / `{type: claude}` / `{type: chatgpt}` / `{type: tidal}` in `filters` (a filter entry may override these per-field). The `tidal` gate is the odd one out twice over. It has no refusal marker: where Tidal refuses an egress the request dies at the CDN (403 + HTML, measured from a RU egress), so the gate is **fail-closed on the status alone** — kept only on 2xx, and since redirects are not followed a 3xx interstitial is a refusal too. The body is deliberately not read (the country it reports gates where a subscription can be bought, not where an existing subscriber streams), so the gate answers only "did the request get through" and a 2xx from an ISP block page or captive portal counts as passed. It also never writes to the geoblock store (a bare status code is too weak a signal to persist host-wide for the store's TTL).
- The `gemini` gate CANNOT be made keyless, and a rejected key must never read as "not blocked". Measured against `generativelanguage.googleapis.com` from a geo-blocked egress: no key -> `403` "Method doesn't allow unregistered callers"; junk key (query param or `x-goog-api-key`) -> `400 API_KEY_INVALID`; valid key -> `400 FAILED_PRECONDITION` + the location marker. The API resolves caller identity, then key validity, and only then the location precondition, so the verdict this gate reads is invisible without a working credential — hence `geminiInconclusive` (401/403/404/429, plus a 400 carrying `API_KEY_INVALID`): those answers predate the verdict, are counted and warned, and a rotated key can no longer turn the gate into a silent no-op. Keyless substitutes are country-list guesses, not this check: `gemini.google.com/app` and `aistudio.google.com/welcome` both answered `200` from that same blocked egress, and the supported-country list is documented at `ai.google.dev/gemini-api/docs/available-regions` (RU/BY/CN/HK/MO/IR/KP/CU/SY/AF/MM absent)
- `geoblock.geotrace.*` (`endpoint`, `timeout`, `concurrency`; default `https://cloudflare.com/cdn-cgi/trace`, 15s, 8) — the egress probe behind the `geotrace` ANNOTATE provider, not a filter type: it gates nothing, drops nobody, and is armed by naming `geotrace` in an `annotate` chain. The offline providers cannot answer this — they place the address the RESOLVER returned, and 41% of the pool's named hosts sit in Cloudflare's shared anycast ranges, which terminate in many countries at once, so the tag described Cloudflare's registration while traffic left from the origin (measured: 47 of 102 published tags corrected). Only `ip=` from that endpoint is a fact; `loc=` is Cloudflare's own geo-IP lookup of it, so this buys the right ADDRESS to ask about, not a more honest database. Two limits: it never runs on `/` (no post-probe stage there), and it does not fix the `type: country` FILTER, which judges the resolved address in preprocess before any probe exists
- `deadcache.ttl` (in-memory cache of probe-dead nodes keyed by `server:port`; default 2h; skips re-probing; not persisted)
- `groups.<name>` (country sets referenced by requests and `exclude_groups`)
- `subscriptions.interval` and every `subscriptions.check.*` param are validated on every load, even with no sources configured (they all arrive from the overlays here, and a list-gated check let a bad value boot clean and then fail every later reload)
- `subscriptions.sources[].name` + `url` *or* inline `body` (base64/raw node URIs; used by the crawler's `tg-inline` harvest)
- `subscriptions.check.*` (`rounds`, `timeout`, `max_fail`, `max_avg_ms`, `test_url`, `expected_status`, `concurrency`, `source_timeout`) — URL-test (latency) prober params ONLY; through-node filters and exclusions live in the top-level `filters` list
- `fetch.timeout` — per-subscription fetch deadline (default 3s)
- `log.level` — zerolog level, hot-reloadable

## Important package map

- `main.go` — root entrypoint
- `internal/app` — app bootstrap, config load, service construction, server start
- `internal/config` — YAML config parsing/validation
- `internal/fetch` — safe HTTP fetching, file-type decoding, SSRF protections
- `internal/geofeed` — geofeed download/parse/lookup
- `internal/resolver` — DNS resolution with an in-memory TTL cache
- `internal/asn` — ASN lookup (Team Cymru) for the ASN name/country filter
- `internal/filter` — country allow/deny bitset
- `internal/subscription` — subscription fetch/normalize/parse (scheme-generic, plus dedicated decoders for `vmess://`, legacy `ss://`, `ssr://` and `mierus://` in `vmess.go`/`ss.go`/`ssr.go`/`mieru.go`, and Xray-JSON→share-link conversion in `xray.go`)
- `internal/rewrite` — node name/fragment rewrite (`[GEO:XX]`, vmess `ps` rewrite)
- `internal/preprocess` — the core per-node filter pipeline
- `internal/geoblock` — SQLite TTL list of node hosts that failed a through-node API reachability check (gemini/claude/chatgpt)
- `internal/stable` — `/stable.txt` worker: merge/dedupe/relabel, dead-node cache skip (pre-probe), Mihomo prober + through-node filters (gemini/claude/chatgpt reachability, tidal reachability, bandwidth), the post-filter `cdn-cgi/trace` egress measurement, one-shot name annotation at publication, checker loop, holder
- `internal/reload` — config file watcher + hot-reload
- `internal/server` — Fiber HTTP layer
- `internal/metrics` — renders stable-cycle stats as hand-rolled Prometheus text exposition; served on `server.metrics_listen`

## API behavior to remember

- `GET /healthz` returns `ok`
- `GET /` requires:
  - `subscription_url`
  - `countries` (comma-separated) OR `groups` (comma-separated, referencing `config.groups`)
  - optional `exclude_countries` / `exclude_groups` — a true **deny-list**, not a subtraction from the allow-list. A node is dropped only when its IP resolves to an excluded country; an IP no geo provider can place SURVIVES an exclusion-only request. (Folding exclusions into `All()` used to drop every unplaceable IP the moment one country was excluded.) Under an explicit `countries`/`groups` allow-list an unplaceable IP is still dropped — that is what an allow-list means. Unknown group names and non-alpha-2 codes are rejected with `400`, not silently ignored
- `GET /` no longer publishes a portless `http`/`https`/`socks`/`socks5`/`socks5h` line as a node: a bare `https://t.me/somechannel` in a source body used to be emitted as a node and now counts in `Stats.Unsupported` (`unsupported=` in `X-Preprocessor-Stats`) instead. A portful one (`https://example.com:8443`) is still a node
- `GET /` bounds one request: a 60s deadline (`504` on expiry, since fasthttp's request context has neither a deadline nor client-disconnect cancellation) and a 50k node ceiling (`413`). The ceiling is a DoS bound shared with the worker's per-source load, not a quality filter — at 20k it dropped a configured 33.4k-node aggregator source outright
- `GET /stable.txt` serves the worker's current list; `503` until the first cycle completes. Stats are returned in `X-Stable-Stats` (`updated=… sources=ok/total merged=… tested=… kept=…`)
- Response is `text/plain; charset=utf-8`
- `/` stats are returned in `X-Preprocessor-Stats`

Example:

```bash
curl "http://127.0.0.1:8080/?subscription_url=https://mifa.world/vless&countries=FI,EE,LV,LT,SE,PL,DE,NL"
curl "http://127.0.0.1:8080/?subscription_url=https://mifa.world/vless&groups=nordics,euronorth"
```

## Monitoring / metrics (Prometheus + Grafana)

The stable worker exports per-cycle stats as Prometheus metrics, visualized by a
Grafana dashboard. **The dashboard AND its NixOS wiring live in this repo** so they
track the metric names; the NixOS host pulls them in as a flake input — do NOT
vendor the dashboard into the nixos repo.

- `internal/metrics` renders `stable.CycleReport` as hand-rolled Prometheus text
  exposition (deliberately no `client_golang` — the `google.golang.org/protobuf =>
  metacubex/protobuf-go` replace in `go.mod` makes it risky). Served on an internal
  listener (`server.metrics_listen`, default `:9090`); `docker-compose.yaml` publishes
  it loopback-only (`127.0.0.1:9091:9090`) — keep it non-public.
- Data flows via the nil-safe `stable.Reporter`: `RunOnce` hands a `CycleReport`
  (per-source drops, per-filter in/kept/dropped-by-reason, kept speeds, cycle
  aggregate + duration) to `metrics.Metrics.Observe` on a published cycle, and
  `ObserveError()` on any abort. **Adding/renaming a metric? Update
  `deploy/grafana/sub-preprocessor.json` in the same commit.**
- `flake.nix` output `nixosModules.monitoring` (`deploy/monitoring.nix`) = the
  Prometheus scrape job (`127.0.0.1:9091`) + the Grafana dashboard provider
  (`deploy/grafana/sub-preprocessor.json`; datasource picked via a template
  variable, so no fixed uid). `nixosModules.default` is the separate systemd-service
  module — leave it.

**Editing the dashboard** — source of truth is `deploy/grafana/sub-preprocessor.json`
(provisioned `editable: false`; validate with `jq`, ideally render against a throwaway
Grafana+Prometheus first):

```bash
# here:
$EDITOR deploy/grafana/sub-preprocessor.json && git commit -am '...' && git push
# in the nixos repo (server imports inputs.sub-preprocessor.nixosModules.monitoring):
nix flake update sub-preprocessor && make switch
```

## Bench / performance notes

- Benchmark results are stored in `./benchmarks/bench-<UTC timestamp>.txt`. That directory
  is gitignored, so a figure quoted out of a snapshot is unreachable from a fresh checkout:
  cite the mechanism, or re-measure with `nix-shell --run "make bench"`
- **`BenchmarkProcessBodyPipeline` moved at `5d06fb6` and the FIXTURE, not the code, is the
  whole of the drop and then some.** Its annotate list is a fixture (`newBenchProcessor`,
  `internal/preprocess/pipeline_bench_test.go`), so the commit that removed the `IP` annotate
  tag also dropped a `{Tag: IP}` entry from it. Re-measured as four trees run round-robin in
  ONE session — `go test -bench '^BenchmarkProcessBodyPipeline$' -benchmem -benchtime 5000x
  -count=1 ./internal/preprocess`, 41 rounds each, first round discarded, medians of the
  remaining 40, 9800X3D: `23df10f` **18686 ns/op / 4642 B/op**; a hybrid of `23df10f`
  production code with `5d06fb6`'s fixture **15933 / 1600**; that hybrid with only
  `5d06fb6`'s `annotator.go` swapped in **16221 / 1600**; this HEAD **16212 / 1600**; 100
  allocs/op throughout. So the fixture is **100% of the B/op drop and 111% of the ns drop** —
  it OVERSHOOTS, because the production change put **+280 ns/op (+1.8%)** back on top of it,
  and swapping `annotator.go` alone reproduces all of that (+288). Two things follow for a
  reader diffing a fresh `make bench` against a pre-`5d06fb6` snapshot: 4640 -> 1600 B/op is
  not an allocation win the pipeline earned, and the post-`5d06fb6` ns baseline is **~16200,
  NOT the hybrid's 15933** — measuring ~16000 today is the baseline, not a regression. Read
  the +1.8% as a median shift, not a clean separation: the interquartile ranges are disjoint
  (hybrid 15856-16027, HEAD 16162-16348) but the full ranges overlap in the tails, and B/op
  itself drifts a byte or two run to run at these sizes. Not extra work either — `go tool
  objdump` on `(*annotator).Annotate` showed the 2-case switch `5d06fb6` left behind
  dispatching on ONE length compare (`CMPQ $3`, then `CMPW "GE"`/`CMPW "AS"`) where the
  3-case one needed two (`CMPQ $2` first, for the 2-byte `IP`) — five immediate compares
  against seven. It is block placement, and the bullet below shows the +1.8% was the same
  build-identity floor that made a null control read +1.97%. That switch is gone now: the
  `ASN` tag removal replaced it with a guard clause (`if t.key != config.TagGEO {
  continue }`), leaving three compares. Its arm was left alone at the time deliberately: a
  never-executed switch arm does not buy back three nanoseconds per node. Every
  `benchmarks/` snapshot older than `5d06fb6` records
  `4640 B/op`, so diffing a fresh `make bench` against one reads a 65% allocation win that
  does not exist.
- **The `ASN` annotate tag removal moved `BenchmarkAnnotate`, and the FIXTURE is the whole
  of it. It did NOT resolvably move `BenchmarkProcessBodyPipeline` — the null control that
  once certified otherwise is retracted below, and retracting it is the most useful thing
  in this entry.** The Annotate tag list lives in `annotator_bench_test.go` — the whole
  point of the benchmark — but the tag it measured second WAS the tag being removed, so its
  fixture had to go from two tags (`GEO`+`ASN`) to one. Controlled with FOUR trees exported
  by `git archive` into `/tmp` (no checkout touched), verified by SHA-256 tree diff, run
  round-robin in ONE session from prebuilt `go test -c` binaries, 31 rounds each, first
  discarded, medians of 30, 9800X3D. Three sessions have now rebuilt all four trees from
  that same recipe; the table is session 1 (the implementing round):

  | tree | differs from A in | `BenchmarkAnnotate` @500000x | `BenchmarkProcessBodyPipeline` @5000x |
  |---|---|---|---|
  | A = `372749b` verbatim | — | **77.61 ns/op, 48 B/op** | 16073.5 ns/op, 1601 B/op |
  | B = removal HEAD verbatim | 16 files | **57.14, 24** | 15971.0, 1601 |
  | C = B + A's `pipeline_bench_test.go` | 15 files (1 vs B) | 57.25, 24 | 15952.5, 1601 |
  | D = A + B's `annotator_bench_test.go` | 1 file | **56.33, 24** | 16073.0, 1601 |

  The file counts are `git diff --name-only 372749b 7f93685`, which lists **16** paths —
  `AGENTS.md` among them, because `3bddb93` edits it before `7f93685` appends this note.
  A vs C is 15, B vs C is 1, A vs D is 1 (`diff -rq` on the exports agrees). 1 alloc/op on
  `BenchmarkAnnotate` and 100 allocs/op on the pipeline throughout, every tree, every
  session; the pipeline's B/op drifts 1600-1603.
  - `BenchmarkAnnotate`: **D is the control.** A -> D (fixture alone) is -21.28 ns/op and
    the WHOLE of 48 -> 24 B/op. So the fixture is **104% of the ns move and 100% of the
    B/op move** — the durable result here, reproduced by two later sessions at 101% and
    104% with the same 48 -> 24 at the fixture swap. A reader diffing a fresh `make bench`
    against a pre-removal snapshot sees 48 -> 24 B/op and must read it as "the benchmark now
    annotates one tag", never as an allocation win.
  - **The D -> B residual is not a quantity this benchmark can report.** Session 1 measured
    +0.82 ns/op, session 2 +0.17 and +0.03 on two runs, session 3 +0.855 — while D and D2,
    the SAME source re-exported under a longer path and rebuilt, differ by 0.36 (session 2:
    57.19 vs 56.83) and 0.26 (session 3: 56.55 vs 56.805) with nothing to explain it but the
    link. Against B's own 20-sample range of 56.29-58.37 the residual is **indistinguishable
    from zero at a floor of roughly ±0.4 ns/op**; quote it as that, never as a number.
  - **`BenchmarkProcessBodyPipeline`: the delta is NOT resolvable at this benchmark's
    precision, and the D null control is withdrawn.** Its fixture did not change
    (`newBenchProcessor` was already GEO-only after `5d06fb6`, and C differs from B in
    `pipeline_bench_test.go` by COMMENT LINES ONLY — comment-stripped, A's and B's copies
    hash identically), so A -> C is the fixture-held production comparison and D, which
    cannot execute one changed instruction, must reproduce A exactly. It does not. Medians,
    each session having rebuilt every tree itself:

    | session | A | C | B | D (must equal A) | D2 (D's source, longer path) |
    |---|---|---|---|---|---|
    | 1 (implement) | 16073.5 | 15952.5 | 15971.0 | 16073.0 | — |
    | 2 (review) | 16104.5 [15992-16249] | 15995.0 [15891-16135] | 15935.0 [15836-16018] | **16421.5 [16357-16582]** | 16116.5 |
    | 3 (this round) | 16075.5 [16028-16567] | 16016.5 [15932-16130] | 15937.0 [15782-16048] | 16113.0 [16033-16301] | 16071.5 [16006-16161] |

    A -> D reads +0.5, **+317.0 (+1.97%, ranges DISJOINT)** and +37.5 ns/op across the three;
    A -> C reads -121.0 (-0.75%), -109.5 (-0.68%) and -59.0 (-0.37%). The direction of A -> C
    is consistent, its magnitude is not, and every value of it is smaller than the spread the
    null control alone covers. Session 2 pinned that down: D's excursion survives reversing
    the round-robin order (D 16413.5, n=20), it is not a response to editing source at all
    (A plus one appended comment line 16049.0, A plus four 16088.5, D 16404.0 in the same
    run), and `go tool objdump` on `processBody`, `(*annotator).Annotate`,
    `(*annotTag).lookupCountry`, `rewrite.NodeName` and `rewrite.StripKnownTags` gives
    opcode-byte-identical listings at IDENTICAL entry addresses for A and D (session 3
    reproduced this: `processBody` 0x6fd720, `Annotate` 0x6f8de0, same SHA-256 over the
    opcode column). So there is a **build-identity floor of 0.2-2.0%** on this benchmark that
    no source difference explains, the -0.75% sits under it, and the `objdump` argument below
    — not the ns number — is what supports the direction. This retroactively confirms the
    `5d06fb6` bullet above calling its +1.8% block placement: that was the same floor.
  - Every Annotate range overlaps too (session 1: D 55.72-57.12 vs B 56.22-58.55).
  - **What did change is real, and it is the static shape.** `go tool objdump` on
    `(*annotator).Annotate`: master emits the ASN test FIRST (`annotator.go:134`,
    `CMPW $0x5341` "AS" then `CMPB $0x4e` 'N') and reaches the GEO test (`CMPW $0x4547` "GE",
    `CMPB $0x4f` 'O') on the JNE. **Five key compares become three** (A: `CMPQ $3`, "AS",
    'N', "GE", 'O'; B: `CMPQ $3`, "GE", 'O') and the function shrinks **205 -> 169
    instructions** — measured 205/205/169/169 for A/D/B/C, and both figures reproduce
    exactly in every session. The DYNAMIC cost, though, was one compare, not two: the `JNE`
    after `CMPW $0x5341` jumps straight to the GEO test, so for key "GEO" the `CMPB $0x4e`
    is never reached. Every GEO tag on every node paid **one compare and one
    perfectly-predicted taken branch** for an arm it never entered — over the pipeline's 100
    nodes that is tens of ns/op, not 121, so it is **consistent with the sign of** the
    pipeline's A -> C, and predicts nothing about its size. `BenchmarkAnnotate` moving the
    other way over a strictly shorter path says the same thing from the other side.
  - No `allocs/op` or `B/op` increase on any of the 49 benchmarks. Five in untouched packages
    (`Parse_1000Entries`, `ParseProxies`, `Parse_Vmess`, `Resolution`, `Resolution_Concurrent`)
    differ between trees at `-benchtime 100x`, and the SAME tree reproduces the same spread
    across three consecutive runs — `Resolution_Concurrent` even flips 64/65 allocs/op — so
    that is the instrument, not the change.
  - **The lesson, since this is the second round it has cost:** a hybrid tree is a control
    for FIXTURE effects only. It is not a session-stability certificate, and a null control
    that comes back clean has not proved the session is quiet — rebuild it, and treat any
    delta smaller than the null control's own spread as unmeasured.
- Recent optimization work improved:
  - geofeed parsing allocations
  - fragment rewrite allocations
  - inner filter hot-path allocations
  - skipping non-URI lines during subscription parse
- Still-hot areas worth revisiting before large refactors:
  - `internal/subscription.Parse`
  - `internal/filter.ParseAllowed`
  - IPv6 support in resolver/filter path is still incomplete / not yet generalized

## Project layout

- `main.go` — entry point
- `config.yaml` — application configuration
- `Makefile` — common targets (`run`, `test`, `fmt`, `race`, `bench`)
- `.golangci.yml` — linter configuration
- `internal/` — internal packages
- `benchmarks/` — timestamped benchmark output snapshots
