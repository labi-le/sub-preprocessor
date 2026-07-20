package stable

import (
	"testing"
	"time"
)

// jitteredTTL must stay within [ttl, 1.5*ttl): shorter would expire entries
// before the configured TTL, longer would defeat the cache's freshness bound.
func TestJitteredTTLBounds(t *testing.T) {
	t.Parallel()

	const ttl = 3 * time.Hour
	for range 1000 {
		got := jitteredTTL(ttl)
		if got < ttl || got >= ttl+ttl/2 {
			t.Fatalf("jitteredTTL(%v) = %v, want [ttl, 1.5*ttl)", ttl, got)
		}
	}
	if got := jitteredTTL(0); got != 0 {
		t.Fatalf("jitteredTTL(0) = %v, want 0 (disabled cache stays disabled)", got)
	}
}
