# Bandwidth Check Design

Add a throughput gate to the `/stable.txt` worker: measure each candidate
node's real download speed by pulling a small payload *through* the node, and
drop nodes below a configurable `min_mbps`. The measured speed is also written
into the node name so it is visible in the published list. This supersedes the
"Bandwidth/throughput testing" item that the stable-subscriptions design listed
as out of scope.

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
- **Annotate speed in the node name** (`[SPD:45M]`), alongside `[GEO][IP]`.
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
  → set Survivor.Mbps on kept; prepend [SPD:45M] into the node fragment
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
      min_mbps: 5               # drop below this; default 5
      timeout: 20s              # per-node download deadline; default 20s
      concurrency: 4            # parallel downloads (host-uplink contention knob); default 4
```

- Enable = presence of `bandwidth` in `check.filters` (mirrors gemini/claude;
  no separate `enabled` flag).
- Defaults applied in `config.Load`; the block is inert unless `bandwidth` is
  listed in `filters`.
- Validation in `CheckConfig.validate` (only when `bandwidth` is listed):
  `test_url` must be an absolute http(s) URL; `min_mbps`, `timeout`,
  `concurrency` must be non-negative. Host-side SSRF rules do **not** apply to
  `test_url` — it egresses through the proxy node, identical to the existing
  latency `test_url`.

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

## Components

### `internal/config`

- `CheckConfig.Bandwidth BandwidthConfig` — new sub-struct
  (`TestURL`, `MinMbps`, `Timeout`, `Concurrency`).
- Defaults in `Load`; validation in `CheckConfig.validate` as above, gated on
  `bandwidth` appearing in `Filters`.
- Reloader diffing: `ProberChanged` already re-applies the worker on
  gemini/claude changes; extend it to also compare `check.bandwidth` so a
  threshold/URL edit hot-reloads the worker.

### `internal/stable`

- `internal/stable/prober_bandwidth.go` (new):
  - `bandwidthProbeOne(ctx, px mihomo.Proxy, target string, timeout) (reachable bool, mbps int)`
    — mirrors `apiProbeOne` (dial the target through `px`, fixed-conn
    `http.Transport`, GET), but reads the whole body (capped by a package
    constant `maxBandwidthBody` ~256 MB against a hostile endpoint) and times
    the transfer from `client.Do` return to EOF. `mbps = bytesRead*8 /
    elapsedSeconds / 1e6`. A partial read at the deadline still yields a rate:
    a slow node reports a low Mbps and is dropped; a stalled node reports ~0; a
    dial/connect failure reports `reachable=false`.
  - `BandwidthCheck(ctx, payload []byte) map[string]BandwidthOutcome` on
    `MihomoProber` — parses proxies once, fans out over
    `check.bandwidth.concurrency` with a shared semaphore, per-node debug log
    (`op=stable.BandwidthCheck`, fields `node`, `n`/`of`, `mbps`/`reachable`)
    and a `progress` reporter, mirroring `apiCheck`/`GeminiCheck`.
  - `BandwidthOutcome{Server string, Reachable bool, Mbps int}`.
- `internal/stable/nodefilter.go`:
  - `bandwidthFilter` (new `NodeFilter`): `apply` measures the survivor subset
    via `BandwidthCheck`, drops `Mbps < min_mbps` and `!Reachable`, sets
    `Survivor.Mbps` on kept survivors, prepends the `[SPD:45M]` tag into each
    kept node's fragment, logs `survivors/kept/slow/unreachable` + mean Mbps.
    `ctx.Err()` guard returns survivors unchanged (no drops on partial data).
  - `bandwidthChecker` interface (`BandwidthCheck` + accessors for `min_mbps`)
    and a `case "bandwidth":` branch in `buildNodeFilters` that type-asserts it
    and constructs the filter. No `store` (no cache/persistence).
- `internal/stable/select.go`: `Survivor` gains `Mbps int`. `BuildPayload`
  unchanged (it already emits `Entry.Tagged`, which now carries `[SPD:]`).

### `internal/rewrite`

- `StripKnownTags` learns to strip `[SPD:…]` (so a re-published node never
  accumulates stale speed tags).
- A helper to prepend `[SPD:45M]` into a node fragment, consistent with the
  existing `[GEO][IP]` tag writing. Used by `bandwidthFilter`.

### Controller / checker

- No new wiring: `Controller.Apply` already passes `subs.Check` to
  `NewMihomoProber` (so the prober carries `check.bandwidth`) and
  `subs.Check.Filters` to `buildNodeFilters`. The gate appears automatically
  when `bandwidth` is listed.

## Data cost

Per cycle: `N × survivors` bytes (only survivors, only once). Default
2 MB × ~100 survivors ≈ 200 MB/cycle. Scales with N and the `subscriptions.interval`;
documented so operators size it deliberately.

## Verification

- Unit: `bandwidthProbeOne` against a local `httptest` server — a throttled
  writer yields a sub-threshold Mbps (dropped), a fast writer passes, a
  stalling handler hits the deadline and is dropped.
- Unit: `bandwidthFilter.apply` — threshold drop, unreachable drop, `Mbps` set
  on survivors, `[SPD:]` present in `Tagged`, ctx-cancel no-op.
- Unit: `rewrite.StripKnownTags` strips `[SPD:…]`.
- Unit: config parse/validate/defaults for `check.bandwidth`; `ProberChanged`
  detects a bandwidth diff.
- `nix-shell --run 'make test'`, `make race`, `make lint` green.
- Live smoke: config with `filters: [bandwidth]`, one worker cycle, `curl -sI
  http://127.0.0.1:<port>/stable.txt`, confirm `[SPD:]` tags in the body and
  that a deliberately low `min_mbps=1000` empties the list (all dropped).

## Out of scope

- Upload-speed testing.
- Caching/persisting bandwidth results across cycles or restarts.
- Probing on the on-demand `GET /` path.
- Multi-round bandwidth averaging (single measurement per node in v1).
- Slow-start warmup discarding (possible later `warmup_bytes` refinement).
