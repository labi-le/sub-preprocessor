// Package stable builds a pre-tested subscription list: it merges source
// subscriptions, probes every node through the mihomo library and keeps only
// nodes that respond fast and consistently. The latest good result is held in
// a Holder and served as a plain URI list.
package stable

import (
	"sync/atomic"
	"time"
)

// Stats describes one completed check cycle.
type Stats struct {
	SourcesOK    int
	SourcesTotal int
	Merged       int
	Tested       int
	Kept         int
}

// Snapshot is an immutable result of one successful check cycle.
type Snapshot struct {
	Payload   []byte
	UpdatedAt time.Time
	Stats     Stats
}

// Holder atomically publishes the latest snapshot.
type Holder struct {
	p atomic.Pointer[Snapshot]
}

func NewHolder() *Holder { return &Holder{} }

// Load returns the latest snapshot, or nil before the first successful cycle.
func (h *Holder) Load() *Snapshot { return h.p.Load() }

func (h *Holder) Store(s *Snapshot) { h.p.Store(s) }
