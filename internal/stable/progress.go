package stable

import (
	"sync/atomic"

	"github.com/rs/zerolog"
)

// progress tracks completion of a concurrent node batch and reports it: the
// caller logs one debug event per item (enriched with the ordinal from step),
// and progress itself emits one info milestone whenever another 10% decade of
// the total completes, so an info-level log still shows "done/total" movement.
type progress struct {
	logger zerolog.Logger
	msg    string
	total  int64
	done   atomic.Int64
}

func newProgress(logger zerolog.Logger, msg string, total int) *progress {
	return &progress{logger: logger, msg: msg, total: int64(total)}
}

// step records one completed item, logs a decade milestone when crossed, and
// returns the completion ordinal for the caller's per-item debug event.
func (p *progress) step() int64 {
	done := p.done.Add(1)
	if p.total <= 0 {
		return done
	}
	if done == p.total || done*10/p.total != (done-1)*10/p.total {
		p.logger.Info().Int64("done", done).Int64("total", p.total).Msg(p.msg)
	}
	return done
}
