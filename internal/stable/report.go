package stable

import "time"

// CycleReport is the full accounting of one completed check cycle, handed to a
// Reporter for metrics. RunOnce assembles it from data that is otherwise only
// logged: per-source drops, per-filter counts, and the kept nodes' speeds and
// probe latencies.
type CycleReport struct {
	SourcesOK    int
	SourcesTotal int
	Merged       int
	// DeadSkipped counts merged nodes the cycle did not probe because the dead
	// cache still holds a recent failed probe for them, so the funnel adds up:
	// Merged = DeadSkipped + Probed.
	DeadSkipped int
	Probed      int
	Kept        int
	// GeoUnknown counts published nodes carrying a [GEO:??] tag: the
	// annotation chain resolved no country for them. It has an irreducible
	// floor and zero is not the target. A DNS-poisoning sinkhole answers with
	// RFC 2544 / RFC 5737 space (198.18.0.0/15, 192.0.2.0/24) and some sources
	// publish 127.0.0.1 or 255.255.255.255 outright; no geo source can place
	// any of those, and Cymru correctly has no record for them either -- so a
	// non-zero value here is NOT a regression from `asn` leaving the shipped
	// chain. Measured with the provider added back: the same addresses render
	// [GEO:??] either way.
	GeoUnknown int
	// KeptCountries counts published nodes per resolved country code.
	KeptCountries map[string]int
	Duration      time.Duration
	Sources       []SourceReport
	Filters       []FilterReport
	KeptSpeeds    []int
	// KeptLatenciesMs is unfiltered where KeptSpeeds skips zeros: every
	// survivor was probed by definition, so a zero here is a real sub-1ms
	// mean and not the "no bandwidth filter ran" hole.
	KeptLatenciesMs []int
	Trace           TraceReport
	Gemini          GeminiReport
}

// GeminiGateState is what the gemini gate did in a cycle. The zero value is
// GeminiGateAbsent, so a CycleReport assembled without a gemini filter -- or
// one whose whole through-node stage was skipped -- says so without anyone
// setting a field.
type GeminiGateState uint8

const (
	// GeminiGateAbsent means no gemini filter ran in this cycle's chain.
	GeminiGateAbsent GeminiGateState = iota
	// GeminiGateSkipped means the filter was configured but had no usable API
	// key, so it checked nothing and passed every survivor through
	// (nodefilter.go, apiFilter.apply). That is NOT "the gate found nothing
	// wrong", and the two must not render alike.
	GeminiGateSkipped
	// GeminiGateRan means the gate issued its checks.
	GeminiGateRan
)

// GeminiReport accounts what the gemini gate could actually verify in one
// cycle: Unverified nodes got an API answer that predates the location verdict
// (401/403/404/429, or a 400 carrying API_KEY_INVALID -- see
// geminiInconclusive), so the gate learned nothing about them and KEPT them.
//
// This is deliberately not part of FilterReport. FilterReport.Dropped renders
// as stable_filter_dropped_nodes{reason=...}, and putting a kept-node count
// there is the defect the trace's corrected/unanswered counts already shipped
// once -- shipped by b545d0a, corrected in e554307 -- appearing on a "drops by
// reason" panel for a filter that drops nothing.
//
// Checks is the gate's OWN denominator and is not interchangeable with
// stable_filter_in_nodes{filter="gemini"}: the check fans out over PROXIES and
// only one that ANSWERED reaches the classifier, so a mierus:// node
// contributes one per answering port, not one per configured port. A port that
// never answered is in neither term, and is not picked up elsewhere either:
// apiFilter.apply counts reason="unreachable" per SURVIVOR, off the outcome
// betterAPIOutcome already folded best-of-ports, so a node with one live port
// is KEPT and its dead siblings are invisible. Only a node whose every proxy
// was unreachable reaches that reason. Unverified/Checks is therefore a closed
// ratio; Unverified over the survivor count is not.
//
// It is three words by value inside CycleReport, filled once per cycle from
// two counters the check already had to keep -- nothing here is on a per-node
// path and it allocates nothing.
type GeminiReport struct {
	State      GeminiGateState
	Checks     int
	Unverified int
}

// TraceReport accounts the cloudflare annotation stage: how many survivors told
// us where their traffic actually leaves from, and how much that changed.
//
// Moved is the number the trace exists to justify — a published country that
// differs from the one the offline chain gives for the RESOLVED address — and
// Answered/Unanswered bound how far it can be trusted, since an unanswered
// node simply keeps the offline guess.
type TraceReport struct {
	Answered   int
	Unanswered int
	Moved      int
}

// SourceReport is one source's contribution to a cycle: how many nodes it
// yielded and why the rest dropped, taken from its preprocess pass.
type SourceReport struct {
	Name         string
	Total        int
	Kept         int
	DNSDrop      int
	GeoDrop      int
	CIDRDrop     int
	ASNDrop      int
	GeoBlockDrop int
	IPv6Drop     int
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
