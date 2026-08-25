package crawl //nolint:testpackage // exercises unexported state helpers

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestRecordDeadStampsAndRefreshes pins the two halves of a record's lifetime:
// a first verdict opens the window, and a renewed one buys a fresh full window
// rather than letting the original expiry run out under a panel that is still
// answering 404.
func TestRecordDeadStampsAndRefreshes(t *testing.T) {
	t.Parallel()

	const u = "https://gone.example/sub"
	now := time.Now()
	var st state

	if changed := st.recordDead([]string{u, ""}, time.Hour, now); !changed {
		t.Fatal("a first verdict must report a change: nothing else persists the state file")
	}
	if got, want := st.Dead[u], now.Add(time.Hour); !got.Equal(want) {
		t.Errorf("expiry = %v, want %v", got, want)
	}
	if len(st.Dead) != 1 {
		t.Errorf("Dead = %v, want the blank URL skipped", st.Dead)
	}

	later := now.Add(30 * time.Minute)
	if changed := st.recordDead([]string{u}, time.Hour, later); !changed {
		t.Error("a renewed verdict must re-stamp, not be swallowed as a no-op")
	}
	if got, want := st.Dead[u], later.Add(time.Hour); !got.Equal(want) {
		t.Errorf("refreshed expiry = %v, want %v", got, want)
	}
	if changed := st.recordDead([]string{u}, time.Hour, later); changed {
		t.Error("re-recording the same verdict at the same instant changes nothing and must not force a write")
	}
}

// TestRecordDeadRespectsDisabledTTL: DeadTTL 0 is the documented off switch, so
// the recorder must write nothing at all rather than stamping records that
// expire immediately and churn the state file every cycle.
func TestRecordDeadRespectsDisabledTTL(t *testing.T) {
	t.Parallel()

	var st state
	if changed := st.recordDead([]string{"https://gone.example/sub"}, 0, time.Now()); changed {
		t.Error("a non-positive TTL must record nothing")
	}
	if len(st.Dead) != 0 {
		t.Errorf("Dead = %v, want empty", st.Dead)
	}
}

// TestPruneDeadDropsOnlyExpired pins the boundary: expiry is what re-admits a
// URL to classification, so an entry exactly at now is spent and one a moment
// past it is not.
func TestPruneDeadDropsOnlyExpired(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := state{Dead: map[string]time.Time{
		"https://expired.example/sub": now.Add(-time.Second),
		"https://exact.example/sub":   now,
		"https://live.example/sub":    now.Add(time.Minute),
	}}

	if changed := st.pruneDead(now); !changed {
		t.Fatal("dropping an expired record is a change the state file must record")
	}
	if _, ok := st.Dead["https://live.example/sub"]; !ok || len(st.Dead) != 1 {
		t.Errorf("Dead = %v, want only the unexpired record", st.Dead)
	}
	if changed := st.pruneDead(now); changed {
		t.Error("a second prune with nothing expired must report no change")
	}
}

// TestClearDeadForgetsLiveAnswers: a live answer outranks any stamp, however
// fresh. The recheck fetches the corpus it owns whatever the memory holds, so a
// managed URL that answers live again is cleared without waiting for expiry.
func TestClearDeadForgetsLiveAnswers(t *testing.T) {
	t.Parallel()

	const revived = "https://revived.example/sub"
	st := state{Dead: map[string]time.Time{
		revived:                    time.Now().Add(time.Hour),
		"https://gone.example/sub": time.Now().Add(time.Hour),
	}}

	if changed := st.clearDead(map[string]origin{revived: {}}); !changed {
		t.Fatal("clearing a held record is a change")
	}
	if _, ok := st.Dead[revived]; ok {
		t.Errorf("Dead = %v, want the revived URL forgotten", st.Dead)
	}
	if len(st.Dead) != 1 {
		t.Errorf("Dead = %v, want the untouched record kept", st.Dead)
	}
	if changed := st.clearDead(map[string]origin{"https://never.example/sub": {}}); changed {
		t.Error("clearing a URL that holds no record must not force a write")
	}
}

// TestRecordDeadCapsTheMemory pins maxDead. Nothing bounds the intake — one
// cycle stamps whatever its pages carried across up to defaultMaxDiscovered
// channels — so without this the state file grows for the whole TTL, and it is
// written on every cycle that changes it.
func TestRecordDeadCapsTheMemory(t *testing.T) {
	t.Parallel()

	now := time.Now()
	var st state
	// One older record must survive: eviction keeps the freshest stamps, and
	// this one is fresher than the flood that follows it.
	st.Dead = map[string]time.Time{"https://keeper.example/sub": now.Add(2 * time.Hour)}

	urls := make([]string, 0, maxDead+50)
	for i := range maxDead + 50 {
		urls = append(urls, fmt.Sprintf("https://flood-%05d.example/sub", i))
	}
	st.recordDead(urls, time.Hour, now)

	if len(st.Dead) != maxDead {
		t.Fatalf("Dead = %d entries, want the cap of %d", len(st.Dead), maxDead)
	}
	if _, ok := st.Dead["https://keeper.example/sub"]; !ok {
		t.Error("eviction dropped a fresher stamp than the ones it kept")
	}
	// Among one cycle's own records every expiry is identical, so the URL
	// tiebreak decides and the highest-sorting URLs go first.
	if _, ok := st.Dead[fmt.Sprintf("https://flood-%05d.example/sub", maxDead+49)]; ok {
		t.Error("the last URL of an overshooting cycle must be the first evicted")
	}
}

// TestDeadRecordsSurviveTheStateFile pins the persistence contract: the memory
// is worthless if a restart re-fetches everything it condemned, and a state
// file written before the field existed must still load.
func TestDeadRecordsSurviveTheStateFile(t *testing.T) {
	t.Parallel()

	const u = "https://gone.example/sub"
	path := filepath.Join(t.TempDir(), "state.json")
	until := time.Now().Add(time.Hour).Round(0)
	if err := saveState(path, state{Productive: map[string]channelState{}, Dead: map[string]time.Time{u: until}}); err != nil {
		t.Fatal(err)
	}

	got := loadState(path, zerolog.Nop())
	if len(got.Dead) != 1 || !got.Dead[u].Equal(until) {
		t.Errorf("Dead = %v, want the record round-tripped as %v", got.Dead, until)
	}

	legacy := filepath.Join(t.TempDir(), "legacy.json")
	if err := saveState(legacy, state{Productive: map[string]channelState{"chan": {}}}); err != nil {
		t.Fatal(err)
	}
	if old := loadState(legacy, zerolog.Nop()); len(old.Dead) != 0 {
		t.Errorf("a state file with no dead records must load empty, got %v", old.Dead)
	}
}
