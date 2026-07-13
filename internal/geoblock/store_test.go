package geoblock_test

import (
	"path/filepath"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/geoblock"
)

func TestBlockAndQuery(t *testing.T) {
	t.Parallel()

	s, err := geoblock.Open(filepath.Join(t.TempDir(), "gb.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if s.Blocked("1.2.3.4") {
		t.Fatal("unknown host should not be blocked")
	}
	if s.Blocked("") {
		t.Fatal("empty host must never be blocked")
	}
	if blockErr := s.Block("1.2.3.4"); blockErr != nil {
		t.Fatal(blockErr)
	}
	if !s.Blocked("1.2.3.4") {
		t.Fatal("host should be blocked after Block")
	}
	if s.Blocked("5.6.7.8") {
		t.Fatal("other host should stay unblocked")
	}
}

func TestExpiryAndPrune(t *testing.T) {
	t.Parallel()

	s, err := geoblock.Open(filepath.Join(t.TempDir(), "gb.db"), 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	_ = s.Block("9.9.9.9")
	if !s.Blocked("9.9.9.9") {
		t.Fatal("should be blocked immediately")
	}
	time.Sleep(80 * time.Millisecond)
	if s.Blocked("9.9.9.9") {
		t.Fatal("should be unblocked after TTL")
	}
	if pruneErr := s.Prune(); pruneErr != nil {
		t.Fatal(pruneErr)
	}
	if s.Count() != 0 {
		t.Fatalf("expired entry should be pruned, count=%d", s.Count())
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gb.db")
	s, err := geoblock.Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Block("10.0.0.1")
	_ = s.Close()

	s2, err := geoblock.Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.Blocked("10.0.0.1") {
		t.Fatal("blocked host should persist across reopen")
	}
}

func TestExpiredPrunedOnLoad(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gb.db")
	s, err := geoblock.Open(path, time.Nanosecond) // expires effectively immediately
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Block("11.0.0.1")
	time.Sleep(5 * time.Millisecond)
	_ = s.Close()

	s2, err := geoblock.Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if s2.Count() != 0 {
		t.Fatalf("expired entry should be pruned on load, count=%d", s2.Count())
	}
}
