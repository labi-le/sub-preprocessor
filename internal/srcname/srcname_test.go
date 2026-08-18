package srcname_test

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/srcname"
)

func TestSplit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		wantFeed    string
		wantManaged bool
	}{
		{"tg-genliberty-7e9b21", "genliberty", true},
		{"tg-vpn-channel-3d9dfd", "vpn-channel", true},
		{"tg-inline", "inline", true},
		// Legacy tg-<sha10> attributes to no channel, so it stands alone.
		{"tg-96c4d7c7a7", "96c4d7c7a7", true},
		{"flat447", "flat447", false},
		// A curated name may look attributed; only the prefix decides owner.
		{"foo-abc123", "foo-abc123", false},
		// hex.EncodeToString never emits upper case, so this tail is slug.
		{"tg-chan-ABC123", "chan-ABC123", true},
		{"tg-chan-abc12", "chan-abc12", true},
		{"tg-chan-abc1234", "chan-abc1234", true},
		{"tg-", "tg-", true},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			feed, managed := srcname.Split(tc.name)
			if feed != tc.wantFeed || managed != tc.wantManaged {
				t.Errorf("Split(%q) = (%q, %v), want (%q, %v)", tc.name, feed, managed, tc.wantFeed, tc.wantManaged)
			}
		})
	}
}

var sinkFeed string

// TestSplitDoesNotAllocate pins the feed as a slice of its input: the exporter
// calls Split once per source on every scrape. It cannot be parallel --
// AllocsPerRun panics in a parallel test.
func TestSplitDoesNotAllocate(t *testing.T) {
	if n := testing.AllocsPerRun(100, func() {
		sinkFeed, _ = srcname.Split("tg-genliberty-7e9b21")
	}); n != 0 {
		t.Errorf("Split allocates %v times per call, want 0", n)
	}
}
