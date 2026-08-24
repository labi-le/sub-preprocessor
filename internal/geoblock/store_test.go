package geoblock_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the legacy-row test writes to the file directly

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

// TestBlockedIsCaseInsensitive: a host is a DNS name, and two sources listing
// the same node in different case are the same node. The store is the only
// mechanism that carries a through-node refusal past the cycle that found it, so
// a case-sensitive key silently re-admitted the mixed-case duplicate of an
// already-blocked host on every later cycle.
func TestBlockedIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	s, err := geoblock.Open(filepath.Join(t.TempDir(), "gb.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if blockErr := s.Block("Node-01.Example.COM"); blockErr != nil {
		t.Fatal(blockErr)
	}
	for _, host := range []string{"node-01.example.com", "NODE-01.EXAMPLE.COM", "Node-01.Example.COM"} {
		if !s.Blocked(host) {
			t.Errorf("%q must read as blocked: a host key is case-insensitive", host)
		}
	}
	// One entry, not one per spelling.
	if got := s.Count(); got != 1 {
		t.Errorf("three spellings of one host must share one entry, got %d", got)
	}
	// And the fold applies to the write side too, not just the read side.
	if blockErr := s.Block("other.example.com"); blockErr != nil {
		t.Fatal(blockErr)
	}
	if !s.Blocked("Other.Example.Com") {
		t.Error("a lowercase write must be found by a mixed-case lookup")
	}
}

// TestLoadFoldsLegacyMixedCaseRows: rows written before the keys were
// normalised keep their original casing on disk. The load path must fold them,
// or a restart would serve a block nothing can ever look up. Two such rows for
// one host collapse to the later expiry, so the merge does not depend on row
// order.
func TestLoadFoldsLegacyMixedCaseRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gb.db")
	s, err := geoblock.Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// `soon` is deliberately in the PAST: min-wins would then resurrect the
	// block from the expired row and Blocked() below would pass either way.
	soon := time.Now().Add(-time.Minute).UnixNano()
	later := time.Now().Add(48 * time.Hour).UnixNano()
	for host, exp := range map[string]int64{"Legacy.Example.COM": later, "legacy.example.com": soon} {
		if _, execErr := db.Exec(
			`INSERT INTO geoblock(host, blocked_until) VALUES(?, ?)`, host, exp,
		); execErr != nil {
			t.Fatal(execErr)
		}
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	s2, err := geoblock.Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.Blocked("legacy.example.com") {
		t.Fatal("a mixed-case legacy row must be reachable by its normalised key")
	}
	if got := s2.Count(); got != 1 {
		t.Errorf("two spellings of one legacy host must fold to one entry, got %d", got)
	}
}
