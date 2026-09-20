package sender

import (
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func testRetry() store.RetryPolicy {
	return store.RetryPolicy{
		Backoff: []time.Duration{
			time.Minute, 5 * time.Minute, 15 * time.Minute,
			time.Hour, 4 * time.Hour, 12 * time.Hour,
		},
		MaxAttempts: 6,
	}
}

func fixedPolicy(r float64) Policy {
	p := DefaultPolicy(testRetry())
	p.Rand = func() float64 { return r }
	return p
}

func TestPolicyTransientBackoffSchedule(t *testing.T) {
	// Rand 0.5 makes the jitter factor exactly 1, so the schedule is visible.
	p := fixedPolicy(0.5)
	now := time.Unix(1_700_000_000, 0).UTC()
	want := []time.Duration{
		time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour,
	}
	for attempts, w := range want {
		d := p.Decide(store.ErrorClassTransient, attempts, now)
		if d.Status != store.DeliveryDeferred {
			t.Fatalf("attempts=%d status = %s, want deferred", attempts, d.Status)
		}
		if !d.IncrementAttempt {
			t.Errorf("attempts=%d should consume a retry", attempts)
		}
		if got := d.NextAttemptAt.Sub(now); got != w {
			t.Errorf("attempts=%d backoff = %v, want %v", attempts, got, w)
		}
	}
	// The sixth consumed attempt exhausts MaxAttempts.
	d := p.Decide(store.ErrorClassTransient, 5, now)
	if d.Status != store.DeliveryFailed {
		t.Fatalf("status = %s, want failed", d.Status)
	}
	if !d.IncrementAttempt {
		t.Error("the final attempt is still consumed")
	}
}

func TestPolicyBackoffReusesLastEntry(t *testing.T) {
	p := fixedPolicy(0.5)
	p.Retry.MaxAttempts = 9
	now := time.Unix(0, 0).UTC()
	for attempts := 5; attempts < 8; attempts++ {
		d := p.Decide(store.ErrorClassTransient, attempts, now)
		if got := d.NextAttemptAt.Sub(now); got != 12*time.Hour {
			t.Errorf("attempts=%d backoff = %v, want the last entry 12h", attempts, got)
		}
	}
}

func TestPolicyJitterStaysInBand(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	for _, r := range []float64{0, 0.25, 0.5, 0.75, 0.999999} {
		p := fixedPolicy(r)
		d := p.Decide(store.ErrorClassTransient, 0, now)
		got := d.NextAttemptAt.Sub(now)
		lo, hi := 48*time.Second, 72*time.Second // 60s ±20%
		if got < lo || got > hi {
			t.Errorf("rand %v: backoff %v outside [%v,%v]", r, got, lo, hi)
		}
	}
	// The default source also stays in band.
	p := DefaultPolicy(testRetry())
	for i := 0; i < 200; i++ {
		got := p.Decide(store.ErrorClassTransient, 0, now).NextAttemptAt.Sub(now)
		if got < 48*time.Second || got > 72*time.Second {
			t.Fatalf("default jitter produced %v", got)
		}
	}
}

func TestPolicyRateLimitedIsCapped(t *testing.T) {
	p := fixedPolicy(0.5)
	now := time.Unix(0, 0).UTC()
	// The schedule says 4h at this point; rate limiting comes back sooner,
	// because the limiter is what spaces the traffic out.
	d := p.Decide(store.ErrorClassRateLimited, 4, now)
	if d.Status != store.DeliveryDeferred {
		t.Fatalf("status = %s", d.Status)
	}
	if got := d.NextAttemptAt.Sub(now); got != p.RateLimitCap {
		t.Errorf("backoff = %v, want the cap %v", got, p.RateLimitCap)
	}
	if !d.IncrementAttempt {
		t.Error("rate_limited consumes a retry (architecture 4.1)")
	}
	// Below the cap the schedule wins.
	if got := p.Decide(store.ErrorClassRateLimited, 0, now).NextAttemptAt.Sub(now); got != time.Minute {
		t.Errorf("first backoff = %v, want 1m", got)
	}
}

func TestPolicyPermanentAndPolicyFail(t *testing.T) {
	p := fixedPolicy(0.5)
	now := time.Unix(0, 0).UTC()
	for _, class := range []store.ErrorClass{store.ErrorClassPermanent, store.ErrorClassPolicy} {
		d := p.Decide(class, 0, now)
		if d.Status != store.DeliveryFailed {
			t.Errorf("%s: status = %s, want failed", class, d.Status)
		}
		if d.IncrementAttempt {
			t.Errorf("%s must not consume a retry", class)
		}
		if !d.RecordAttempt {
			t.Errorf("%s still records an attempt", class)
		}
	}
}

func TestPolicyAuthKeepsDeliveryQueued(t *testing.T) {
	p := fixedPolicy(0.5)
	now := time.Unix(0, 0).UTC()
	d := p.Decide(store.ErrorClassAuth, 3, now)
	if d.Status != store.DeliveryQueued {
		t.Fatalf("status = %s, want queued (ADR-0003)", d.Status)
	}
	if d.IncrementAttempt {
		t.Error("a transport fault must not exhaust a delivery")
	}
	if got := d.NextAttemptAt.Sub(now); got != p.AuthRetryAfter {
		t.Errorf("next attempt in %v, want %v", got, p.AuthRetryAfter)
	}
}

func TestPolicySuccess(t *testing.T) {
	d := fixedPolicy(0.5).Decide(store.ErrorClassNone, 0, time.Unix(0, 0))
	if d.Status != store.DeliverySent || d.IncrementAttempt {
		t.Fatalf("got %+v", d)
	}
}

func TestPolicyDegenerateRetryConfig(t *testing.T) {
	p := Policy{Retry: store.RetryPolicy{}}
	now := time.Unix(0, 0).UTC()
	// MaxAttempts 0 means "one attempt": the first transient failure is final.
	if d := p.Decide(store.ErrorClassTransient, 0, now); d.Status != store.DeliveryFailed {
		t.Fatalf("status = %s, want failed", d.Status)
	}
	p.Retry.MaxAttempts = 3
	if got := p.Decide(store.ErrorClassTransient, 0, now).NextAttemptAt.Sub(now); got != time.Minute {
		t.Fatalf("empty schedule fallback = %v, want 1m", got)
	}
}
