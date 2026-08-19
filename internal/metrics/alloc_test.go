//go:build !race

package metrics //nolint:testpackage // prices unexported writeSources

import (
	"io"
	"testing"
)

// TestWriteSourcesFillsOneArena pins what nothing else in this package can see.
// sourceLabelBytes only SIZES the label arena; it changes not one byte of the
// exposition, so golden_test.go and the golden fixture are structurally blind to
// a drift in it. The single symptom is the arena regrowing inside writeSources,
// which is a benchmark reading — and `make bench` tees output without comparing
// it, so `make test` was the whole net behind a figure the benchmark guide
// publishes as an item-14 result (docs/guides/benchmarks.md, BenchmarkWriteSources
// 86016 -> 73728 B/op). Dropping the feed term alone reads 102400 B / 4 allocs,
// and reducing the estimate to len(s.Name) reads 141824 B / 8 — 19% and 65%
// ABOVE the HEAD it is published as beating.
//
// A cap rather than an equality, for alloc_test.go's reason in internal/crawl:
// below it lies a genuine win, and an absolute count moves with the toolchain.
// The build tag is that file's too — AllocsPerRun counts the race detector's
// allocations as readily as the code's.
func TestWriteSourcesFillsOneArena(t *testing.T) {
	// Measured 2026-08-19, stable to -count=200: the label arena, its ends, and
	// the exposition buffer, one each.
	const maxScrapeAllocs = 3

	sources := benchSourceReports()
	// The cap prices a corpus, so it must still be given one: a fixture that
	// shrank to nothing would satisfy any bound at all.
	if len(sources) != benchSourceTotal {
		t.Fatalf("fixture has %d sources, want benchSourceTotal = %d: the cap below would be read off the wrong corpus", len(sources), benchSourceTotal)
	}

	allocs := testing.AllocsPerRun(50, func() {
		// A fresh buffer per iteration, as a scrape pays it.
		w := newExposition(io.Discard)
		writeSources(w, sources)
		w.flush()
	})
	if allocs > maxScrapeAllocs {
		t.Fatalf("one scrape's source render costs %.0f allocations, want at most %d: the label arena is regrowing, so sourceLabelBytes no longer sizes what writeSources appends",
			allocs, maxScrapeAllocs)
	}
}
