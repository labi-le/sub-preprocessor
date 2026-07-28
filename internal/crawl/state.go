package crawl

import (
	"encoding/json"
	"fmt"
	"os"
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

// state is the crawler's persistent memory across cycles: which channels proved
// productive (they become depth-0 seeds until they go stale past the TTL), and
// whether a cycle already asked to bulk-prune the managed corpus.
type state struct {
	Productive map[string]channelState `json:"productive"`
	// BulkPruneAt is when a cycle first proposed deleting a large slice of the
	// managed corpus and was refused; see confirmBulkPrune.
	BulkPruneAt time.Time `json:"bulk_prune_at,omitzero"`
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

// bulkPruneConfirmAfter is how long a refused bulk prune must stand before a
// later cycle may carry it out. Six hours outlasts the faults that fabricate a
// mass-death verdict (tunnel restarts, provider outages, a captive portal
// answering 404 for everything) yet still lets a genuine mass expiry converge
// the same day.
const bulkPruneConfirmAfter = 6 * time.Hour

// confirmBulkPrune reports whether a cycle may delete a large slice of the
// managed corpus. The first such cycle is refused and remembered; only a cycle
// at least bulkPruneConfirmAfter later is honoured, and honouring it consumes
// the record so the next mass deletion must earn its own confirmation. With no
// state file (StatePath empty) nothing is remembered and every bulk prune is
// refused — the safe direction.
func (s *state) confirmBulkPrune(now time.Time) bool {
	if s.BulkPruneAt.IsZero() {
		s.BulkPruneAt = now
		return false
	}
	if now.Before(s.BulkPruneAt.Add(bulkPruneConfirmAfter)) {
		return false
	}
	s.BulkPruneAt = time.Time{}
	return true
}

// clearBulkPrune forgets a pending proposal, so an old refusal can never
// authorize a later, unrelated mass deletion.
func (s *state) clearBulkPrune() { s.BulkPruneAt = time.Time{} }

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
