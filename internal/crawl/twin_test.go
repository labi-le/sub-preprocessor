//go:build !race

package crawl //nolint:testpackage // holds a benchmark twin's apparatus to the unexported harvest it mirrors

import "testing"

// TestHarvestPagesBlindAllocatesLikeTheShippedHarvest pins the items of
// harvestPagesBlind's divergence list (reject_bench_test.go, above the twin) that
// behaviour cannot show: that the twin reuses ONE url slice and ONE scratch across
// the pages of a call, exactly as harvestPages does. Apparatus, not product — and
// apparatus drift is what published a false figure, the twin scanning per page for
// a whole wave after harvestPage stopped, with 1600 B of a reported dedupe delta
// belonging to the twin's own slices.
//
// The twin against the shipped harvest is an equality, the opposite choice from
// alloc_test.go's, because a twin drifting DOWNWARD from what it mirrors is the
// fidelity failure itself. The absolute levels are caps and get a condition of
// their own: an equality between two harvests survives a change moving both, but
// below a cap lies a genuine win the closure's len(cand) already qualifies, and
// folding the two claims together let each be reported as the other.
//
// The build tag and its argument are alloc_test.go:11-15; not restated here,
// because two copies of that argument would drift apart.
func TestHarvestPagesBlindAllocatesLikeTheShippedHarvest(t *testing.T) {
	const (
		runs = 50
		// benchDistinctPages(1) is the shape in which every listed divergence is
		// inert: no repost for the dedupe to skip, no data-post to walk, no
		// entity to decode, so what is left over is apparatus. Measured
		// 2026-08-19, stable to -count=200. It equals
		// BenchmarkHarvestPagesByDistinct/120's allocs/op through a different
		// harness, a reading item 14 guards and this cap does not.
		maxDistinctAllocs = 254
		// benchInlinePages carries an entity on every page and no candidate at
		// all, so this is one scratch for the whole call; a scratch per page
		// reads as benchPageCount of them. Measured 2026-08-19, stable to
		// -count=200.
		maxEntityAllocs = 2
	)

	c := &Crawler{}
	allocs := func(fn func(*Crawler, []string, *[]string, *rejects, string) map[string]uint64, pages []string, wantCand int) float64 {
		return testing.AllocsPerRun(runs, func() {
			var inline []string
			if cand := fn(c, pages, &inline, nil, benchChannel); len(cand) != wantCand {
				t.Fatalf("harvest kept %d candidates, want %d: this bound is being read off the wrong path", len(cand), wantCand)
			}
		})
	}

	distinct := benchDistinctPages(1)
	shippedDistinct := allocs((*Crawler).harvestPages, distinct, benchPageCount*benchLinkRepeats)
	twinDistinct := allocs(harvestPagesBlind, distinct, benchPageCount*benchLinkRepeats)
	if shippedDistinct != twinDistinct {
		t.Fatalf("on the divergence-free fixture the shipped harvest costs %.0f allocations and the twin %.0f: on this fixture the two bodies differ in apparatus alone, so the gap is the twin drifting from what it mirrors",
			shippedDistinct, twinDistinct)
	}
	if shippedDistinct > maxDistinctAllocs {
		t.Fatalf("the divergence-free harvest costs %.0f allocations, want at most %d: both bodies moved together, which the equality above cannot see, so this is not the twin drifting",
			shippedDistinct, maxDistinctAllocs)
	}

	entity := benchInlinePages()
	shippedEntity := allocs((*Crawler).harvestPages, entity, 0)
	twinEntity := allocs(harvestPagesBlind, entity, 0)
	if shippedEntity != twinEntity {
		t.Fatalf("on the entity fixture the shipped harvest costs %.0f allocations and the twin %.0f: the twin is drifting from what it mirrors",
			shippedEntity, twinEntity)
	}
	if shippedEntity > maxEntityAllocs {
		t.Fatalf("the entity fixture's harvest costs %.0f allocations over %d pages, want at most %d: one scratch per call is no longer what both bodies fill",
			shippedEntity, benchPageCount, maxEntityAllocs)
	}

	// The dedupe divergence must be PRESENT as well as bounded elsewhere: a twin
	// that grew a dedupe would satisfy every clause above while making
	// BenchmarkHarvestPages report a delta of nothing. One suppressed clone per
	// occurrence the shipped harvest skips is the floor the fixture's own
	// constants give; the delta's size is the benchmark's subject, not this test's.
	repost := benchPages()
	gap := allocs(harvestPagesBlind, repost, benchPageCount) - allocs((*Crawler).harvestPages, repost, benchPageCount)
	if floor := float64(benchPageCount * (benchLinkRepeats - 1)); gap < floor {
		t.Fatalf("the twin costs %.0f allocations more than the shipped harvest on the repost fixture, want at least %.0f: it is deduping, which is the one branch it must not",
			gap, floor)
	}
}
