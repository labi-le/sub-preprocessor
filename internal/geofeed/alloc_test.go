//go:build !race

package geofeed //nolint:testpackage // prices LoadAll's slice adoption via the stubbed fetchBytes var

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
)

// feedA keeps a comment line so parseBody's make (sized by the newline count)
// leaves one slot of spare capacity: the second source's append then fits in
// the adopted array and a two-source load costs exactly the two parses.
var (
	feedA = []byte("198.51.100.0/24,DE\n198.51.100.1/24,DE\n198.51.100.2/24,DE\n# comment\n")
	feedB = []byte("203.0.113.0/24,US\n")
)

// TestLoadAllAdoptsFirstParsedSlice prices the join in LoadAll. Appending the
// first source into a nil entries slice duplicates that (largest) source with
// a full-size []Entry allocation and a memcpy on every database build; the
// loader must adopt its backing array instead (the trick cidrset.Load
// documents). The bound is the two parses measured alongside, so both sides
// move together with the toolchain and only the join's own allocation shows:
// adoption reads 0 joins, append-into-nil reads 2 (the duplicate of the first
// source plus the regrowth the duplication forces when the second source no
// longer fits).
func TestLoadAllAdoptsFirstParsedSlice(t *testing.T) {
	orig := fetchBytes
	t.Cleanup(func() { fetchBytes = orig })

	n := 0
	fetchBytes = func(context.Context, fetch.SubscriptionURL, int64, fetch.FileType) ([]byte, error) {
		// Parity: AllocsPerRun repeats the load, and each load fetches twice.
		n++
		if n%2 == 1 {
			return feedA, nil
		}
		return feedB, nil
	}

	ctx := context.Background()
	logger := zerolog.Nop()
	sources := []Source{
		{URL: "https://feed-a.example/geofeed", Type: fetch.FileType("raw")},
		{URL: "https://feed-b.example/geofeed", Type: fetch.FileType("raw")},
	}

	load := testing.AllocsPerRun(50, func() {
		entries, failed, err := LoadAll(ctx, sources, logger)
		if err != nil || failed != 0 || len(entries) != 4 {
			t.Fatalf("LoadAll = %d entries, %d failed, %v; want 4, 0, nil", len(entries), failed, err)
		}
	})
	parses := testing.AllocsPerRun(50, func() {
		entries, err := Parse(feedA)
		if err != nil || len(entries) != 3 {
			t.Fatal("feedA must parse to 3 entries")
		}
	}) + testing.AllocsPerRun(50, func() {
		entries, err := Parse(feedB)
		if err != nil || len(entries) != 1 {
			t.Fatal("feedB must parse to 1 entry")
		}
	})
	if load > parses {
		t.Fatalf("two-source LoadAll costs %.0f allocations, want the two parses' %.0f: the join is copying the first source instead of adopting its slice",
			load, parses)
	}
}
