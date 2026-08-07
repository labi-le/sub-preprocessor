// Package stable builds a pre-tested subscription list: it merges source
// subscriptions, probes every node through the mihomo library and keeps only
// nodes that respond fast and consistently. The latest good result is held in
// a Holder and served as a plain URI list.
package stable

import (
	"sync/atomic"
	"time"
)

// Stats describes one completed check cycle. The json tags are the on-disk
// snapshot format (see snapshot.go); renaming one changes a file older
// processes wrote.
type Stats struct {
	SourcesOK    int `json:"sources_ok"`
	SourcesTotal int `json:"sources_total"`
	Merged       int `json:"merged"`
	Tested       int `json:"tested"`
	Kept         int `json:"kept"`
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
