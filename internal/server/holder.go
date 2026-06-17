package server

import "sync/atomic"

type Snapshot struct {
	Svc    Filterer
	Groups map[string][]string
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
