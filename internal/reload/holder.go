package reload

import (
	"sync/atomic"

	"domains.lst/sub-preprocessor/internal/preprocess"
)

// Holder records the processor the reloader last built. It is not read per
// cycle: stable.Controller resolves its getter once when it publishes a
// generation (Apply with the new policy, UseProcessor on its own), so the
// processor and the worker policy it was built from always travel together.
// The reloader stores a rebuilt one after a successful build; a failed config
// load or processor build never stores, which keeps the previous processor
// serving until the next successful reload.
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
