package crawl

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/rs/zerolog"
)

// channelState records when a channel last proved productive (yielded a live
// subscription), so productivity survives days when its recent pages happen to
// carry no live sub.
type channelState struct {
	FirstSeen time.Time `json:"first_seen"`
	LastSubAt time.Time `json:"last_sub_at"`
}

// managedState is one managed source's liveness history. LastLiveAt zero means
// the crawler has never yet seen this URL serve nodes — "no history" and "was
// live, then stopped" are different verdicts and the retirement log says which.
// NotLiveSince anchors the current not-live streak and NotLiveCycles counts the
// cycles in it; a live answer clears both. See ageManaged.
type managedState struct {
	LastLiveAt    time.Time `json:"last_live_at,omitzero"`
	NotLiveSince  time.Time `json:"not_live_since,omitzero"`
	NotLiveCycles int       `json:"not_live_cycles,omitempty"`
}

// state is the crawler's persistent memory across cycles: which channels proved
// productive (they become depth-0 seeds until they go stale past the TTL), how
// long each managed source has been failing to serve nodes, and which bulk-prune
// proposal a cycle already made and was refused.
type state struct {
	Productive map[string]channelState `json:"productive"`
	// Managed is keyed by managed source URL and bounded by the corpus, not by
	// time: ageManaged drops any record whose URL private.yaml no longer holds.
	Managed map[string]managedState `json:"managed,omitempty"`
	// BulkPruneAt and BulkPruneURLs are the refused proposal: when a cycle
	// first asked to delete a large slice of the managed corpus, and the sorted
	// set of URLs it asked to delete. The URL set is half the record — a bare
	// timestamp lets a refusal recorded for one deletion authorize a completely
	// different one made later. See confirmBulkPrune.
	BulkPruneAt   time.Time `json:"bulk_prune_at,omitzero"`
	BulkPruneURLs []string  `json:"bulk_prune_urls,omitempty"`
	// Dead is keyed by subscription URL judged DEFINITIVELY not live (a gone
	// status or an origin-advertised expiry), holding when the record expires.
	// A remembered URL is not FETCHED again while the record stands, on the
	// discovery path — the recheck deliberately still fetches the corpus it
	// owns. Bounded twice: by the record's own expiry (pruneDead) and by
	// maxDead (recordDead).
	Dead map[string]time.Time `json:"dead,omitempty"`
	// loadFailed marks state that stands in for a file loadState could not
	// read or parse. saveState refuses to write it: the real file may hold
	// weeks of productive-channel memory that nothing can reconstruct, and a
	// transient EACCES must not turn into permanent amnesia.
	loadFailed bool
}

// record marks a channel productive as of now, preserving its first-seen time.
func (s *state) record(ch string, now time.Time) {
	if s.Productive == nil {
		s.Productive = map[string]channelState{}
	}
	e := s.Productive[ch]
	if e.FirstSeen.IsZero() {
		e.FirstSeen = now
	}
	e.LastSubAt = now
	s.Productive[ch] = e
}

// maxProductive caps the remembered-channel memory. Every productive channel
// becomes a seed, and every seed's discoveries can promote themselves to seeds
// too, so an uncapped map makes each cycle cost more than the last — the TTL
// alone brakes nothing, since one live sub a month keeps a channel forever.
// Keeping the most recently productive N bounds that feedback loop.
const maxProductive = 200

// prune drops channels whose last productive moment is before cutoff, then caps
// the memory at maxProductive, keeping the most recently productive.
func (s *state) prune(cutoff time.Time) {
	for ch, e := range s.Productive {
		if e.LastSubAt.Before(cutoff) {
			delete(s.Productive, ch)
		}
	}
	if len(s.Productive) <= maxProductive {
		return
	}
	slugs := make([]string, 0, len(s.Productive))
	for ch := range s.Productive {
		slugs = append(slugs, ch)
	}
	// Most recently productive first, slug breaking ties, so the survivors are
	// the same set on every cycle (map iteration order is randomized).
	sort.Slice(slugs, func(i, j int) bool {
		a, b := s.Productive[slugs[i]].LastSubAt, s.Productive[slugs[j]].LastSubAt
		if !a.Equal(b) {
			return a.After(b)
		}
		return slugs[i] < slugs[j]
	})
	for _, ch := range slugs[maxProductive:] {
		delete(s.Productive, ch)
	}
}

// maxDead caps the remembered-dead memory. Dead is the one persisted map whose
// intake nothing else bounds: Productive is capped at maxProductive and Managed
// by the corpus itself (ageManaged drops a record whose URL private.yaml no
// longer holds), while Dead takes every gone link the channels advertise — one
// cycle stamps whatever its pages carried, over up to defaultMaxDiscovered
// channels, and holds it for the whole TTL. The file cost scales with URL
// length, not just the count: at the ~66-byte URLs this corpus mostly carries
// 5000 records is ~545 KB, and at the 150-200-byte panel URLs the deleted
// blocked list was full of it is nearer 1 MB — that whole file is rewritten on
// every cycle that changes it.
const maxDead = 5000

// recordDead stamps urls as dead until now+ttl, refreshing entries already on
// file: every renewed definitive verdict buys the URL a fresh full window,
// which is what keeps a permanently gone panel out of every later cycle.
// Blank strings are skipped, and a non-positive ttl records nothing — that is
// the documented off switch (Options.DeadTTL), not a fallback.
//
// The cap is applied here rather than in pruneDead alone: pruneDead runs before
// the scan, so a cap there is one cycle late and the cycle that overshoots
// still writes the oversized map.
func (s *state) recordDead(urls []string, ttl time.Duration, now time.Time) (changed bool) {
	if ttl <= 0 {
		return false
	}
	until := now.Add(ttl)
	for _, u := range urls {
		if u == "" {
			continue
		}
		if s.Dead == nil {
			s.Dead = make(map[string]time.Time, len(urls))
		}
		if e, ok := s.Dead[u]; ok && e.Equal(until) {
			continue
		}
		s.Dead[u] = until
		changed = true
	}
	return s.capDead() || changed
}

// capDead evicts the soonest-expiring records once the memory is over maxDead,
// keeping the freshest stamps: those are the URLs most recently proven gone and
// so the ones a repost is likeliest to offer again. Every URL a cycle stamps
// shares one expiry (rememberVerdicts stamps at a single instant), so among one
// cycle's own records the URL tiebreak decides, and an overshooting cycle
// evicts its own newest records in reverse alphabetical order — deterministic,
// which is the property that matters against randomized map iteration.
//
// The early return keeps the sort off every cycle that fits, as prune does.
func (s *state) capDead() (changed bool) {
	if len(s.Dead) <= maxDead {
		return false
	}
	urls := make([]string, 0, len(s.Dead))
	for u := range s.Dead {
		urls = append(urls, u)
	}
	sort.Slice(urls, func(i, j int) bool {
		if a, b := s.Dead[urls[i]], s.Dead[urls[j]]; !a.Equal(b) {
			return a.After(b)
		}
		return urls[i] < urls[j]
	})
	for _, u := range urls[maxDead:] {
		delete(s.Dead, u)
	}
	return true
}

// pruneDead drops expired records; expiry itself is what admits a URL back into
// classification. Expiry alone does NOT bound the map — its population is every
// gone link the channels advertise inside a TTL window, not the corpus — so the
// bound is recordDead's cap.
func (s *state) pruneDead(now time.Time) (changed bool) {
	for u, until := range s.Dead {
		if !until.After(now) {
			delete(s.Dead, u)
			changed = true
		}
	}
	return changed
}

// clearDead forgets the records of URLs that answered live: the verdict
// outranks any stamp, however fresh. It reads the cycle's live map directly, as
// ageManaged does, rather than a key-set copy of it.
func (s *state) clearDead(live map[string]origin) (changed bool) {
	for u := range live {
		if _, ok := s.Dead[u]; ok {
			delete(s.Dead, u)
			changed = true
		}
	}
	return changed
}

const (
	// staleRetireAfter and staleRetireCycles are the retirement window: a managed
	// source is retired only once it has answered as something other than a live
	// subscription on staleRetireCycles consecutive cycles AND for staleRetireAfter
	// of wall clock since the first of them. Both are required — the duration alone
	// would let a crawler resuming after days of downtime retire a source on a
	// single bad answer, and the count alone can be burned through in minutes by
	// the CRAWL_HTTP trigger, which runs cycles back to back.
	//
	// 24h outlasts a nightly panel maintenance window, a day-long WAF block and a
	// momentarily empty pool, and it is one full rotation of the 24h-validity links
	// this rule exists for: one that has served nothing for a whole period is not
	// coming back. Six independent fetches spread over at least that day cannot all
	// be the same momentary fault.
	//
	// At the shipped hourly interval a daily-rotating seed therefore leaves at most
	// two stale entries in the corpus at once (dead ~24h after harvest, retired
	// ~24h later), against unbounded accrual before — one entry per day forever,
	// each costing two classify fetches per cycle.
	staleRetireAfter  = 24 * time.Hour
	staleRetireCycles = 6
)

// ageManaged folds one cycle's liveness verdicts into the per-URL streaks and
// returns the managed URLs whose streak has run past the retirement window.
// managed is the cycle-start managed snapshot — every URL that got a verdict this
// cycle — and live is every URL seen serving nodes, whether rediscovered in a
// channel or revived by a recheck.
//
// A missing record means "no history yet", never "stale since the zero time": a
// streak is anchored at the observation that opens it, so the first cycle after
// this bookkeeping ships grandfathers the whole corpus instead of condemning all
// of it at once. With no state file nothing is remembered and nothing is ever
// retired — the safe direction, as with confirmBulkPrune.
//
// Callers must skip a cycle that learned nothing (recheckResult.dark): its
// not-live answers are a crawler-side fault, not evidence about any source.
func (s *state) ageManaged(managed map[string]bool, live map[string]origin, now time.Time) (stale map[string]bool) {
	for u := range s.Managed {
		if !managed[u] {
			delete(s.Managed, u)
		}
	}
	if s.Managed == nil && len(managed) > 0 {
		s.Managed = make(map[string]managedState, len(managed))
	}
	for u := range managed {
		if _, isLive := live[u]; isLive {
			s.Managed[u] = managedState{LastLiveAt: now}
			continue
		}
		m := s.Managed[u]
		if m.NotLiveSince.IsZero() {
			m.NotLiveSince = now
		}
		m.NotLiveCycles++
		s.Managed[u] = m
		if m.NotLiveCycles >= staleRetireCycles && !now.Before(m.NotLiveSince.Add(staleRetireAfter)) {
			if stale == nil {
				stale = make(map[string]bool)
			}
			stale[u] = true
		}
	}
	return stale
}

const (
	// bulkPruneConfirmAfter is how long a refused bulk prune must stand before a
	// later cycle may carry it out. Six hours outlasts the faults that fabricate
	// a mass-death verdict (tunnel restarts, provider outages, a captive portal
	// answering 404 for everything) yet still lets a genuine mass expiry converge
	// the same day.
	bulkPruneConfirmAfter = 6 * time.Hour

	// bulkPruneRecordTTL bounds how long a refusal stays confirmable. Cycles run
	// hourly, so a real mass expiry re-proposes itself on the first cycle past
	// the six-hour mark; a record still unconfirmed a day later means every
	// cycle in between either found those sources alive or never reached the
	// merge, and consenting then would be consenting on day-old evidence. Past
	// the TTL the proposal counts as brand new: refused, and re-armed.
	bulkPruneRecordTTL = 24 * time.Hour

	// bulkPruneOverlap is how much of the refused set, in percent, a later cycle
	// must re-propose for the record to authorize it. Demanding an identical set
	// would never converge on a real mass expiry — one source recovering, or one
	// more dying, between the two cycles would restart the wait forever. Asking
	// only that the new proposal contain the old one would let a refusal covering
	// 12 sources authorize deleting 200. So the test is symmetric on the larger
	// set: neither proposal may differ from the other by more than a fifth.
	bulkPruneOverlap = 80
)

// confirmBulkPrune reports whether a cycle may delete the managed URLs in
// condemned (which must be sorted), and whether the record changed and has to
// be persisted.
//
// The first proposal is refused and remembered. Only a cycle that re-proposes
// substantially the same deletion (sameProposal), between bulkPruneConfirmAfter
// and bulkPruneRecordTTL after that refusal, is honoured — and honouring it
// consumes the record, so the next mass deletion earns its own confirmation.
// Anything else is itself a first proposal: refused, with the record replaced by
// it. With no state file (StatePath empty) nothing is remembered and every bulk
// prune is refused — the safe direction.
func (s *state) confirmBulkPrune(now time.Time, condemned []string) (allow, changed bool) {
	if s.BulkPruneAt.IsZero() ||
		now.After(s.BulkPruneAt.Add(bulkPruneRecordTTL)) ||
		!sameProposal(s.BulkPruneURLs, condemned) {
		s.BulkPruneAt = now
		s.BulkPruneURLs = slices.Clone(condemned)
		return false, true
	}
	if now.Before(s.BulkPruneAt.Add(bulkPruneConfirmAfter)) {
		// The record already describes this proposal, so its timestamp stays
		// put: re-arming on every hourly cycle would push the deadline out
		// forever and a genuine mass expiry would never converge. The recorded
		// URL set is frozen for the same reason — refreshing it to each cycle's
		// slightly different set would let the proposal drift arbitrarily far
		// from the one that was actually refused.
		return false, false
	}
	s.BulkPruneAt, s.BulkPruneURLs = time.Time{}, nil
	return true, true
}

// sameProposal reports whether proposed re-proposes substantially the recorded
// refused set (bulkPruneOverlap). Both slices must be sorted, which makes the
// intersection a merge walk that allocates nothing.
func sameProposal(recorded, proposed []string) bool {
	if len(recorded) == 0 || len(proposed) == 0 {
		return false
	}
	common := 0
	for i, j := 0, 0; i < len(recorded) && j < len(proposed); {
		switch {
		case recorded[i] == proposed[j]:
			common++
			i++
			j++
		case recorded[i] < proposed[j]:
			i++
		default:
			j++
		}
	}
	return common*percentScale >= max(len(recorded), len(proposed))*bulkPruneOverlap
}

// clearBulkPrune forgets a pending proposal, reporting whether there was one to
// forget. A cycle that reaches the merge and proposes no bulk deletion has
// withdrawn any earlier one, so an old refusal can never be left standing to
// authorize a later, unrelated mass deletion.
func (s *state) clearBulkPrune() (changed bool) {
	if s.BulkPruneAt.IsZero() && s.BulkPruneURLs == nil {
		return false
	}
	s.BulkPruneAt, s.BulkPruneURLs = time.Time{}, nil
	return true
}

// seeds returns the persisted productive channel slugs.
func (s *state) seeds() []string {
	out := make([]string, 0, len(s.Productive))
	for ch := range s.Productive {
		out = append(out, ch)
	}
	return out
}

// loadState reads the state file. An empty path or a missing file yields empty
// state and is normal (first cycle, or no state configured).
//
// A read or unmarshal failure is not normal and is not silently absorbed: the
// file holds up to StateTTL of productive-channel memory, every entry of which
// is also a depth-0 seed, so losing it shrinks the crawl surface to the
// configured channels alone. Such a failure is logged and the returned state
// is marked loadFailed, which makes saveState leave the file alone for the
// rest of the cycle — a truncated file or an EACCES on the shared volume then
// costs one degraded cycle instead of destroying the memory for good. A
// genuinely corrupt file must be deleted by hand.
func loadState(path string, logger zerolog.Logger) state {
	st := state{Productive: map[string]channelState{}}
	if path == "" {
		return st
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Error().Err(err).Str("path", path).
				Msg("read crawler state failed; crawling without remembered channels and leaving the file untouched")
			st.loadFailed = true
		}
		return st
	}
	if unmarshalErr := json.Unmarshal(b, &st); unmarshalErr != nil {
		// Unmarshal may have filled part of st before failing; discard it
		// rather than crawl on a half-decoded map.
		logger.Error().Err(unmarshalErr).Str("path", path).
			Msg("crawler state file is malformed; leaving it untouched, delete it to start over")
		return state{Productive: map[string]channelState{}, loadFailed: true}
	}
	if st.Productive == nil {
		st.Productive = map[string]channelState{}
	}
	return st
}

// stateFileMode: the crawler state is private to the crawler uid.
const stateFileMode os.FileMode = 0o600

// saveState writes the state file atomically (fsynced temp + rename). A no-op
// when path is empty or when the state is the stand-in for a file that could
// not be read this cycle (see state.loadFailed).
func saveState(path string, st state) error {
	if path == "" || st.loadFailed {
		return nil
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	return writeFileAtomic(path, b, stateFileMode)
}
