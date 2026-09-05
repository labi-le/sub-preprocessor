package server

import (
	"sync/atomic"

	"domains.lst/sub-preprocessor/internal/stable"
)

type Snapshot struct {
	// Svc serves GET /, which only filters. Worker is the same processor under
	// the wider interface the /stable.txt cycle needs — the per-node IP stage
	// and the annotator it publishes with. Both are filled from one concrete
	// processor, so the worker can never read a snapshot the request path is
	// not already serving.
	Svc    Filterer
	Worker stable.Filterer
	Groups map[string][]string

	// CountryFilter reports whether Svc's IP-stage chain can enforce the
	// request's country allow/deny sets — only a filters[].type country or asn
	// entry does. False (an empty or cidr-only filters list) makes GET / 400
	// every countries/groups/exclude_* request instead of answering 200 with a
	// list the parameters never constrained.
	CountryFilter bool
}

// NewSnapshot is how production wires a snapshot: both halves are positional,
// so the /stable.txt worker cannot be left nil by a literal that simply forgot
// the field it does not care about. Tests that exercise only GET / still build
// the literal directly and pass no worker — deliberately, not by omission.
//
// CountryFilter defaults to true because the two halves are one processor
// built from config and the shipped deployment runs a country filter; a wiring
// site with the config in hand sets the field false when no country/asn spec
// is configured. A literal-built snapshot must set it explicitly — the zero
// value refuses every GET / request.
func NewSnapshot(svc Filterer, worker stable.Filterer, groups map[string][]string) *Snapshot {
	return &Snapshot{Svc: svc, Worker: worker, Groups: groups, CountryFilter: true}
}

type Holder struct {
	v atomic.Pointer[Snapshot]
}

func NewHolder(initial *Snapshot) *Holder {
	h := &Holder{}
	h.v.Store(initial)
	return h
}

func (h *Holder) Load() *Snapshot {
	return h.v.Load()
}

func (h *Holder) Store(s *Snapshot) {
	h.v.Store(s)
}
