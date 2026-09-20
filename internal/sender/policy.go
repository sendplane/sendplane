package sender

import (
	"time"

	"github.com/sendplane/sendplane/store"
)

// Decision is what happens to a delivery after one attempt: the status to
// commit, when it becomes eligible again, and whether this attempt consumes
// one of the retries (architecture 4.1, ADR-0003).
type Decision struct {
	Status        store.DeliveryStatus
	NextAttemptAt time.Time
	// IncrementAttempt is true for transient and rate_limited only. Transport
	// faults (auth, TLS) are recorded but must not exhaust a delivery.
	IncrementAttempt bool
	// RecordAttempt is false when nothing was attempted over SMTP at all
	// (suppressed, skipped), so no DeliveryAttempt row is written.
	RecordAttempt bool
}

// Policy turns an error class into a Decision. It holds the tenant retry
// policy plus the two knobs that are not tenant-configurable.
type Policy struct {
	Retry store.RetryPolicy
	// RateLimitCap shortens the backoff for rate_limited: architecture 4.2
	// asks for "a short retry plus a transport-level slowdown", and the
	// slowdown (ratelimit.go) is what actually spaces the traffic out, so the
	// delivery itself should come back soon.
	RateLimitCap time.Duration
	// AuthRetryAfter is how long an auth-class failure parks the delivery. It
	// stays queued and does not consume an attempt, so without a delay the
	// same delivery would be re-claimed in a tight loop while the transport is
	// being probed.
	AuthRetryAfter time.Duration
	// Jitter is the fraction the backoff is randomized by, ±Jitter.
	Jitter float64
	// Rand returns a value in [0,1). nil uses an internal source.
	Rand func() float64
}

// DefaultPolicy fills in the non-tenant knobs.
func DefaultPolicy(retry store.RetryPolicy) Policy {
	return Policy{
		Retry:          retry,
		RateLimitCap:   5 * time.Minute,
		AuthRetryAfter: time.Minute,
		Jitter:         0.2,
	}
}

// Decide applies the policy. attemptCount is the delivery's current
// AttemptCount, i.e. the number of retries already consumed.
func (p Policy) Decide(class store.ErrorClass, attemptCount int, now time.Time) Decision {
	switch class {
	case store.ErrorClassNone:
		return Decision{Status: store.DeliverySent, RecordAttempt: true}

	case store.ErrorClassTransient, store.ErrorClassRateLimited:
		consumed := attemptCount + 1
		max := p.Retry.MaxAttempts
		if max <= 0 {
			max = 1
		}
		if consumed >= max {
			return Decision{
				Status: store.DeliveryFailed, IncrementAttempt: true, RecordAttempt: true,
			}
		}
		d := p.backoff(consumed)
		if class == store.ErrorClassRateLimited && p.RateLimitCap > 0 && d > p.RateLimitCap {
			d = p.RateLimitCap
		}
		return Decision{
			Status:           store.DeliveryDeferred,
			NextAttemptAt:    now.Add(p.jittered(d)),
			IncrementAttempt: true,
			RecordAttempt:    true,
		}

	case store.ErrorClassPermanent, store.ErrorClassPolicy:
		return Decision{Status: store.DeliveryFailed, RecordAttempt: true}

	case store.ErrorClassAuth:
		// The transport is broken, not the delivery: it goes back to queued
		// with its attempt count untouched and waits for the transport probe
		// (architecture 8.3).
		return Decision{
			Status:        store.DeliveryQueued,
			NextAttemptAt: now.Add(p.AuthRetryAfter),
			RecordAttempt: true,
		}
	}
	return Decision{Status: store.DeliveryFailed, RecordAttempt: true}
}

// backoff is the schedule entry for the n-th consumed attempt (1-based). The
// last entry is reused when the schedule is shorter than MaxAttempts
// (store.RetryPolicy).
func (p Policy) backoff(n int) time.Duration {
	b := p.Retry.Backoff
	if len(b) == 0 {
		return time.Minute
	}
	i := n - 1
	if i < 0 {
		i = 0
	}
	if i >= len(b) {
		i = len(b) - 1
	}
	return b[i]
}

// jittered spreads retries so that a whole campaign deferred by one outage
// does not come back in one burst (architecture 4.2).
func (p Policy) jittered(d time.Duration) time.Duration {
	if p.Jitter <= 0 || d <= 0 {
		return d
	}
	r := p.Rand
	if r == nil {
		r = defaultRand
	}
	// r() in [0,1) -> factor in [1-jitter, 1+jitter).
	factor := 1 + p.Jitter*(2*r()-1)
	out := time.Duration(float64(d) * factor)
	if out < 0 {
		return 0
	}
	return out
}
