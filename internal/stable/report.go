package stable

import "time"

// CycleReport is the full accounting of one completed check cycle, handed to a
// Reporter for metrics. RunOnce assembles it from data that is otherwise only
// logged: per-source drops, per-filter counts, and the kept nodes' speeds.
type CycleReport struct {
	SourcesOK    int
	SourcesTotal int
	Merged       int
	DeadSkipped  int
	Probed       int
	Kept         int
	// GeoUnknown counts published nodes carrying a [GEO:??] tag: the
	// annotation chain resolved no country for them.
	GeoUnknown int
	// KeptCountries counts published nodes per resolved country code.
	KeptCountries map[string]int
	Duration      time.Duration
	Sources       []SourceReport
	Filters       []FilterReport
	KeptSpeeds    []int
}

// SourceReport is one source's contribution to a cycle: how many nodes it
// yielded and why the rest dropped, taken from its preprocess pass.
type SourceReport struct {
	Name         string
	Total        int
	Kept         int
	DNSDrop      int
	GeoDrop      int
	ASNDrop      int
	GeoBlockDrop int
	Unsupported  int
}

// FilterReport is one through-node filter's effect on the survivor set: how
// many entered, how many it kept, and how many it dropped keyed by reason
// (blocked/slow/unreachable).
type FilterReport struct {
	Name    string
	In      int
	Kept    int
	Dropped map[string]int
}

// Reporter receives the outcome of each cycle. A nil Reporter disables
// reporting; Observe fires on a published cycle, ObserveError on any cycle that
// aborts or yields no list.
type Reporter interface {
	Observe(CycleReport)
	ObserveError()
}
