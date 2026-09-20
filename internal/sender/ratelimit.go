package sender

import (
	"context"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// defaultRand is the shared jitter source. math/rand/v2's top-level functions
// are safe for concurrent use and seeded by the runtime.
func defaultRand() float64 { return rand.Float64() }

// Limiter is the token bucket set of architecture 8.2.
//
// A transport's configured RatePerSecond is a cluster-wide target. Each
// replica applies its own share of it - the target divided by the number of
// sender replicas that have a live heartbeat - so the cluster converges on the
// target without a central lock, within one heartbeat period of a replica
// count change.
//
// On a rate_limited reply the bucket halves (Penalize) and then recovers by
// 10% per minute up to its share again: additive-increase /
// multiplicative-decrease, the same shape TCP uses.
type Limiter struct {
	// Workers reports the number of active sender replicas. It must return at
	// least 1; a nil func means "this replica is alone".
	Workers func() int
	// Now is the clock (tests inject one).
	Now func() time.Time
	// Sleep waits; tests replace it to avoid real time.
	Sleep func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	buckets map[string]*bucket
}

// Recovery constants of the AIMD loop.
const (
	// recoveryPerMinute is the multiplicative increase applied per minute
	// after a penalty.
	recoveryPerMinute = 0.10
	// minShareFraction floors a penalized bucket at this fraction of its
	// share, so that repeated 421s cannot drive the rate to zero and stall the
	// transport for good.
	minShareFraction = 1.0 / 64.0
)

// NewLimiter returns a limiter. workers may be nil.
func NewLimiter(workers func() int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{Workers: workers, Now: now, buckets: map[string]*bucket{}}
}

// Key is one bucket: a name and the cluster-wide rate configured for it. A
// zero or negative rate means unlimited and costs nothing.
type Key struct {
	Name string
	Rate float64
}

type bucket struct {
	// share is this replica's slice of the cluster-wide rate.
	share float64
	// rate is the currently applied rate: share, or less while penalized.
	rate   float64
	tokens float64
	last   time.Time
	// penalized is false while rate tracks share exactly, which is the common
	// case and lets a worker-count change take effect immediately.
	penalized bool
	// started is false until the first share is known. A fresh bucket starts
	// full, so a transport that has been quiet can send one burst immediately
	// instead of ramping up from zero.
	started bool
}

// Wait blocks until one token is available in every key's bucket, or the
// context is done. It returns the time spent waiting.
func (l *Limiter) Wait(ctx context.Context, keys ...Key) (time.Duration, error) {
	d := l.reserve(keys)
	if d <= 0 {
		return 0, ctx.Err()
	}
	sleep := l.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	return d, sleep(ctx, d)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// reserve takes one token from each limited bucket and returns how long the
// caller must wait before using them. Both buckets are advanced to the same
// instant, so a message limited by the recipient domain does not also burn
// transport capacity it will not use until later.
func (l *Limiter) reserve(keys []Key) time.Duration {
	now := l.Now()
	workers := 1
	if l.Workers != nil {
		if n := l.Workers(); n > 1 {
			workers = n
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var wait time.Duration
	var active []*bucket
	for _, k := range keys {
		if k.Rate <= 0 || k.Name == "" {
			continue
		}
		b := l.bucketLocked(k.Name, now)
		b.setShare(k.Rate / float64(workers))
		b.advance(now)
		if w := b.waitFor(); w > wait {
			wait = w
		}
		active = append(active, b)
	}
	at := now.Add(wait)
	for _, b := range active {
		b.advance(at)
		b.tokens--
	}
	return wait
}

func (l *Limiter) bucketLocked(name string, now time.Time) *bucket {
	b, ok := l.buckets[name]
	if !ok {
		b = &bucket{last: now}
		l.buckets[name] = b
	}
	return b
}

// Penalize halves the rate of every named bucket after a rate_limited reply.
func (l *Limiter) Penalize(names ...string) {
	now := l.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, n := range names {
		b, ok := l.buckets[n]
		if !ok {
			continue
		}
		b.advance(now)
		floor := b.share * minShareFraction
		b.rate = math.Max(b.rate/2, floor)
		b.penalized = true
		// A penalty takes effect now, not after the tokens already banked run
		// out.
		if b.tokens > 1 {
			b.tokens = 1
		}
	}
}

// Rate reports a bucket's current applied rate; it exists for tests and
// metrics. It returns 0 for an unknown bucket.
func (l *Limiter) Rate(name string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[name]
	if !ok {
		return 0
	}
	b.advance(l.Now())
	return b.rate
}

// setShare applies a new share. An unpenalized bucket follows the share
// exactly, so adding or removing a replica converges within one heartbeat.
func (b *bucket) setShare(share float64) {
	b.share = share
	if !b.started {
		b.rate, b.started = share, true
		b.tokens = b.burst()
		return
	}
	if !b.penalized {
		b.rate = share
		return
	}
	if b.rate >= share {
		b.rate, b.penalized = share, false
	}
}

// advance adds the tokens earned since the last call and applies the AIMD
// recovery. Both are driven by the same elapsed time, so a bucket nobody
// touches for an hour is fully recovered the moment it is used again.
func (b *bucket) advance(now time.Time) {
	if now.Before(b.last) {
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	if elapsed <= 0 {
		return
	}
	if b.penalized && b.rate < b.share {
		b.rate *= 1 + recoveryPerMinute*(elapsed/60)
		if b.rate >= b.share {
			b.rate, b.penalized = b.share, false
		}
	}
	b.tokens += b.rate * elapsed
	if burst := b.burst(); b.tokens > burst {
		b.tokens = burst
	}
}

// burst is one second of capacity, and at least one message, so a bucket
// configured below 1/s still lets a single message through.
func (b *bucket) burst() float64 {
	if b.rate < 1 {
		return 1
	}
	return b.rate
}

func (b *bucket) waitFor() time.Duration {
	if b.tokens >= 1 || b.rate <= 0 {
		return 0
	}
	need := 1 - b.tokens
	return time.Duration(need / b.rate * float64(time.Second))
}
