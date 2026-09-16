package reload

import (
	"sync/atomic"

	"domains.lst/sub-preprocessor/internal/preprocess"
)

// Holder publishes the processor the stable worker picks up at the start of
// each cycle. The reloader stores a rebuilt one after a successful reload, so
// new settings land without a restart; a failed reload never stores, which is
// what keeps the previous processor serving.
type Holder struct {
	v atomic.Pointer[preprocess.Processor]
}

func NewHolder(initial *preprocess.Processor) *Holder {
	h := &Holder{}
	h.v.Store(initial)
	return h
}

func (h *Holder) Load() *preprocess.Processor {
	return h.v.Load()
}

func (h *Holder) Store(p *preprocess.Processor) {
	h.v.Store(p)
}
