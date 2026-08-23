package crawl //nolint:testpackage // drives prune against unexported state fixtures

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestProductivePoolIsOneLRUAcrossKinds pins that '<slug>/<topic>' keys share
// the single most-recently-productive pool with channel keys: a forum yielding
// in many topics outcompetes stale channels for slots, and that exposure is a
// recorded decision (forum-agreement item 7 note), not an accident this test
// would silently bless — it fails if the sharing ever becomes per-kind.
func TestProductivePoolIsOneLRUAcrossKinds(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := state{Productive: map[string]channelState{}}
	for i := range maxProductive {
		st.record(fmt.Sprintf("chan%03d", i), now.Add(-time.Hour))
	}
	for i := range 41 {
		st.record(fmt.Sprintf("bigforum/%d", 100+i), now)
	}
	if len(st.Productive) != maxProductive+41 {
		t.Fatalf("seeded %d entries, want %d", len(st.Productive), maxProductive+41)
	}

	st.prune(now.Add(-2 * time.Hour))

	if len(st.Productive) != maxProductive {
		t.Errorf("prune kept %d entries, want the %d cap", len(st.Productive), maxProductive)
	}
	var topics, channels int
	for ch := range st.Productive {
		if strings.HasPrefix(ch, "bigforum/") {
			topics++
		} else {
			channels++
		}
	}
	if topics != 41 || channels != maxProductive-41 {
		t.Errorf("pool after prune = %d topics + %d channels; freshest topic keys must evict stale channel keys under one shared cap", topics, channels)
	}
}
