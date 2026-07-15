# Bandwidth Check Design

Add a throughput gate to the `/stable.txt` worker: measure each candidate
node's real download speed by pulling a small payload *through* the node, and
drop nodes below a configurable `min_mbps`. The measured speed is also written
into the node name so it is visible in the published list. This supersedes the
"Bandwidth/throughput testing" item that the stable-subscriptions design listed
as out of scope.

> Revised after an independent code-review pass (findings folded into
> Measurement mechanics, Config schema, and Components below).

## Motivation

- The current worker keeps a node if it passes latency selection (`max_fail` /
  `max_avg_ms`) and the through-node API gates (gemini/claude). Latency is not
  bandwidth: an oversold or throttled node can answer a 204 in 20 ms and still
  deliver 1 Mbps. Those nodes currently survive.
- Users want slow nodes cut from the stable list, keyed on actual throughput.

## Physics constraint (why this shape)

Throughput = bytes / time. There is no way to learn a node's Mbps without
transferring some bytes through it; RTT/handshake latency (what `URLTest`
already measures with a HEAD) carries no bandwidth signal. The service never
carries the user's data-plane traffic (it only emits a list; real traffic goes
through the user's mihomo client), so there is no passive signal to piggyback
on. The only levers are **how many bytes** and **how often** — this design
minimizes both.

## Decisions

- **Worker-only.** The on-demand `GET /` path never probes nodes (architecture
  invariant); the bandwidth gate lives only in the `/stable.txt` worker.
- **No cache.** Survivors are measured every cycle. Data is bounded by using a
  small payload and testing only the post-latency (and post-API-gate)
  survivors, not every merged node.
- **Small exact-size download.** Default target is Cloudflare's speed-test
  generator `https://speed.cloudflare.com/__down?bytes=N`, which returns exactly
  N bytes (publicly reachable via anycast; the endpoint that powers
  speed.cloudflare.com). Not a file server; the size is chosen by the caller.
- **Configurable, low-bar default.** `min_mbps=5`, N=2 MB, so the default catches
  near-dead nodes cheaply. Higher thresholds require a larger N (see accuracy).
- **Annotate speed in the node name** (`[SPD:45M]`, where `M` = Mbps),
  alongside `[GEO][IP]`, and **gated on the same `workflow.annotate` flag** — an
  operator who publishes clean names (`annotate: false`) gets no `[SPD:]` either.
- **Sorting unchanged** — survivors stay sorted by latency ascending; the
  client's mihomo re-tests and picks anyway, so speed is used only as a
  threshold, not a sort key.

## Integration approach

The bandwidth gate is a **Layer-2 `NodeFilter`**, selected by listing
`bandwidth` in `subscriptions.check.filters` — the same mechanism as the
`gemini`/`claude` through-node gates. It slots into the existing
`for _, f := range c.filters` loop in `Checker.RunOnce`, after `SelectSurvivors`.
Filter order is caller-controlled via the `filters` list, so placing `bandwidth`
last runs it on the fewest nodes and minimizes data:

```yaml
check:
  filters: [gemini, bandwidth]   # gemini gate first, bandwidth last
```

Rejected alternative: a dedicated always-on stage + `enabled` flag outside the
filters loop. It duplicates the enable/order semantics the `filters` list
already provides and adds checker/controller wiring for no benefit. The gate is
genuinely a through-node check run after latency selection, which is exactly
what `NodeFilter` is for.

## Data flow

```
SelectSurvivors (latency: max_fail / max_avg_ms, sorted by mean delay)
  │  survivors []Survivor
  ▼
NodeFilters loop (order = check.filters):
  gemini / claude gate → keeps API-reachable, records geo-blocked hosts
  ▼
bandwidth NodeFilter (last):
  BandwidthCheck routes __down?bytes=N through each survivor (bounded by
    check.bandwidth.concurrency), times the body transfer, computes Mbps
  → drop Mbps < min_mbps and unreachable
  → set Survivor.Mbps on kept; when workflow.annotate, prepend [SPD:45M]
    into the node name via the vmess-aware relabel path
  ▼
BuildPayload → stable.Holder snapshot → GET /stable.txt
```

`ctx` cancellation mid-check yields partial measurements; the filter treats a
cancelled context as a no-op (survivors pass through unchanged), and the
existing post-filter `ctx.Err()` guard in `RunOnce` aborts the cycle and keeps
the previous snapshot — same contract as the gemini/claude filters.

## Config schema

New `check.bandwidth` block (parsed in `internal/config`):

```yaml
subscriptions:
  check:
    filters: [bandwidth]        # enables the gate
    bandwidth:
      test_url: "https://speed.cloudflare.com/__down?bytes=2000000"  # default
      min_mbps: 5               # drop below this; default 5; 0 = no floor (annotate + drop-unreachable only)
      timeout: 20s              # per-node download deadline; default 20s; must be > 0
      concurrency: 4            # parallel downloads (host-uplink contention knob); default 4; must be >= 1
```

- Enable = presence of `bandwidth` in `check.filters` (mirrors gemini/claude;
  no separate `enabled` flag).
- Defaults applied in `config.Load`; the block is inert unless `bandwidth` is
  listed in `filters`.
- `min_mbps` is an `*int` (matching the existing `WorkflowConfig.Annotate *bool`
  precedent) so an unset field defaults to 5 while an explicit `0` is preserved
  as "no speed floor" (measure + annotate, drop only unreachable). `timeout`
  and `concurrency` use the standard `==0 → default` coercion.
- Validation in `CheckConfig.validate` (only when `bandwidth` is listed), matching
  the sibling checks (config.go:476-490): `test_url` must be an absolute http(s)
  URL; `timeout` must be **> 0** and `concurrency` **>= 1** (non-positive is
  unsafe — `concurrency=0` makes the `sem <- struct{}{}` send in the fan-out
  block forever and hang the cycle; `timeout=0` makes every dial immediately
  expire and drops all survivors); `min_mbps` must be **>= 0**.
- Host-side SSRF rules do **not** apply to `test_url` — it egresses through the
  proxy node, identical to the existing latency `test_url` (config.go:500-509).

## Accuracy vs. size

TCP slow-start means a payload too small to sustain ~1-2 s of transfer at the
target speed under-reports fast nodes. Rule of thumb for choosing N in the
`?bytes=` query:

| Target `min_mbps` | Suggested N |
|---|---|
| ~5 Mbps  | 1-2 MB  |
| ~20-30 Mbps | 5-10 MB |
| ~100 Mbps | 20-50 MB |

Timing starts after response headers arrive (connect/TLS/TTFB excluded), so the
number reflects body transfer only. Slow-start within the body window is not
discarded in v1 (unnecessary at the low-bar default; a `warmup_bytes` knob is a
possible later refinement).

`min_mbps` and the reported Mbps are **integers** — no fractional thresholds,
and the measurement is floored (a 5.9 Mbps node tags `[SPD:5M]`). Floor biases
toward dropping (`floor(x) >= min` iff `x >= min` for integer `min`), acceptable
for a low-bar gate; a conscious v1 limitation, not an accident.

## Measurement mechanics (correctness-critical)

`bandwidthProbeOne(ctx, px, target, timeout) (reachable bool, mbps int)` mirrors
`apiProbeOne` (dial `target` through `px`, fixed-conn `http.Transport`, GET) with
these **mandatory** differences (each addresses a review finding):

- **Pin `Accept-Encoding: identity`** on the request (and set
  `Transport.DisableCompression = true`). Otherwise Go transparently adds
  `Accept-Encoding: gzip` and decompresses the body, so `bytesRead` would be the
  *decompressed* size while `elapsed` only paid for the compressed wire transfer
  — Mbps inflates massively (Cloudflare compresses per request; `__down`'s
  zero-fill payload is highly compressible), defeating the gate. `identity` makes
  `bytesRead == wire bytes` regardless of endpoint behavior.
- **Preserve `CheckRedirect: http.ErrUseLastResponse`** (as `apiProbeOne` does):
  the transport is pinned to one pre-dialed conn to the original host, so
  following a cross-host 3xx would reuse the wrong connection. Consequence to
  document: a `test_url` that 3xx-redirects yields `bytesRead ≈ 0` → the node is
  false-dropped as slow. `speed.cloudflare.com/__down` returns 200 directly.
- **Count bytes with `io.Copy(io.Discard, io.LimitReader(resp.Body, maxBandwidthBody))`**,
  not `io.ReadAll` — the `int64` return is `bytesRead` with zero buffering (vs.
  allocating up to the cap per node × concurrency). `maxBandwidthBody` (~256 MB)
  guards a hostile endpoint; if an operator's `?bytes=` exceeds it the read is
  truncated at the cap (rate stays valid), so validation should clamp/warn when
  the URL's `bytes=` is larger than the cap.
- **Timing:** `start := time.Now()` immediately after `client.Do` returns
  (headers in); `elapsed := time.Since(start)` after the copy completes
  (monotonic clock). `mbps = int(float64(bytesRead) * 8 / elapsed.Seconds() / 1e6)`
  using **float** seconds. Guard `elapsed <= 0` and `bytesRead == 0` → `mbps = 0`
  to avoid divide-by-zero/NaN. (Integer `elapsedSeconds` would be 0 for any
  sub-second transfer — i.e. every node above ~20 Mbps at N=2 MB — and panic.)
- **Partial read on timeout still yields a rate:** `bytesRead / elapsed` over
  whatever transferred. A slow node reports a low Mbps and is dropped; a stalled
  node reports ~0; a dial/connect failure reports `reachable=false`. (A fast node
  that merely hasn't finished N reports ~its true rate — sound, since rate is
  bytes/time regardless of completion.)

`BandwidthCheck(ctx, payload) map[string]BandwidthOutcome` on `MihomoProber`
parses proxies once, fans out over `check.bandwidth.concurrency` with a single
shared semaphore (correct here — the check is single-measurement, unlike the
multi-round URL-test prober), per-node debug log (`op=stable.BandwidthCheck`,
fields `node`, `n`/`of`, `mbps`/`reachable`) and a `progress` reporter, mirroring
`apiCheck`. `BandwidthOutcome{Server string, Reachable bool, Mbps int}`.

> Contention direction to document for operators: concurrent downloads run
> through *different* nodes but share the *host's single uplink*; when
> `concurrency × per-node-rate` exceeds the host uplink, fast nodes measure
> *slower* (host-limited) and risk false drops — raising `concurrency` does not
> increase accuracy. The default (4 × ≤5 Mbps ≈ ≤20 Mbps aggregate) is safe on a
> large-uplink host.

## Components

### `internal/config`

- `CheckConfig.Bandwidth BandwidthConfig` — new sub-struct
  (`TestURL string`, `MinMbps *int`, `Timeout time.Duration`, `Concurrency int`).
- Defaults in `Load`; validation in `CheckConfig.validate` as above, gated on
  `bandwidth` appearing in `Filters`.
- **Hot-reload needs no diff-helper change.** `check.bandwidth` lives inside
  `Subscriptions`, and `SubscriptionsChanged` is
  `!reflect.DeepEqual(old.Subscriptions, new.Subscriptions)` (config.go:514-516),
  which the reloader already ORs into `ctl.Apply` (reloader.go:121-126). Any
  bandwidth edit therefore rebuilds the prober + filters automatically.
  `ProberChanged` compares only `GeoBlock.Gemini/Claude` (which live *outside*
  `Subscriptions`) and must **not** be touched.

### `internal/stable`

- `internal/stable/prober_bandwidth.go` (new): `bandwidthProbeOne` and
  `BandwidthCheck` as specified in Measurement mechanics.
- `internal/stable/nodefilter.go`:
  - `bandwidthFilter` (new `NodeFilter`): holds `min_mbps`, `annotate bool`, and
    the `BandwidthCheck` fn. `apply` measures the survivor subset, drops
    `Mbps < min_mbps` (skipped when `min_mbps == 0`) and `!Reachable`, sets
    `Survivor.Mbps` on kept survivors, and — when `annotate` — rewrites each kept
    survivor's `Tagged` name. `ctx.Err()` guard returns survivors unchanged.
  - **Name injection (vmess-safe):** re-parse the survivor's `Tagged` line with
    `subscription.Parse` to recover the `Node`, then relabel via the existing
    `relabelNode(node, "[SPD:<n>M] "+node.Name)` (merge.go:66-75), which routes
    vmess through `subscription.RewriteVmessName` (base64 `ps` field) and every
    other scheme through the `#fragment`. This handles both tagged
    (`#[GEO][IP] label`) and untagged (`#label`) shapes because `node.Name`
    already carries whatever tags are present. Naive string surgery on the
    fragment is rejected (breaks vmess).
  - `bandwidthChecker` interface (`BandwidthCheck` + `MinMbps` accessor) and a
    `case "bandwidth":` branch in `buildNodeFilters` that type-asserts it. No
    `store` (no cache/persistence).
- `internal/stable/select.go`: `Survivor` gains `Mbps int`. `BuildPayload`
  unchanged (it already emits `Entry.Tagged`).

### `internal/rewrite`

- `StripKnownTags` and `LeadingTags` learn the `[SPD:…]` tag (defensive: keeps
  the tag set consistent if a source name ever carries one; sources do not carry
  our own tags, so this is not on the hot path).

### Controller / checker (minimal wiring)

- `Controller.Apply` already passes `subs.Check` to `NewMihomoProber` (so the
  prober carries `check.bandwidth`, controller.go:55) and `subs.Check.Filters` to
  `buildNodeFilters` (controller.go:59). The **only** new wiring is threading the
  resolved `workflow.annotate` bool into `buildNodeFilters` so the
  `bandwidthFilter` can gate `[SPD:]` on it. (This corrects the earlier
  "no new wiring" claim.)

## Data cost

Per cycle: `N × survivors` bytes (only survivors, only once). Default
2 MB × ~100 survivors ≈ 200 MB/cycle. Scales with N and `subscriptions.interval`;
documented so operators size it deliberately.

## Verification

- Unit: `bandwidthProbeOne` against a local `httptest` server — a throttled
  writer yields a sub-threshold Mbps (dropped); a fast writer passes; a stalling
  handler hits the deadline and is dropped; a **gzip-encoding** handler confirms
  `Accept-Encoding: identity` keeps `bytesRead == wire bytes`; a sub-second fast
  transfer confirms no divide-by-zero and a sane Mbps.
- Unit: `bandwidthFilter.apply` — threshold drop, `min_mbps==0` keeps all
  reachable, unreachable drop, `Mbps` set on survivors, `[SPD:]` present when
  `annotate` (and **absent** when not), ctx-cancel no-op.
- Unit: `[SPD:]` injection on a **vmess** survivor decodes to a valid `ps` with
  the tag (not a broken fragment).
- Unit: `rewrite.StripKnownTags`/`LeadingTags` handle `[SPD:…]`.
- Unit: config parse/validate/defaults for `check.bandwidth` (positive
  timeout/concurrency; `min_mbps` unset→5 vs explicit 0 preserved).
- `nix-shell --run 'make test'`, `make race`, `make lint` green.
- Live smoke: config with `filters: [bandwidth]`, one worker cycle, `curl -s
  http://127.0.0.1:<port>/stable.txt`, confirm `[SPD:]` tags in the body and that
  a deliberately high `min_mbps` empties the list (all dropped).

## Out of scope

- Upload-speed testing.
- Caching/persisting bandwidth results across cycles or restarts.
- Probing on the on-demand `GET /` path.
- Multi-round bandwidth averaging (single measurement per node in v1).
- Slow-start warmup discarding (possible later `warmup_bytes` refinement).
- Fractional-Mbps thresholds (integer only in v1).
