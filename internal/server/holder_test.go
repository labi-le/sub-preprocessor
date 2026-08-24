package server_test

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"domains.lst/sub-preprocessor/internal/preprocess"
	"domains.lst/sub-preprocessor/internal/server"
)

type stubFilterer struct{}

func (s *stubFilterer) Filter(_ context.Context, _ *bytes.Buffer, _ preprocess.FilterRequest) (preprocess.Stats, error) {
	return preprocess.Stats{}, nil
}

func TestHolder_StoreLoad(t *testing.T) {
	snap1 := &server.Snapshot{Svc: &stubFilterer{}, Groups: map[string][]string{"g": {"US"}}}
	h := server.NewHolder(snap1)
	got := h.Load()
	if got == nil {
		t.Fatal("Load() returned nil after NewHolder")
	}
	if got != snap1 {
		t.Fatal("Load() returned wrong snapshot")
	}

	snap2 := &server.Snapshot{Svc: &stubFilterer{}, Groups: map[string][]string{"g": {"DE"}}}
	h.Store(snap2)
	got2 := h.Load()
	if got2 != snap2 {
		t.Fatal("Load() should return the new snapshot after Store()")
	}
}

func TestHolder_ConcurrentRace(t *testing.T) {
	snaps := []*server.Snapshot{
		{Svc: &stubFilterer{}, Groups: map[string][]string{"g": {"US"}}},
		{Svc: &stubFilterer{}, Groups: map[string][]string{"g": {"DE"}}},
	}
	h := server.NewHolder(snaps[0])
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.Store(snaps[i%2])
		}()
		go func() {
			defer wg.Done()
			if got := h.Load(); got != snaps[0] && got != snaps[1] {
				t.Error("Load() returned a snapshot that was never stored")
			}
		}()
	}
	wg.Wait()
}
