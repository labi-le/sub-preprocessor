package crawl

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// channelState records when a channel last proved productive (yielded a live
// subscription), so productivity survives days when its recent pages happen to
// carry no live sub.
type channelState struct {
	FirstSeen time.Time `json:"first_seen"`
	LastSubAt time.Time `json:"last_sub_at"`
}

// state is the crawler's persistent memory of productive channels. Every
// productive channel becomes a permanent seed (crawled at depth 0 and always
// expanded) until it goes stale past the TTL.
type state struct {
	Productive map[string]channelState `json:"productive"`
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

// prune drops channels whose last productive moment is before cutoff.
func (s *state) prune(cutoff time.Time) {
	for ch, e := range s.Productive {
		if e.LastSubAt.Before(cutoff) {
			delete(s.Productive, ch)
		}
	}
}

// seeds returns the persisted productive channel slugs.
func (s *state) seeds() []string {
	out := make([]string, 0, len(s.Productive))
	for ch := range s.Productive {
		out = append(out, ch)
	}
	return out
}

// loadState reads the state file. A missing file or empty path yields empty
// state; a malformed file is treated as empty (best-effort memory, never fatal).
func loadState(path string) state {
	st := state{Productive: map[string]channelState{}}
	if path == "" {
		return st
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	if st.Productive == nil {
		st.Productive = map[string]channelState{}
	}
	return st
}

// saveState writes the state file atomically (temp + rename). A no-op when path
// is empty.
func saveState(path string, st state) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp := path + ".tmp"
	if writeErr := os.WriteFile(tmp, b, 0o600); writeErr != nil {
		return fmt.Errorf("write temp: %w", writeErr)
	}
	if renameErr := os.Rename(tmp, path); renameErr != nil {
		return fmt.Errorf("rename: %w", renameErr)
	}
	return nil
}
