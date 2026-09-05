package stable

import (
	"math/rand/v2"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// deadKey identifies one dead entry: the endpoint's server:port AND the
// address the IP stage resolved for it the cycle the verdict was reached. The
// address is in the key so a hostname re-pointed to a new address is re-probed
// instead of skipped for the old verdict — the same host:port behind a
// different server is a different server for the cache's purposes.
type deadKey struct {
	addr string
	ip   netip.Addr
}

// DeadSet is an in-memory TTL set of nodes that failed a recent probe, keyed
// by server:port plus resolved address. It is deliberately not persisted: the
// data is cheap to regenerate (just probe) and short-lived, so a restart
// simply re-probes once instead of paying disk writes for every dead node
// every cycle.
type DeadSet struct {
	ttl time.Duration
	mu  sync.RWMutex
	m   map[deadKey]int64 // key -> unixnano expiry
}

func NewDeadSet(ttl time.Duration) *DeadSet {
	return &DeadSet{ttl: ttl, m: make(map[deadKey]int64)}
}

// Blocked reports whether addr at resolved address ip is present and not
// expired.
func (d *DeadSet) Blocked(addr string, ip netip.Addr) bool {
	d.mu.RLock()
	exp, ok := d.m[deadKey{addr: addr, ip: ip}]
	d.mu.RUnlock()
	return ok && exp > time.Now().UnixNano()
}

// Block marks addr at resolved address ip dead until now + jittered ttl
// (refreshing an existing entry).
//
// The clone honours the DeadCache contract: addr is a view into Merge's key
// arena, and a retained view pins its whole 1 KiB block for the entry's whole
// TTL. Unconditional, because a Go map assignment REPLACES the stored string
// with the one passed in (needkeyupdate is true for strings; verified on
// go1.26.5), so a refresh would swap a durable key for the view. filterDead
// keeps an already-cached node out of the probe anyway, so nearly every key
// reaching here is new and would be cloned regardless. ip is a value and is
// copied with the key.
func (d *DeadSet) Block(addr string, ip netip.Addr) error {
	exp := time.Now().Add(jitteredTTL(d.ttl)).UnixNano()
	d.mu.Lock()
	d.m[deadKey{addr: strings.Clone(addr), ip: ip}] = exp
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
