package metrics //nolint:testpackage // prices unexported writeSources

import (
	"fmt"
	"io"
	"testing"

	"domains.lst/sub-preprocessor/internal/stable"
)

// The fixture's shape is prod's on 2026-08-18: 501 sources on :9091, 457 of them
// crawler-minted, over the 46 channels last counted in config/private.yaml on
// 2026-08-15 (docs/guides/sources.md) -- the corpus grew 354 -> 457 since, and
// t.me has been unreachable since, so that count is dated, not current.
// writeSources renders five families over the set on every scrape, so its cost
// is per-source, not per-cycle.
const (
	benchSourceTotal   = 501
	benchManagedCount  = 457
	benchChannelCount  = 46
	benchHashCount     = 8
	benchNoPostCount   = 8
	benchFirstPostID   = 3000
	benchSharedPostMod = 10
)

// benchSourceReports mints all four name shapes plus the inline aggregate. Name
// and feed lengths are what the label arena is sized by, so a fixture of one
// shape would price one arena and one branch of the feed fallback.
func benchSourceReports() []stable.SourceReport {
	out := make([]stable.SourceReport, 0, benchSourceTotal)
	for i := range benchManagedCount {
		slug := fmt.Sprintf("channel%02d", i%benchChannelCount)
		shard := i * 0x9e3779b1 & 0xffffff
		var name string
		switch {
		case i == 0:
			// The inline aggregate spans channels, and a bare <sha10> was minted
			// with no origin at all: neither records a feed.
			name, slug = "inline", ""
		case i <= benchHashCount:
			name, slug = fmt.Sprintf("%010x", i*0x9e3779b1&0xffffffffff), ""
		case i <= benchHashCount+benchNoPostCount:
			name = fmt.Sprintf("%s-%06x", slug, shard)
		case i%benchSharedPostMod == 0:
			name = fmt.Sprintf("%s-%d-%06x", slug, benchFirstPostID+i, shard)
		default:
			name = fmt.Sprintf("%s-%d", slug, benchFirstPostID+i)
		}
		out = append(out, benchSourceReport(name, slug, i, true))
	}
	for i := benchManagedCount; i < benchSourceTotal; i++ {
		out = append(out, benchSourceReport(fmt.Sprintf("flat%03d", i), "", i, false))
	}
	return out
}

func benchSourceReport(name, feed string, i int, managed bool) stable.SourceReport {
	return stable.SourceReport{
		Name: name, Managed: managed, Feed: feed,
		Total: 400 + i, Valid: 300 + i, Tested: 200 + i, Filtered: 100 + i,
		DNSDrop: i % 7, GeoDrop: i % 11, CIDRDrop: i % 5, ASNDrop: i % 3,
		GeoBlockDrop: i % 13, IPv6Drop: i % 2, Unsupported: i % 17,
	}
}

// BenchmarkWriteSources prices one scrape's per-source render. The fixture is
// checked before the timer starts on both axes a drift would silently halve the
// intended work: the owner split, and how many sources fall back to their own
// name for the feed label -- which decides the arena's size as much as the owner
// split decides its content.
func BenchmarkWriteSources(b *testing.B) {
	sources := benchSourceReports()
	if len(sources) != benchSourceTotal {
		b.Fatalf("fixture has %d sources, want benchSourceTotal = %d", len(sources), benchSourceTotal)
	}
	managed, attributed := 0, 0
	for _, s := range sources {
		if s.Managed {
			managed++
		}
		if s.Feed != "" {
			attributed++
		}
	}
	if managed != benchManagedCount {
		b.Fatalf("fixture has %d managed sources, want benchManagedCount = %d", managed, benchManagedCount)
	}
	// inline and the hash-only mints record no channel, so those are the
	// managed sources whose feed label is their own name.
	if want := benchManagedCount - 1 - benchHashCount; attributed != want {
		b.Fatalf("fixture attributes %d of %d managed sources to a channel, want %d", attributed, managed, want)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		// A fresh buffer per iteration, as a scrape pays it.
		w := newExposition(io.Discard)
		writeSources(w, sources)
		w.flush()
	}
}
