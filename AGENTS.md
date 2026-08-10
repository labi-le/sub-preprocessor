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
a running container (`docker-compose.yaml:14`). **Mutate in a `git archive` export under
`/tmp`, never in the shared worktree.** A mutation is a deliberately wrong tree and
concurrent agents are the normal mode here, so a peer reading the shared checkout
mid-round sees a defect that does not exist — this round one reviewer's harness showed
another a red they could not reproduce. Isolation makes the empty-diff check vacuous
rather than optional: it still binds anything mutated in place, and in place is what you
must avoid — and an untracked package (`internal/cidrset` on this branch) has no `git
diff` to be empty at all, so there the check is `cmp` against a pre-mutation copy.
Figures measured in an export tree describe that tree, which is why none of this round's
mutation numbers reached a document. Mutation has repeatedly found tests here
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
the pre-bandwidth name shape. (There is no `geotrace` filter now, and no `geotrace` at all: `abf452b` deleted
`nodefilter_trace.go` and moved it into the annotate chain, which this branch then renamed to `cloudflare`. The
two mentions BEFORE this parenthetical name the symbol as it was spelled at the time, so the commits stay
findable.)

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
multiple — at the benchmark's `n = 500` and today's 136-byte `Survivor`, 73728 B/op. That
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
shipped by `b545d0a`, both corrected in `e554307`); `betterTraceOutcome`'s test exercised
only the tiebreak, so its primary key could be deleted with the suite green (`364a50d`).

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
source, runs it through the same IP-stage filter chain (geo/ASN/cidr), merges and dedupes by
`server:port` (first source wins), relabels kept nodes to `<source>-NNN`, probes
every node with an embedded Mihomo URL test, keeps only those that pass all
rounds under the latency threshold, then runs the configured through-node
filters (`gemini`/`claude`/`chatgpt`/`tidal`/`bandwidth`) on the survivors. Only
then are the published names built — once, from what the probes learned, with
the egress each survivor reports about itself through `cdn-cgi/trace` when the
`annotate` chain names the `cloudflare` provider. The result is
swapped in atomically; the last good list is kept if a cycle fails, and with
`subscriptions.snapshot_path` set it is also written to disk and reloaded at
startup, so `503` is left for a genuinely cold start rather than every restart.

**Two instances of this binary run from the one image** (`docker-compose.yaml`):
`sub-preprocessor` on `:7008` reading `./config`, and `sub-preprocessor-vassago`
on `:7009` reading `./config-vassago`. Same code, same two modes, different
`filters:`. The vassago instance gates on the ENTRY address — a `cidr` allow-list
holding `hxehex/russia-mobile-internet-whitelist` — then `country`, then `bandwidth`.
The `country` entry carries no `exclude_*`, so it is inert on `/stable.txt`
(`GeofeedFilter` early-returns on a full allow set with an empty deny set) and is
there for `GET /`: without an entry of that type `buildFilters` builds nothing,
and the server goes on demanding a `countries`/`groups`/`exclude_*` parameter that
no filter then reads. It arms no through-node geo gate and no `cloudflare`
provider: its nodes are meant to be used UNDER that whitelist, so an egress-geo
gate answers a question nobody asked and each one costs a request per survivor to
do it. Only the first instance runs the crawler and holds the Gemini key; the
second's 54 sources are curated by hand, against a measurement its
`config-vassago/sources.yaml` header records. A change to "the shipped config" now
has to be checked against BOTH directories.

**Do not read that upstream repository name as a description of the data.** Measured
2026-08-10 over the whitelist's 15649 intervals: AS749 DNIC (US DoD) is 20.97% of the
covered addresses, AS0 (unrouted) a further 6.52%, and by country it is 28.32% US against
10.60% RU — a worldwide `0.0.0.0/0` scan artifact, not an operator ACL. Nothing is kept
falsely by this, since no node is hosted in DoD space, but list size is NOT useful size:
no deployment may be sized off the 15649, only off a measured node count.

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
- The `cidr` allow-list **fails closed at BUILD**, unlike every downloadable database beside it. Two gates, both fatal to `NewProcessor`: `cidrset.Load` errors when no URL yielded a range, and `newCIDRStore` refuses an empty set (`errEmptyCIDRSet`) even where a load reported success. A warning would be wrong here, because an allow-list that failed to download is not a degraded filter but the INVERTED one — it drops every node while every counter reads healthy, and the instance publishes an empty subscription. Verified against the real config: pointing `filters[].urls` at a 404 exits 1 with `no cidr ranges loaded (1 source(s) failed)`. A hot reload hitting the same error keeps the previous processor, so the running list survives a typo.
- **The allow-list's swap guard counts COVERED ADDRESSES, never merged ranges.** `swapRefusalSize` is shared with the geo databases, whose unit is ranges, and the cidr store hands it `Set.Covered()` instead. The two quantities move in opposite directions, and the upstream itself is the proof: `cidrwhitelist.txt` is 30228 lines -> 15649 merged ranges -> 36265984 addresses, while the SAME repo's `ipwhitelist.txt` publishes that identical space as 141664 lines — every one of them the `.1` of a /24 already covered, 0 outside. Configure both URLs and lose the CIDR file, and a range-count guard reads 15649 -> 141664 (+805%) as healthy growth while swapping in a set covering 0.39% of the space, whose only survivors are nodes resolving to a literal `.1`. Coverage refuses that swap; `Len()` applauds it. That is why `cidrset.Len`'s doc comment says what it must not be used for.
- **That 15649 is an INTERVAL count and no CIDR tool reproduces it.** `cidrset.newSet` coalesces any two ranges that TOUCH (`next.lo <= ranges[last].hi+1`), where `ipaddress.collapse_addresses()` merges only ALIGNED prefix pairs and answers 30222 CIDRs over the same `cidrwhitelist.txt`. Both are right about different things. This is written down because a reviewer filed the repo's own 15649 as stale over exactly that disagreement and retracted: check a range count against the merge rule that produced it, never against a tool with a different one.
- **At most ONE `cidr` entry per config**, rejected at load rather than merged. `urls:` already expresses the union, so a second entry could only mean intersection — a shape nothing here wants — and one entry means one live store, which is what lets the reload carry-over stay a single `CIDRState` instead of a keyed collection.

## Curating a subscription source list

`config-vassago/sources.yaml` is curated entirely by hand — no crawler writes into that
directory — and one measured round of it (2026-08-10) produced four findings that cost hours
to obtain. That file's header carries the round's numbers; what follows is the part that
outlives them, and it applies to `config/sources.yaml` just as well.

- **The biggest names marketed FOR whitelist bypass contribute nothing; the bulk comes from
  undifferentiated aggregators.** Measured across ten independent search channels:
  `github.com/zieng2/wl` and all 15 files of `igareck/vpn-configs-for-russia` (7992 stars)
  scored ZERO new whitelisted `server:port` against the configured pool. That is a statement
  about the famous ones, NOT about the class. Counting by the boundary that needs no
  judgement — whitelist marketing in the FILENAME — six of the 33 added qualify and carry
  377 of the 2247 marginal gains, 17%. State which boundary you used or the next reader
  derives a different answer: the configured `name:` happens to select the same six here,
  while whitelist marketing ANYWHERE in the URL selects eight and 519, 23%. The bulk
  came from aggregators making no such claim under every one of those readings, and the
  mechanism was measured on `zieng2/vless_universal.txt`, 325 nodes: 88 hold an IP genuinely
  inside the whitelist, 29 present a whitelisted SNI from a non-whitelisted IP, and 208 are
  ordinary foreign nodes. Those 29 are SNI spoofing — a different bypass from CIDR
  membership, and one this pipeline deliberately does not implement. Marketing predicts
  nothing and a star count predicts nothing; measuring against the CURRENT pool is the only
  signal.
- **A count is not a coverage, and a sum is not a union.** Ten agents each reported a sum of
  per-source `wl_new` that overstated their slice's true union by 2.2x to 3.3x — these
  sources overlap heavily. Worse, a marginal gain measured greedily against the sources
  accepted BEFORE it is a lower bound that cannot be added, subtracted or reordered: this
  round's 33 additions have gains summing to 2247, the 36 they were picked from showed a
  union of +2368, and the shipped list re-measures at 4006 against the 1709 baseline.
  Three separate measurements over three different sets; none is derivable from the
  others, and the last supersedes the rest. Union the sets. Never total the counts, and
  never derive a new figure by taking one source's number out of an old one.
  **And re-check every COMPARISON when you re-measure its operands.** A "staler than", a
  "more than half", a "2.3x" is a derived quantity that keeps its old truth value while the
  numbers around it move, so it survives exactly the sweep that fixes everything else. Two
  shipped here — three sources called "all staler than the 94-day reject" when two were
  fresher than it, and two survivors called "both worse than" a bar one of them sat inside —
  with every individual figure correct in both.
- **Yield alone is not selection. Three gates are, and a candidate must pass all of them.**
  1. FRESH. A frozen fork scores well ONCE, as an archive of servers the live upstream has
     since rotated out, then decays while still costing a DNS resolve every cycle. The
     signal used was the repository's commits atom feed — and that feed is this gate's hole:
     two `storage.yandexcloud.net` candidates publish none, sailed through untouched at 94
     and 122 days stale, and were caught by hand on `Last-Modified`. A candidate with no feed
     needs that manual check or the gate is not applied to it at all — and the hole is not
     caught at candidate time. It survives into the SHIPPED list: three no-feed sources were
     accepted and shipped at 52-104 days before the by-hand check reached them, so audit the
     entries you already have, not only the ones you are adding.
  2. FETCHABLE inside `fetch.timeout`, shipped at 3s, AND under the worker's 10 MiB body
     cap (`subscription.maxSubscriptionSize`), which rejects an oversized source with
     `response too large` so it loads NOTHING. Both halves bit this round: the
     best-yielding candidate found — 409969 nodes — takes 9s, and
     `gitverse.ru/LimonTH/proxy-list` `output/live` (forge-native; 404 on GitHub)
     downloads in 1.4s for +445 but is 10.4 MiB. Neither contributes a
     single node however good its content is, and the second was shipped for one cycle
     before the log said so. Measure best-of-3: the failure mode here is bimodal, not
     slow. A selection script that times the fetch but never weighs the body credits
     candidates production cannot use.
  3. MARGINAL, and worth its DNS. It must contribute whitelisted `server:port` that no
     ALREADY-ACCEPTED source carries — a source that only re-carries an accepted source's
     nodes is rejected however large it is, and that test alone condemned all sixteen
     removals — and the whole ADDED BLOCK must fit a per-cycle budget of resolved hosts.
     The cost unit of that budget is DISTINCT HOSTS per cycle, never node lines: past the
     `cidr` filter a non-whitelisted node is one cached lookup and dies pre-probe, so a large
     source's tail is not free and the budget a candidate is charged against is a host budget.
- **One sample is not evidence for removal.** These lists rotate within the hour. Six audit
  samples over 1.5h backed dropping 16 redundant sources, and four more (`aetris bijandi
  flat447 prominbro`) were deliberately KEPT because they were redundant in some samples and
  unique in others. A single sample showing a source adds nothing shows only that.

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
- `geo.registry.urls[]` / `geo.registry.refresh_interval` — the five RIR delegated-extended files (defaults built-in, 24h refresh; explicit `0` = that default, as for `geo.dbip`); in-memory registration-country DB for the `registry` annotate provider, built only when referenced. **APNIC's file comes from `ftp.lacnic.net`, not `ftp.apnic.net`, and that is not cosmetic.** APNIC's own host answers the ClientHello without echoing the legacy session ID RFC 8446 4.1.3 requires, so `crypto/tls` aborts with "server did not echo the legacy session ID" (curl: "invalid session id" / connection reset) — the RIR then drops out of the build with `sources_failed=1` and the database loads 4 of 5 registries. Measured cost of that one dead URL: 255972 ranges instead of 330937, and 97.30% of the addresses behind the configured subscriptions placed instead of 99.85%. **LACNIC rather than the obvious RIPE copy is also not cosmetic**: both serve the file byte-identically under APNIC's own published `.md5` (`077c5ac4…`, checked against both mirrors and APNIC's `.md5` over FTP), so the tiebreak is single-host concentration. `ripencc` already comes from `ftp.ripe.net`; adding `apnic` there puts 201810 of 330937 ranges = 61.0% behind one outage, while `ftp.lacnic.net` leaves the worst host at ripencc's own 126845 = 38.3% — the floor no mirror choice can beat — with `ftp.lacnic.net` itself only second at 108784 = 32.9% — and `newGeoDB` at STARTUP takes the partial build with only a Warn, so that cliff is live. Byte-identity is a statement about the instant it was checked and NOT about freshness: a mirror is a copy re-cut on the upstream's schedule, so `LoadRegistry` logs each source's header serial (`serial=20260804` for apnic/lacnic/afrinic, a unix timestamp for ripencc and unix ms for arin — monotonic per source, not comparable across them) as an observable for lag. Nothing gates on it. Do not "fix" the URL back to the direct host, and do not "tidy" it over to the RIPE one
- `geo.asn.timeout` / `geo.asn.cache_ttl` — Team-Cymru ASN lookups, in-memory TTL cache (default 24h). **`asn` is not in the shipped `annotate` chain and re-adding it needs a new measurement, not a hunch.** Cymru's `origin.asn.cymru.com` redistributes over DNS the same RIR delegation data `registry` already holds in RAM, so it is a registry lookup with a network hop, not a geolocation source: measured over every address behind every configured subscription it was a strict SUBSET of a complete `registry` (zero countries it placed that registry did not, zero disagreements where both answered, and its disagreement rate against `dbip` was indistinguishable from `registry`'s), and behind the shipped chain its MARGINAL contribution was zero hits — `dbip` sits two steps ahead at ~99.85% coverage, and the residue is unroutable sinkhole / RFC 5737+2544 space Cymru correctly has no record for. Dropping it from the chain added no `[GEO:??]`. What it uniquely carries is the AS **name**, which no local database has, so `{type: asn}` deny patterns and `{type: country, provider: asn}` stay live and stay tested
- `resolver.timeout`
- `resolver.address` — upstream DNS server, passed verbatim to the dialer, so it MUST be `host:port` (`1.1.1.1:53`); a portless value is rejected at load, because it dials nothing and would drop every node as a DNS failure. Empty keeps the system resolver
- `resolver.cache_ttl` / `resolver.cache_negative_ttl` (DNS TTL cache)
- `filters` — ONE ordered list for both stages. IP-stage entries (`type: country` with `provider: geofeed|asn`; `type: asn` with `deny_patterns`; `type: cidr` with `urls` + `file_type` + `refresh_interval`) run per node in preprocess on both `/` and the worker; through-node entries (`type: gemini`/`claude`/`chatgpt`/`tidal`/`bandwidth`) run post-probe in the stable worker only, and every one of them is a gate that drops. `exclude_groups`/`exclude_countries` on a `country` entry are **worker-only** — their single consumer is `config.Config.DeniedCountries()`, which feeds the `/stable.txt` worker; on `/` the country constraint comes from the query params alone, and only when a `country` entry exists at all: `buildFilters` builds nothing for an absent type, so a list without one leaves `GET /` demanding a `countries`/`groups`/`exclude_*` parameter (`server.go` answers `400` without one) that nothing then consults. A `cidr` entry is an ALLOW-list of IPv4 ranges downloaded from `urls` and unioned (`file_type` `raw`|`gzip`, default `raw`; `refresh_interval` unset or an explicit `0` means 24h, as for `geo.dbip`, negative rejected); it reads nothing from the request, so it is the one IP-stage entry whose verdict is identical on `/` and the worker. Its drops book `Stats.CIDRDrop` / `cidr_drop=` / `stable_source_dropped_nodes{reason="cidr"}`
- **The IP-stage order is load-bearing, not cosmetic, and nothing validates it.** `buildFilters` appends in written order and `processNode` returns at the first filter that empties the IP slice, so a cheap local gate belongs ahead of one that makes a network call per IP. The case that matters is `cidr` before `asn` (and before `{type: country, provider: asn}`): written the other way round, `ASNFilter.Process` spends a Cymru round trip per IP on nodes the allow-list discards one step later — 54535 of the measured pool's 59558 nodes. `config-vassago/config.yaml`, the only shipped config carrying a `cidr` entry, puts it first. This stays a documented rule rather than a validator, because a validator would have to encode a cost model it has no business owning
- `annotate` — ordered tag list (`tag: GEO` is the only accepted tag; it takes `providers:`, an ordered chain of `cloudflare|geofeed|dbip|registry|asn` — first provider that resolves wins, all-miss renders `??`) prepended to node names on both `/` and `/stable.txt`; empty list disables annotation. One accepted tag does not make the LIST redundant: entries render in order and may repeat, so two `GEO` entries with different chains publish two tags (`preprocess.annotator` reports the leftmost that resolved as the node's country), and the empty list is a distinct mode rather than a milder one. **Repetition reaches BOTH stages.** `preprocess.countryChainOrder` builds the country FILTER's provider order by concatenating EVERY `GEO` entry's chain in written order, de-duplicated by first occurrence, so the filter's provider set is the union of what the entries name and repetition changes only what is RENDERED. Measured on an IP the geofeed cannot place and DB-IP puts in DE, one entry chaining `[geofeed, dbip]` and two entries (`[geofeed]` then `[dbip]`) now reach the same verdict — kept under `countries=DE`, dropped under `exclude_countries=DE` — and differ only in the tag, `[GEO:DE]` against `[GEO:??][GEO:DE]`. Reading the first entry alone inverted BOTH verdicts for the split config, the deny-list one silently: it published `[GEO:??][GEO:DE]` for a node the operator excluded DE for, and `select.go` then wrote `DE` into `Entry.Country` and `stable_kept_country_nodes{country="DE"}`. `cloudflare` is the only provider that is not consulted offline: it is a geo-IP database like the rest, but it is asked about the egress the node reported for itself, which exists only in the worker's post-probe stage, so on `/` it always misses and the chain falls through. `config.Config.AnnotateUsesProvider(config.ProviderCloudflare)` is what arms that probe — an offline-only chain never spends a request on it. The retired singular `provider:` key is rejected as an unknown key by the strict decode. The `IP` and `ASN` tags are retired — both removed outright, since nothing consumed them and no shipped config selected them, so `- tag: IP` and `- tag: ASN` now fail the load as unknown tags; `rewrite.isKnownTag` still recognises `[IP:…]` and `[ASN:…]` on the STRIP side, deliberately, so an upstream-authored address or AS attribution is removed instead of riding along. Note that `asn` the PROVIDER and `{type: asn}` the FILTER are untouched and load-bearing — only the tag went.
- `geoblock.db_path` / `geoblock.ttl` (SQLite per-host geo-block list; default TTL 720h)
- `geoblock.gemini.*` / `geoblock.claude.*` / `geoblock.chatgpt.*` / `geoblock.tidal.*` (`endpoint`, `timeout`, `concurrency`; plus `marker` for gemini/claude/chatgpt, `model` + `api_key`/`key_file`/`key_var` for gemini, `version` for claude) — base params for the `gemini`/`claude`/`chatgpt`/`tidal` node-filters; enabled by listing `{type: gemini}` / `{type: claude}` / `{type: chatgpt}` / `{type: tidal}` in `filters` (a filter entry may override these per-field). The `tidal` gate is the odd one out twice over. It has no refusal marker: where Tidal refuses an egress the request dies at the CDN (403 + HTML, measured from a RU egress), so the gate is **fail-closed on the status alone** — kept only on 2xx, and since redirects are not followed a 3xx interstitial is a refusal too. The body is deliberately not read (the country it reports gates where a subscription can be bought, not where an existing subscriber streams), so the gate answers only "did the request get through" and a 2xx from an ISP block page or captive portal counts as passed. It also never writes to the geoblock store (a bare status code is too weak a signal to persist host-wide for the store's TTL).
- The `gemini` gate CANNOT be made keyless, and a rejected key must never read as "not blocked". Measured against `generativelanguage.googleapis.com` from a geo-blocked egress: no key -> `403` "Method doesn't allow unregistered callers"; junk key (query param or `x-goog-api-key`) -> `400 API_KEY_INVALID`; valid key -> `400 FAILED_PRECONDITION` + the location marker. The API resolves caller identity, then key validity, and only then the location precondition, so the verdict this gate reads is invisible without a working credential — hence `geminiInconclusive` (401/403/404/429, plus a 400 carrying `API_KEY_INVALID`): those answers predate the verdict, and the nodes are KEPT UNVERIFIED. Warning about them was never enough — measured on the production host it fired every cycle (`nodes=22 of=306`, then `nodes=7 of=285`) with nothing on the dashboard able to tell a working gate from a credential-broken one — so the count is also PUBLISHED, as `stable_gemini_gate_checks` / `stable_gemini_gate_unverified_checks` / `stable_gemini_gate_enabled` (`stable.GeminiReport`). Those nodes are kept, so the count MUST NOT ride `FilterReport.Dropped`. Keyless substitutes are country-list guesses, not this check: `gemini.google.com/app` and `aistudio.google.com/welcome` both answered `200` from that same blocked egress, and the supported-country list is documented at `ai.google.dev/gemini-api/docs/available-regions` (RU/BY/CN/HK/MO/IR/KP/CU/SY/AF/MM absent)
- `geo.cloudflare.*` (`timeout`, `concurrency`; default 15s, 8) — the cdn-cgi/trace probe behind the `cloudflare` ANNOTATE provider, not a filter type: it gates nothing, drops nobody, and is armed by naming `cloudflare` in an `annotate` chain. It sits under `geo.` with the other geo databases because that is what it is — Cloudflare's own geo-IP database, whose verdict is the `loc=` line the tag carries. What differs from its siblings is the ADDRESS it is asked about: they place the address the RESOLVER returned, and 41% of the pool's named hosts sit in Cloudflare's shared anycast ranges, which terminate in many countries at once, so the tag described Cloudflare's registration while traffic left from the origin (measured: 47 of 102 published tags corrected). `ip=` is the means, not the published value — the trace buys the right address to ask about, not a more honest database. **There is no `endpoint` key**, and dropping it was the point: `stable.parseTrace`/`validCountry` encode Cloudflare's documented reserved `loc` values (`XX`, `T1`) and its uppercase convention, so no other VENDOR's endpoint could satisfy the parser and every cross-vendor value would fail SILENTLY, as an unanswered node (another Cloudflare-fronted host does parse — `/cdn-cgi/trace` is served on every domain Cloudflare proxies — so that substitution is made in the constant). Two limits remain: it never runs on `/` (no post-probe stage there), and it does not fix the `type: country` FILTER, which judges the resolved address in preprocess before any probe exists. **Reload class is `live-worker`, not the `live-processor` its `geo.*` neighbours carry** — the gate follows the CONSUMER (`config.ProberChanged`, `stable.filterAndMeasureEgress`), never the namespace; `internal/reload/reload_coverage_test.go` pins both halves
- `deadcache.ttl` (in-memory cache of probe-dead nodes keyed by `server:port`; default 2h; skips re-probing; not persisted)
- `groups.<name>` (country sets referenced by requests and `exclude_groups`)
- `subscriptions.interval` and every `subscriptions.check.*` param are validated on every load, even with no sources configured (they all arrive from the overlays here, and a list-gated check let a bad value boot clean and then fail every later reload)
- `subscriptions.sources[].name` + `url` *or* inline `body` (base64/raw node URIs; used by the crawler's `tg-inline` harvest)
- `subscriptions.check.*` (`rounds`, `timeout`, `max_fail`, `max_avg_ms`, `test_url`, `expected_status`, `concurrency`, `source_timeout`) — URL-test (latency) prober params ONLY; through-node filters and exclusions live in the top-level `filters` list
- `subscriptions.snapshot_path` — where the worker persists the list it just published, reloaded at startup so `/stable.txt` serves the last good list instead of `503` while the first cycle runs (measured 58 minutes on a 68266-node pool). Empty disables. The shipped value is `/config/.stable-snapshot.json`: `./config` (`docker-compose.yaml:14`) is that service's only WRITABLE host bind mount — its other one, the agenix secret at line 16, is a read-only file — and uid 1000 already writes `.geoblock.db` into it — `.crawler-state.json` sits there too, but the crawler container writes that one as root (`docker-compose.yaml:42`) — so persistence adds no volume and no host-side provisioning. `config-vassago/config.yaml` sets the same key against that instance's own `./config-vassago` mount, and that snapshot is the only file the instance writes there: it ships no `geoblock` block, so `app.Run` opens no store for it. It is absolute because every OTHER path key the SHIPPED file sets is (`geoblock.db_path`, `geoblock.gemini.key_file`), and the runtime image is `WORKDIR /`. A path under `/tmp` would land in the container's OWN writable layer, which `docker compose up -d` recreates after a build — it would survive `docker restart` but not the redeploy those 58 minutes came from. The accepted cost is that the snapshot now outlives a host reboot too, the same guarantee its two neighbours already have. Writing it into the watched directory does NOT self-trigger a reload: `reload.Watcher` adds that directory to fsnotify, but `matches` compares each event against the three exact overlay paths, so the per-cycle temp-create + rename is ignored. There is no TTL, deliberately — the in-memory rule already keeps the last good list through failing cycles, and the age stays visible in `X-Stable-Stats updated=`. It takes no validator beyond the strict decoder, exactly like the three other path keys in the SCHEMA (`geoblock.db_path`, `geoblock.gemini.key_file`, `filters[].key_file`) — a wider population than the two the shipped file sets, and `routes.md` enumerates that one. **Startup-only:** it is in `config.StoresChanged` and EXCLUDED from `config.SubscriptionsChanged`, so a reload warns instead of re-applying the worker — the only key in the block that behaves that way
- `fetch.timeout` — per-subscription fetch deadline (default 3s)
- `log.level` — zerolog level, hot-reloadable

## Important package map

- `main.go` — root entrypoint
- `internal/app` — app bootstrap, config load, service construction, server start
- `internal/config` — YAML config parsing/validation
- `internal/fetch` — safe HTTP fetching, file-type decoding, SSRF protections
- `internal/geofeed` — geofeed download/parse/lookup
- `internal/cidrset` — IPv4 allow-list: fetch/parse a CIDR (or bare-address) list into merged sorted ranges, membership by binary search
- `internal/resolver` — DNS resolution with an in-memory TTL cache
- `internal/asn` — ASN lookup (Team Cymru) for the ASN name/country filter
- `internal/filter` — country allow/deny bitset
- `internal/subscription` — subscription fetch/normalize/parse (scheme-generic, plus dedicated decoders for `vmess://`, legacy `ss://`, `ssr://` and `mierus://` in `vmess.go`/`ss.go`/`ssr.go`/`mieru.go`, and Xray-JSON→share-link conversion in `xray.go`)
- `internal/rewrite` — node name/fragment rewrite (`[GEO:XX]`, vmess `ps` rewrite)
- `internal/preprocess` — the core per-node filter pipeline
- `internal/geoblock` — SQLite TTL list of node hosts that failed a through-node API reachability check (gemini/claude/chatgpt)
- `internal/stable` — `/stable.txt` worker: merge/dedupe/relabel, dead-node cache skip (pre-probe), Mihomo prober + through-node filters (gemini/claude/chatgpt reachability, tidal reachability, bandwidth), the post-filter `cdn-cgi/trace` egress measurement, one-shot name annotation at publication, checker loop, holder, and the JSON snapshot (`snapshot.go`) that carries the published list across a restart
- `internal/reload` — config file watcher + hot-reload
- `internal/server` — Fiber HTTP layer
- `internal/metrics` — renders stable-cycle stats as hand-rolled Prometheus text exposition; served on `server.metrics_listen`
- `internal/geo` — the provider adapters (`geofeed`/`dbip`/`registry`/`asn`) the country filter and the annotator share, each named after the data source it queries
- `internal/classify` — decides whether a URL serves a usable subscription; behind the `classify` subcommand and every crawler candidate
- `internal/crawl` — the `crawl` subcommand: Telegram-preview crawler writing the `private.yaml` overlay (instance 1 only)
- `internal/log` — zerolog setup, runtime level changes, the `ctxlog.Op` child-logger helper
- `internal/ioutil` — `Lines` (non-empty, non-comment line iteration) and `UnsafeString`, shared by `cidrset`, `geofeed`, `subscription` and `stable`

## API behavior to remember

- `GET /healthz` returns `ok`
- `GET /` requires:
  - `subscription_url`
  - `countries` (comma-separated) OR `groups` (comma-separated, referencing `config.groups`)
  - optional `exclude_countries` / `exclude_groups` — a true **deny-list**, not a subtraction from the allow-list. A node is dropped only when its IP resolves to an excluded country; an IP no geo provider can place SURVIVES an exclusion-only request. (Folding exclusions into `All()` used to drop every unplaceable IP the moment one country was excluded.) Under an explicit `countries`/`groups` allow-list an unplaceable IP is still dropped — that is what an allow-list means. Unknown group names and non-alpha-2 codes are rejected with `400`, not silently ignored
- `GET /` no longer publishes a portless `http`/`https`/`socks`/`socks5`/`socks5h` line as a node: a bare `https://t.me/somechannel` in a source body used to be emitted as a node and now counts in `Stats.Unsupported` (`unsupported=` in `X-Preprocessor-Stats`) instead. A portful one (`https://example.com:8443`) is still a node
- `GET /` bounds one request: a 60s deadline (`504` on expiry, since fasthttp's request context has neither a deadline nor client-disconnect cancellation) and a 50k node ceiling (`413`). The ceiling is a DoS bound shared with the worker's per-source load, not a quality filter — at 20k it dropped a configured 33.4k-node aggregator source outright
- `GET /stable.txt` serves the worker's current list; `503` until there is one — the first completed cycle, or the snapshot restored at startup when `subscriptions.snapshot_path` is set (a restored list keeps its original `updated=`, so its age shows). Stats are returned in `X-Stable-Stats` (`updated=… sources=ok/total merged=… tested=… kept=…`)
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
  it loopback-only once per instance — `127.0.0.1:9091:9090` for `sub-preprocessor`,
  `127.0.0.1:9092:9090` for `sub-preprocessor-vassago` — keep both non-public.
- Data flows via the nil-safe `stable.Reporter`: `RunOnce` hands a `CycleReport`
  (per-source drops, per-filter in/kept/dropped-by-reason, kept speeds AND kept
  mean latencies, cycle aggregate + duration) to `metrics.Metrics.Observe` on a
  published cycle, and
  `ObserveError()` on any abort. **Adding/renaming a metric? Update
  `deploy/grafana/sub-preprocessor.json` in the same commit.**
- **A histogram bound that marks a gate is a CONTRACT with the config, not a default.**
  `latencyBuckets` MUST carry a bound equal to every `check.max_avg_ms` that any shipped
  config sets, because `SelectSurvivors` admits on exactly that value and a threshold
  landing between two bounds hides the one edge `stable_kept_latency_ms` was added to make
  visible. So moving a `max_avg_ms` ONTO a bound the ladder already carries is a one-file
  edit — that is what the tail past every gate is for — and moving it anywhere else adds
  its bound in the same commit. Never append a bound the ladder already has:
  `writeHistogram` emits one line per element with no dedupe, so a duplicate renders two
  identical `le=` series and Prometheus rejects the whole scrape.
  The two instances scrape this metric name under different jobs and may disagree on the
  threshold, so the ladder carries EVERY shipped value, not one of them. All three halves
  are enforced rather than merely written here: `TestLatencyBucketsCoverShippedGates`
  reads both shipped configs through `config.Load`,
  `TestLatencyBucketsCoverTheDefaultGate` covers the gate neither of them exercises by
  loading a shipped config with the key stripped, and
  `TestLatencyBucketsAreStrictlyIncreasing` catches the duplicate.
- **A count of nodes the pipeline KEPT never rides `FilterReport.Dropped`.** That map renders as
  `stable_filter_dropped_nodes{filter,reason}`, which the dashboard titles "drops by reason"; the trace's
  `corrected`/`unanswered` shipped through it for a filter that drops nothing — shipped by `b545d0a`,
  corrected in `e554307`, which moved them out of `Dropped` (into a `FilterReport.Notes` map that `abf452b`
  then retired along with the whole filter, geotrace now being the annotate stage behind `TraceReport`). The
  gemini gate's `stable_gemini_gate_{enabled,checks,unverified_checks}` are the worked example of the right
  shape: a stage-specific report on `CycleReport` (like `TraceReport`), rendered by its own `writeGemini`.
  They also fix the three states apart, because a gate that is OFF must not read like a gate that is FINE —
  `enabled 0` = configured but no usable key so it checked nothing, `enabled 1` = it ran, and nothing
  rendered = the gate did not run, which is FOUR causes rather than "gemini not configured" alone: no
  scrape, no cycle published yet, no `gemini` in `filters`, or a configured one that never reached its check
  (`buildNodeFilters` skipped it with only a WARN for want of Gemini support on the prober, or
  `filterAndMeasureEgress` returned before the chain when `ParseProxies` failed — which publishes every
  survivor UNFILTERED). Metric names are a wire format from the moment they ship, exactly like the
  drop-reason strings.
- `flake.nix` output `nixosModules.monitoring` (`deploy/monitoring.nix`) = the
  Prometheus scrape jobs + the Grafana dashboard provider
  (`deploy/grafana/sub-preprocessor.json`; datasource picked via a template
  variable, so no fixed uid). `nixosModules.default` is the separate systemd-service
  module — leave it.
- **Two instances, two scrape JOBS — not two targets in one job.** `sub-preprocessor`
  (`127.0.0.1:9091`) and `sub-preprocessor-vassago` (`127.0.0.1:9092`) each carry their
  own `job_name`, because the dashboard's Instance picker is
  `label_values(stable_cycles_total, job)` and every panel expression is scoped
  `{job="$job"}`. Sharing a job leaves the two deployments unselectable AND silently
  sums their funnels. Two consequences bind any dashboard edit: a panel added later
  MUST carry that selector, and a panel description MUST NOT name one config file as
  the authority for what ran — which filters exist depends on which instance `$job`
  selects.

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
- **Two benchmarks moved because their FIXTURE moved, and both times the fixture was the
  whole of it.** `BenchmarkProcessBodyPipeline`'s annotate list lives in `newBenchProcessor`
  (`internal/preprocess/pipeline_bench_test.go`) and `BenchmarkAnnotate`'s in
  `annotator_bench_test.go`. Both name the annotate tags, so removing a tag edits them:
  `5d06fb6` (the `IP` tag) took the pipeline 4640 -> 1600 B/op, and the `ASN` tag removal
  took `BenchmarkAnnotate` 48 -> 24 B/op. **Neither is an allocation win the code earned.**
  Controlled both times against a hybrid tree holding the fixture constant, the fixture was
  100% of each B/op move. Every `benchmarks/` snapshot older than `5d06fb6` records
  `4640 B/op`, so diffing a fresh `make bench` against one reads a 65% win that does not
  exist; the post-`5d06fb6` pipeline baseline is **~16200 ns/op**, not the hybrid's ~15900.
- **A sub-2% ns delta on these benchmarks is not measurable here and MUST NOT be quoted as
  a result.** The variable is the linked image, not the session: two `git archive` exports
  of byte-identical source, relinked, disagree by up to ~2% on a must-be-zero null control,
  while each individual link holds its own offset to a handful of ns across sessions hours
  apart. So a clean null control proves nothing - it says only that this link sits near its
  control - and re-running only sharpens a constant you cannot attribute. **Re-export and
  RELINK the null control; if two links of one source disagree by more than the delta you
  are chasing, that delta is unmeasured.** This was paid for twice: a +1.8% regression
  attributed to the `IP` removal and a -0.75% win attributed to the `ASN` one. Neither
  survived a relinked control. What DOES survive is the static shape - `go tool objdump`
  shows the annotate dispatch going from a 3-case switch to a 2-case one to a guard clause
  (`if t.key != config.TagGEO { continue }`) as the two tags were removed, seven immediate
  compares down to three. That is an argument about work removed, not about nanoseconds.
- **Allocations are the binding constraint and they are clean across both changes**: no
  `allocs/op` or `B/op` increase on any benchmark. A handful in untouched packages
  (`Parse_1000Entries`, `ParseProxies`, `Parse_Vmess`, `Resolution`, `Resolution_Concurrent`,
  `Merge`, `MergeSSR`, and `CacheStore_RWMutexMap/parallel` / `CacheStore_SyncMap/parallel`)
  differ between trees at `-benchtime 100x` while the SAME tree
  reproduces the same spread across consecutive runs - `Resolution_Concurrent` even flips
  64/65 allocs/op - so that is the instrument, and the honest comparison diffs the TOUCHED
  packages rather than the whole list. Do NOT replace that with a quoted range: the two
  `CacheStore/parallel` cases have a STABLE FLOOR (13 B/op) and a ceiling that is a draw of
  the instrument, because `B/op` is a total divided by 100 iterations spread over 16 P's, so
  a handful of fixed per-P costs lands the quotient anywhere. Three `-benchtime 100x
  -count=20` sweeps, two links: RWMutexMap 13 x10 / 14 x5 / 28,57,71,85,85, then 13->71,
  then 13 x9 / 14 x2 up through 43,53,72,77,86,91; SyncMap 13->86, then 13->61, then
  13 x15 / 14 x3 / 53,76. Same floor every time, a different ceiling every time - which is
  why the pair that used to be quoted here ("13 <-> 52") was outside its own band by the
  second sweep. Name the floor and the mechanism, quote no ceiling.
  The one deliberate increase is in `countryChainOrder`: a config splitting
  one `GEO` chain across several entries now pays a per-request `chainLookup` it did not
  before (0 -> 56 B/op, 2 allocs), which is exactly what the equivalent single-entry chain
  already cost. The shipped config is unmoved, and the alternative was the wrong filter
  verdict - see `Processor.countryChain`.
- **STRUCK by explicit agreement, after six review rounds:** the per-link median tables,
  per-session interquartile ranges and A/B/C/D build enumerations that used to fill this
  section. They were measured in `/tmp` export trees that no longer exist, so no reader
  could check them from a fresh checkout - which is the rule at the top of this section.
  They existed to support a NULL result, so the apparatus could never be load-bearing and
  could only be wrong in new ways; each rewrite minted the next round's findings ("five
  commits" was seven, "four independent links" was three links and a re-run, a tree that
  "reproduced exactly" was a different tree). The three bullets above are what four
  independent reviewers verified and what stands without figures. Re-measure with
  `nix-shell --run "make bench"` rather than trusting a number here.
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
- `config/` — the first instance's config directory (`config.yaml` + the `sources.yaml` / `private.yaml` overlays)
- `config-vassago/` — the second instance's (`config.yaml` + `sources.yaml`; no crawler writes here)
- `Makefile` — common targets (`run`, `test`, `fmt`, `race`, `bench`)
- `.golangci.yml` — linter configuration
- `internal/` — internal packages
- `benchmarks/` — timestamped benchmark output snapshots
