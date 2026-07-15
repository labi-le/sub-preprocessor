# Bandwidth Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a through-node download-speed gate to the `/stable.txt` worker that drops nodes below a configurable `min_mbps` and annotates kept nodes with `[SPD:<n>M]`.

**Architecture:** A new Layer-2 `NodeFilter` named `bandwidth`, selected via `subscriptions.check.filters`, runs after latency selection in `Checker.RunOnce`. It downloads a small payload (`speed.cloudflare.com/__down?bytes=N`) through each survivor node — reusing the `apiProbeOne` dial-through-node pattern — times the body transfer, computes Mbps, drops slow/unreachable nodes, and (when `workflow.annotate`) prepends the speed tag via the existing vmess-aware relabel path. No caching; survivors are measured every cycle.

**Tech Stack:** Go, `github.com/metacubex/mihomo` (proxy adapter/URLTest), `github.com/rs/zerolog`, `net/http`, `net/http/httptest` (tests). Spec: `docs/superpowers/specs/2026-07-15-bandwidth-check-design.md`.

## Global Constraints

- Run every command via `nix-shell --run "<cmd>"` (toolchain pinned in `shell.nix`).
- `CGO_ENABLED=0` must keep working (pure-Go deps only; no new cgo).
- `ctx context.Context` is the first argument through the stack; only `main.go` introduces `context.Background()`.
- The gate is **worker-only** — never touch the on-demand `GET /` path.
- No caching/persistence of bandwidth results.
- `test_url` egresses through the proxy node, so host-side SSRF rules do **not** apply; only require a well-formed absolute http(s) URL (mirror the existing `check.test_url` validation).
- Match existing patterns: `NodeFilter` shape (`nodefilter.go`), the `apiProbeOne`/`apiCheck` fan-out (`prober_api.go`), config defaults/validation idioms (`config.go`).
- After the feature works, update `routes.md` (stable + config + rewrite package entries) — this is the final cleanup step, not a task.

---

### Task 1: Config — `check.bandwidth` block

**Files:**
- Modify: `internal/config/config.go` (const block ~34-48; `CheckConfig` struct ~95-105; `SubscriptionsConfig.applyDefaults` ~412-438; `CheckConfig.validate` ~476-512)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.BandwidthConfig{ TestURL string; MinMbps *int; Timeout time.Duration; Concurrency int }`; field `CheckConfig.Bandwidth BandwidthConfig`.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestLoadBandwidthDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "subscriptions:\n  sources:\n    - name: mifa\n      url: https://mifa.world/vless\n")
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Subscriptions.Check.Bandwidth
	if b.TestURL != "https://speed.cloudflare.com/__down?bytes=2000000" {
		t.Fatalf("test_url default = %q", b.TestURL)
	}
	if b.MinMbps == nil || *b.MinMbps != 5 {
		t.Fatalf("min_mbps default = %v, want 5", b.MinMbps)
	}
	if b.Timeout != 20*time.Second {
		t.Fatalf("timeout default = %v, want 20s", b.Timeout)
	}
	if b.Concurrency != 4 {
		t.Fatalf("concurrency default = %d, want 4", b.Concurrency)
	}

	// An explicit min_mbps: 0 means "no floor" and must survive defaulting.
	cfg0, err := writeConfig(t, "subscriptions:\n  sources:\n    - name: mifa\n      url: https://mifa.world/vless\n  check:\n    bandwidth:\n      min_mbps: 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg0.Subscriptions.Check.Bandwidth.MinMbps; m == nil || *m != 0 {
		t.Fatalf("explicit min_mbps=0 must be preserved, got %v", m)
	}
}

func TestLoadRejectsInvalidBandwidth(t *testing.T) {
	t.Parallel()

	// Negative values survive the "==0 -> default" coercion and reach validation.
	const subs = "subscriptions:\n  sources:\n    - name: a\n      url: https://a.example.com/s\n  check:\n    bandwidth:\n"
	const base = "geofeed:\n  sources:\n    - url: https://example.com/geofeed.csv.gz\n      type: gzip\n"
	cases := map[string]string{
		"negative timeout":     subs + "      timeout: -1s\n",
		"negative concurrency": subs + "      concurrency: -1\n",
		"negative min_mbps":    subs + "      min_mbps: -1\n",
		"non-http test_url":    subs + "      test_url: ftp://example.com/x\n",
	}
	for name, block := range cases {
		if _, err := loadRaw(t, base+block); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
```

These reuse the existing `writeConfig`/`loadRaw` helpers in `config_test.go` (external `package config_test`); do NOT add exported test shims. `applyDefaults`/`validate` are exercised through `config.Load`.

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell --run "go test ./internal/config/ -run 'TestLoadBandwidthDefaults|TestLoadRejectsInvalidBandwidth' -v"`
Expected: FAIL (compile error: `BandwidthConfig` undefined / `Bandwidth` field missing).

- [ ] **Step 3: Add constants**

In `internal/config/config.go`, in the `const (...)` block after `defaultCheckConcurr = 16` (line ~35):

```go
	defaultBandwidthTestURL  = "https://speed.cloudflare.com/__down?bytes=2000000"
	defaultBandwidthMinMbps  = 5
	defaultBandwidthTimeout  = 20 * time.Second
	defaultBandwidthConcurr  = 4
```

- [ ] **Step 4: Add the struct and field**

In `internal/config/config.go`, add the `Bandwidth` field to `CheckConfig` (after `Filters []string` ~line 104):

```go
	Bandwidth      BandwidthConfig `yaml:"bandwidth"`
```

And define the struct immediately after the `CheckConfig` struct block:

```go
// BandwidthConfig configures the through-node download-speed gate (the
// "bandwidth" node filter). Enabled by listing "bandwidth" in check.filters.
// MinMbps is a pointer so an unset value defaults to defaultBandwidthMinMbps
// while an explicit 0 means "no speed floor" (annotate + drop-unreachable only).
type BandwidthConfig struct {
	TestURL     string        `yaml:"test_url"`
	MinMbps     *int          `yaml:"min_mbps"`
	Timeout     time.Duration `yaml:"timeout"`
	Concurrency int           `yaml:"concurrency"`
}
```

- [ ] **Step 5: Apply defaults**

In `SubscriptionsConfig.applyDefaults`, after the `c.Concurrency` default (line ~437, before the closing brace):

```go
	if c.Bandwidth.TestURL == "" {
		c.Bandwidth.TestURL = defaultBandwidthTestURL
	}
	if c.Bandwidth.MinMbps == nil {
		mbps := defaultBandwidthMinMbps
		c.Bandwidth.MinMbps = &mbps
	}
	if c.Bandwidth.Timeout == 0 {
		c.Bandwidth.Timeout = defaultBandwidthTimeout
	}
	if c.Bandwidth.Concurrency == 0 {
		c.Bandwidth.Concurrency = defaultBandwidthConcurr
	}
```

- [ ] **Step 6: Validate**

In `CheckConfig.validate`, before the final `return nil` (line ~510):

```go
	b := c.Bandwidth
	if b.MinMbps != nil && *b.MinMbps < 0 {
		return errors.New("subscriptions.check.bandwidth.min_mbps must not be negative")
	}
	if b.Timeout <= 0 {
		return errors.New("subscriptions.check.bandwidth.timeout must be positive")
	}
	if b.Concurrency < 1 {
		return errors.New("subscriptions.check.bandwidth.concurrency must be at least 1")
	}
	if b.TestURL != "" {
		// Egresses THROUGH the proxy node, so host-side SSRF rules don't apply;
		// only require a well-formed absolute http(s) URL.
		u, err := url.Parse(b.TestURL)
		if err != nil {
			return fmt.Errorf("subscriptions.check.bandwidth.test_url: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("subscriptions.check.bandwidth.test_url: must be an absolute http(s) URL, got %q", b.TestURL)
		}
	}
```

- [ ] **Step 7: Run test to verify it passes**

Run: `nix-shell --run "go test ./internal/config/ -run 'TestLoadBandwidthDefaults|TestLoadRejectsInvalidBandwidth' -v"`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add check.bandwidth block (defaults + validation)"
```

---

### Task 2: rewrite — recognize the `[SPD:…]` tag

**Files:**
- Modify: `internal/rewrite/rewrite.go` (`isKnownTag` ~145-159)
- Test: `internal/rewrite/rewrite_test.go`

**Interfaces:**
- Produces: `StripKnownTags`/`LeadingTags` now treat `[SPD:...]` as a known tag (no signature change).

- [ ] **Step 1: Write the failing test**

Add to `internal/rewrite/rewrite_test.go`:

```go
func TestKnownTagsIncludeSPD(t *testing.T) {
	t.Parallel()

	if got := rewrite.StripKnownTags("[SPD:45M] Tokyo"); got != "Tokyo" {
		t.Fatalf("StripKnownTags dropped SPD wrong: %q", got)
	}
	if got := rewrite.StripKnownTags("[GEO:FI][IP:1.2.3.4][SPD:5M] node"); got != "node" {
		t.Fatalf("StripKnownTags mixed tags: %q", got)
	}
	if got := rewrite.LeadingTags("[SPD:12M] node"); got != "[SPD:12M]" {
		t.Fatalf("LeadingTags = %q, want [SPD:12M]", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell --run "go test ./internal/rewrite/ -run TestKnownTagsIncludeSPD -v"`
Expected: FAIL (SPD treated as unknown; `StripKnownTags` returns the tag verbatim).

- [ ] **Step 3: Add SPD to `isKnownTag`**

In `internal/rewrite/rewrite.go`, inside `isKnownTag`, before `return false`:

```go
	if len(tag) >= 4 && tag[:4] == "SPD:" {
		return true
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `nix-shell --run "go test ./internal/rewrite/ -run TestKnownTagsIncludeSPD -v"`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rewrite/rewrite.go internal/rewrite/rewrite_test.go
git commit -m "feat(rewrite): recognize [SPD:] as a known tag"
```

---

### Task 3: stable — Mbps math + measurement (`prober_bandwidth.go`)

**Files:**
- Create: `internal/stable/prober_bandwidth.go`
- Test: `internal/stable/prober_bandwidth_test.go`

**Interfaces:**
- Consumes: `config.BandwidthConfig` (Task 1); `*MihomoProber.cfg config.CheckConfig` (existing, prober.go:26).
- Produces:
  - `type BandwidthOutcome struct { Server string; Reachable bool; Mbps int }`
  - `func computeMbps(bytesRead int64, elapsed time.Duration) int`
  - `func measure(ctx context.Context, client *http.Client, target string) (reachable bool, n int64, elapsed time.Duration)`
  - `func bandwidthProbeOne(ctx context.Context, px mihomo.Proxy, target string, timeout time.Duration) (reachable bool, mbps int)`
  - `func (m *MihomoProber) BandwidthCheck(ctx context.Context, payload []byte) map[string]BandwidthOutcome`
  - `func (m *MihomoProber) BandwidthMinMbps() int`

- [ ] **Step 1: Write the failing test**

Create `internal/stable/prober_bandwidth_test.go`:

```go
package stable //nolint:testpackage // exercises unexported stable internals

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestComputeMbps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		bytes   int64
		elapsed time.Duration
		want    int
	}{
		{2_500_000, time.Second, 20},       // 2.5MB*8/1s = 20 Mbps
		{1_250_000, time.Second, 10},       // 10 Mbps
		{1_000_000, 100 * time.Millisecond, 80},
		{0, time.Second, 0},                // no bytes
		{2_000_000, 0, 0},                  // zero elapsed guarded (no divide/panic)
	}
	for _, c := range cases {
		if got := computeMbps(c.bytes, c.elapsed); got != c.want {
			t.Errorf("computeMbps(%d, %v) = %d, want %d", c.bytes, c.elapsed, got, c.want)
		}
	}
}

func TestMeasureSendsIdentityAndCountsBytes(t *testing.T) {
	t.Parallel()

	const n = 200_000
	var gotEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		_, _ = w.Write(make([]byte, n))
	}))
	defer srv.Close()

	reachable, read, elapsed := measure(context.Background(), srv.Client(), srv.URL)
	if !reachable {
		t.Fatal("expected reachable")
	}
	if read != n {
		t.Fatalf("bytesRead = %d, want %d", read, n)
	}
	if gotEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotEncoding)
	}
	if elapsed <= 0 {
		t.Fatalf("elapsed must be positive, got %v", elapsed)
	}
}

func TestMeasureRedirectYieldsNoBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/other", http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	reachable, read, _ := measure(context.Background(), client, srv.URL)
	if !reachable {
		t.Fatal("a 3xx is still a response (reachable)")
	}
	if read != 0 {
		t.Fatalf("redirect body should be ~0, got %d", read)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell --run "go test ./internal/stable/ -run 'TestComputeMbps|TestMeasure' -v"`
Expected: FAIL (compile error: `computeMbps`/`measure` undefined).

- [ ] **Step 3: Write the implementation**

Create `internal/stable/prober_bandwidth.go`:

```go
package stable

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	mihomo "github.com/metacubex/mihomo/constant"

	"domains.lst/sub-preprocessor/internal/log"
)

// maxBandwidthBody caps the body read per node against a hostile/misconfigured
// endpoint that streams forever. Normal test payloads (a few MB) are far below.
const maxBandwidthBody = 256 << 20

// BandwidthOutcome is the per-node result of a through-node download-speed test.
// Reachable is false when the dial/GET failed; a reachable-but-slow node has
// Reachable=true and a low Mbps.
type BandwidthOutcome struct {
	Server    string
	Reachable bool
	Mbps      int
}

// computeMbps converts a byte count and transfer duration to integer Mbps.
// Guards elapsed<=0 and bytesRead<=0 so a sub-second or empty transfer never
// divides by zero or yields NaN.
func computeMbps(bytesRead int64, elapsed time.Duration) int {
	if bytesRead <= 0 || elapsed <= 0 {
		return 0
	}
	return int(float64(bytesRead) * 8 / elapsed.Seconds() / 1e6)
}

// measure issues a GET to target through the supplied client, forcing
// Accept-Encoding: identity so bytesRead equals wire bytes (Go otherwise adds
// gzip and transparently decompresses, inflating the rate). Timing starts after
// the response headers arrive (connect/TLS/TTFB excluded) and covers only the
// body transfer. A partial read at the deadline still returns its byte count.
func measure(ctx context.Context, client *http.Client, target string) (bool, int64, time.Duration) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, 0, 0
	}
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return false, 0, 0
	}
	defer func() { _ = resp.Body.Close() }()

	start := time.Now()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBandwidthBody))
	elapsed := time.Since(start)
	return true, n, elapsed
}

// bandwidthProbeOne dials target through px, downloads it over a fixed-conn
// transport (mirroring apiProbeOne), and returns the measured Mbps. Compression
// is disabled and redirects are not followed (the conn is pinned to one host).
func bandwidthProbeOne(ctx context.Context, px mihomo.Proxy, target string, timeout time.Duration) (bool, int) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	host, port, err := net.SplitHostPort(hostPort(target))
	if err != nil {
		return false, 0
	}
	var meta mihomo.Metadata
	if addrErr := meta.SetRemoteAddress(net.JoinHostPort(host, port)); addrErr != nil {
		return false, 0
	}
	conn, err := px.DialContext(tctx, &meta)
	if err != nil {
		return false, 0
	}
	defer func() { _ = conn.Close() }()

	transport := &http.Transport{
		DialContext:         func(context.Context, string, string) (net.Conn, error) { return conn, nil },
		TLSHandshakeTimeout: apiTLSHandshakeTimeout,
		DisableCompression:  true,
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	reachable, n, elapsed := measure(tctx, client, target)
	if !reachable {
		return false, 0
	}
	return true, computeMbps(n, elapsed)
}

// BandwidthCheck downloads the configured test_url through every node in payload
// (bounded by check.bandwidth.concurrency) and returns each node's measured
// speed. Mirrors apiCheck's fan-out: one shared semaphore, per-node debug log,
// progress reporter.
func (m *MihomoProber) BandwidthCheck(ctx context.Context, payload []byte) map[string]BandwidthOutcome {
	proxies, err := m.parseProxies(payload)
	if err != nil {
		m.logger.Warn().Err(err).Msg("bandwidth check: no proxies")
		return nil
	}
	defer func() {
		for _, px := range proxies {
			_ = px.Close()
		}
	}()

	target := m.cfg.Bandwidth.TestURL
	timeout := m.cfg.Bandwidth.Timeout
	concurrency := m.cfg.Bandwidth.Concurrency

	opLog := log.Op(m.logger, "stable.BandwidthCheck")
	prog := newProgress(opLog, "bandwidth check progress", len(proxies))

	out := make(map[string]BandwidthOutcome, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reachable, mbps := bandwidthProbeOne(ctx, px, target, timeout)
			host, _, splitErr := net.SplitHostPort(px.Addr())
			if splitErr != nil {
				host = px.Addr()
			}
			o := BandwidthOutcome{Server: host, Reachable: reachable, Mbps: mbps}
			n := prog.step()
			opLog.Debug().Str("node", px.Name()).Str("server", host).
				Bool("reachable", o.Reachable).Int("mbps", o.Mbps).
				Int64("n", n).Int64("of", prog.total).Msg("bandwidth check")
			mu.Lock()
			out[px.Name()] = o
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// BandwidthMinMbps resolves the configured floor; nil (unset) means 0 = no floor.
func (m *MihomoProber) BandwidthMinMbps() int {
	if m.cfg.Bandwidth.MinMbps == nil {
		return 0
	}
	return *m.cfg.Bandwidth.MinMbps
}
```

Add the `hostPort` helper (target host:port with a default of 443 for https / 80 for http) at the bottom of the same file — it replaces `apiProbeOne`'s inline `url.Parse`/port logic so `bandwidthProbeOne` stays readable:

```go
// hostPort extracts host:port from a URL, defaulting the port by scheme.
func hostPort(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}
```

Add `"net/url"` to the import block.

- [ ] **Step 4: Run test to verify it passes**

Run: `nix-shell --run "go test ./internal/stable/ -run 'TestComputeMbps|TestMeasure' -v"`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/stable/prober_bandwidth.go internal/stable/prober_bandwidth_test.go
git commit -m "feat(stable): through-node bandwidth probe + Mbps math"
```

---

### Task 4: stable — `bandwidthFilter` selection + annotation

**Files:**
- Modify: `internal/stable/select.go` (`Survivor` struct ~14-18)
- Modify: `internal/stable/nodefilter.go` (add const, interface, `bandwidthFilter`, `annotateSpeed`)
- Test: `internal/stable/nodefilter_test.go`

**Interfaces:**
- Consumes: `BandwidthOutcome`, `BandwidthCheck`, `BandwidthMinMbps` (Task 3); `entriesPayload` (checker.go:257); `relabelNode` (merge.go:66); `subscription.Parse`.
- Produces: `Survivor.Mbps int`; `bandwidthFilter` implementing `NodeFilter`; `bandwidthChecker` interface; `bandwidthFilterName = "bandwidth"`.

- [ ] **Step 1: Add `Mbps` to `Survivor`**

In `internal/stable/select.go`, add to the `Survivor` struct:

```go
type Survivor struct {
	Entry
	MeanMs int
	Mbps   int
}
```

- [ ] **Step 2: Write the failing test**

Add to `internal/stable/nodefilter_test.go` (imports: add `"context"`, `"strings"`):

```go
func TestBandwidthFilterApply(t *testing.T) {
	t.Parallel()

	survivors := []Survivor{
		{Entry: Entry{Label: "s-001", Tagged: "vless://u@h1:443#[GEO:FI][IP:1.1.1.1] s-001"}},
		{Entry: Entry{Label: "s-002", Tagged: "vless://u@h2:443#[GEO:SE][IP:2.2.2.2] s-002"}},
		{Entry: Entry{Label: "s-003", Tagged: "vless://u@h3:443#[GEO:DE][IP:3.3.3.3] s-003"}},
	}
	check := func(context.Context, []byte) map[string]BandwidthOutcome {
		return map[string]BandwidthOutcome{
			"s-001": {Server: "h1", Reachable: true, Mbps: 50}, // fast -> keep
			"s-002": {Server: "h2", Reachable: true, Mbps: 3},  // slow -> drop
			"s-003": {Server: "h3", Reachable: false},          // unreachable -> drop
		}
	}

	f := &bandwidthFilter{minMbps: 10, annotate: true, check: check, logger: zerolog.Nop()}
	kept := f.apply(context.Background(), survivors)
	if len(kept) != 1 || kept[0].Label != "s-001" {
		t.Fatalf("expected only s-001 kept, got %+v", kept)
	}
	if kept[0].Mbps != 50 {
		t.Fatalf("Mbps not recorded: %d", kept[0].Mbps)
	}
	if !strings.Contains(kept[0].Tagged, "[SPD:50M]") {
		t.Fatalf("missing speed tag: %q", kept[0].Tagged)
	}

	// annotate=false: kept but no tag injected.
	f2 := &bandwidthFilter{minMbps: 10, annotate: false, check: check, logger: zerolog.Nop()}
	kept2 := f2.apply(context.Background(), survivors)
	if len(kept2) != 1 || strings.Contains(kept2[0].Tagged, "[SPD:") {
		t.Fatalf("annotate=false must not inject SPD: %q", kept2[0].Tagged)
	}

	// minMbps=0: keep all reachable (no floor).
	f3 := &bandwidthFilter{minMbps: 0, annotate: false, check: check, logger: zerolog.Nop()}
	if kept3 := f3.apply(context.Background(), survivors); len(kept3) != 2 {
		t.Fatalf("minMbps=0 keeps all reachable, got %d", len(kept3))
	}

	// cancelled ctx: no-op, survivors unchanged.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := f.apply(ctx, survivors); len(got) != len(survivors) {
		t.Fatalf("cancelled ctx must pass survivors through, got %d", len(got))
	}
}

func TestBandwidthFilterAnnotatesVmess(t *testing.T) {
	t.Parallel()

	// vmess name lives in base64 JSON ps; annotation must go through the
	// vmess-aware relabel path, not fragment surgery.
	vmess := vmessLine(`{"v":"2","ps":"s-001","add":"1.2.3.4","port":"443","id":"uuid","net":"ws"}`)
	survivors := []Survivor{{Entry: Entry{Label: "s-001", Tagged: vmess}}}
	check := func(context.Context, []byte) map[string]BandwidthOutcome {
		return map[string]BandwidthOutcome{"s-001": {Server: "1.2.3.4", Reachable: true, Mbps: 42}}
	}
	f := &bandwidthFilter{minMbps: 1, annotate: true, check: check, logger: zerolog.Nop()}
	kept := f.apply(context.Background(), survivors)
	if len(kept) != 1 {
		t.Fatalf("expected 1 kept, got %d", len(kept))
	}
	// Re-parse the annotated vmess and confirm the ps carries the tag.
	var name string
	subscription.Parse([]byte(kept[0].Tagged), func(n subscription.Node) bool {
		name = n.Name
		return false
	})
	if !strings.Contains(name, "[SPD:42M]") {
		t.Fatalf("vmess ps missing speed tag: %q", name)
	}
}
```

`vmessLine` already exists in the subscription test package; replicate the one-liner helper locally in `nodefilter_test.go` if it is not visible from `package stable`:

```go
func vmessLine(payload string) string {
	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(payload))
}
```

(imports for the test file: add `"encoding/base64"` and `"domains.lst/sub-preprocessor/internal/subscription"`.)

- [ ] **Step 3: Run test to verify it fails**

Run: `nix-shell --run "go test ./internal/stable/ -run TestBandwidthFilter -v"`
Expected: FAIL (compile error: `bandwidthFilter` undefined).

- [ ] **Step 4: Implement `bandwidthFilter` + `annotateSpeed`**

In `internal/stable/nodefilter.go`, add the name to the const block:

```go
const (
	geminiFilterName    = "gemini"
	claudeFilterName    = "claude"
	bandwidthFilterName = "bandwidth"
)
```

Add the interface (near `geminiChecker`/`claudeChecker`):

```go
// bandwidthChecker is the through-node download-speed capability of a Prober.
type bandwidthChecker interface {
	BandwidthCheck(ctx context.Context, payload []byte) map[string]BandwidthOutcome
	BandwidthMinMbps() int
}
```

Add the filter type and its methods (imports: add `"strconv"` and `"domains.lst/sub-preprocessor/internal/subscription"`):

```go
// bandwidthFilter keeps only survivors whose measured through-node download
// speed is at least minMbps (minMbps==0 disables the floor and keeps all
// reachable nodes). It records Mbps on each kept survivor and, when annotate is
// set, prepends a [SPD:<n>M] tag to the published name via the vmess-aware
// relabel path. No store: bandwidth results are never persisted.
type bandwidthFilter struct {
	minMbps  int
	annotate bool
	check    func(ctx context.Context, payload []byte) map[string]BandwidthOutcome
	logger   zerolog.Logger
}

func (f *bandwidthFilter) name() string { return bandwidthFilterName }

func (f *bandwidthFilter) apply(ctx context.Context, survivors []Survivor) []Survivor {
	subset := make([]Entry, 0, len(survivors))
	for _, s := range survivors {
		subset = append(subset, s.Entry)
	}
	outcomes := f.check(ctx, entriesPayload(subset))
	if outcomes == nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter skipped: no outcomes")
		return survivors
	}
	if ctx.Err() != nil {
		f.logger.Warn().Str("filter", bandwidthFilterName).Msg("filter cancelled; keeping survivors unchanged")
		return survivors
	}

	kept := make([]Survivor, 0, len(survivors))
	var slow, unreachable int
	for _, s := range survivors {
		o := outcomes[s.Label]
		switch {
		case !o.Reachable:
			unreachable++
		case f.minMbps > 0 && o.Mbps < f.minMbps:
			slow++
		default:
			s.Mbps = o.Mbps
			if f.annotate {
				s.Tagged = annotateSpeed(s.Tagged, o.Mbps)
			}
			kept = append(kept, s)
		}
	}
	f.logger.Info().Str("filter", bandwidthFilterName).Int("survivors", len(survivors)).
		Int("kept", len(kept)).Int("slow", slow).Int("unreachable", unreachable).Msg("node filter")
	return kept
}

// annotateSpeed prepends [SPD:<mbps>M] to a node's published name. It re-parses
// the line and relabels through relabelNode so vmess (base64 ps) and URI
// (#fragment) nodes are both handled; on any parse failure the line is returned
// unchanged (annotation is best-effort, never fatal).
func annotateSpeed(line string, mbps int) string {
	var out string
	found := false
	subscription.Parse([]byte(line), func(n subscription.Node) bool {
		if relabeled, ok := relabelNode(n, "[SPD:"+strconv.Itoa(mbps)+"M] "+n.Name); ok {
			out = relabeled
			found = true
		}
		return false
	})
	if !found {
		return line
	}
	return out
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `nix-shell --run "go test ./internal/stable/ -run TestBandwidthFilter -v"`
Expected: PASS (both `TestBandwidthFilterApply` and `TestBandwidthFilterAnnotatesVmess`).

- [ ] **Step 6: Commit**

```bash
git add internal/stable/select.go internal/stable/nodefilter.go internal/stable/nodefilter_test.go
git commit -m "feat(stable): bandwidth node filter with speed annotation"
```

---

### Task 5: stable — wire `bandwidth` into `buildNodeFilters` + controller

**Files:**
- Modify: `internal/stable/nodefilter.go` (`buildNodeFilters` ~101-135)
- Modify: `internal/stable/controller.go` (`Apply` ~59)
- Test: `internal/stable/nodefilter_test.go` (update existing `TestBuildNodeFilters`)

**Interfaces:**
- Consumes: `bandwidthChecker`, `bandwidthFilter` (Task 4); `cfg.Workflow.Annotate` (config).
- Produces: `buildNodeFilters(names []string, prober Prober, store Blocklist, annotate bool, logger zerolog.Logger) []NodeFilter`.

- [ ] **Step 1: Update the failing test**

In `internal/stable/nodefilter_test.go`, update `TestBuildNodeFilters` — every `buildNodeFilters(...)` call gains an `annotate` arg, and assert the bandwidth branch:

```go
func TestBuildNodeFilters(t *testing.T) {
	t.Parallel()

	prober, err := NewMihomoProber(config.CheckConfig{ExpectedStatus: "204"}, config.GeminiConfig{}, "KEY", config.ClaudeConfig{}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	if fs := buildNodeFilters(nil, prober, nil, true, zerolog.Nop()); len(fs) != 0 {
		t.Fatalf("no names -> no filters, got %d", len(fs))
	}

	fs := buildNodeFilters([]string{"gemini", "claude", "bandwidth", "bogus"}, prober, nil, true, zerolog.Nop())
	if len(fs) != 3 {
		t.Fatalf("gemini + claude + bandwidth + unknown -> 3 filters, got %d", len(fs))
	}
	if fs[2].name() != "bandwidth" {
		t.Fatalf("expected bandwidth filter third, got %q", fs[2].name())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell --run "go test ./internal/stable/ -run TestBuildNodeFilters -v"`
Expected: FAIL (compile error: too few args / no `bandwidth` case yet).

- [ ] **Step 3: Extend `buildNodeFilters`**

In `internal/stable/nodefilter.go`, change the signature and add the case:

```go
func buildNodeFilters(names []string, prober Prober, store Blocklist, annotate bool, logger zerolog.Logger) []NodeFilter {
```

Add, alongside the `gemini`/`claude` cases:

```go
		case bandwidthFilterName:
			bc, ok := prober.(bandwidthChecker)
			if !ok {
				logger.Warn().Msg("bandwidth filter requested but prober lacks bandwidth support; skipping")
				continue
			}
			filters = append(filters, &bandwidthFilter{
				minMbps:  bc.BandwidthMinMbps(),
				annotate: annotate,
				check:    bc.BandwidthCheck,
				logger:   logger,
			})
```

- [ ] **Step 4: Update the controller call**

In `internal/stable/controller.go`, in `Apply`, replace the `buildNodeFilters` call (~line 59):

```go
	annotate := cfg.Workflow.Annotate == nil || *cfg.Workflow.Annotate
	filters := buildNodeFilters(subs.Check.Filters, prober, c.store, annotate, c.logger)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix-shell --run "go test ./internal/stable/ -v"`
Expected: PASS (whole stable package compiles and all tests green).

- [ ] **Step 6: Commit**

```bash
git add internal/stable/nodefilter.go internal/stable/controller.go internal/stable/nodefilter_test.go
git commit -m "feat(stable): select bandwidth filter via check.filters (annotate-gated)"
```

---

### Task 6: Full verification + docs

**Files:**
- Modify: `routes.md` (stable, config, rewrite entries)
- Modify: `config.yaml` (add a commented `check.bandwidth` example under `subscriptions.check`)

- [ ] **Step 1: Full test + race + lint**

Run: `nix-shell --run "make test"`
Then: `nix-shell --run "make race"`
Then: `nix-shell --run "make lint"`
Expected: all green. Fix any failures before proceeding.

- [ ] **Step 2: Live smoke test**

Add to `config.yaml` under `subscriptions.check`:

```yaml
    filters: [bandwidth]
    bandwidth:
      test_url: "https://speed.cloudflare.com/__down?bytes=2000000"
      min_mbps: 5
      timeout: 20s
      concurrency: 4
```

Run the service (`nix-shell --run "make run"` or the project's run target), wait for one worker cycle, then:

```bash
curl -s "http://127.0.0.1:<listen-port>/stable.txt" | head
```

Expected: node names carry `[SPD:<n>M]` tags. Then set `min_mbps: 100000` (absurdly high), reload, wait one cycle, and confirm `/stable.txt` returns `503`/empty or the previous list with the log showing all nodes dropped as slow (`op=stable.BandwidthCheck` debug lines + `node filter kept=0`). Restore `min_mbps: 5`.

- [ ] **Step 3: Update `routes.md`**

Update the `internal/stable` entry (new `prober_bandwidth.go`; `bandwidth` node filter; `Survivor.Mbps`), the `internal/config` entry (`BandwidthConfig`, `CheckConfig.Bandwidth`), and the `internal/rewrite` entry (`[SPD:]` known tag). Keep it consistent with the existing terse style.

- [ ] **Step 4: Commit**

```bash
git add routes.md config.yaml
git commit -m "docs: document bandwidth check (routes.md + config.yaml example)"
```

---

## Notes for the implementer

- **Do not** re-probe latency in the bandwidth pass — it runs on the already-selected survivors.
- **Do not** add caching/persistence — a slow node is re-measured next cycle by design.
- The `apiTLSHandshakeTimeout` constant is already defined in `prober_api.go` (same package) — reuse it, don't redefine.
- `parseProxies`, `newProgress`/`prog.step()`/`prog.total`, `entriesPayload`, and `relabelNode` are existing package-internal helpers — reuse them; do not reimplement.
- Order matters in `check.filters`: put `bandwidth` last so it runs on the fewest nodes (least data). This is operator config, not code.
- `test_url`'s `?bytes=` is not parsed or clamped against `maxBandwidthBody` (256 MB) in v1. If an operator sets `bytes=` above the cap, `io.LimitReader` truncates the read at the cap and the rate stays valid (computed over bytes actually read). Default N=2 MB is far below, so this is a conscious non-issue.
