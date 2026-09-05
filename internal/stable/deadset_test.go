package stable_test

import (
	"net/netip"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/ioutil"
	"domains.lst/sub-preprocessor/internal/stable"
)

func TestDeadSet(t *testing.T) {
	t.Parallel()

	d := stable.NewDeadSet(40 * time.Millisecond)
	if d.Blocked("1.1.1.1:443", netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("empty set should not block")
	}
	_ = d.Block("1.1.1.1:443", netip.MustParseAddr("1.1.1.1"))
	if !d.Blocked("1.1.1.1:443", netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("should be blocked after Block")
	}
	if d.Blocked("2.2.2.2:443", netip.MustParseAddr("2.2.2.2")) {
		t.Fatal("unrelated key must stay unblocked")
	}
	// The same endpoint behind a different address is a different server: a
	// hostname re-pointed to a live address must not inherit the dead verdict
	// (LogicCycle#2).
	if d.Blocked("1.1.1.1:443", netip.MustParseAddr("192.0.2.10")) {
		t.Fatal("a hostname re-pointed to another address must not stay blocked")
	}

	time.Sleep(80 * time.Millisecond)
	if d.Blocked("1.1.1.1:443", netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("entry should expire after TTL")
	}
	if err := d.Prune(); err != nil {
		t.Fatal(err)
	}
	if d.Blocked("1.1.1.1:443", netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("an expired key must stay unblocked across Prune")
	}
}

// TestDeadSetBlockCopiesRetainedKey pins the DeadCache promise Merge relies on:
// Block stores a copy of addr, because the value it is handed views Merge's key
// arena and a retained view would pin its whole block for the TTL. Overwriting
// the caller's buffer is how the copy is made observable — the arena's own
// bytes never change under the set. The second Block is the refresh path, where
// a Go map assignment replaces the stored key with the caller's — the reason
// the clone is unconditional.
func TestDeadSetBlockCopiesRetainedKey(t *testing.T) {
	t.Parallel()

	buf := []byte("1.1.1.1:443")
	ip := netip.MustParseAddr("1.1.1.1")
	d := stable.NewDeadSet(time.Hour)
	_ = d.Block(ioutil.UnsafeString(buf), ip)
	_ = d.Block(ioutil.UnsafeString(buf), ip)

	copy(buf, "2.2.2.2:443")

	if !d.Blocked("1.1.1.1:443", ip) {
		t.Error("the key as it read at Block time must stay blocked")
	}
	if d.Blocked("2.2.2.2:443", ip) {
		t.Error("overwriting the caller's buffer must not move the cached key")
	}
	if !d.Blocked("1.1.1.1:443", ip) {
		t.Error("refreshing one key must leave exactly that key blocked")
	}
}
