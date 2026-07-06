# Stable Subscriptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Background worker that fetches subscription sources through the existing geo/ASN pipeline, delay-tests every node via the mihomo Go library, and serves survivors at `GET /stable.txt`.

**Architecture:** New `internal/stable` package (holder, merge/relabel, prober, checker, controller) wired into `app` and `reload`; config gains a `subscriptions` section; server gains one route. Spec: `docs/superpowers/specs/2026-07-07-stable-subscriptions-design.md`.

**Tech Stack:** Go 1.26, fiber/v2, zerolog, `github.com/metacubex/mihomo v1.19.27` (library only).

## Global Constraints

- Run everything via `nix-shell --run "make ..."` from the repo root.
- Commit style: short lowercase subjects, direct to master, no footers.
- `ctx` is always the first argument; only `main.go` creates `context.Background()`.
- SSRF protections stay: source URLs must pass `fetch.ValidatePublicHTTPSURL`.
- Update `routes.md` when packages/public API change (Task F).
- Tests: `package xxx_test`, stdlib `testing` only, `t.Parallel()` where safe.

---

### Task A: mihomo dependency

**Files:**
- Modify: `go.mod`

- [ ] **Step A1:** Add dependency + protobuf replace (mihomo's own `replace` does not propagate to consumers):

```bash
nix-shell --run 'go get github.com/metacubex/mihomo@v1.19.27'
nix-shell --run 'go mod edit -replace google.golang.org/protobuf=github.com/metacubex/protobuf-go@v0.0.0-20260306035419-7ceee0674686 && go mod tidy'
```

(If the tidy fails on the protobuf pseudo-version, take the exact version from mihomo v1.19.27's own go.mod `replace` line.)

- [ ] **Step A2:** Prove the imports compile:

```bash
nix-shell --run 'cat > /tmp/mihomocheck_test.go <<EOF
package main
import (
  "github.com/metacubex/mihomo/adapter"
  "github.com/metacubex/mihomo/common/convert"
  "github.com/metacubex/mihomo/common/utils"
)
var _ = adapter.ParseProxy
var _ = convert.ConvertsV2Ray
var _ = utils.NewUnsignedRanges[uint16]
func main() {}
EOF
true'
```

Instead of a scratch file, simplest real check: `nix-shell --run 'go build ./...'` after Task D imports it. For now: `nix-shell --run 'go build ./... && go vet ./...'` must stay green.

- [ ] **Step A3:** Commit: `git add go.mod go.sum && git commit -m "add mihomo library dependency"`

### Task B: config `subscriptions` section

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (extend existing)

**Interfaces (Produces):**
```go
type SubscriptionSource struct{ Name, URL string }
type CheckConfig struct{ Rounds int; RoundPause, Timeout time.Duration; TestURL, ExpectedStatus string; MaxFail, MaxAvgMs, Concurrency int }
type SubscriptionsConfig struct{ Interval time.Duration; ExcludeCountries, ExcludeGroups []string; Check CheckConfig; Sources []SubscriptionSource }
func (c Config) SubscriptionsEnabled() bool
func SubscriptionsChanged(old, new Config) bool  // reflect.DeepEqual on the section
func GroupsChanged(old, new Config) bool
```

- [ ] **Step B1:** Write failing tests in `config_test.go`: yaml parse of full subscriptions block; defaults (interval 30m, rounds 5, pause 3s, timeout 2s, gstatic URL, "204", max_fail 0, max_avg_ms 1000, concurrency 16) when only sources given; validation failures: bad name `Mifa!`, duplicate name, `http://` url, private-IP url, unknown exclude group, interval 10s, rounds 0; `SubscriptionsChanged` true on source add / false on unrelated change; `GroupsChanged`.
- [ ] **Step B2:** Run: `nix-shell --run 'go test ./internal/config/'` — expect FAIL (fields undefined).
- [ ] **Step B3:** Implement: yaml tags `interval`, `exclude_countries`, `exclude_groups`, `check` (`rounds`, `round_pause`, `timeout`, `test_url`, `expected_status`, `max_fail`, `max_avg_ms`, `concurrency`), `sources` (`name`, `url`). Defaults in `applyDefaults` only meaningful when `len(Sources)>0` except static check defaults (apply unconditionally, harmless). Validation in `Config.Validate`: name `^[a-z0-9-]+$` (package-level `regexp.MustCompile`), uniqueness, `fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(src.URL))`, exclude_groups keys exist in `Groups`, exclude_countries 2 ASCII letters, `Interval >= time.Minute`, `Rounds >= 1`, `Concurrency >= 1`, `Timeout > 0`, `RoundPause >= 0`, `MaxFail >= 0`, `MaxAvgMs >= 1`.
- [ ] **Step B4:** `nix-shell --run 'go test ./internal/config/'` — PASS.
- [ ] **Step B5:** Commit: `git add internal/config && git commit -m "add subscriptions config section"`

### Task C: `internal/stable` pure core

**Files:**
- Create: `internal/stable/stable.go` (types, holder), `internal/stable/merge.go`, `internal/stable/select.go`
- Test: `internal/stable/merge_test.go`, `internal/stable/select_test.go`

**Interfaces (Produces):**
```go
type Stats struct{ SourcesOK, SourcesTotal, Merged, Tested, Kept int }
type Snapshot struct{ Payload []byte; UpdatedAt time.Time; Stats Stats }
func NewHolder() *Holder; (h *Holder) Load() *Snapshot; (h *Holder) Store(*Snapshot)
type SourceBody struct{ Name string; Body []byte }
type Entry struct{ Label, Raw string }              // Raw already relabeled
func Merge(bodies []SourceBody) []Entry             // dedupe Server:Port first-wins, label <name>-NNN
type ProbeResult struct{ Successes, MeanMs int }
type Survivor struct{ Entry; MeanMs int }
func SelectSurvivors(entries []Entry, res map[string]ProbeResult, rounds, maxFail, maxAvgMs int) []Survivor // sorted by MeanMs asc, stable
func BuildPayload(s []Survivor) []byte              // raw lines + '\n' each
```

- [ ] **Step C1:** Failing tests. Merge: two sources with one shared `host:443` endpoint → first source wins, labels `alpha-001`, `alpha-002`, `beta-001` (numbering counts kept nodes per source); fragment replaced when `FragmentIdx >= 0`, appended when absent; non-URI garbage lines ignored (subscription.Parse skips). SelectSurvivors: node missing from results = all-failed → dropped when maxFail=0; mean boundary (=maxAvgMs kept, >maxAvgMs dropped); fail boundary; sort by mean; BuildPayload joins raws with trailing newline.
- [ ] **Step C2:** `nix-shell --run 'go test ./internal/stable/'` — FAIL (package missing).
- [ ] **Step C3:** Implement. Merge uses `subscription.Parse(body, yield)`; key `node.Server + ":" + node.Port`; relabel: `raw := n.Raw; if n.FragmentIdx >= 0 { raw = raw[:n.FragmentIdx] }; raw += "#" + label`. Labels `fmt.Sprintf("%s-%03d", name, count)`.
- [ ] **Step C4:** `nix-shell --run 'go test ./internal/stable/'` — PASS.
- [ ] **Step C5:** Commit: `git add internal/stable && git commit -m "add stable core: holder, merge, survivor selection"`

### Task D: prober + checker + controller

**Files:**
- Create: `internal/stable/prober.go`, `internal/stable/checker.go`, `internal/stable/controller.go`
- Test: `internal/stable/checker_test.go`

**Interfaces (Produces):**
```go
type Filterer interface{ Filter(ctx context.Context, b *bytes.Buffer, req preprocess.FilterRequest) (preprocess.Stats, error) }
type Prober interface{ Probe(ctx context.Context, payload []byte) (map[string]ProbeResult, error) }
func NewMihomoProber(cfg config.CheckConfig, logger zerolog.Logger) (*MihomoProber, error)
func NewChecker(sources []config.SubscriptionSource, allowed filter.CountrySet, interval time.Duration, rounds, maxFail, maxAvgMs int, filterer func() Filterer, prober Prober, holder *Holder, logger zerolog.Logger) *Checker
func (c *Checker) Run(ctx context.Context)          // blocking loop: cycle now, then every interval
func NewController(ctx context.Context, holder *Holder, filterer func() Filterer, logger zerolog.Logger) *Controller
func (ctl *Controller) Apply(cfg config.Config) error  // stop old worker; start new one when enabled
func (ctl *Controller) Stop()                          // idempotent, joins the goroutine
```

- [ ] **Step D1:** Failing checker tests (fake Filterer writing canned URI lines per source URL, fake Prober with canned results): cycle stores snapshot with sorted survivors payload + stats; all sources error → holder untouched; zero survivors → holder untouched (previous snapshot kept); prober error → holder untouched; source order respected in dedupe; `Run` exits promptly on ctx cancel (use interval 1h, cancel after first cycle observed via holder polling with deadline).
- [ ] **Step D2:** `go test ./internal/stable/` — FAIL.
- [ ] **Step D3:** Implement checker (`runCycle` as in spec data-flow; per-source `bytes.Buffer`, copy bytes out; `filterer()` resolved once per cycle). Controller: `Apply` = `Stop()` + when `cfg.SubscriptionsEnabled()`: build allowed set (`filter.All()` minus `ParseAllowed(exclude_countries...)` minus group expansions via `set.Exclude`), `NewMihomoProber(cfg.Subscriptions.Check, ...)`, `NewChecker(...)`, `ctx, cancel := context.WithCancel(ctl.baseCtx)`, goroutine `checker.Run(ctx); close(done)`. Prober per spec: `convert.ConvertsV2Ray` → `adapter.ParseProxy` each (skip+count failures, log once per cycle), `defer Close` all; rounds loop with ctx-aware pause (`select ctx.Done / time.After`), per-round `sync.WaitGroup` + `chan struct{}` semaphore of `concurrency`, per-node `context.WithTimeout(ctx, timeout)` → `URLTest(tctx, testURL, expected)`; mutex-guarded accumulation `succ++, sum += delay` on `err == nil`; result mean = `sum/succ`. Expected ranges: `utils.NewUnsignedRanges[uint16](cfg.ExpectedStatus)` at construction.
- [ ] **Step D4:** `nix-shell --run 'go test ./internal/stable/ && go vet ./...'` — PASS.
- [ ] **Step D5:** Commit: `git add internal/stable && git commit -m "add stable prober, checker and controller"`

### Task E: `GET /stable.txt`

**Files:**
- Modify: `internal/server/server.go` (`New` signature + route + handler)
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: `stable.Holder`, `stable.Snapshot` from Task C.
- Produces: `server.New(logger zerolog.Logger, listen string, holder *Holder, stableHolder *stable.Holder) *Server`.

- [ ] **Step E1:** Failing tests: empty holder → 503 plain text; seeded holder (`stable.NewHolder()` + `Store(&stable.Snapshot{Payload: []byte("vless://x#a-001\n"), ...}` ) → 200, body exact, `Content-Type` text/plain, `X-Stable-Stats` present; POST → 405 (fiber default) or GET-only route. Update `newServer` helper (and all `server.New` callsites in tests) to pass a stable holder.
- [ ] **Step E2:** `go test ./internal/server/` — FAIL (signature).
- [ ] **Step E3:** Implement handler: snapshot nil or empty payload → `fiber.NewError(fiber.StatusServiceUnavailable, "stable list not ready")`; else headers `fiber.MIMETextPlainCharsetUTF8`, `X-Stable-Stats: updated=<RFC3339> sources=<ok>/<total> merged=<n> tested=<n> kept=<n>`, `c.Send(snap.Payload)`.
- [ ] **Step E4:** `go test ./internal/server/` — PASS.
- [ ] **Step E5:** Commit: `git add internal/server && git commit -m "serve stable list at /stable.txt"`

### Task F: wiring (app, reload, config.yaml, routes.md)

**Files:**
- Modify: `internal/app/app.go`, `internal/reload/reloader.go`, `config/config.yaml`, `routes.md`

- [ ] **Step F1:** `app.Run`: after server holder — `stableHolder := stable.NewHolder()`; `ctl := stable.NewController(ctx, stableHolder, func() stable.Filterer { return holder.Load().Svc }, logger)`; `if err := ctl.Apply(cfg); err != nil { return err }`; pass `stableHolder` to `server.New`; pass `ctl` to `NewReloader`; on shutdown path call `ctl.Stop()` after `srv.Shutdown`.
- [ ] **Step F2:** `Reloader`: new field `ctl *stable.Controller` (param in `NewReloader`); in `Reload`, after the existing snapshot swap: `if config.SubscriptionsChanged(r.currentCfg, next) || config.GroupsChanged(r.currentCfg, next) { r.ctl.Apply(next) }` (log the action; error → log, keep old worker stopped state consistent). Note the gemini sidecar rewrites `asn.deny_patterns` — that diff must not touch the worker (covered: neither helper reports change).
- [ ] **Step F3:** Reload test: existing reload tests get the extra `NewReloader` arg (nil-safe: `Apply` guard on nil controller, or pass a real controller with stub filterer). Add test: reload with only `asn.deny_patterns` changed → controller not restarted (observable via counter on a stub prober factory or via Controller instance equality — simplest: export nothing, assert через holder untouched + no goroutine leak; acceptable minimal: unit test `SubscriptionsChanged` already covers the diff, so reload test just verifies no panic with controller wired).
- [ ] **Step F4:** `config/config.yaml`: add `subscriptions:` block — interval 30m, exclude_groups [geo_blocked], check defaults omitted (rely on defaults), sources: mifa `https://mifa.world/vless`, aetris `https://raw.githubusercontent.com/flaafix/AetrisVPN-black-list/refs/heads/main/configs.txt`, purple `https://hub.mos.ru/panosenk/sukasubs/-/raw/main/purple.txt`, mehrtat `https://raw.githubusercontent.com/mehrtat/vless-collector/main/vless.txt`, mhditaheri `https://raw.githubusercontent.com/MhdiTaheri/V2rayCollector/main/sub/vless`, iboxz `https://raw.githubusercontent.com/iboxz/free-v2ray-collector/main/main/vless`, ebrasha `https://raw.githubusercontent.com/ebrasha/free-v2ray-public-list/main/V2Ray-Config-By-EbraSha.txt`, bahemmat `https://raw.githubusercontent.com/MohammadBahemmat/V2ray-Collector/main/all_servers.txt`.
- [ ] **Step F5:** `routes.md`: add `GET /stable.txt` route section + `internal/stable` package section (types, cycle algorithm, controller semantics, reload interaction).
- [ ] **Step F6:** `nix-shell --run 'make fmt && make test'` — PASS. Commit: `git add -A && git commit -m "wire stable subscriptions worker"`

### Task G: full verification + live e2e

- [ ] **Step G1:** `nix-shell --run 'make test && make race && make lint'` — all green (lint may flag mihomo indirects; fix only own code).
- [ ] **Step G2:** `docker compose build` (long first build — mihomo deps), then `docker compose up -d`.
- [ ] **Step G3:** Watch first cycle: `docker logs -f sub-preprocessor` until cycle summary; then `curl -s http://127.0.0.1:7008/stable.txt` → vless URIs with `<src>-NNN` fragments; header check `curl -sI .../stable.txt | grep X-Stable-Stats`. Before first cycle completes expect 503.
- [ ] **Step G4:** Geo check: no nodes resolving to excluded countries (spot-check: fragments only, trust pipeline stats in logs `geo_drop>0`).
- [ ] **Step G5:** Payload accepted by mihomo: temp config with file provider pointing at downloaded payload + `mihomo -t` (run from domains.lst nix-shell).
- [ ] **Step G6:** Commit any fixes; working tree clean.

### Task H: domains.lst migration

**Files (domains.lst repo):**
- Delete: `filter-subs.sh`, `Dockerfile`, `docker-compose.yml`, `docker/`, `subs/`
- Modify: `mihomo/config.yaml` (provider url → `http://192.168.1.2:7008/stable.txt`), `shell.nix` (drop `mihomo-filter-subs`), `AGENTS.md`, `REFERENCE_MAP.md` (if they mention removed files)

- [ ] **Step H1:** `docker compose down` old stack (project domainslst) — keep volume removal explicit: `docker compose down -v`.
- [ ] **Step H2:** Remove files, update shell.nix (helper + menu line), update config.yaml url + comment.
- [ ] **Step H3:** `nix-shell --run 'mihomo-yaml-check && mihomo-validate'` — test successful; `nix-instantiate --parse shell.nix` OK.
- [ ] **Step H4:** Verify router-facing endpoint from LAN perspective: `curl -s http://192.168.1.2:7008/stable.txt | head`.
- [ ] **Step H5:** Commits (atomic): `remove local subs updater stack` (deletions + shell.nix), `fetch stable list from sub-preprocessor` (config.yaml). Docs mentions in same commit as deletions.

### Final QA (spec Verification section)

- [ ] Restart the sub-preprocessor stack once more (`docker compose restart`) and confirm `/stable.txt` 503→200 transition after one cycle (keep-last-good is in-memory; restart loses it by design).
- [ ] Confirm `/` (existing preprocess endpoint) still works: `curl -s 'http://127.0.0.1:7008/?subscription_url=https://mifa.world/vless&exclude_groups=geo_blocked' | head -3`.
- [ ] Report: what ran, evidence, limitations.

## Self-review notes

- Spec coverage: config schema→B, stable core→C, prober/checker/controller→D, endpoint→E, wiring+hot-reload→F, verification→G, migration→H. Covered.
- Type consistency: `ProbeResult{Successes,MeanMs}` used in C(select)/D(prober); `server.New` 4-arg form used in E tests and F app. `Controller.Apply(cfg)` (not `Restart`) everywhere.
- No placeholders: every step has code/commands or exact acceptance criteria.
