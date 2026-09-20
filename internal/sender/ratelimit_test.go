package sender

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock, so the limiter tests never sleep.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
	// slept accumulates what Sleep was asked to wait for.
	slept time.Duration
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// sleep advances the clock instead of waiting, which is exactly what a real
// sleep would do to the limiter's view of time.
func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.slept += d
	c.mu.Unlock()
	return nil
}

func newTestLimiter(workers int) (*Limiter, *fakeClock) {
	c := newClock()
	l := NewLimiter(func() int { return workers }, c.now)
	l.Sleep = c.sleep
	return l, c
}

func TestLimiterUnlimitedRateNeverWaits(t *testing.T) {
	l, c := newTestLimiter(1)
	for i := 0; i < 100; i++ {
		if d, err := l.Wait(context.Background(), Key{Name: "t", Rate: 0}); err != nil || d != 0 {
			t.Fatalf("wait = %v %v", d, err)
		}
	}
	if c.slept != 0 {
		t.Fatalf("slept %v with no rate configured", c.slept)
	}
}

func TestLimiterShareIsDividedByActiveWorkers(t *testing.T) {
	// 100/s cluster-wide over 4 replicas is 25/s here, so 25 messages fit in
	// the first second and the 26th waits 40ms.
	l, c := newTestLimiter(4)
	const clusterRate = 100.0
	for i := 0; i < 25; i++ {
		if d, _ := l.Wait(context.Background(), Key{Name: "tr", Rate: clusterRate}); d != 0 {
			t.Fatalf("message %d waited %v, the first second should be free", i, d)
		}
	}
	d, _ := l.Wait(context.Background(), Key{Name: "tr", Rate: clusterRate})
	if want := 40 * time.Millisecond; d != want {
		t.Fatalf("wait = %v, want %v", d, want)
	}
	if got := l.Rate("tr"); got != 25 {
		t.Fatalf("share = %v, want 25", got)
	}
	_ = c
}

func TestLimiterShareFollowsWorkerCount(t *testing.T) {
	workers := 1
	c := newClock()
	l := NewLimiter(func() int { return workers }, c.now)
	l.Sleep = c.sleep
	_, _ = l.Wait(context.Background(), Key{Name: "tr", Rate: 60})
	if got := l.Rate("tr"); got != 60 {
		t.Fatalf("alone: share = %v, want 60", got)
	}
	workers = 3
	_, _ = l.Wait(context.Background(), Key{Name: "tr", Rate: 60})
	if got := l.Rate("tr"); got != 20 {
		t.Fatalf("three replicas: share = %v, want 20", got)
	}
	// A zero or negative count never divides by less than one replica.
	workers = 0
	_, _ = l.Wait(context.Background(), Key{Name: "tr", Rate: 60})
	if got := l.Rate("tr"); got != 60 {
		t.Fatalf("no heartbeat: share = %v, want 60", got)
	}
}

func TestLimiterAIMD(t *testing.T) {
	l, c := newTestLimiter(1)
	key := Key{Name: "tr", Rate: 100}
	_, _ = l.Wait(context.Background(), key)
	if got := l.Rate("tr"); got != 100 {
		t.Fatalf("initial rate = %v", got)
	}

	// Multiplicative decrease: each rate_limited reply halves the bucket.
	l.Penalize("tr")
	if got := l.Rate("tr"); got != 50 {
		t.Fatalf("after one penalty = %v, want 50", got)
	}
	l.Penalize("tr")
	if got := l.Rate("tr"); got != 25 {
		t.Fatalf("after two penalties = %v, want 25", got)
	}

	// Recovery: +10% per minute, up to the share again.
	c.advance(time.Minute)
	if got := l.Rate("tr"); math.Abs(got-27.5) > 0.001 {
		t.Fatalf("after a minute = %v, want 27.5", got)
	}
	c.advance(time.Hour)
	if got := l.Rate("tr"); got != 100 {
		t.Fatalf("after an hour = %v, want the full share 100", got)
	}
}

func TestLimiterPenaltyFloor(t *testing.T) {
	l, _ := newTestLimiter(1)
	_, _ = l.Wait(context.Background(), Key{Name: "tr", Rate: 64})
	for i := 0; i < 20; i++ {
		l.Penalize("tr")
	}
	if got := l.Rate("tr"); got != 1 {
		t.Fatalf("floor = %v, want share/64 = 1", got)
	}
}

func TestLimiterTwoBucketsWaitForTheSlowest(t *testing.T) {
	l, _ := newTestLimiter(1)
	transport := Key{Name: "tr", Rate: 1000}
	domain := Key{Name: "tr|gmail.com", Rate: 2}

	// The first two are free (one second of burst at 2/s), the third waits for
	// the domain bucket, not the transport one.
	for i := 0; i < 2; i++ {
		if d, _ := l.Wait(context.Background(), transport, domain); d != 0 {
			t.Fatalf("message %d waited %v", i, d)
		}
	}
	d, _ := l.Wait(context.Background(), transport, domain)
	if want := 500 * time.Millisecond; d != want {
		t.Fatalf("wait = %v, want %v (the per-domain cap)", d, want)
	}
	// The transport bucket was charged at the same instant the message is
	// actually sent, so its tokens were not spent while waiting.
	if got := l.Rate("tr"); got != 1000 {
		t.Fatalf("transport rate changed to %v", got)
	}
}

func TestLimiterThroughputMatchesRate(t *testing.T) {
	l, c := newTestLimiter(1)
	key := Key{Name: "tr", Rate: 10}
	start := c.now()
	const n = 50
	for i := 0; i < n; i++ {
		if _, err := l.Wait(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := c.now().Sub(start)
	// 50 messages at 10/s: one second of burst, then 4 seconds of tokens.
	if want := 4 * time.Second; elapsed < want-50*time.Millisecond || elapsed > want+50*time.Millisecond {
		t.Fatalf("50 messages took %v, want about %v", elapsed, want)
	}
}

func TestLimiterRespectsContext(t *testing.T) {
	c := newClock()
	l := NewLimiter(func() int { return 1 }, c.now)
	// Real sleeping here, with a context that is already done.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := Key{Name: "tr", Rate: 0.001}
	_, _ = l.Wait(context.Background(), key) // drain the burst
	if _, err := l.Wait(ctx, key); err == nil {
		t.Fatal("wait ignored a cancelled context")
	}
}

func TestLimiterConcurrentUse(t *testing.T) {
	l, _ := newTestLimiter(1)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = l.Wait(context.Background(), Key{Name: "tr", Rate: 1000})
				l.Rate("tr")
			}
		}()
	}
	wg.Wait()
}
