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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"domains.lst/sub-preprocessor/internal/classify"
	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/subscription"
)

const (
	classifyConcurrency = 8
	// defaultProbeTimeout is the liveness probe's bound when the embedder never
	// threaded a budget (Options.FetchTimeout). It mirrors config's own
	// defaultFetchTimeout, the worker's per-source subscription fetch budget:
	// the crawler reads no config.yaml, so the mirror is what keeps a probe
	// from outliving a fetch the worker could actually perform. An operator
	// who tunes subscriptions.fetch.timeout must set Options.FetchTimeout to
	// match, or the crawler judges sources on a budget the worker no longer
	// uses.
	defaultProbeTimeout = 3 * time.Second
	fetchTimeout        = 20 * time.Second
	maxPageBytes        = 8 << 20 // cap on bytes read from a single channel page
	oneDay              = 24 * time.Hour
)

var trimSet = ".,;:!?)]}'\""

// legacyManagedPrefix is what every crawler-minted name wore before ownership
// became a field. It is the adoption trigger for UNMARKED entries only: a marked
// one has already been adopted, and a slug may itself begin with it.
const legacyManagedPrefix = "tg-"

// unattributedNameHex sizes the whole of a name minted with no origin at all,
// which is what managedName builds and unattributedNameRe matches.
const unattributedNameHex = 10

// unattributedNameRe matches managedName's form. Such a name carries no origin,
// so it is the one existing name the mint may replace once a channel is known.
// The literal repeats unattributedNameHex: composing the pattern from the
// constant costs an import and reads worse than the shape it describes.
var unattributedNameRe = regexp.MustCompile("^[0-9a-f]{10}$")

// Options configures a crawl run.
type Options struct {
	Channels     []string // static seed channels (from CRAWL_CHANNELS); merged with ChannelsPath
	ChannelsPath string   // YAML file of seed channels, re-read each cycle for hot-reload
	PrivatePath  string
	CuratedPaths []string // YAML files of hand-written sources (CRAWL_CURATED), read for names the mint must avoid — never crawled
	Pages        int      // t.me/s pages (~20 msgs each) to walk back per configured seed channel
	Prune        bool     // drop managed sources proven no longer live
	MaxDepth     int      // repost-recursion depth; 0 = only seed channels (no recursion)
	MaxChannels  int      // cap on discovered (non-seed) channels per cycle; 0 = defaultMaxDiscovered
	// DeadTTL is how long a URL judged DEFINITIVELY not live (gone status or
	// origin-advertised expiry) is remembered and not fetched again on the
	// discovery path. The zero value disables the memory outright — no
	// recording and no withholding — so an embedder gets the feature only by
	// asking for it; main supplies the 720h default from CRAWL_DEAD_TTL.
	DeadTTL       time.Duration // remember a dead subscription URL for this long
	StatePath     string        // persisted productive-channel memory; empty disables persistence
	StateTTL      time.Duration // drop a productive channel from memory after this long without a live sub
	InlineEnabled bool          // harvest raw inline proxy URIs pasted in channel messages
	InlineMax     int           // cap on inline nodes kept per cycle (first N after dedup); 0 keeps none, negative is uncapped
	// FetchTimeout is the per-source budget the worker applies to a
	// subscription fetch (subscriptions.fetch.timeout, shipped at 3s). The
	// liveness probe gives a source no more than that budget, so a source the
	// worker can never fetch is not admitted as live and converges through the
	// retirement window instead of being kept forever on verdicts the worker
	// cannot reproduce. Zero or negative, the value embedders that do not
	// thread it leave, falls back to defaultProbeTimeout — an unbounded probe
	// is the worse failure.
	FetchTimeout time.Duration
}

type source struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url,omitempty"`
	Body string `yaml:"body,omitempty"`
	// Feed is the channel slug the entry was minted from, carried as data
	// because a name cannot be parsed back into slug plus discriminator: the
	// slug may itself end in a dash-separated digit run (channelSlug folds "_"
	// to "-", so free_vpn_2026 becomes free-vpn-2026).
	Feed string `yaml:"feed,omitempty"`
	// Managed is the whole of the crawler's write authority over an entry.
	// Absent means hand-added and sheltered, so an operator who forgets the
	// field is safe by default; omitempty is what keeps it out of their entry.
	Managed bool `yaml:"managed,omitempty"`
	// HWID mirrors config.SubscriptionSource.HWID and is never authored by
	// the crawler, which has no source of one: the shelter re-emits a
	// hand-added entry verbatim and the mint carries prev's value, so an
	// operator-set hwid on either survives the cycle. It exists because every
	// cycle rewrites private.yaml in full from this struct, so a field missing
	// here is stripped off the file, reverting the source to the placeholder
	// the panel serves without the header.
	HWID string `yaml:"hwid,omitempty"`
}

type privateFile struct {
	Subscriptions struct {
		Sources []source `yaml:"sources"`
	} `yaml:"subscriptions"`
	// adopted reports that the load migrated legacy names (adoptLegacyNames).
	// The merge compares its output against this snapshot, which the migration
	// already rewrote, so without this flag a cycle that had nothing else to do
	// would call the file unchanged and never write the migration out.
	adopted bool
}

// Crawler runs crawl cycles.
type Crawler struct {
	opts       Options
	client     fetchClient
	httpClient *http.Client
	// classifyFn classifies one URL; hwid is the managed entry's x-hwid value,
	// empty when the URL has no source config to carry one (the discovery
	// pass). The liveness verdict must describe what the worker's own fetch of
	// that source would see, and the worker sends the hwid (checker.go), so
	// judging a hwid-carrying managed entry without it reads a device-limited
	// panel's placeholder as nodeless.
	classifyFn func(ctx context.Context, client *http.Client, u fetch.SubscriptionURL, hwid string) (classify.Result, error)
	logger     zerolog.Logger
	// running serializes crawl cycles: a triggered cycle and a scheduled tick
	// never overlap. TryLock lets a scheduled tick (or HTTP trigger) skip
	// cleanly when a cycle is already in flight instead of queueing behind it.
	running sync.Mutex
	// scheduleBudget is the per-cycle wall-clock bound the schedule loop
	// publishes (see Run, RunDaily) for the HTTP trigger to share; zero means
	// no schedule loop is running and the trigger falls back to the daily cap.
	scheduleBudget atomic.Int64
}

// fetchClient fetches a channel page; an interface so tests can avoid the network.
type fetchClient interface {
	page(ctx context.Context, u string) (string, error)
}

// httpFetcher fetches a page with the crawler's unrestricted client (no IP
// guard, so t.me via the fake-ip tunnel is reachable) and the shared rotating
// client identity (fetch.UserAgent), one pool pick per request.
type httpFetcher struct{ client *http.Client }

func (f httpFetcher) page(ctx context.Context, u string) (string, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", fetch.UserAgent())
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
	return &Crawler{opts: opts, client: httpFetcher{client: client}, httpClient: client, classifyFn: classify.URLWithHWID, logger: logger}
}

// Run executes a cycle immediately, then every interval until ctx is done.
// Cycles go through runGuarded, so a tick that fires while a cycle (scheduled
// or HTTP-triggered) is still running is skipped rather than overlapped, and
// each cycle is bounded by cycleBudget so it cannot starve the schedule.
func (c *Crawler) Run(ctx context.Context, interval time.Duration) {
	budget := cycleBudget(interval)
	// Published before the first cycle so an HTTP trigger firing mid-run
	// shares the schedule's budget (see triggerBudget).
	c.scheduleBudget.Store(int64(budget))
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
	// Published before the first cycle so an HTTP trigger firing while the
	// daily loop waits shares the schedule's budget (see triggerBudget).
	c.scheduleBudget.Store(int64(budget))
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
			// An HTTP-triggered cycle that straddles the scheduled instant must
			// not consume the day's run: wait it out (it is bounded by the
			// shared per-cycle budget) instead of skipping to the next 24h.
			c.runDailyGuarded(ctx, budget)
		}
	}
}

// runDailyGuarded runs the day's scheduled cycle, retrying while another cycle
// (an HTTP trigger) holds the lock, until it has run or ctx ended. Run's ticker
// can drop a collided tick because the next interval comes soon; a dropped
// daily tick would be gone for 24h, so the daily schedule waits instead. Each
// retry is a cheap TryLock; dailyRetryAfter only paces the attempts.
func (c *Crawler) runDailyGuarded(ctx context.Context, budget time.Duration) {
	waited := false
	for {
		if c.runGuarded(ctx, budget) {
			return
		}
		if !waited {
			waited = true
			c.logger.Warn().Msg("previous crawl cycle still running; waiting for it before the scheduled run")
		}
		t := time.NewTimer(dailyRetryAfter)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// dailyRetryAfter paces runDailyGuarded's attempts when a triggered cycle holds
// the lock at the daily instant. A var, not a const, so the retry test can
// shorten it; tests mutating it must not call t.Parallel.
var dailyRetryAfter = time.Minute

// runBounded wraps ctx in the cycle's wall-clock budget and runs one cycle
// under it. The budget exists for schedules long enough that a fraction of the
// interval bounds nothing (see cycleBudget); a triggered cycle shares it so an
// HTTP trigger cannot start what a scheduled tick would have aborted.
func (c *Crawler) runBounded(ctx context.Context, budget time.Duration) {
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	c.RunOnce(ctx)
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
	c.runBounded(ctx, budget)
	return true
}

// triggerBudget bounds a POST /crawl cycle: the schedule loop's own per-cycle
// budget when one is running, so a triggered cycle cannot start what a
// scheduled tick would have aborted, and cycleBudget(oneDay) when the crawler
// is trigger-only (no Run or RunDaily has published a budget).
func (c *Crawler) triggerBudget() time.Duration {
	if b := c.scheduleBudget.Load(); b > 0 {
		return time.Duration(b)
	}
	return cycleBudget(oneDay)
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

// loadCycle reads the cycle's managed overlay and remembered state, refusing
// the cycle when the overlay holds no managed sources while the state remembers
// a managed corpus. A missing private.yaml loads as an empty corpus, and so does
// a present-but-empty one (a truncation, `> private.yaml`, a failed external
// write) — rewriting either from this cycle's discoveries would silently
// replace the whole harvested corpus and wipe the liveness streaks st.Managed
// still holds, since ageManaged deletes every record whose URL the cycle-start
// file no longer carries. Only an empty corpus beside an empty memory (a
// genuine first cycle) proceeds. RunOnce re-runs this same content test on the
// overlay's second read, before the merge, because the file can change between
// the two loads. ok is false when the cycle must stop.
func (c *Crawler) loadCycle() (pf privateFile, st state, ok bool) {
	var err error
	pf, _, err = loadPrivate(c.opts.PrivatePath)
	if err != nil {
		c.logger.Error().Err(err).Str("path", c.opts.PrivatePath).Msg("read private.yaml failed")
		return privateFile{}, state{}, false
	}
	// The state load comes first because the guard reads st.Managed. managedCount
	// is the same test on CONTENT the prune floor divides by: it counts managed
	// URL sources, which is what st.Managed remembers, so a file holding only
	// hand-added entries is as empty a corpus as a missing one.
	st = loadState(c.opts.StatePath, c.logger)
	if managedCount(pf) == 0 && len(st.Managed) > 0 {
		c.logger.Error().Int("managed", len(st.Managed)).Str("path", c.opts.PrivatePath).
			Msg("private.yaml holds no managed sources while state remembers a managed corpus; refusing the cycle rather than rewriting the corpus from this cycle's discoveries — restore the file, or delete the crawler state to rebuild the corpus from scratch")
		return privateFile{}, st, false
	}
	return pf, st, true
}

// RunOnce performs one crawl+classify+merge cycle. The private overlay is only
// rewritten when the managed source set actually changes, so an unchanged cycle
// triggers no reload, and never when the cycle failed to learn anything or wants
// to delete a large slice of the corpus at once (see recheckResult.dark and
// allowShrink).
func (c *Crawler) RunOnce(ctx context.Context) {
	pf, st, ready := c.loadCycle()
	if !ready {
		return
	}

	// Discover live subscription URLs by scanning the channel repost graph,
	// seeded by configured channels plus remembered productive ones. scan
	// records freshly productive channels into st; stale ones are pruned.
	// pruneDead runs before the scan so a record expiring mid-cycle is judged
	// by its own rule: expired means classifiable again, this cycle.
	st.pruneDead(time.Now())
	live, inline, discDead := c.scan(ctx, &st, c.deadSet(&st))
	if ctx.Err() != nil {
		c.logger.Warn().Str("reason", abortReason(ctx.Err())).
			Msg("cycle aborted mid-scan; skipping state save and merge")
		return
	}
	st.prune(time.Now().Add(-c.opts.StateTTL))
	// Saved before the recheck, which is network-bound over the whole managed
	// corpus: an abort in that window would otherwise discard this cycle's
	// productive-channel discoveries and the expiries pruneDead just dropped.
	c.persistState(&st)
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
	dark := rr.dark(discovered)
	// A dark cycle is a crawler-side fault (see dark): its not-live answers
	// are no evidence about any source, so it records and clears nothing —
	// the same reasoning that stops it pruning.
	if dark {
		c.logger.Error().Int("rechecked", rr.checked).
			Msg("cycle discovered no subscription and revived none; treating as crawler-side fault, pruning nothing and advancing no retirement streak")
	} else {
		c.rememberVerdicts(&st, discDead, rr.dead, live)
		rr.stale = c.trackLiveness(&st, rr.managedURL, live)
	}
	prune := c.opts.Prune && !dark
	// A cycle takes minutes to hours; re-load private.yaml so the merge sees
	// concurrent hand edits instead of clobbering them with a stale snapshot.
	pf, _, err := loadPrivate(c.opts.PrivatePath)
	if err != nil {
		c.logger.Error().Err(err).Str("path", c.opts.PrivatePath).Msg("re-read private.yaml failed")
		return
	}
	// loadCycle's guard has to hold on this second read too: the file can
	// change between the two loads, and an overlay emptied mid-cycle —
	// truncated by the same editor save or failed external write loadCycle
	// names, or deleted outright — must not be rebuilt from this cycle's
	// discoveries. A vanished file reads as empty, so one content test covers
	// both shapes.
	before := managedCount(pf)
	if before == 0 && len(st.Managed) > 0 {
		c.logger.Error().Int("managed", len(st.Managed)).Str("path", c.opts.PrivatePath).
			Msg("private.yaml holds no managed sources while state remembers a managed corpus; refusing the cycle rather than rewriting the corpus from this cycle's discoveries — restore the file, or delete the crawler state to rebuild the corpus from scratch")
		return
	}
	// The deny list is read alongside the merge, not at cycle start, for the
	// same reason private.yaml is re-read here: an operator broadening the
	// curated set expects the withholding to apply to the write it races.
	denied := curatedURLs(c.opts.CuratedPaths, c.logger)
	next, managed, deleted, curated := c.mergeManaged(pf, live, rr, prune, denied)
	inlineCount := 0
	if c.opts.InlineEnabled {
		if s, n, ok := c.buildInlineSource(inline); ok {
			// A hand-added entry is never renamed, and the duplicate would make
			// validatePrivate refuse this write and every later one.
			if slices.ContainsFunc(next, func(e source) bool { return e.Name == s.Name }) {
				c.logger.Warn().Str("source", s.Name).
					Msg("a hand-added entry holds the inline aggregate's name; skipping the inline harvest rather than writing a duplicate")
			} else {
				next = append(next, s)
				inlineCount = n
			}
		}
	}
	// pf.adopted first: the migration rewrote the very snapshot this compares
	// against, so an otherwise idle cycle would call the file unchanged and
	// leave the corpus prefixed forever.
	if !pf.adopted && sameSources(pf.Subscriptions.Sources, next) {
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
		Int("curated", curated).Int("rechecked", rr.checked).Int("revived", rr.revived).Msg("private.yaml updated")
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
	// stale is filled by RunOnce from the persisted streaks, not by the recheck:
	// one cycle's undetermined answer never retires a source, N of them do.
	stale   map[string]bool
	checked int      // URLs rechecked (not rediscovered in a channel)
	revived int      // rechecked URLs that answered as a live subscription
	dead    []string // definitive not-live verdicts this pass, for recordDead
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
//
// The recheck deliberately passes a nil dead set: these URLs are the corpus the
// crawler already owns, so a stale record must not stop it asking whether they
// still serve. That keeps a record refreshable by a live answer only while the
// entry stays in the corpus: a URL this cycle prunes keeps its stamp and stays
// withheld from rediscovery — the discovery pass skips remembered-dead URLs —
// until it expires, which is the DeadTTL's designed re-probe period, not a
// grace. Their definitive verdicts come back in rr.dead.
func (c *Crawler) recheckManaged(ctx context.Context, pf privateFile, live map[string]origin) recheckResult {
	rr := recheckResult{managedURL: map[string]bool{}}
	var pending []string
	var hwids map[string]string
	for _, s := range pf.Subscriptions.Sources {
		// The field, not the name: a minted name is now indistinguishable from
		// a hand-added one, and rechecking someone else's source would hand the
		// prune a verdict on an entry the crawler may not delete.
		if !s.Managed {
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
			// The worker fetches this source with its hwid (checker.go), and a
			// device-limited panel serves a placeholder to a header-less fetch,
			// so the liveness fetch must carry the value or it would condemn a
			// source the worker keeps publishing.
			if s.HWID != "" {
				if hwids == nil {
					hwids = make(map[string]string, len(pending))
				}
				hwids[s.URL] = s.HWID
			}
		}
	}
	// nil recorder, no channel and no ids: these are existing managed sources,
	// not candidates this cycle discovered, and their fate is already reported
	// by checked/revived and the prune decision.
	relive, unknown, dead := c.classifyAll(ctx, pending, nil, nil, "", nil, hwids)
	rr.unknown = unknown
	rr.checked = len(pending)
	rr.revived = len(relive)
	rr.dead = dead
	for u := range relive {
		// Revived by recheck, not seen in a channel this cycle: origin unknown.
		if _, ok := live[u]; !ok {
			live[u] = origin{}
		}
	}
	return rr
}

// mergeManaged combines the retained hand-added sources with the current managed
// set (deduped and sorted by name) and returns the full next source list, the
// managed subset for logging, and deleted: the cycle-start managed URLs this
// merge drops, in sorted order, which is both the size and the identity of the
// deletion the prune floor rules on (see allowShrink), and curated: how many
// names the overlays reserved, which is the only way to see from a log that a
// mis-set CRAWL_CURATED silently disabled the seeding. Which of the not-live
// managed URLs survive is retainManaged's decision.
//
// denied is the curated-source deny list (curatedURLs): any URL listed verbatim
// as subscriptions.sources[].url in a CRAWL_CURATED file is withheld, whatever
// its liveness — the curated entry itself is what the worker fetches. It sits in
// this single funnel every candidate URL passes through, so a withheld source
// cannot re-enter from rediscovery in a channel or from a recheck reviving it.
// A withholding is deliberately NOT counted as a deletion: a first cycle after a
// broadened curated set can withhold many managed mirrors at once, and counting
// them would trip the bulk-prune floor against an operator action rather than a
// crawler mistake — the floor exists to catch the crawler deleting sources by
// accident, not to throttle deliberate curation.
func (c *Crawler) mergeManaged(pf privateFile, live map[string]origin, rr recheckResult, prune bool, denied map[string]struct{}) (kept, managed []source, deleted []string, curated int) {
	all := map[string]struct{}{}
	existing := map[string]source{}
	// Every name the file already holds is spoken for, whoever owns it:
	// <slug>-<postid> is per-POST, so without this a second URL of one post takes
	// the name sourceName then returns verbatim to its incumbent, and the
	// duplicate makes validatePrivate refuse the write on every later cycle too.
	// Curated names too, though they live in files the crawler never writes:
	// validateSources runs over the merged list, so one minted name equal to a
	// curated one refuses the WHOLE config -- fatal at startup, silent at
	// reload, where the live config freezes at the last good version.
	curatedSet := curatedNames(c.opts.CuratedPaths, c.logger, len(pf.Subscriptions.Sources))
	curated = len(curatedSet)
	used := make(map[string]bool, curated+len(pf.Subscriptions.Sources))
	for name := range curatedSet {
		used[name] = true
	}
	// The mint avoids curated names only for names it is CREATING: an existing
	// entry that already holds one keeps it, because sourceName returns an
	// existing name verbatim. That collision is detected here at error level —
	// the merged config is unloadable until one of them is renamed. The
	// curated set stays apart from used so the file's own duplicates, which
	// validatePrivate already refuses on the write, are not misreported as
	// curated collisions.
	curatedCollision := func(s source) {
		if curatedSet[s.Name] {
			c.logger.Error().Str("source", s.Name).Str("path", c.opts.PrivatePath).
				Msg("a managed-overlay entry already holds a curated name; the merged config would be unloadable until one of them is renamed")
		}
	}
	// A sheltered entry's URL is reserved like its name: the entry is already
	// the operator's fetch of that URL, so a live rediscovery of it must not
	// mint a managed duplicate. The minted mirror would carry no hwid of its
	// own (the crawler authors none), and a device-limited panel answers a
	// header-less fetch with its placeholder body — so the mirror would fetch
	// nothing the operator's entry does not already fetch, spending a name and
	// a fetch slot for it; over a hwid-less sheltered entry the byte-identical
	// pair is additionally a list validatePrivate refuses to write, this cycle
	// and every later one.
	var shelteredURLs map[string]struct{}
	for _, s := range pf.Subscriptions.Sources {
		switch {
		case s.Body != "" && s.Managed:
			// Only the crawler's own aggregate is regenerated by RunOnce; drop
			// the stale one so it is not double-counted. Nobody regenerates a
			// hand-added body source, so it falls through to the shelter.
			continue
		// The field is the authority: a name says nothing about who owns the
		// entry, so switching on one would put hand-added sources under the
		// crawler's prune and let its own entries be sheltered by a rename.
		case s.Managed:
			all[s.URL] = struct{}{}
			existing[s.URL] = s
			curatedCollision(s)
			used[s.Name] = true
		default:
			// Sheltered because the field is absent, which is now the whole
			// test: an unmarked entry is nobody's but the operator's.
			kept = append(kept, s)
			curatedCollision(s)
			used[s.Name] = true
			if s.URL != "" {
				if shelteredURLs == nil {
					shelteredURLs = make(map[string]struct{})
				}
				shelteredURLs[s.URL] = struct{}{}
			}
		}
	}
	for u := range live {
		if _, held := shelteredURLs[u]; held {
			continue
		}
		all[u] = struct{}{}
	}
	managed, deleted, deniedDropped := c.mintRetained(all, existing, live, rr, prune, denied, used)
	if deniedDropped > 0 {
		c.logger.Info().Int("denied", deniedDropped).
			Msg("managed sources withheld by the curated-source deny list")
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].Name < managed[j].Name })
	kept = append(kept, managed...)
	return kept, managed, deleted, curated
}

// mintRetained walks the merged URL set (live ∪ the cycle-start managed corpus)
// in sorted order and decides each candidate's fate, returning what it minted,
// the managed URLs it deleted, and how many the curated deny list withheld. all
// is a set: the sort below, not map iteration, is what keeps each candidate's
// ordinal (sourceName) stable across cycles. existing lets the loop tell a
// managed URL the cycle is dropping from a candidate it never owned, and feeds
// the mint the entry being superseded; used is the mint's taken-name set.
func (c *Crawler) mintRetained(all map[string]struct{}, existing map[string]source, live map[string]origin, rr recheckResult, prune bool, denied map[string]struct{}, used map[string]bool) (managed []source, deleted []string, deniedDropped int) {
	urls := make([]string, 0, len(all))
	for u := range all {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	for _, u := range urls {
		_, wasManaged := existing[u]
		if _, isDenied := denied[u]; isDenied {
			deniedDropped++
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
		managed = append(managed, mintSource(u, existing[u], live[u], used))
	}
	return managed, deleted, deniedDropped
}

// mintSource is what one retained URL becomes. prev is the entry the cycle-start
// file held for it, zero for a URL the crawler has never named. The feed follows
// the name, changing exactly when it does, so an entry rediscovered in another
// channel keeps both rather than half of each.
//
// The minted name joins used, so the next URL of the same post cannot take it
// too. That set is mergeManaged's, seeded from the curated overlays and every
// name the cycle-start file held, then filled in sorted-URL order: that order is
// what keeps the choice stable across cycles, deciding which URL of a post gets
// the plain <slug>-<postid> and which sibling gets which ordinal beside it. A
// name is therefore no longer a pure function of its URL but of the set it was
// minted beside, so a corpus rebuilt after private.yaml is lost re-derives names
// that shift wherever the URL set has changed.
func mintSource(u string, prev source, o origin, used map[string]bool) source {
	name := sourceName(u, prev.Name, o, used)
	feed := prev.Feed
	if name != prev.Name {
		feed = channelSlug(o.Slug)
	}
	used[name] = true
	return source{Name: name, URL: u, Feed: feed, Managed: true, HWID: prev.HWID}
}

// retainManaged decides whether one managed URL survives the cycle. A definitive
// not-live verdict prunes at once, and the same verdict's Dead stamp (see
// rememberVerdicts) then withholds the URL from rediscovery for the stamp's
// whole TTL: the prune is not a one-cycle reprieve, and a pruned URL's revival
// needs no edit only after that re-probe period expires. An undetermined verdict
// prunes only after the retirement window has run out on it (state.ageManaged),
// because a panel can serve a placeholder or an empty pool for a cycle. A source
// that appeared in the re-loaded file mid-cycle was never checked and is kept.
// prune is the cycle's decision, not opts.Prune — a cycle that learned nothing
// prunes nothing.
func retainManaged(u string, live map[string]origin, rr recheckResult, prune bool) bool {
	if _, isLive := live[u]; isLive {
		return true
	}
	if !rr.managedURL[u] {
		// Absent from the cycle-start snapshot: added mid-cycle behind the
		// crawler's back, so it was never a candidate for a liveness verdict.
		return true
	}
	if !prune {
		return true
	}
	if rr.unknown[u] {
		return !rr.stale[u]
	}
	return false
}

// rememberVerdicts folds this cycle's DEFINITIVE not-live verdicts into the
// persisted dead memory and forgets the records of URLs that answered live. Both
// verdict slices are stamped at one instant so a cycle's records share an
// expiry, and they stay separate slices rather than being appended: the
// discovery half is scan's own buffer, which nothing else may extend.
//
// A cycle that saw no live subscription anywhere records NOTHING. Its 404s are
// no evidence about any panel, because nothing in it proved the egress works —
// a DNS sinkhole answering 404 for every host produces exactly this shape.
// recheckResult.dark catches that only once the corpus is non-empty (it needs
// rr.checked > 0), so a fresh deploy behind such an egress would otherwise
// condemn every candidate it ever saw for the whole TTL, and the withholding
// would then hide them from the healthy cycles that follow.
func (c *Crawler) rememberVerdicts(st *state, discovered, rechecked []string, live map[string]origin) {
	if len(live) == 0 {
		if len(discovered)+len(rechecked) > 0 {
			c.logger.Warn().Int("verdicts", len(discovered)+len(rechecked)).
				Msg("cycle proved nothing live; not remembering its not-live verdicts")
		}
		return
	}
	now := time.Now()
	changed := st.recordDead(discovered, c.opts.DeadTTL, now)
	if st.recordDead(rechecked, c.opts.DeadTTL, now) {
		changed = true
	}
	if changed {
		c.logger.Info().Int("dead", len(st.Dead)).Msg("remembered dead subscription URLs")
	}
	if st.clearDead(live) {
		c.logger.Info().Msg("cleared dead records for URLs that answered live")
	}
}

// deadSet is the cycle's skip set. A DeadTTL of 0 is the documented off switch,
// and it has to switch off WITHHOLDING as well as recording: otherwise records
// already on disk would go on suppressing fetches that nothing will ever
// re-stamp. They stay on disk, inert, until pruneDead ages them out.
func (c *Crawler) deadSet(st *state) map[string]time.Time {
	if c.opts.DeadTTL <= 0 {
		return nil
	}
	return st.Dead
}

// trackLiveness folds this cycle's liveness verdicts into the persisted streaks
// and returns the managed URLs the retirement window has run out on, logging the
// evidence behind each. It saves the state itself, which is what persists both
// the streaks and the dead records rememberVerdicts stamped just before it: the
// streak is this cycle's only record that these sources were observed at all,
// and every path from the merge on may return without another save.
func (c *Crawler) trackLiveness(st *state, managed map[string]bool, live map[string]origin) map[string]bool {
	now := time.Now()
	stale := st.ageManaged(managed, live, now)
	for u := range stale {
		m := st.Managed[u]
		ev := c.logger.Info().Str("url", u).Int("not_live_cycles", m.NotLiveCycles).
			Str("not_live_for", now.Sub(m.NotLiveSince).Truncate(time.Minute).String())
		if m.LastLiveAt.IsZero() {
			ev.Msg("condemning managed source: no live answer since the crawler began tracking it")
		} else {
			ev.Time("last_live_at", m.LastLiveAt).
				Msg("condemning managed source: was live, then not live for the whole retirement window")
		}
	}
	c.persistState(st)
	return stale
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
// prune floor protects, and the denominator allowShrink's ratio divides by.
// Inline (Body) sources are regenerated every cycle and hand-added ones are
// never touched, so neither belongs in the comparison.
//
// It counts the field: a name test here would read 0 the moment the prefix went,
// and a 0 denominator collapses allowShrink to its absolute arm, so every drop
// over bulkPruneMinDrop would wait for a second cycle to confirm it and
// private.yaml would quietly stop being written in between.
func managedCount(pf privateFile) int {
	n := 0
	for _, s := range pf.Subscriptions.Sources {
		if s.Body == "" && s.Managed {
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
		c.persistState(st)
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
		c.persistState(st)
	}
}

// persistState writes the state file only when the cycle's state has changed
// since the last write: every mutating state method marks st dirty, so a scan
// that learned nothing and a recheck that re-verified everything stop short of
// a whole-file MarshalIndent plus fsync/rename (the file runs past 0.5 MB at
// the Dead cap). saveState keeps its unconditional contract for direct callers
// (tests seeding a file); the dirty gate lives here on the cycle path.
func (c *Crawler) persistState(st *state) {
	if !st.dirty {
		return
	}
	if err := saveState(c.opts.StatePath, *st); err != nil {
		c.logger.Warn().Err(err).Msg("save crawler state failed")
		return
	}
	st.dirty = false
}

// sourceName picks the managed name for url u: <slug>-<postid> where a known
// post's bare stem is free, <slug>-<postid>-N where it is not (a post is a
// container, not a URL), <slug>-N when the channel is known but the post is not,
// and managedName's origin-less hash when the slug is unusable. N is the lowest
// ordinal free in used, counting from 2 where a post id is known — the bare
// <slug>-<postid> is offered to that post's first URL first, so a post whose
// stem is already held starts its own first URL at -2 — and from 1 where none
// is, no bare stem having been offered at all. The walk needs no cap:
// every ordinal it rejects is one distinct name already in used, so it cannot
// run past len(used)+1.
//
// An existing name is kept verbatim, because a rename churns private.yaml and
// relabels every published node (merge.go derives <source>-NNN) and buys
// nothing. The one exception is the bare-hash form, which names no origin: that
// upgrades the first time the URL is seen in a channel. A usable slug always
// yields a free ordinal, so the two arms below the slug block are reached only
// when the slug is empty.
//
// Every candidate is assembled in one stack buffer, wide enough that no ordinal
// can spill it onto the heap: the mint allocates the name it returns and nothing
// else.
func sourceName(u, existingName string, o origin, used map[string]bool) string {
	if existingName != "" && !unattributedNameRe.MatchString(existingName) {
		return existingName
	}
	var buf [maxSlug + 1 + maxPostDigits + 1 + maxOrdinalDigits]byte
	if stem := appendChannelSlug(buf[:0], o.Slug); len(stem) > 0 {
		ordinal := uint64(1)
		if o.Post != 0 {
			stem = strconv.AppendUint(append(stem, '-'), o.Post, decimalBase)
			if !used[string(stem)] {
				return string(stem)
			}
			ordinal = 2
		}
		stem = append(stem, '-')
		for ; ; ordinal++ {
			if cand := strconv.AppendUint(stem, ordinal, decimalBase); !used[string(cand)] {
				return string(cand)
			}
		}
	}
	if existingName != "" {
		return existingName
	}
	return managedName(u)
}

const (
	// maxSlug caps a normalized channel slug.
	maxSlug = 24
	// maxPostDigits is the decimal width of a uint64 message id.
	maxPostDigits = 20
	// maxOrdinalDigits sizes the sibling ordinal, which len(used)+1 bounds and
	// nothing bounds statically. No corpus reaches 20 digits; the spare stack
	// bytes cost nothing.
	maxOrdinalDigits = maxPostDigits
	decimalBase      = 10
)

// channelSlug normalizes a Telegram channel slug into the config source-name
// alphabet (^[a-z0-9-]+$): lowercase, "_" folded to "-", anything else dropped,
// runs of "-" collapsed, capped at maxSlug bytes. Empty result means the
// channel is unusable for attribution.
func channelSlug(ch string) string {
	return string(appendChannelSlug(make([]byte, 0, len(ch)), ch))
}

func appendChannelSlug(dst []byte, ch string) []byte {
	start := len(dst)
	for i := 0; i < len(ch) && len(dst)-start < maxSlug; i++ {
		r := ch[i]
		switch {
		case r >= 'A' && r <= 'Z':
			dst = append(dst, r+'a'-'A')
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			dst = append(dst, r)
		case r == '_' || r == '-':
			if len(dst) > start && dst[len(dst)-1] != '-' {
				dst = append(dst, '-')
			}
		}
	}
	for len(dst) > start && dst[len(dst)-1] == '-' {
		dst = dst[:len(dst)-1]
	}
	return dst
}

// probeBudget is the per-URL wall-clock bound classifyAll applies: the
// embedder's Options.FetchTimeout when threaded (the worker's own per-source
// fetch budget), else defaultProbeTimeout, which mirrors the worker's default
// for exactly the reason Options.FetchTimeout documents.
func (c *Crawler) probeBudget() time.Duration {
	if c.opts.FetchTimeout > 0 {
		return c.opts.FetchTimeout
	}
	return defaultProbeTimeout
}

// hwidFor returns the x-hwid a URL's liveness fetch must carry. hwids is
// classifyAll's per-URL map for the recheck pass; nil (the discovery pass, and
// any URL without an entry) fetches header-less.
func hwidFor(hwids map[string]string, u string) string {
	if hwids == nil {
		return ""
	}
	return hwids[u]
}

// classifyAll classifies urls with bounded concurrency, returning the set that
// classify as live, the set whose verdict is undetermined, and deadOut: the
// URLs that received a DEFINITIVE not-live verdict this call — a gone
// status (404/410/451) or an origin-advertised expiry. Everything else —
// transport failure, transient status (403/408/425/429, any 5xx), oversized
// body, or a 2xx carrying no node — is undetermined, and never reaches deadOut,
// because callers prune on the absence of both live and unknown.
//
// A non-nil dead set is the URLs already remembered dead: they are not fetched
// at all, land in neither returned set, are reported to rej under rejectDead,
// and stay out of deadOut — a record is only re-stamped by a fresh definitive
// verdict, which a skipped URL cannot produce. nil skips nothing; the recheck
// pass passes nil so it can still harvest deadOut for records it must refresh.
//
// hwids carries the managed entry's x-hwid per URL for the recheck pass, whose
// URLs are the corpus the crawler owns; the discovery pass has no hwid for a
// never-seen candidate and passes nil. Every fetch is bounded by the worker's
// own per-source budget (Options.FetchTimeout) so the verdict describes a
// source the worker could actually fetch.
//
// Every URL that does not end up live is reported to rej under the origin it was
// harvested from: one call spans a whole channel, so the slug is that channel's
// and the id is looked up per URL. rej is nil for the recheck pass: those URLs
// are existing managed sources, not candidates discovered this cycle, and
// folding them into the discovery summary would mix two populations under one
// total; slug is empty and posts nil with it, which is the zero origin that
// pass has always recorded.
func (c *Crawler) classifyAll(ctx context.Context, urls []string, dead map[string]time.Time, rej *rejects, slug string, posts map[string]uint64, hwids map[string]string) (live, unknown map[string]bool, deadOut []string) {
	live = make(map[string]bool, len(urls))
	unknown = map[string]bool{}
	perURL := c.probeBudget()
	var mu sync.Mutex
	sem := make(chan struct{}, classifyConcurrency)
	var wg sync.WaitGroup
	for _, u := range urls {
		if _, remembered := dead[u]; remembered {
			// Counted, not logged, and never fetched: see rejects.noteDead.
			mu.Lock()
			rej.noteDead(u)
			mu.Unlock()
			continue
		}
		sem <- struct{}{} // acquire before spawning so goroutines stay bounded
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, perURL)
			defer cancel()
			res, err := c.classifyFn(cctx, c.httpClient, fetch.SubscriptionURL(u), hwidFor(hwids, u))
			// rej is not concurrency-safe; recording under the same mutex that
			// guards the result maps is what makes the fan-out safe.
			mu.Lock()
			defer mu.Unlock()
			o := origin{Slug: slug, Post: posts[u]}
			if err != nil {
				var statusErr *classify.StatusError
				if !errors.As(err, &statusErr) || !statusErr.Gone() {
					// Transport failure, or a status that proves nothing about
					// the subscription: the verdict stays undetermined.
					unknown[u] = true
				} else {
					deadOut = append(deadOut, u)
				}
				if statusErr != nil {
					rej.record(o, u, rejectStatus, statusErr.Code, nil)
				} else {
					rej.record(o, u, rejectFetch, 0, err)
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
				rej.record(o, u, rejectNodeless, 0, nil)
			case classify.ReasonExpired:
				// The only 2xx answer that proves the subscription is over,
				// because the origin itself advertised the expiry. It joins
				// neither set, so it prunes.
				deadOut = append(deadOut, u)
				rej.record(o, u, rejectExpired, 0, nil)
			}
		}(u)
	}
	wg.Wait()
	return live, unknown, deadOut
}

// inlineSourceName is the one managed name that is not minted from a URL: the
// inline source has neither, being an aggregate of node URIs across messages and
// channels, which is also why it carries no Feed.
const inlineSourceName = "inline"

// buildInlineSource parses the raw inline URIs collected this cycle into nodes,
// dedupes them by lowercased "server:port" (first wins, mirroring stable.Merge),
// caps the survivors to opts.InlineMax, and packs the kept node URIs into a
// single base64 Body under the managed inline source. It returns ok=false when
// no usable inline node was found.
func (c *Crawler) buildInlineSource(uris []string) (source, int, bool) {
	// Refused before Parse, not inside it: an append-then-refuse callback
	// strands the first node past the cap it enforces.
	if c.opts.InlineMax == 0 {
		return source{}, 0, false
	}
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
		if c.opts.InlineMax > 0 && len(kept) >= c.opts.InlineMax {
			return false
		}
		return true
	})
	if len(kept) == 0 {
		return source{}, 0, false
	}
	body := base64.StdEncoding.EncodeToString([]byte(strings.Join(kept, "\n")))
	return source{Name: inlineSourceName, Body: body, Managed: true}, len(kept), true
}

// pageCursor returns the smallest message id on a t.me/s page, used as the
// ?before= cursor for the next older page. It hand-reads the
// `data-post="[^"]+/(\d+)"` shape cursorRe used to match: FindAllStringSubmatch
// allocated a pair of slices per data-post attribute on every listing page the
// crawl fetches, for the same attribute nextMessage hand-scans off the
// unescaped text. A refused value resumes inside itself (pos = val), exactly
// where the regexp's next attempt would look; a matched one is re-found only as
// itself, so best cannot change. TestPageCursor holds the scan equal to the
// regexp over the fixtures.
func pageCursor(page string) string {
	best := ""
	for pos := 0; ; {
		i := strings.Index(page[pos:], dataPost)
		if i < 0 {
			return best
		}
		val := pos + i + len(dataPost)
		q := strings.IndexByte(page[val:], '"')
		if q < 0 {
			return best
		}
		if id, ok := cursorID(page[val : val+q]); ok && (best == "" || less(id, best)) {
			best = id
		}
		pos = val
	}
}

// cursorID returns the "<digits>" of a data-post value that ends in "/<digits>"
// — cursorRe's capture of the same attribute. The slash must not open the value
// (the regexp needs one non-quote character before it), and the digit run is
// any width, leading zeros included, because pageCursor compares captures by
// length then byte order rather than numerically. A value with no trailing
// digit run carries no cursor.
func cursorID(value string) (string, bool) {
	i := strings.LastIndexByte(value, '/')
	if i <= 0 {
		return "", false
	}
	id := value[i+1:]
	if id == "" {
		return "", false
	}
	for j := range len(id) {
		if id[j] < '0' || id[j] > '9' {
			return "", false
		}
	}
	return id, true
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

// managedName is the name for a managed source with no origin at all: a bare
// hash of the URL, unique by construction and stable across cycles.
func managedName(u string) string { return urlHash(u, unattributedNameHex) }

// urlHash is the first n (even) hex digits of sha256(u). Only the bytes those
// digits need are encoded, so the result does not hold a 64-byte backing array
// alive for the 8 or 10 digits the reject correlator and the mint pass.
func urlHash(u string, n int) string {
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:n/2])
}

// loadPrivate reads the managed-overlay file and reports whether it EXISTED:
// a missing file is deliberately not an error (a fresh deployment has nothing
// to read yet), but RunOnce must tell "no file" from "a file holding no
// sources" — an absent corpus must never be rewritten from one cycle's
// discoveries alone (see RunOnce).
func loadPrivate(path string) (privateFile, bool, error) {
	var pf privateFile
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pf, false, nil
		}
		return pf, false, fmt.Errorf("read private.yaml: %w", err)
	}
	if unmarshalErr := yaml.Unmarshal(b, &pf); unmarshalErr != nil {
		return pf, true, fmt.Errorf("unmarshal private.yaml: %w", unmarshalErr)
	}
	adoptLegacyNames(&pf)
	return pf, true, nil
}

// curatedFile is any file on CRAWL_CURATED as the crawler sees it: names, and
// no opinion about the keys config.Load owns, so an unknown one is ignored
// rather than rejected. config.yaml carries subscriptions.sources under that
// same key path, so one struct reads both.
type curatedFile struct {
	Subscriptions struct {
		Sources []struct {
			Name string `yaml:"name"`
			URL  string `yaml:"url"`
		} `yaml:"sources"`
	} `yaml:"subscriptions"`
}

// curatedNames is the mint's taken-name set seeded from the curated overlays,
// unioned, with room for that many more (mergeManaged adds the cycle-start
// file's). Nothing in the overlays is crawled; only their names are read. A
// blank entry is skipped and a missing file is normal. A read or parse failure
// warns and the remaining paths are still read: a typo in one file must not
// stop discovery, and the cycle runs the collision risk for that file's names
// instead -- which is why the failure is never silent.
func curatedNames(paths []string, logger zerolog.Logger, room int) map[string]bool {
	var names []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Warn().Err(err).Str("path", path).Msg("read curated sources failed; minting without that file's curated names")
			}
			continue
		}
		var cf curatedFile
		if unmarshalErr := yaml.Unmarshal(b, &cf); unmarshalErr != nil {
			logger.Warn().Err(unmarshalErr).Str("path", path).Msg("unmarshal curated sources failed; minting without that file's curated names")
			continue
		}
		for _, s := range cf.Subscriptions.Sources {
			if s.Name != "" {
				names = append(names, s.Name)
			}
		}
	}
	used := make(map[string]bool, len(names)+room)
	for _, name := range names {
		used[name] = true
	}
	return used
}

// curatedURLs collects every subscription URL the curated overlays carry,
// verbatim: mergeManaged withholds exactly these from the managed corpus, so
// the crawler never mirrors a source the operator already curates by hand. The
// match is verbatim only — a different query spelling is a different URL and a
// different source. Read best-effort like curatedNames, and separately from it
// on purpose: names feed the mint's taken-set, URLs the deny funnel, and the
// two evolve at different rates for files this small that reading them twice
// per cycle costs nothing.
func curatedURLs(paths []string, logger zerolog.Logger) map[string]struct{} {
	var urls []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Warn().Err(err).Str("path", path).Msg("read curated sources failed; denying without that file's curated URLs")
			}
			continue
		}
		var cf curatedFile
		if unmarshalErr := yaml.Unmarshal(b, &cf); unmarshalErr != nil {
			logger.Warn().Err(unmarshalErr).Str("path", path).Msg("unmarshal curated sources failed; denying without that file's curated URLs")
			continue
		}
		for _, s := range cf.Subscriptions.Sources {
			if s.URL != "" {
				urls = append(urls, s.URL)
			}
		}
	}
	denied := make(map[string]struct{}, len(urls))
	for _, u := range urls {
		denied[u] = struct{}{}
	}
	return denied
}

// adoptLegacyNames migrates the corpus off the tg- prefix, once. Its gate is
// the shape behind the prefix, not the prefix (needsAdoption says why), and it
// recovers Feed only where that shape carries a slug. Every phase downstream
// then tests the field alone, and a marked entry is never adopted again.
// Renaming churns private.yaml and relabels every published node with it
// (merge.go derives <source>-NNN); that is this migration's accepted one-time
// cost. It is not the crawler's only rename: sourceName upgrades a bare-hash
// name to the attributed form at the same cost, under its own rule.
//
// The name it lands on comes from adoptedName, which may have to fall back: a
// duplicate would fail validatePrivate, so the crawler would refuse to write the
// file at all, this cycle and every later one. Feed is recovered from the
// stripped name rather than from whatever name won, because a fallback hash
// names no channel and the slug it replaced was never ambiguous.
func adoptLegacyNames(pf *privateFile) {
	if !slices.ContainsFunc(pf.Subscriptions.Sources, needsAdoption) {
		return
	}
	pf.adopted = true
	taken := make(map[string]struct{}, len(pf.Subscriptions.Sources))
	for _, s := range pf.Subscriptions.Sources {
		if !needsAdoption(s) {
			taken[s.Name] = struct{}{}
		}
	}
	for i := range pf.Subscriptions.Sources {
		s := &pf.Subscriptions.Sources[i]
		if !needsAdoption(*s) {
			continue
		}
		stripped := s.Name[len(legacyManagedPrefix):]
		name, ok := adoptedName(*s, stripped, taken)
		if !ok {
			continue
		}
		taken[name] = struct{}{}
		s.Name, s.Feed, s.Managed = name, legacyFeed(stripped), true
	}
}

// adoptedName is the name one legacy entry migrates to; !ok means every form it
// may take is already spoken for.
//
// The strip is the name the corpus keeps. A collision falls to the URL hash,
// per-URL unique and also the one form the mint may replace later, so that entry
// self-heals into <slug>-<postid> on its next rediscovery. Two prefixed entries
// sharing one URL collide there too, and the hash of the name being migrated
// separates them, being distinct wherever the file's names were. Nothing
// guarantees they are — validatePrivate runs on the write, not the read — so an
// entry duplicated outright can exhaust the chain, and is then left as it stands:
// that file already held two identical names and was already unloadable, and a
// name minted from nothing would hide the operator's duplicate rather than fix
// it. The forms are tried in order and only when needed: the corpus is thousands
// of entries and the first form answers for all but a handful. stripped is never
// empty, every shape needsAdoption accepts leaving something behind the prefix.
func adoptedName(s source, stripped string, taken map[string]struct{}) (string, bool) {
	if _, dup := taken[stripped]; !dup {
		return stripped, true
	}
	byURL := managedName(s.URL)
	if _, dup := taken[byURL]; !dup {
		return byURL, true
	}
	byName := urlHash(s.Name, unattributedNameHex)
	if _, dup := taken[byName]; !dup {
		return byName, true
	}
	return "", false
}

// needsAdoption reports that one entry is a pre-cutover mint the migration has
// still to claim. The mark is the record that it was claimed, so a marked entry
// is never read again: channelSlug folds "_" to "-", so the channel tg_vpn slugs
// to tg-vpn and every name minted from it wears a prefix it never carried as
// authority.
//
// The prefix alone cannot be the trigger either, for the same reason from the
// other side: the mint still produces those names, so an unmarked tg-vpn-123 is
// a post-cutover entry whose operator left the field off, and seizing it would
// put a hand-added source under the prune — the one failure this wave exists to
// remove. So the rest of the name must also wear a shape the pre-cutover mint
// actually produced: slug + "-" + 6 hex (legacyFeed), the bare hash
// (unattributedNameRe), or the inline aggregate, which is named and not derived.
// A hand-added name that genuinely wears one of those shapes is adopted too, and
// nothing can tell the two apart: before the cutover that shape WAS the mark.
func needsAdoption(s source) bool {
	if s.Managed || !strings.HasPrefix(s.Name, legacyManagedPrefix) {
		return false
	}
	rest := s.Name[len(legacyManagedPrefix):]
	return rest == inlineSourceName || legacyFeed(rest) != "" || unattributedNameRe.MatchString(rest)
}

// legacyFeed recovers the channel slug from a pre-field managed name, which was
// exactly slug + "-" + 6 lowercase hex with no post segment. That one known tail
// is what makes the strip sound here where a general fold is not: a slug may
// itself end in a dash-separated digit run. A bare-hash name yields "", having
// never named an origin.
func legacyFeed(name string) string {
	const tail = 6
	if len(name) < tail+2 || name[len(name)-tail-1] != '-' {
		return ""
	}
	for i := len(name) - tail; i < len(name); i++ {
		if c := name[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return name[:len(name)-tail-1]
}

// sourceNameRe mirrors config's source-name alphabet; a name outside it makes
// the service reject the whole config.
var sourceNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// validatePrivate re-checks the whole list against the rules the consumer
// applies in config.SubscriptionsConfig.Validate: name alphabet, unique names,
// a public https URL unless the source carries an inline Body, and no two
// sources performing the same fetch — the same trimmed URL under the same
// hwid, the empty value included. Keep all of them in sync: one bad entry
// fails config.Load for the entire config, which is fatal at service startup,
// so refusing to write is always the better outcome — including when the
// offending source was hand-added. The hwid is part of that identity exactly
// as it is in config: the worker sends each source's value as x-hwid
// (checker.go) and a device-limited panel answers differently with and
// without it, so a differing hwid is a distinct fetch whose payload can
// contribute. The crawler never authors the field (source.HWID), so a
// hwid-bearing entry is always the operator's and a pair differing only in
// hwid must be written back verbatim, not refused as a duplicate.
func validatePrivate(pf privateFile) error {
	seen := make(map[string]struct{}, len(pf.Subscriptions.Sources))
	// Fetch identity, not URL identity — the same key config.validateSources
	// uses, so the write gate admits exactly what config.Load admits.
	type fetchKey struct{ url, hwid string }
	urlOwner := make(map[fetchKey]string, len(pf.Subscriptions.Sources))
	for _, s := range pf.Subscriptions.Sources {
		if !sourceNameRe.MatchString(s.Name) {
			return fmt.Errorf("invalid source name %q", s.Name)
		}
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("duplicate source name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
		// Name is not a uniqueness key for the WORK: two names performing the
		// same fetch — same URL, same hwid — fetch the same payload twice
		// every cycle while Merge's first-source-wins dedupe keeps the later
		// one's nodes out of the list. Adoption can produce this shape from a
		// pre-cutover file (two legacy entries sharing a URL migrate to
		// distinct names over it), so the write must refuse it rather than
		// brick the next boot. A Body source carries no URL and is skipped, as
		// in config.
		if url := strings.TrimSpace(s.URL); url != "" {
			key := fetchKey{url: url, hwid: s.HWID}
			if owner, dup := urlOwner[key]; dup {
				return fmt.Errorf("source %s: url %s is already fetched as %s with the same hwid %q; sources sharing a url AND hwid fetch the same payload twice and the later one contributes no node", s.Name, url, owner, s.HWID)
			}
			urlOwner[key] = s.Name
		}
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
	seen := make(map[source]int, len(a))
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

func keys[V any](m map[string]V) []string {
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
