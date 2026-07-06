# Stable Subscriptions Design

Migrate the subscription stability pipeline (previously `filter-subs.sh` + a
docker sidecar stack in the `domains.lst` repo) into sub-preprocessor as a
first-class feature: subscription sources live in `config.yaml`, a background
worker delay-tests all nodes through the mihomo Go library every N minutes,
and `GET /stable.txt` serves the last good filtered list.

## Motivation

- The shell pipeline lived outside the service, required a docker-in-docker
  style sidecar (mihomo test instance) and a separate nginx container.
- The interim setup served the filtered list directly, bypassing this
  service's geo/ASN filtering. Geo-blocked nodes (e.g. RU) are fast when
  measured from inside the country, so a pure delay filter lets exactly the
  wrong nodes dominate the output. Stability filtering must compose with the
  existing geo/ASN pipeline.
- One binary, one container, one config.

## Data flow

```
config.yaml subscriptions.sources (ordered: name + url)
  │ every subscriptions.interval (default 30m), and once at startup
  ▼
per source: Processor.Filter(url, allowed = All() − exclude_*)   [existing pipeline:
  fetch/SSRF → normalize(base64) → parse URI nodes → DNS resolve → ASN deny → geofeed country]
  │ source fetch error → log warn, skip source
  ▼
merge in source order → dedupe by Server:Port (first wins)
  → relabel fragment to <source>-<NNN> (NNN = 3-digit per-source counter)
  ▼
mihomo library probe (internal/stable prober):
  convert.ConvertsV2Ray(payload) → adapter.ParseProxy (once per node)
  → rounds × URLTest(ctx timeout, test_url, expected_status), pause between rounds
  → per-node: success count, mean delay of successful rounds
  ▼
survivors: (rounds − successes) ≤ max_fail AND mean ≤ max_avg_ms
  → sort by mean ascending → payload = raw relabeled URIs, one per line
  ▼
stable.Holder (atomic snapshot: payload, updated_at, stats)
  ▼
GET /stable.txt → 200 text/plain payload | 503 until first successful cycle
```

Zero merged nodes or zero survivors → keep the previous snapshot untouched,
log an error, retry next interval (same semantics as the shell pipeline).

## Config schema

```yaml
subscriptions:
  interval: 30m               # default 30m, min 1m
  exclude_countries: []       # optional, ISO alpha-2
  exclude_groups: [geo_blocked] # optional, references config.groups
  check:
    rounds: 5                 # default 5, min 1
    round_pause: 3s           # default 3s
    timeout: 2s               # per-node URLTest timeout, default 2s
    test_url: https://www.gstatic.com/generate_204  # default
    expected_status: "204"    # mihomo IntRanges syntax, default "204"
    max_fail: 0               # default 0
    max_avg_ms: 1000          # default 1000
    concurrency: 16           # default 16 (mihomo healthcheck default is 10)
  sources:                    # ordered list; order = dedupe priority
    - name: mifa              # ^[a-z0-9-]+$, unique
      url: https://mifa.world/vless  # https only, SSRF-validated at load
```

`subscriptions` absent or `sources` empty → feature disabled: no worker, and
`/stable.txt` returns 503.

Allowed set for the pre-filter = `filter.All()` minus `exclude_countries`
minus expanded `exclude_groups`. Empty exclusions → `All()` (no geo
restriction; DNS/ASN stages still apply per `workflow.stages`).

## Components

### `internal/config`

- `SubscriptionsConfig` (`interval`, `exclude_countries`, `exclude_groups`,
  `check CheckConfig`, `sources []SubscriptionSource`), defaults applied in
  `Load`, validation: name regex + uniqueness, `fetch.ValidatePublicHTTPSURL`
  per source URL, group references must exist in `config.groups`,
  numeric/duration bounds.
- `SubscriptionsChanged(old, new Config) bool` and
  `GroupsChanged(old, new Config) bool` diff helpers for the reloader.

### `internal/stable` (new package)

- `Filterer` — local interface, same signature as `server.Filterer`
  (avoids a server↔stable import cycle; `app` passes
  `func() Filterer { return serverHolder.Load().Svc }`).
- `Snapshot{Payload []byte, UpdatedAt time.Time, Stats Stats}` +
  `Holder` (atomic.Pointer, same pattern as `server.Holder`).
- `Prober` interface:
  `Probe(ctx, payload []byte) (map[string]ProbeResult, error)` where
  `ProbeResult{Successes int, MeanMs int}`; label = node name.
- `MihomoProber` — implements `Prober` via
  `github.com/metacubex/mihomo`: `convert.ConvertsV2Ray` →
  `adapter.ParseProxy` once per node (`defer Close`), then
  `rounds × URLTest` under an errgroup with `SetLimit(concurrency)`;
  `utils.NewUnsignedRanges[uint16](expected_status)`. Unparseable nodes are
  dropped and counted.
- `Checker` — owns the cycle: filter sources → merge/dedupe/relabel →
  probe → select survivors → store snapshot. Pure helpers
  (`merge`, `selectSurvivors`) are unit-tested without network.
- `Controller` — start/stop lifecycle wrapper; `Restart(cfg, groups)`
  cancels the old worker goroutine and starts a new one against the same
  `Holder`. Used by `app` at boot and by the reloader on config change.

### `internal/server`

- `GET /stable.txt`: loads `stable.Holder` snapshot; empty/nil → `503
  stable list not ready`; otherwise `200 text/plain` + `X-Stable-Stats`
  header (`updated_at`, sources ok/total, merged, tested, kept counts).
- `server.New` gains the stable holder parameter.

### `internal/app` + `internal/reload`

- `app.Run` builds `stable.Holder` + `Controller`; starts the worker when
  the feature is enabled; joins it on shutdown (same pattern as watcher).
- `Reloader` gets the `Controller`; on reload, if
  `SubscriptionsChanged || GroupsChanged` → `Controller.Restart`. The
  holder survives restarts, so the endpoint keeps serving the last list.
  (The gemini-block-checker sidecar rewrites `asn.deny_patterns`
  periodically; that diff must NOT restart the worker.)

### go.mod

- `github.com/metacubex/mihomo v1.19.27` +
  `replace google.golang.org/protobuf => github.com/metacubex/protobuf-go`
  (mihomo's replace does not propagate). CGO stays disabled; binary grows
  to ~40–80 MB — accepted.

## Node naming

Output fragments are rewritten to `<source>-<NNN>` (unique by construction,
required: mihomo providers reject duplicate names and
`ConvertsV2Ray` renames dupes unpredictably). Geo tags from the preprocess
rewrite are intentionally discarded in favor of stable short labels — same
UX as the previous `stable.txt`.

## Known limitations (accepted)

- `vmess://base64json` lines have no URI authority, fail the existing
  `subscription.parseNode`, and are dropped at the preprocess stage — same
  behavior as the pre-migration converter chain.
- Snapshot is in-memory only: after a container restart `/stable.txt` is 503
  for ~1 cycle (≤1 min); the router's provider keeps its on-disk copy and
  tolerates fetch failures, so this is harmless.
- Probing measures latency from the workstation, not the router — same LAN,
  same uplink; accepted since day one of the shell pipeline.

## Verification

- TDD unit tests: config parse/validate/diff, merge/dedupe/relabel,
  survivor selection, checker loop with fake `Filterer` + fake `Prober`,
  `/stable.txt` handler via `TestApp` with seeded holder.
- `nix-shell --run 'make test'`, `make race`, `make lint` green.
- Live e2e: `docker compose up -d --build`, wait one cycle, `curl
  http://127.0.0.1:7008/stable.txt` returns vless URIs with `<src>-NNN`
  fragments; payload accepted by `mihomo -t` with a file provider.

## Migration (domains.lst repo, after live e2e passes)

1. `docker compose down` the old subs-updater stack.
2. Remove `filter-subs.sh`, `Dockerfile`, `docker-compose.yml`, `docker/`,
   `subs/`; drop the `mihomo-filter-subs` helper from `shell.nix`.
3. `mihomo/config.yaml` provider url → `http://192.168.1.2:7008/stable.txt`;
   validate with `mihomo-validate`.
4. Update `AGENTS.md` / `REFERENCE_MAP.md` mentions.

## Out of scope

- Bandwidth/throughput testing.
- Persisting snapshots to disk.
- Per-source endpoints or query-time filtering of the stable list.
