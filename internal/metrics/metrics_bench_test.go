package metrics //nolint:testpackage // prices unexported writeSources

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"domains.lst/sub-preprocessor/internal/stable"
)

// The fixture's shape is prod's on 2026-08-18: 501 sources on :9091, 457 of
// them crawler-minted. writeSources renders five families over that set on
// every scrape, so its cost is per-source, not per-cycle.
const (
	benchSourceTotal  = 501
	benchManagedCount = 457
	benchLegacyCount  = 8
)

// The name literals are deliberately the crawler's three shapes: attributed,
// legacy hash-only, and inline. A fixture of one shape would price a single
// branch of the split.
func benchSourceReports() []stable.SourceReport {
	out := make([]stable.SourceReport, 0, benchSourceTotal)
	for i := range benchManagedCount {
		var name string
		switch {
		case i == 0:
			name = "tg-inline"
		case i <= benchLegacyCount:
			name = fmt.Sprintf("tg-%010x", i*0x9e3779b1&0xffffffffff)
		default:
			name = fmt.Sprintf("tg-channel-%d-%06x", i, i*0x9e3779b1&0xffffff)
		}
		out = append(out, benchSourceReport(name, i))
	}
	for i := benchManagedCount; i < benchSourceTotal; i++ {
		out = append(out, benchSourceReport(fmt.Sprintf("flat%03d", i), i))
	}
	return out
}

func benchSourceReport(name string, i int) stable.SourceReport {
	return stable.SourceReport{
		Name: name, Total: 400 + i, Valid: 300 + i, Tested: 200 + i, Filtered: 100 + i,
		DNSDrop: i % 7, GeoDrop: i % 11, CIDRDrop: i % 5, ASNDrop: i % 3,
		GeoBlockDrop: i % 13, IPv6Drop: i % 2, Unsupported: i % 17,
	}
}

// BenchmarkWriteSources prices one scrape's per-source render. Both owner
// branches are asserted before the timer starts: a fixture that drifted to one
// owner would keep reporting a figure off half the intended work.
func BenchmarkWriteSources(b *testing.B) {
	sources := benchSourceReports()
	if len(sources) != benchSourceTotal {
		b.Fatalf("fixture has %d sources, want benchSourceTotal = %d", len(sources), benchSourceTotal)
	}
	managed := 0
	for _, s := range sources {
		if strings.HasPrefix(s.Name, "tg-") {
			managed++
		}
	}
	if managed != benchManagedCount {
		b.Fatalf("fixture has %d managed sources, want benchManagedCount = %d", managed, benchManagedCount)
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
