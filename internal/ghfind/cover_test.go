package ghfind_test

import (
	"slices"
	"testing"

	"domains.lst/sub-preprocessor/internal/ghfind"
)

// TestCoverPicksUnionMaximalFirstAndTieBreaks pins two properties: the first
// pick is the set with the largest marginal gain (the one that covers the
// most of the union), and equal gains resolve to the lower index.
func TestCoverPicksUnionMaximalFirstAndTieBreaks(t *testing.T) {
	t.Parallel()

	sets := [][]uint64{
		{1, 2},
		{1, 2},
		{3, 4, 5}, // union-maximal: covers everything the first two share plus more
	}
	got := ghfind.Cover(sets, 1, 4)
	want := []int{2, 0} // index 2 first, then the 0/1 tie goes to index 0; 1 is never repeated
	if !slices.Equal(got, want) {
		t.Fatalf("Cover = %v, want %v", got, want)
	}
}

// TestCoverFloor pins the stop condition at the exact boundary: a gain equal
// to floor is picked, a gain one below it stops the run.
func TestCoverFloor(t *testing.T) {
	t.Parallel()

	sets := [][]uint64{
		{1, 2, 3},          // 0: adds 2 after the maximal pick
		{3, 4, 5, 6, 7, 8}, // 1: maximal first pick (gain 6)
		{9},                // 2: adds 1 after the maximal pick
	}
	if got := ghfind.Cover(sets, 3, 10); !slices.Equal(got, []int{1}) {
		t.Fatalf("floor 3: Cover = %v, want [1] (next best gain 2 is below floor)", got)
	}
	if got := ghfind.Cover(sets, 2, 10); !slices.Equal(got, []int{1, 0}) {
		t.Fatalf("floor 2: Cover = %v, want [1 0] (a gain equal to floor is picked)", got)
	}
}

// TestCoverRespectsAccept pins the pick cap: the run stops at accept picks
// even when further sets still meet the floor, and degenerate inputs pick
// nothing.
func TestCoverRespectsAccept(t *testing.T) {
	t.Parallel()

	sets := [][]uint64{{1}, {2}, {3}, {4}}
	if got := ghfind.Cover(sets, 1, 2); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("Cover = %v, want [0 1] (capped at 2 picks)", got)
	}
	if got := ghfind.Cover(nil, 1, 4); got != nil {
		t.Fatalf("Cover(nil) = %v, want nil", got)
	}
	if got := ghfind.Cover(sets, 1, 0); got != nil {
		t.Fatalf("Cover(accept 0) = %v, want nil", got)
	}
}

// TestCoverSkipsRedundantSets pins that a set whose hashes are all already in
// the union is never picked again once its gain reaches zero.
func TestCoverSkipsRedundantSets(t *testing.T) {
	t.Parallel()

	sets := [][]uint64{
		{1, 2, 3, 4},
		{1, 2, 3, 4}, // duplicate of the first: zero gain after it is picked
		{5},
	}
	got := ghfind.Cover(sets, 1, 10)
	want := []int{0, 2}
	if !slices.Equal(got, want) {
		t.Fatalf("Cover = %v, want %v (the duplicate set must never be picked)", got, want)
	}
}
