package stable

import (
	"math/rand/v2"
	"sync"
	"time"
)

// DeadSet is an in-memory TTL set of nodes that failed a recent probe, keyed by
// server:port. It is deliberately not persisted: the data is cheap to
// regenerate (just probe) and short-lived, so a restart simply re-probes once
// instead of paying disk writes for every dead node every cycle.
type DeadSet struct {
	ttl time.Duration
	mu  sync.RWMutex
	m   map[string]int64 // key -> unixnano expiry
}

func NewDeadSet(ttl time.Duration) *DeadSet {
	return &DeadSet{ttl: ttl, m: make(map[string]int64)}
}

// Blocked reports whether key is present and not expired.
func (d *DeadSet) Blocked(key string) bool {
	d.mu.RLock()
	exp, ok := d.m[key]
	d.mu.RUnlock()
	return ok && exp > time.Now().UnixNano()
}

// Block marks key dead until now + jittered ttl (refreshing an existing entry).
func (d *DeadSet) Block(key string) error {
	exp := time.Now().Add(jitteredTTL(d.ttl)).UnixNano()
	d.mu.Lock()
	d.m[key] = exp
	d.mu.Unlock()
	return nil
}

// jitteredTTL stretches ttl by a uniform factor in [1, 1.5). A full re-probe
// marks tens of thousands of nodes dead in one batch; with a fixed TTL that
// batch expires as one batch too, making every TTL-th cycle another full
// re-probe. The jitter spreads the expiries over ~ttl/2 (a few cycles), so no
// single cycle re-probes the whole graveyard at once.
func jitteredTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	return ttl + time.Duration(rand.Float64()*0.5*float64(ttl)) //nolint:gosec // cache-expiry jitter needs no cryptographic randomness
}

// Prune drops expired entries to reclaim memory.
func (d *DeadSet) Prune() error {
	now := time.Now().UnixNano()
	d.mu.Lock()
	for k, e := range d.m {
		if e <= now {
			delete(d.m, k)
		}
	}
	d.mu.Unlock()
	return nil
}

// Len returns the current entry count (may include not-yet-pruned expired ones).
func (d *DeadSet) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.m)
}
