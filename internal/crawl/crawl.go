// Package crawl scrapes public Telegram channel web previews (t.me/s/<channel>),
// treats every https link found as a subscription candidate, and keeps the ones
// that classify as a live subscription — appending them to the private.yaml
// overlay the preprocessor merges into subscriptions.sources. It is format
// agnostic: it matches the artifact (an https URL that returns a subscription),
// not any channel-specific wrapper pattern.
package crawl

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// managedPrefix marks sources this crawler owns. Sources without it (hand-added
// private subscriptions) are never touched.
const managedPrefix = "tg-"

const (
	classifyConcurrency = 8
	classifyTimeout     = 15 * time.Second
	fetchTimeout        = 20 * time.Second
	userAgent           = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/125.0 Safari/537.36"
	maxPageBytes        = 8 << 20 // cap on bytes read from a single channel page
	oneDay              = 24 * time.Hour
)

var (
	urlRe    = regexp.MustCompile(`https://[^\s"'<>\p{Z}]+`) // \p{Z}: URLs never contain unicode whitespace (e.g. &nbsp; adjacent to a link)
	cursorRe = regexp.MustCompile(`data-post="[^"]+/(\d+)"`)
	trimSet  = ".,;:!?)]}'\""
	// inlineRe matches raw proxy URIs pasted directly in channel messages.
	// Alternation order is not load-bearing: Go's leftmost-first alternation
	// still backtracks, so "ss" cannot shadow "ssr://" — the whole pattern,
	// "://" included, has to match. http/https/socks* are absent on purpose:
	// parseNode rejects only the PORTLESS form, so a
	// "https://example.com:8443/docs" pasted in a message IS a valid node, and
	// harvesting those would turn every documentation link a channel posts into
	// a proxy.
	inlineRe = regexp.MustCompile(`\b(?:vless|vmess|ss|ssr|trojan|tuic|hysteria2|hysteria|hy2|anytls|mierus)://[^\s"'<>]+`)
)

// legacyNameRe matches the pre-attribution managed name form tg-<sha10>. Such
// names carry no origin info, so they are upgraded to the channel-attributed
// form the first time the URL is rediscovered in a channel.
var legacyNameRe = regexp.MustCompile(`^tg-[0-9a-f]{10}$`)

// Options configures a crawl run.
type Options struct {
	Channels      []string // static seed channels (from CRAWL_CHANNELS); merged with ChannelsPath
	ChannelsPath  string   // YAML file of seed channels, re-read each cycle for hot-reload
	PrivatePath   string
	Pages         int           // t.me/s pages (~20 msgs each) to walk back per configured seed channel
	Prune         bool          // drop managed sources proven no longer live
	MaxDepth      int           // repost-recursion depth; 0 = only seed channels (no recursion)
	MaxChannels   int           // cap on discovered (non-seed) channels per cycle; 0 = defaultMaxDiscovered
	StatePath     string        // persisted productive-channel memory; empty disables persistence
	StateTTL      time.Duration // drop a productive channel from memory after this long without a live sub
	InlineEnabled bool          // harvest raw inline proxy URIs pasted in channel messages
	InlineMax     int           // cap on inline nodes kept per cycle (first N after dedup)
}

type source struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url,omitempty"`
	Body string `yaml:"body,omitempty"`
}

type privateFile struct {
	Subscriptions struct {
		Sources []source `yaml:"sources"`
	} `yaml:"subscriptions"`
}

// Crawler runs crawl cycles.
type Crawler struct {
	opts       Options
	client     fetchClient
	httpClient *http.Client
	classifyFn func(ctx context.Context, client *http.Client, u fetch.SubscriptionURL) (classify.Result, error)
	logger     zerolog.Logger
	// running serializes crawl cycles: a triggered cycle and a scheduled tick
	// never overlap. TryLock lets a scheduled tick (or HTTP trigger) skip
	// cleanly when a cycle is already in flight instead of queueing behind it.
	running sync.Mutex
}

// fetchClient fetches a channel page; an interface so tests can avoid the network.
type fetchClient interface {
	page(ctx context.Context, u string) (string, error)
}

// httpFetcher fetches a page with the crawler's unrestricted client (no IP
// guard, so t.me via the fake-ip tunnel is reachable) and a browser User-Agent.
type httpFetcher struct{ client *http.Client }

func (f httpFetcher) page(ctx context.Context, u string) (string, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(b), nil
}

func New(opts Options, logger zerolog.Logger) *Crawler {
	client := fetch.NewUnrestrictedHTTPClient()
	return &Crawler{opts: opts, client: httpFetcher{client: client}, httpClient: client, classifyFn: classify.URL, logger: logger}
}

// Run executes a cycle immediately, then every interval until ctx is done.
// Cycles go through runGuarded, so a tick that fires while a cycle (scheduled
// or HTTP-triggered) is still running is skipped rather than overlapped, and
// each cycle is bounded by cycleBudget so it cannot starve the schedule.
func (c *Crawler) Run(ctx context.Context, interval time.Duration) {
	budget := cycleBudget(interval)
	c.runGuarded(ctx, budget)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !c.runGuarded(ctx, budget) {
				c.logger.Warn().Msg("previous crawl cycle still running; skipping scheduled tick")
			}
		}
	}
}

// RunDaily runs one cycle at the next occurrence of hour:min in the process's
// local time zone, then once every 24h at that wall-clock time, until ctx is
// done. Unlike Run it does not fire immediately — it waits for the scheduled
// time.
func (c *Crawler) RunDaily(ctx context.Context, hour, minute int) {
	budget := cycleBudget(oneDay)
	for {
		next := nextDaily(time.Now(), hour, minute)
		c.logger.Info().Time("next_run", next).Str("in", time.Until(next).Truncate(time.Second).String()).
			Msg("crawl scheduled")
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			if !c.runGuarded(ctx, budget) {
				c.logger.Warn().Msg("previous crawl cycle still running; skipping scheduled run")
			}
		}
	}
}

// runGuarded runs a single crawl cycle only if none is already in flight. It
// TryLocks the cycle mutex: on success it runs RunOnce and returns true; if a
// cycle is already running it returns false immediately without waiting, so a
// scheduled tick or an HTTP trigger that collides with a live cycle is skipped
// safely rather than queued. A non-zero budget bounds the cycle's wall clock.
func (c *Crawler) runGuarded(ctx context.Context, budget time.Duration) bool {
	if !c.running.TryLock() {
		return false
	}
	defer c.running.Unlock()
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	c.RunOnce(ctx)
	return true
}

const (
	// maxCycleBudget caps the per-cycle wall clock for schedules long enough
	// that a fraction of the interval bounds nothing (CRAWL_AT is daily). A
	// cycle still running after this long is not converging, and cutting it
	// short costs little: an aborted cycle writes nothing, and the sources it
	// never reached are simply rechecked next cycle.
	maxCycleBudget = 2 * time.Hour
	// cycleBudgetNum/cycleBudgetDen: a cycle may spend three quarters of its
	// schedule interval. The remaining quarter is slack, so an overrunning
	// cycle is aborted and its tick skipped rather than every later tick
	// queueing behind it.
	cycleBudgetNum = 3
	cycleBudgetDen = 4
)

// cycleBudget bounds one cycle to its share of the schedule interval, so a crawl
// whose fan-out grows can never outrun the next tick.
func cycleBudget(interval time.Duration) time.Duration {
	return min(interval*cycleBudgetNum/cycleBudgetDen, maxCycleBudget)
}

// nextDaily returns the next instant at hour:min (local) strictly after now.
func nextDaily(now time.Time, hour, minute int) time.Time {
	n := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !n.After(now) {
		n = n.Add(oneDay)
	}
	return n
}

// RunOnce performs one crawl+classify+merge cycle. The private overlay is only
// rewritten when the managed source set actually changes, so an unchanged cycle
// triggers no reload, and never when the cycle failed to learn anything or wants
// to delete a large slice of the corpus at once (see recheckResult.dark and
// allowShrink).
func (c *Crawler) RunOnce(ctx context.Context) {
	pf, err := loadPrivate(c.opts.PrivatePath)
	if err != nil {
		c.logger.Error().Err(err).Str("path", c.opts.PrivatePath).Msg("read private.yaml failed")
		return
	}

	// Discover live subscription URLs by scanning the channel repost graph,
	// seeded by configured channels plus remembered productive ones. scan
	// records freshly productive channels into st; stale ones are pruned.
	st := loadState(c.opts.StatePath, c.logger)
	live, inline := c.scan(ctx, &st)
	if ctx.Err() != nil {
		c.logger.Warn().Str("reason", abortReason(ctx.Err())).
			Msg("cycle aborted mid-scan; skipping state save and merge")
		return
	}
	st.prune(time.Now().Add(-c.opts.StateTTL))
	c.persistState(st)
	// Captured before recheckManaged folds revived URLs into live.
	discovered := len(live)
	c.logger.Info().Int("discovered", discovered).Int("productive", len(st.Productive)).
		Msg("live subscriptions discovered")

	rr := c.recheckManaged(ctx, pf, live)
	if ctx.Err() != nil {
		c.logger.Warn().Str("reason", abortReason(ctx.Err())).
			Msg("cycle aborted mid-recheck; skipping merge")
		return
	}
	prune := c.opts.Prune
	if prune && rr.dark(discovered) {
		c.logger.Error().Int("rechecked", rr.checked).
			Msg("cycle discovered no subscription and revived none; treating as crawler-side fault, pruning nothing")
		prune = false
	}
	// A cycle takes minutes to hours; re-load private.yaml so the merge sees
	// concurrent hand edits instead of clobbering them with a stale snapshot.
	pf, err = loadPrivate(c.opts.PrivatePath)
	if err != nil {
		c.logger.Error().Err(err).Str("path", c.opts.PrivatePath).Msg("re-read private.yaml failed")
		return
	}
	before := managedCount(pf)
	// Re-read alongside the merge, not at cycle start: a cycle runs for
	// minutes to hours and an operator retiring a source expects the block to
	// take effect on the write it is racing.
	blocked := blockedSet(loadChannels(c.opts.ChannelsPath, c.logger).Blocked)
	next, managed, deleted := c.mergeManaged(pf, live, rr, prune, blocked)
	inlineCount := 0
	if c.opts.InlineEnabled {
		if s, n, ok := c.buildInlineSource(inline); ok {
			next = append(next, s)
			inlineCount = n
		}
	}
	if sameSources(pf.Subscriptions.Sources, next) {
		// A merge that reproduces the file exactly proposes no deletion at all,
		// which withdraws any earlier bulk-prune proposal — otherwise six quiet
		// hours would leave that refusal standing to authorize whatever the
		// next fault comes up with.
		c.withdrawBulkPrune(&st)
		c.logger.Info().Int("managed", len(managed)).
			Int("rechecked", rr.checked).Int("revived", rr.revived).Msg("no change")
		return
	}
	if !c.allowShrink(&st, before, deleted, len(managed)) {
		return
	}
	pf.Subscriptions.Sources = next
	if writeErr := writePrivate(c.opts.PrivatePath, pf); writeErr != nil {
		c.logger.Error().Err(writeErr).Msg("write private.yaml failed")
		return
	}
	// rechecked/revived ride the terminal lines because nothing else reports
	// them on a healthy cycle: rr.checked reaches a log only on the dark-cycle
	// fault above, and without them the recheck population — the one that
	// produces prunes — is invisible whenever it works.
	c.logger.Info().Int("managed", len(managed)).Int("inline", inlineCount).Int("total", len(next)).
		Int("rechecked", rr.checked).Int("revived", rr.revived).Msg("private.yaml updated")
}

// abortReason names why a cycle stopped early: its own budget (see cycleBudget)
// or process shutdown. Either way the merge is skipped — a partial cycle is
// never evidence that a source died.
func abortReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "cycle budget exhausted"
	}
	return "shutdown"
}

// recheckResult is what the recheck phase learned about the managed set.
type recheckResult struct {
	managedURL map[string]bool // every managed URL in the cycle-start snapshot
	unknown    map[string]bool // rechecked URLs whose verdict is undetermined
	checked    int             // URLs rechecked (not rediscovered in a channel)
	revived    int             // rechecked URLs that answered as a live subscription
}

// dark reports a cycle that learned nothing anywhere: no live subscription found
// in any channel and not one recheck answering live, with rechecks actually
// attempted. That shape is a crawler-side fault — egress down, DNS interception,
// a proxy answering every request with an error page — not every panel dying in
// the same hour, so such a cycle must prune nothing.
func (r recheckResult) dark(discovered int) bool {
	return discovered == 0 && r.checked > 0 && r.revived == 0
}

// recheckManaged records the URLs of existing managed sources and re-classifies
// the ones not rediscovered this cycle, marking any still live in live so prune
// can drop the dead ones. Only a definitive answer prunes — a gone status
// (404/410/451) or an origin-advertised expiry. A recheck that failed on
// transport (DNS, timeout, TLS, read), answered a transient status (403, 408,
// 425, 429, any 5xx) or answered 2xx with no node at all lands in unknown, since
// none of those show the subscription is gone; see classifyAll.
func (c *Crawler) recheckManaged(ctx context.Context, pf privateFile, live map[string]string) recheckResult {
	rr := recheckResult{managedURL: map[string]bool{}}
	var pending []string
	for _, s := range pf.Subscriptions.Sources {
		if !strings.HasPrefix(s.Name, managedPrefix) {
			continue
		}
		if s.Body != "" {
			// Inline (Body) sources have an empty URL and are regenerated
			// fresh each cycle; never recheck or classify them.
			continue
		}
		rr.managedURL[s.URL] = true
		if _, ok := live[s.URL]; !ok {
			pending = append(pending, s.URL)
		}
	}
	// nil recorder: these are existing managed sources, not candidates this
	// cycle discovered, and their fate is already reported by checked/revived
	// and the prune decision.
	relive, unknown := c.classifyAll(ctx, pending, nil, "")
	rr.unknown = unknown
	rr.checked = len(pending)
	rr.revived = len(relive)
	for u := range relive {
		// Revived by recheck, not seen in a channel this cycle: origin unknown.
		if _, ok := live[u]; !ok {
			live[u] = ""
		}
	}
	return rr
}

// mergeManaged combines the retained hand-added sources with the current managed
// set (deduped and sorted by name) and returns the full next source list, the
// managed subset for logging, and deleted: the cycle-start managed URLs this
// merge drops, in sorted order, which is both the size and the identity of the
// deletion the prune floor rules on (see allowShrink). Which of the not-live
// managed URLs survive is retainManaged's decision.
//
// blocked is the operator's retirement list (channels.yaml `blocked:`). It is
// applied here, the single funnel every candidate URL passes through, so a
// blocked source cannot re-enter from the re-loaded file, from rediscovery in
// a channel, or from a recheck reviving it.
func (c *Crawler) mergeManaged(pf privateFile, live map[string]string, rr recheckResult, prune bool, blocked map[string]struct{}) (kept, managed []source, deleted []string) {
	all := map[string]struct{}{}
	existing := map[string]string{}
	for _, s := range pf.Subscriptions.Sources {
		switch {
		case s.Body != "":
			// Inline (Body) sources are regenerated fresh each cycle by
			// RunOnce; drop the stale one here so it is not double-counted.
			continue
		case strings.HasPrefix(s.Name, managedPrefix):
			all[s.URL] = struct{}{}
			existing[s.URL] = s.Name
		default:
			kept = append(kept, s)
		}
	}
	for u := range live {
		all[u] = struct{}{}
	}
	// Hand-added names occupy the namespace too; a channel-attributed name may
	// never collide with them.
	used := map[string]bool{}
	for _, s := range kept {
		used[s.Name] = true
	}
	// Deterministic naming order so a hash-fallback on collision is stable
	// across cycles (map iteration order is randomized).
	urls := make([]string, 0, len(all))
	for u := range all {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	blockedDropped := 0
	for _, u := range urls {
		_, wasManaged := existing[u]
		if _, isBlocked := blocked[u]; isBlocked {
			// An operator block is a deliberate removal, so it is deliberately
			// NOT counted as a deletion: the prune floor exists to catch the
			// crawler deleting sources by mistake, not to throttle the operator.
			blockedDropped++
			continue
		}
		if !retainManaged(u, live, rr, prune) {
			if wasManaged {
				deleted = append(deleted, u)
			}
			continue
		}
		// Last gate before the URL reaches private.yaml: the crawler fetches
		// through an unrestricted client, but config.Load runs every source
		// through ValidatePublicHTTPSURL and fails the WHOLE config on one bad
		// entry — which is fatal at service startup, not just at reload.
		if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(u)); err != nil {
			if wasManaged {
				deleted = append(deleted, u)
			}
			c.logger.Warn().Err(err).Str("url", u).Msg("dropping managed source the service would refuse to load")
			continue
		}
		name := sourceName(u, existing[u], live[u], used)
		used[name] = true
		managed = append(managed, source{Name: name, URL: u})
	}
	if blockedDropped > 0 {
		c.logger.Info().Int("blocked", blockedDropped).Msg("managed sources withheld by the channels.yaml blocked list")
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].Name < managed[j].Name })
	kept = append(kept, managed...)
	return kept, managed, deleted
}

// retainManaged decides whether one managed URL survives the cycle. Only a
// definitive not-live verdict prunes: a source whose status came back
// undetermined, or one that appeared in the re-loaded file mid-cycle and was
// therefore never checked, is kept. prune is the cycle's decision, not
// opts.Prune — a cycle that learned nothing prunes nothing.
func retainManaged(u string, live map[string]string, rr recheckResult, prune bool) bool {
	if _, isLive := live[u]; isLive {
		return true
	}
	if !rr.managedURL[u] {
		// Absent from the cycle-start snapshot: added mid-cycle behind the
		// crawler's back, so it was never a candidate for a liveness verdict.
		return true
	}
	return rr.unknown[u] || !prune
}

const (
	// bulkPruneMinDrop and bulkPrunePercent are the cycle-level prune floor: a
	// single cycle may not delete more than bulkPrunePercent of the managed
	// corpus unless the drop is also small in absolute terms. BOTH thresholds
	// must be exceeded to trip it, so ordinary churn flows through — 20 expired
	// panels out of 150 clear the count but not the ratio, and a young corpus of
	// 12 losing 4 clears the ratio but not the count — while a correlated fault
	// condemning a third of the corpus in one hour is stopped.
	// The corpus is unrecoverable if lost: there is no backup, and a dropped URL
	// only returns if the same channel post is rescraped.
	bulkPruneMinDrop = 10
	bulkPrunePercent = 30
	percentScale     = 100
)

// managedCount counts the managed URL sources of a private file: the corpus the
// prune floor protects. Inline (Body) sources are regenerated every cycle and
// hand-added ones are never touched, so neither belongs in the comparison.
func managedCount(pf privateFile) int {
	n := 0
	for _, s := range pf.Subscriptions.Sources {
		if s.Body == "" && strings.HasPrefix(s.Name, managedPrefix) {
			n++
		}
	}
	return n
}

// allowShrink applies the prune floor to a proposed write, persisting and
// logging a refusal. False means the caller must skip the write and leave the
// previous private.yaml intact; a drop that big is only carried out once a later
// cycle re-proposes the same deletion (state.confirmBulkPrune), so no single
// cycle can wipe the harvested corpus while a genuine mass expiry still
// converges.
//
// deleted names the cycle-start managed sources this cycle would remove; its
// length is the drop, NOT the net change in corpus size. Netting them off would
// let each newly discovered source buy back one deletion: a cycle that condemns
// 75 of 206 while finding 15 nets to 60, which slips under a 30% floor and
// deletes 36% of the corpus. The URLs themselves matter too — they fingerprint
// the proposal, so a refusal recorded for one set cannot authorize another.
func (c *Crawler) allowShrink(st *state, before int, deleted []string, after int) bool {
	drop := len(deleted)
	if drop <= bulkPruneMinDrop || drop*percentScale <= before*bulkPrunePercent {
		c.withdrawBulkPrune(st)
		return true
	}
	allow, changed := st.confirmBulkPrune(time.Now(), deleted)
	if changed {
		c.persistState(*st)
	}
	if !allow {
		c.logger.Error().Int("managed_before", before).Int("managed_after", after).
			Int("deleted", drop).Str("confirm_after", bulkPruneConfirmAfter.String()).
			Msg("bulk prune floor tripped; private.yaml left unchanged pending confirmation by a later cycle")
	}
	return allow
}

// withdrawBulkPrune forgets any refused bulk-prune proposal and persists the
// withdrawal. Every cycle that reaches the merge and proposes no bulk deletion
// must call it: leaving the record on disk is what lets a refusal survive hours
// of quiet cycles and then rubber-stamp whatever the next fault proposes.
func (c *Crawler) withdrawBulkPrune(st *state) {
	if st.clearBulkPrune() {
		c.persistState(*st)
	}
}

func (c *Crawler) persistState(st state) {
	if err := saveState(c.opts.StatePath, st); err != nil {
		c.logger.Warn().Err(err).Msg("save crawler state failed")
	}
}

// sourceName picks the managed name for url u. An already-attributed name is
// kept verbatim (renames churn private.yaml, restart the stable worker, and
// relabel published nodes). A legacy hash-only name upgrades to the
// channel-attributed form tg-<slug>-<sha6> the first time the URL is seen in a
// channel; on a (never observed, ~2^-24) name collision or when no channel is
// known, the legacy hash form is used so the name stays valid and unique.
func sourceName(u, existingName, channel string, used map[string]bool) string {
	if existingName != "" && !legacyNameRe.MatchString(existingName) {
		return existingName
	}
	if slug := channelSlug(channel); slug != "" {
		sum := sha256.Sum256([]byte(u))
		cand := managedPrefix + slug + "-" + hex.EncodeToString(sum[:])[:6]
		if !used[cand] {
			return cand
		}
	}
	if existingName != "" {
		return existingName
	}
	return managedName(u)
}

// channelSlug normalizes a Telegram channel slug into the config source-name
// alphabet (^[a-z0-9-]+$): lowercase, "_" folded to "-", anything else dropped,
// runs of "-" collapsed, capped at 24 bytes. Empty result means the channel is
// unusable for attribution.
func channelSlug(ch string) string {
	const maxSlug = 24
	b := make([]byte, 0, len(ch))
	for i := 0; i < len(ch) && len(b) < maxSlug; i++ {
		r := ch[i]
		switch {
		case r >= 'A' && r <= 'Z':
			b = append(b, r+'a'-'A')
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b = append(b, r)
		case r == '_' || r == '-':
			if len(b) > 0 && b[len(b)-1] != '-' {
				b = append(b, '-')
			}
		}
	}
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	return string(b)
}

// classifyAll classifies urls with bounded concurrency, returning the set that
// classify as live and the set whose verdict is undetermined. A URL lands in
// neither set only when it is provably no longer a subscription: the origin
// answered a definitively gone status, or it advertised an expiry already in the
// past. Everything else — transport failure, transient status, oversized body,
// or a 2xx carrying no node — is undetermined, because callers prune on the
// absence of both verdicts.
//
// Every URL that does not end up live is reported to rej, attributed to channel.
// rej is nil for the recheck pass: those URLs are existing managed sources, not
// candidates discovered this cycle, and folding them into the discovery summary
// would mix two populations under one total.
func (c *Crawler) classifyAll(ctx context.Context, urls []string, rej *rejects, channel string) (live, unknown map[string]bool) {
	live = make(map[string]bool, len(urls))
	unknown = map[string]bool{}
	var mu sync.Mutex
	sem := make(chan struct{}, classifyConcurrency)
	var wg sync.WaitGroup
	for _, u := range urls {
		sem <- struct{}{} // acquire before spawning so goroutines stay bounded
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, classifyTimeout)
			defer cancel()
			res, err := c.classifyFn(cctx, c.httpClient, fetch.SubscriptionURL(u))
			// rej is not concurrency-safe; recording under the same mutex that
			// guards the result maps is what makes the fan-out safe.
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				var statusErr *classify.StatusError
				if !errors.As(err, &statusErr) || !statusErr.Gone() {
					// Transport failure, or a status that proves nothing about
					// the subscription: the verdict stays undetermined.
					unknown[u] = true
				}
				if statusErr != nil {
					rej.record(channel, u, rejectStatus, statusErr.Code, nil)
				} else {
					rej.record(channel, u, rejectFetch, 0, err)
				}
				return
			}
			switch res.Reason() {
			case classify.ReasonLive:
				live[u] = true
			case classify.ReasonNodeless:
				// A 2xx carrying no proxy-scheme node is not proof of death.
				// The parser counts only real node schemes (PP-07), so a
				// captive portal, a Cloudflare interstitial, a panel login page
				// and a JSON error object all arrive here with zero nodes —
				// the same "host is alive and told us nothing" condition that,
				// delivered as 403 or 503, is already undetermined. A panel
				// whose pool is momentarily empty looks identical.
				unknown[u] = true
				rej.record(channel, u, rejectNodeless, 0, nil)
			case classify.ReasonExpired:
				// The only 2xx answer that proves the subscription is over,
				// because the origin itself advertised the expiry. It joins
				// neither set, so it prunes.
				rej.record(channel, u, rejectExpired, 0, nil)
			}
		}(u)
	}
	wg.Wait()
	return live, unknown
}

// extractURLs returns every https URL in an already-unescaped HTML page,
// stripped of trailing punctuation. Links appear both in href attributes and as
// plain text inside <pre> blocks, so it scans the whole page.
func extractURLs(page string) []string {
	matches := urlRe.FindAllString(page, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.TrimRight(m, trimSet))
	}
	return out
}

// extractInlineNodes returns every raw proxy URI (vless://, vmess://, ss://,
// ssr://, trojan://, tuic://, hysteria://, hysteria2://, hy2://, anytls://,
// mierus://) pasted directly in a channel page, stripped of trailing
// punctuation. Unlike extractURLs these are node URIs, not subscription links,
// and the caller has already HTML-unescaped page.
func extractInlineNodes(page string) []string {
	matches := inlineRe.FindAllString(page, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.TrimRight(m, trimSet))
	}
	return out
}

// buildInlineSource parses the raw inline URIs collected this cycle into nodes,
// dedupes them by lowercased "server:port" (first wins, mirroring stable.Merge),
// caps the survivors to opts.InlineMax, and packs the kept node URIs into a
// single base64 Body under the managed "tg-inline" source. It returns ok=false
// when no usable inline node was found.
func (c *Crawler) buildInlineSource(uris []string) (source, int, bool) {
	seen := make(map[string]struct{}, len(uris))
	var kept []string
	subscription.Parse([]byte(strings.Join(uris, "\n")), func(n subscription.Node) bool {
		if n.Server == "" || n.Port == "" {
			return true
		}
		key := strings.ToLower(n.Server) + ":" + n.Port
		if _, dup := seen[key]; dup {
			return true
		}
		seen[key] = struct{}{}
		kept = append(kept, n.Raw)
		return c.opts.InlineMax <= 0 || len(kept) < c.opts.InlineMax
	})
	if len(kept) == 0 {
		return source{}, 0, false
	}
	body := base64.StdEncoding.EncodeToString([]byte(strings.Join(kept, "\n")))
	return source{Name: managedPrefix + "inline", Body: body}, len(kept), true
}

// pageCursor returns the smallest message id on a t.me/s page, used as the
// ?before= cursor for the next older page.
func pageCursor(page string) string {
	best := ""
	for _, m := range cursorRe.FindAllStringSubmatch(page, -1) {
		id := m[1]
		if best == "" || less(id, best) {
			best = id
		}
	}
	return best
}

// candidate reports whether a URL is worth fetching: not obvious Telegram noise
// and a URL the service itself would accept. The IP guard matters even though
// the crawler's own client is unrestricted — a harvested literal-private-IP
// source is one config.Load rejects, and channel content is third-party
// controlled.
//
// A false verdict carries the gate that produced it, and the validator's own
// error where there is one, so the caller can log a candidate's fate instead of
// dropping it silently. The gates and their order are unchanged: which URLs pass
// is exactly what it was.
//
// The fmt.Errorf wraps are eager: both are built before record knows whether the
// line survives dedupe or the cap. Kept deliberately — only invalid-url pays
// them, that reason is rare, and moving a wrap into record means handing back a
// bare external error and buying a wrapcheck suppression. What they cost is
// BenchmarkCandidate against BenchmarkCandidateUnwrapped, which is this function
// without them: not one number, since %w re-renders the wrapped message and the
// cost scales with its text.
//
// The accept path was ALLOCATION-identical to 26c8fe2's — which is what was
// measured and all that argument needs — as BenchmarkCandidate/accept against
// BenchmarkCandidatePreReason, whose body is that commit's. It is now cheaper
// than both: raw is parsed once here and the *url.URL handed to the validator,
// where every earlier shape had the validator parse the same string again. That
// second parse is gone, not moved: go tool objdump finds one CALL net/url.Parse
// in fetch.ValidatePublicHTTPSURL and none in the parsed entry point this calls.
// It is NOT instruction-identical either — go tool nm -size puts candidate at
// 413 B against candidatePreReason's 124 B, and its frame is SUBQ $0x50 against
// that body's SUBQ $0x10 — which is the word the review round that this branch's
// feature commit squashed declined the wrap finding with, and the word was
// wrong, since an allocation counter cannot establish it. (That round's own sha
// is gone — the squash rewrote it — which is why it is named by position here.)
func candidate(raw string) (bool, rejectReason, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, rejectInvalidURL, fmt.Errorf("parse candidate url: %w", err)
	}
	if isNoiseHost(u.Hostname()) {
		return false, rejectNoiseHost, nil
	}
	if err = fetch.ValidatePublicParsedHTTPSURL(u); err != nil {
		return false, rejectInvalidURL, fmt.Errorf("validate candidate url: %w", err)
	}
	return true, "", nil
}

// isNoiseHost matches hosts that never serve subscriptions (Telegram itself, its
// media CDN, and the login-widget host every discussion embed loads a script
// from), so they are skipped before the fetch.
func isNoiseHost(host string) bool {
	host = strings.ToLower(host)
	switch host {
	case "t.me", "telegram.org", "www.telegram.org", "telegram.me", "telegram.dog", "oauth.tg.dev":
		return true
	}
	return host == "telesco.pe" || strings.HasSuffix(host, ".telesco.pe")
}

func managedName(u string) string {
	sum := sha256.Sum256([]byte(u))
	return managedPrefix + hex.EncodeToString(sum[:])[:10]
}

func loadPrivate(path string) (privateFile, error) {
	var pf privateFile
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pf, nil
		}
		return pf, fmt.Errorf("read private.yaml: %w", err)
	}
	if unmarshalErr := yaml.Unmarshal(b, &pf); unmarshalErr != nil {
		return pf, fmt.Errorf("unmarshal private.yaml: %w", unmarshalErr)
	}
	return pf, nil
}

// sourceNameRe mirrors config's source-name alphabet; a name outside it makes
// the service reject the whole config.
var sourceNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// validatePrivate re-checks the whole list against the rules the consumer
// applies in config.SubscriptionsConfig.Validate (name alphabet, unique names,
// public https URL unless the source carries an inline Body). Keep the two in
// sync: one bad entry fails config.Load for the entire config, which is fatal at
// service startup, so refusing to write is always the better outcome — including
// when the offending source was hand-added.
func validatePrivate(pf privateFile) error {
	seen := make(map[string]struct{}, len(pf.Subscriptions.Sources))
	for _, s := range pf.Subscriptions.Sources {
		if !sourceNameRe.MatchString(s.Name) {
			return fmt.Errorf("invalid source name %q", s.Name)
		}
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("duplicate source name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
		if strings.TrimSpace(s.Body) != "" {
			continue
		}
		if err := fetch.ValidatePublicHTTPSURL(fetch.SubscriptionURL(s.URL)); err != nil {
			return fmt.Errorf("source %s: %w", s.Name, err)
		}
	}
	return nil
}

func writePrivate(path string, pf privateFile) error {
	if err := validatePrivate(pf); err != nil {
		return fmt.Errorf("private.yaml would be unloadable: %w", err)
	}
	b, err := yaml.Marshal(pf)
	if err != nil {
		return fmt.Errorf("marshal private.yaml: %w", err)
	}
	return writeFileAtomic(path, b, privateFileMode)
}

// privateFileMode keeps private.yaml world-readable: the service reads it
// under another uid.
const privateFileMode os.FileMode = 0o644

// writeFileAtomic writes b to path via a same-directory temp file that is
// fsynced before the rename, so a crash mid-write never leaves a truncated
// file behind. The temp file is removed when any step after its creation fails.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open temp: %w", err)
	}
	if _, writeErr := f.Write(b); writeErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp: %w", writeErr)
	}
	if syncErr := f.Sync(); syncErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync temp: %w", syncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp: %w", closeErr)
	}
	if renameErr := os.Rename(tmp, path); renameErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", renameErr)
	}
	return nil
}

func sameSources(a, b []source) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[source]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// less compares two decimal message-id strings numerically without allocating
// an int when lengths differ.
func less(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
