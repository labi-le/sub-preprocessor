//go:build !race

package crawl //nolint:testpackage // measures allocations of unexported harvestPages

import (
	"strings"
	"testing"
)

// The race runtime allocates inside the window AllocsPerRun measures, and it
// does so unevenly from run to run, so an allocation count is only an
// assertable number in the shipped build. Hence the build tag rather than a
// slacker threshold: a bound loose enough to survive -race would no longer
// separate one copy of a page from two.

// TestHarvestPagesUnescapesPageZeroOnce prices the copy by difference: the two
// fixtures are the same page apart from one HTML entity in the prose, and
// html.UnescapeString allocates only when it finds an '&', so what separates
// them is the copies of page 0 and nothing else — the url and the node the
// harvest keeps are byte-identical either way. Page 0 is the page both scans
// read, which is why unescaping per scan doubled this.
func TestHarvestPagesUnescapesPageZeroOnce(t *testing.T) {
	const (
		// html.UnescapeString's copy is []byte(s) plus string(b[:dst]).
		allocsPerCopy = 2
		runs          = 50
		escaped       = `me &amp; you https://sub.example/a?x=1 <pre>vless://u@1.2.3.4:443?a=1#n</pre>`
	)
	plain := strings.ReplaceAll(escaped, "&amp;", "-and-")

	c := &Crawler{opts: Options{InlineEnabled: true}}
	harvest := func(page string) float64 {
		return testing.AllocsPerRun(runs, func() {
			var inline []string
			if cand := c.harvestPages([]string{page}, &inline, nil, "chan"); len(cand) != 1 || len(inline) != 1 {
				t.Fatalf("fixture %q harvested %v / %q, want one of each", page, cand, inline)
			}
		})
	}

	if got := harvest(escaped) - harvest(plain); got > allocsPerCopy {
		t.Fatalf("the entity costs %.0f allocations, want <= %d (one copy of page 0, not one per scan)",
			got, allocsPerCopy)
	}
}
