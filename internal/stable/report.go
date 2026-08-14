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
	// ProbeStages counts the probed set by how far each node's probe got, and
	// sums to Probed: a label the prober never named -- its proxies did not
	// parse -- counts as StageUnknown rather than falling out of the total.
	ProbeStages map[ProbeStage]int
	// Precheck is what the reachability pre-check inside Probe did, in
	// ENDPOINTS rather than nodes -- see PrecheckReport. It is the only place a
	// discarded pre-check verdict survives: ProbeStages then holds no
	// StageCondemned at all.
	Precheck PrecheckReport
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
	// Duration is the whole cycle; Phases is where it went, and the two do
	// not agree exactly -- see CyclePhases.
	Duration   time.Duration
	Phases     CyclePhases
	Sources    []SourceReport
	Filters    []FilterReport
	KeptSpeeds []int
	// KeptLatenciesMs is unfiltered where KeptSpeeds skips zeros: every
	// survivor was probed by definition, so a zero here is a real sub-1ms
	// mean and not the "no bandwidth filter ran" hole.
	KeptLatenciesMs []int
	Trace           TraceReport
	Gemini          GeminiReport
}

// CyclePhases is where one cycle's wall time went, timed by RunOnce at the
// boundaries between its stages. Probe and Egress are separate because both do
// per-node network work, bounded differently: each Egress filter by its own
// timeout, Probe by check.timeout per round PLUS the reachability pre-check,
// whose per-endpoint budget is one round's timeout again
// (MihomoProber.precheckDialBudget) and is NOT part of any round.
//
// The fields sum to slightly LESS than CycleReport.Duration: the steps between
// phases (dead-cache write, SelectSurvivors, pruneCaches, report assembly) are
// in no phase, so the residue is the cycle's non-stage overhead rather than a
// rounding error.
type CyclePhases struct {
	// Fetch covers fetchSources: download, parse and the per-node IP stage
	// (DNS, geo/asn/cidr) of every source.
	Fetch      time.Duration
	Merge      time.Duration
	DeadFilter time.Duration
	// Probe covers Prober.Probe whole: parsing the payload into live mihomo
	// adapters, the TCP pre-check, and every URL-test round. Splitting the
	// pre-check's own duration out is DEFERRED — it would need a per-cycle
	// plumbing path back through Probe, which the third return value was
	// already refused for — so its share is inferred from the endpoints it
	// dialled (Precheck) and the stage="condemned" count.
	Probe time.Duration
	// Egress covers filterAndMeasureEgress: the through-node filters and the
	// cdn-cgi/trace measurement.
	Egress time.Duration
	// Publish covers BuildPayload, movedCount and c.publish: the in-memory
	// swap, SaveSnapshot and the published-cycle log.
	Publish time.Duration
}

// PrecheckState is what the TCP reachability pre-check did in a cycle. The zero
// value is PrecheckAbsent, so a CycleReport from a Prober that runs no
// pre-check says so without anyone setting a field.
type PrecheckState uint8

const (
	// PrecheckAbsent means no pre-check ran: the cycle's Prober does not
	// implement one, or Probe failed before reaching it.
	PrecheckAbsent PrecheckState = iota
	// PrecheckTripped means the breaker judged the pre-check's own verdict
	// implausible, so it was DISCARDED and every node was URL-tested. Nothing
	// is condemned, which makes stage="condemned" read 0 exactly as it does
	// for a pre-check that ran and condemned nobody -- the case this state
	// exists to tell apart, on the gemini gate's precedent.
	PrecheckTripped
	// PrecheckRan means the verdict was used: refused endpoints were condemned
	// unprobed and dead-cached.
	PrecheckRan
)

// PrecheckReport accounts the reachability pre-check in ENDPOINTS: one distinct
// server:port is dialled once, while a multi-port mierus:// node is several, so
// none of these is interchangeable with the stage="condemned" NODE count.
//
// Refused is what the pre-check proved unreachable -- condemned under
// PrecheckRan, thrown away under PrecheckTripped, where this field is the only
// surviving trace of the rejected verdict. Unresolved is the DNS fail-open
// share: the name never became an address, which proves nothing about the
// endpoint, so those nodes were probed instead (see filterReachable).
//
// Deliberately not a FilterReport: Dropped renders as
// stable_filter_dropped_nodes{reason=...}, the pre-check is not a filter, and
// that is the defect b545d0a shipped for the trace's counts and e554307
// corrected.
type PrecheckReport struct {
	State      PrecheckState
	Dialled    int
	Refused    int
	Unresolved int
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
