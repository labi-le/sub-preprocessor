package ghfind

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// GitHub meters three independent REST buckets; each response reports its
// category's allowance in x-ratelimit-* headers. search/code is the binding
// constraint of the discovery phase, so its pacing is deliberate.
const (
	codeSearchMinInterval = 6 * time.Second // search/code: 10 per minute
	searchMinInterval     = 2 * time.Second // search: 30 per minute
	coreMinInterval       = 0               // core: 5000 per hour

	maxRetries           = 2 // attempts beyond the first for retryable statuses
	secondaryBackoffBase = 5 * time.Second
	secondaryBackoffCap  = 60 * time.Second
	serverErrorDelay     = time.Second
)

// bucket paces calls to one GitHub rate-limit category. mu guards all fields;
// zero remaining with a zero reset means "unknown", which never blocks.
type bucket struct {
	mu          sync.Mutex
	remaining   int
	reset       time.Time
	minInterval time.Duration
	last        time.Time
}

func newBucket(minInterval time.Duration) *bucket {
	return &bucket{minInterval: minInterval}
}

// wait blocks until a call may start: first until reset when the allowance is
// spent, then to keep minInterval between calls. Both sleeps run with mu
// released, so a context cancel never leaves other users of the bucket locked
// out; sleep is the API's injectable sleeper, which tests replace.
func (b *bucket) wait(ctx context.Context, sleep func(context.Context, time.Duration) error) error {
	b.mu.Lock()
	for {
		now := time.Now()
		var delay time.Duration
		if b.remaining <= 0 && b.reset.After(now) {
			delay = time.Until(b.reset)
		} else if !b.last.IsZero() {
			delay = b.minInterval - now.Sub(b.last)
		}
		if delay <= 0 {
			b.mu.Unlock()
			return nil
		}
		b.mu.Unlock()
		if err := sleep(ctx, delay); err != nil {
			return err
		}
		b.mu.Lock()
	}
}

// observe folds the response's rate-limit headers into the bucket and marks
// the call time for pacing. The two headers travel together; if either is
// missing the previous allowance is kept rather than half-updated.
func (b *bucket) observe(h http.Header) {
	remaining, okRemaining := parseHeaderInt(h, "X-Ratelimit-Remaining")
	reset, okReset := parseHeaderInt(h, "X-Ratelimit-Reset")
	b.mu.Lock()
	if okRemaining && okReset {
		b.remaining = remaining
		b.reset = time.Unix(int64(reset), 0)
	}
	b.last = time.Now()
	b.mu.Unlock()
}

func parseHeaderInt(h http.Header, name string) (int, bool) {
	raw := h.Get(name)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// retryAfter parses Retry-After, which GitHub sends either as whole seconds or
// as an HTTP-date; the ok result is false when the header is absent or
// unparsable. A date already past yields a negative delay, which callers treat
// as "retry immediately".
func retryAfter(h http.Header) (time.Duration, bool) {
	raw := h.Get("Retry-After")
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(raw); err == nil {
		return time.Until(when), true
	}
	return 0, false
}

// backoff is the bounded doubling delay for the nth retry (0-based); GitHub
// secondary limits last minutes, so the cap keeps a dry run short.
func backoff(n int) time.Duration {
	d := secondaryBackoffBase << n
	if d > secondaryBackoffCap {
		return secondaryBackoffCap
	}
	return d
}
