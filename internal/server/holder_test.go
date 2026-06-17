package server

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"domains.lst/sub-preprocessor/internal/preprocess"
)

type stubFilterer struct{ id string }

func (s *stubFilterer) Filter(_ context.Context, _ *bytes.Buffer, _ preprocess.FilterRequest) (preprocess.Stats, error) {
	return preprocess.Stats{}, nil
}

func TestHolder_StoreLoad(t *testing.T) {
	snap1 := &Snapshot{Svc: &stubFilterer{id: "A"}, Groups: map[string][]string{"g": {"US"}}}
	h := NewHolder(snap1)
	got := h.Load()
	if got == nil {
		t.Fatal("Load() returned nil after NewHolder")
	}
	if got.Svc != snap1.Svc {
		t.Fatal("Load() returned wrong snapshot")
	}

	snap2 := &Snapshot{Svc: &stubFilterer{id: "B"}, Groups: map[string][]string{"g": {"DE"}}}
	h.Store(snap2)
	got2 := h.Load()
	if got2.Svc != snap2.Svc {
		t.Fatal("Load() should return the new snapshot after Store()")
	}
}

func TestHolder_ConcurrentRace(t *testing.T) {
	snap := &Snapshot{Svc: &stubFilterer{id: "X"}, Groups: nil}
	h := NewHolder(snap)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.Store(&Snapshot{Svc: &stubFilterer{id: "Y"}})
		}()
		go func() {
			defer wg.Done()
			_ = h.Load()
		}()
	}
	wg.Wait()
}
