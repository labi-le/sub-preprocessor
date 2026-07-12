package stable_test

import (
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/stable"
)

func TestDeadSet(t *testing.T) {
	t.Parallel()

	d := stable.NewDeadSet(40 * time.Millisecond)
	if d.Blocked("1.1.1.1:443") {
		t.Fatal("empty set should not block")
	}
	_ = d.Block("1.1.1.1:443")
	if !d.Blocked("1.1.1.1:443") {
		t.Fatal("should be blocked after Block")
	}
	if d.Blocked("2.2.2.2:443") {
		t.Fatal("unrelated key must stay unblocked")
	}

	time.Sleep(80 * time.Millisecond)
	if d.Blocked("1.1.1.1:443") {
		t.Fatal("entry should expire after TTL")
	}
	if err := d.Prune(); err != nil {
		t.Fatal(err)
	}
	if d.Len() != 0 {
		t.Fatalf("expired entry should be pruned, len=%d", d.Len())
	}
}
