//go:build !race

package crawl //nolint:testpackage // measures allocations of unexported harvestPages

import (
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// The race runtime allocates inside the window AllocsPerRun measures, and it
// does so unevenly from run to run, so an allocation count is only an
// assertable number in the shipped build. Hence the build tag rather than a
// slacker threshold: a bound loose enough to survive -race would no longer
// separate one copy of a page from two.

// TestHarvestPagesUnescapesPageZeroOnce prices the copy by difference: the two
// fixtures are the same page apart from one HTML entity in the prose, and
// unescapeInto copies only when it finds an '&', so what separates them is the
// scratch the harvest fills for page 0 and nothing else — the url and the node
// the harvest keeps are byte-identical either way. Page 0 is the page both scans
// read, which is why unescaping per scan doubled this. The difference is an
// equality because a copy on every page raises BOTH sides: it then reads zero
// off a harvest that copies twice as much. The absolute bound each fixture
// also owes is a cap, because that same every-page copy is the only direction
// it has to catch — below it lies a genuine win, or a URL kept as a sub-slice
// of its page, which reject_test.go's identity guards diagnose by name where a
// count would misattribute it. An equality there would also fail on a
// toolchain that inlines better and allocates less, and nothing published
// rides on these two numbers: no figure in docs/guides/benchmarks.md is one.
func TestHarvestPagesUnescapesPageZeroOnce(t *testing.T) {
	const (
		// One scratch buffer for the whole call, whatever the page count.
		allocsPerCopy = 1
		// Measured 2026-08-19, stable to -count=200.
		maxEscapedAllocs = 9
		maxPlainAllocs   = 8
		runs             = 50
		escaped          = `me &amp; you https://sub.example/a?x=1 <pre>vless://u@1.2.3.4:443?a=1#n</pre>`
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

	// The difference prices the copy only while exactly one fixture triggers it:
	// unescapeInto hands back an entity-free page uncopied, so two plain
	// fixtures difference to zero and the bound holds on nothing.
	if text, _ := unescapeInto(nil, escaped); text == escaped {
		t.Fatal("escaped fixture holds no entity, so neither side of the difference copies")
	}
	// By identity, not content: a defensive copy compares equal to its original.
	if text, _ := unescapeInto(nil, plain); unsafe.StringData(text) != unsafe.StringData(plain) {
		t.Fatalf("unescapeInto hands the entity-free fixture back copied as %q: both sides of the difference copy now", text)
	}

	escapedAllocs, plainAllocs := harvest(escaped), harvest(plain)

	// A difference is blind to whatever moves both fixtures together: a
	// strings.Clone of text in harvestPages, a scratch re-made per page, an
	// unescape that stopped reusing buf.
	if escapedAllocs > maxEscapedAllocs || plainAllocs > maxPlainAllocs {
		t.Fatalf("one harvest allocates %.0f (escaped) / %.0f (plain), want at most %d / %d: the harvest copies on every page, not once for page 0",
			escapedAllocs, plainAllocs, maxEscapedAllocs, maxPlainAllocs)
	}

	if got := escapedAllocs - plainAllocs; got != allocsPerCopy {
		t.Fatalf("the entity costs %.0f allocations, want exactly %d (one copy of page 0, not one per scan and not none)",
			got, allocsPerCopy)
	}
}

// TestHarvestPagesSegmentsWithoutAllocating pins the per-message scan to one
// shared slice and a numeric id: the fixtures are the same bytes and the same
// urls, and only one of them splits into messages, so anything segmentation
// allocates per message is the difference. origin.Post is a uint64 cut out of
// no buffer, so a message owes no copy either — the bound is exactly zero, and a
// slice per message, a string id per message, or a flat path that started
// allocating on its own all break it.
func TestHarvestPagesSegmentsWithoutAllocating(t *testing.T) {
	const (
		runs     = 50
		messages = 20
	)
	var segmented, flat strings.Builder
	for i := range messages {
		id := strconv.Itoa(i + 1)
		tail := `"chan/` + id + `"><a href="https://sub.example/` + id + `">x</a></div>`
		segmented.WriteString(`<div data-post=` + tail)
		flat.WriteString(`<div data-host=` + tail)
	}

	c := &Crawler{}
	harvest := func(page string) float64 {
		return testing.AllocsPerRun(runs, func() {
			var inline []string
			if cand := c.harvestPages([]string{page}, &inline, nil, "chan"); len(cand) != messages {
				t.Fatalf("fixture harvested %d urls, want %d", len(cand), messages)
			}
		})
	}

	if segmented.Len() != flat.Len() {
		t.Fatalf("fixtures are %d and %d B: the difference is no longer segmentation alone", segmented.Len(), flat.Len())
	}

	// The difference prices segmentation only while one fixture segments and the
	// other does not: with both on the same path the bound holds on nothing.
	var probe []string
	for u, post := range c.harvestPages([]string{segmented.String()}, &probe, nil, "chan") {
		if post == 0 {
			t.Fatalf("segmented fixture harvested %q with post 0: it no longer splits into messages", u)
		}
	}
	for u, post := range c.harvestPages([]string{flat.String()}, &probe, nil, "chan") {
		if post != 0 {
			t.Fatalf("flat fixture harvested %q with post %d: it splits into messages too", u, post)
		}
	}

	if got := harvest(segmented.String()) - harvest(flat.String()); got != 0 {
		t.Fatalf("%d messages differ from the flat page by %.0f allocations, want exactly none",
			messages, got)
	}
}
