package metrics //nolint:testpackage // renders the unexported writer with a pinned lastAt

import (
	"bytes"
	"flag"
	"io"
	"os"
	"testing"
	"time"

	"domains.lst/sub-preprocessor/internal/stable"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/exposition.golden from the current renderer")

const goldenPath = "testdata/exposition.golden"

// goldenAt pins the timestamp gauge, which time.Now would otherwise drift.
var goldenAt = time.Unix(1755474123, 0)

type goldenCase struct {
	name   string
	cycles int64
	failed int64
	report *stable.CycleReport
}

// goldenSpeeds crosses every speed bound and sums past 1e6, so the histogram's
// _sum renders in exponent form where the latency one renders in fixed form.
func goldenSpeeds() []int {
	const bulk, bulkSpeed = 1500, 823
	crossings := []int{3, 7, 12, 30, 60, 120, 300, 700, 1200}
	speeds := make([]int, 0, len(crossings)+bulk)
	speeds = append(speeds, crossings...)
	for range bulk {
		speeds = append(speeds, bulkSpeed)
	}
	return speeds
}

// goldenCases exercise every branch of the renderer: the nil-report page, a
// full cycle, and the degraded one where each guarded series is absent or
// explicitly zero. Between them every name shape, both owners -- a curated name
// shaped exactly like a minted one among them, since the fields and not the name
// decide -- a feed that is recorded and a feed that falls back to the name, all
// seven drop reasons with zeros among them, every histogram bound, a label value
// needing each of the three escapes in the feed and in the name independently,
// one that is not valid UTF-8, and floats on both sides of the exponent switch
// reach the wire.
func goldenCases() []goldenCase {
	full := &stable.CycleReport{
		SourcesOK: 6, SourcesTotal: 7,
		Merged: 21000, DeadSkipped: 19383, Probed: 1617, Kept: 1509,
		// A capable prober attributes every result-map miss: the stage counts
		// and the refusal counts together close on Probed (no unknown bucket).
		ProbeStages: map[stable.ProbeStage]int{ //nolint:exhaustive // an attributed cycle carries no unknown bucket
			stable.StageCondemned: 700, stable.StageConnect: 400,
			stable.StageFetch: 100, stable.StagePassed: 405,
		},
		Refusals: stable.RefusalReport{
			State: stable.RefusalRan, Unparsable: 4, Unconvertible: 8,
		},
		Precheck:   stable.PrecheckReport{State: stable.PrecheckRan, Dialled: 1200, Refused: 707, Unresolved: 61},
		GeoUnknown: 12,
		KeptCountries: map[string]int{
			"FI": 40, "??": 3, `x"y`: 1, "\xffz": 2, "a\nb": 4, `c\d`: 5,
		},
		Duration: 1234567891 * time.Millisecond,
		Phases: stable.CyclePhases{
			Fetch: 12500 * time.Millisecond, Merge: 300 * time.Millisecond,
			DeadFilter: 42 * time.Millisecond, Probe: 900 * time.Second,
			Egress: 250 * time.Second, Publish: 1500 * time.Millisecond,
		},
		Sources: []stable.SourceReport{
			{
				Name: "genliberty-3631", Managed: true, Feed: "genliberty",
				Total: 412, Valid: 388, Tested: 201, Filtered: 96,
				DNSDrop: 3, GeoDrop: 0, CIDRDrop: 5, ASNDrop: 0,
				GeoBlockDrop: 11, IPv6Drop: 0, Unsupported: 7,
			},
			{
				// The second URL of the same post: it shares the feed above,
				// which is the whole point of recording the slug.
				Name: "genliberty-3631-7e9b21", Managed: true, Feed: "genliberty",
				Total: 88, Valid: 80, Tested: 44, Filtered: 20,
				DNSDrop: 2, GeoDrop: 6, CIDRDrop: 0, ASNDrop: 0,
				GeoBlockDrop: 0, IPv6Drop: 0, Unsupported: 0,
			},
			{
				Name: "genliberty-1444c8", Managed: true, Feed: "genliberty",
				Total: 17, Valid: 16, Tested: 9, Filtered: 3,
			},
			{
				Name: "inline", Managed: true, Total: 40, Valid: 39, Tested: 30, Filtered: 12,
				DNSDrop: 0, GeoDrop: 1, CIDRDrop: 0, ASNDrop: 2,
				GeoBlockDrop: 0, IPv6Drop: 4, Unsupported: 0,
			},
			{
				Name: "96c4d7c7a7", Managed: true, Total: 1, Valid: 1, Tested: 1, Filtered: 1,
			},
			{
				Name: "seyedng-4102", Total: 6, Valid: 6, Tested: 5, Filtered: 5,
			},
			{
				Name: "flat447", Total: 9000, Valid: 8000, Tested: 1234567, Filtered: 900,
				DNSDrop: 7, GeoDrop: 7, CIDRDrop: 7, ASNDrop: 7,
				GeoBlockDrop: 7, IPv6Drop: 7, Unsupported: 7,
			},
			{
				Name: "quote\"back\\slash\nnewline", Feed: "esc\"aped\\feed\nvalue",
				Total: 5, Valid: 4, Tested: 3, Filtered: 2,
				DNSDrop: 1, Unsupported: 1,
			},
			{
				Name: "bad\xffutf8", Total: 2, Valid: 2, Tested: 2, Filtered: 2,
			},
		},
		Filters: []stable.FilterReport{
			{Name: "geoblock", In: 900, Kept: 850, Dropped: map[string]int{"blocked": 50}, State: stable.FilterRan},
			{Name: "bandwidth", In: 850, Kept: 700, Dropped: map[string]int{"slow": 150, "unreachable": 0}, State: stable.FilterRan},
			// A ran gate books both drop keys, zeros included: only a skipped
			// gate (no verdict) keeps the map empty.
			{Name: "gemini", In: 700, Kept: 700, Dropped: map[string]int{"blocked": 0, "unreachable": 0}, State: stable.FilterRan},
		},
		KeptSpeeds:      goldenSpeeds(),
		KeptLatenciesMs: []int{0, 50, 150, 300, 600, 900, 1200, 2000, 3500, 5700, 11000, 20000},
		Trace:           stable.TraceReport{State: stable.TraceRan, Answered: 1400, Unanswered: 109, Moved: 87},
		Gemini:          stable.GeminiReport{State: stable.GeminiGateRan, Checks: 654, Unverified: 12},
	}
	degraded := &stable.CycleReport{
		SourcesOK: 0, SourcesTotal: 3,
		Precheck: stable.PrecheckReport{State: stable.PrecheckTripped, Dialled: 900, Refused: 880, Unresolved: 0},
		Duration: 500 * time.Millisecond,
		Gemini:   stable.GeminiReport{State: stable.GeminiGateSkipped},
	}
	return []goldenCase{
		{name: "no cycle published yet", cycles: 7, failed: 2},
		{name: "full cycle", cycles: 4211, failed: 19, report: full},
		{name: "tripped pre-check, keyless gate, nothing kept", cycles: 4212, failed: 20, report: degraded},
	}
}

func renderGolden() []byte {
	var out bytes.Buffer
	for _, c := range goldenCases() {
		out.WriteString("# case: " + c.name + "\n")
		m := &Metrics{last: c.report, lastAt: goldenAt, cyclesTotal: c.cycles, cyclesFailed: c.failed}
		m.writeMetrics(&out)
	}
	return out.Bytes()
}

// TestExpositionGolden pins the exposition byte for byte. Two Prometheus jobs
// scrape this text and a dashboard reads it, so a renderer change that moves one
// byte -- a float verb, a label order, a missing escape -- is a wire-format
// change and must be made deliberately, by updating this file with -update.
func TestExpositionGolden(t *testing.T) {
	t.Parallel()

	got := renderGolden()
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		line, gotLine, wantLine := firstDiff(got, want)
		t.Fatalf("exposition moved at line %d:\n got: %q\nwant: %q\n"+
			"(%d bytes rendered, %d in %s; re-run with -update only for a deliberate wire-format change)",
			line, gotLine, wantLine, len(got), len(want), goldenPath)
	}
}

type countingWriter struct {
	w      io.Writer
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return c.w.Write(p)
}

// TestExpositionFlushesMidRender pins the streaming bound the golden cannot
// reach: the per-source families of a production-sized cycle run past flushAt,
// so they must leave in several writes with no line lost or repeated at a
// boundary.
func TestExpositionFlushesMidRender(t *testing.T) {
	t.Parallel()

	sources := benchSourceReports()
	// 5 family headers of 2 lines, 4 count samples per source, 7 drops each.
	wantLines := 2*5 + 4*len(sources) + len(dropReasons)*len(sources)

	var out bytes.Buffer
	counter := &countingWriter{w: &out}
	w := newExposition(counter)
	writeSources(w, sources)
	w.flush()

	if counter.writes < 2 {
		t.Errorf("%d bytes reached the writer in %d write(s); the buffer is not draining at its bound",
			out.Len(), counter.writes)
	}
	if got := bytes.Count(out.Bytes(), []byte("\n")); got != wantLines {
		t.Errorf("rendered %d lines, want %d", got, wantLines)
	}
}

func firstDiff(got, want []byte) (line int, gotLine, wantLine string) {
	gotLines := bytes.Split(got, []byte("\n"))
	wantLines := bytes.Split(want, []byte("\n"))
	for i := range max(len(gotLines), len(wantLines)) {
		g, w := "<eof>", "<eof>"
		if i < len(gotLines) {
			g = string(gotLines[i])
		}
		if i < len(wantLines) {
			w = string(wantLines[i])
		}
		if g != w {
			return i + 1, g, w
		}
	}
	return 0, "", ""
}
