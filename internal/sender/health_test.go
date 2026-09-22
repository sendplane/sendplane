package sender

import (
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

// transportUsable arbitrates between the persisted transport status and this
// replica's own circuit (architecture 8.3). The cases that matter are the two
// a single source of truth gets wrong.
func TestTransportUsable(t *testing.T) {
	now := time.Now()
	const tenant, transport = "acme", "tr1"

	cases := []struct {
		name string
		// stored is the row another replica (or this one) last wrote.
		stored store.Transport
		// localUnhealthy trips this replica's own circuit first.
		localUnhealthy bool
		want           bool
	}{
		{
			name:   "healthy everywhere",
			stored: store.Transport{ID: transport, Status: store.TransportHealthy},
			want:   true,
		},
		{
			name: "another replica marked it unhealthy and the window still holds",
			stored: store.Transport{ID: transport, Status: store.TransportUnhealthy,
				StatusUntil: now.Add(time.Minute)},
			want: false,
		},
		{
			name: "the replica that marked it unhealthy never came back",
			stored: store.Transport{ID: transport, Status: store.TransportUnhealthy,
				StatusUntil: now.Add(-time.Minute)},
			localUnhealthy: true,
			want:           true,
		},
		{
			name:           "locally unhealthy, nothing persisted yet",
			stored:         store.Transport{ID: transport, Status: store.TransportHealthy},
			localUnhealthy: true,
			want:           false,
		},
		{
			name: "a stale window on a healthy row does not override the local circuit",
			stored: store.Transport{ID: transport, Status: store.TransportHealthy,
				StatusUntil: now.Add(-time.Hour)},
			localUnhealthy: true,
			want:           false,
		},
		{
			name:   "unhealthy with no window is never retried by itself",
			stored: store.Transport{ID: transport, Status: store.TransportUnhealthy},
			want:   false,
		},
		{
			name: "cooldown is a slowdown, not a block",
			stored: store.Transport{ID: transport, Status: store.TransportCooldown,
				StatusUntil: now.Add(time.Minute)},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sender{health: newHealthTracker(1, time.Hour)}
			if tc.localUnhealthy {
				if _, changed := s.health.fail(tenant, transport, store.ErrorClassAuth, now); !changed {
					t.Fatal("the local circuit did not trip")
				}
			}
			ts := &tenantState{id: tenant}
			if got := s.transportUsable(ts, &tc.stored, now); got != tc.want {
				t.Errorf("transportUsable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHealthRateLimitCooldown is the regression test for the flapping in the
// 1M load-test CI run: a lone rate_limited reply must not touch the status,
// only a burst inside the window does, and recovery waits for the window to
// go quiet rather than firing on the very next success.
func TestHealthRateLimitCooldown(t *testing.T) {
	const tenant, transport = "acme", "tr1"

	t.Run("one rate_limited reply does not change status", func(t *testing.T) {
		c := newClock()
		h := newHealthTracker(3, time.Hour)
		if _, changed := h.fail(tenant, transport, store.ErrorClassRateLimited, c.now()); changed {
			t.Fatal("a single rate_limited reply changed the status")
		}
	})

	t.Run("threshold rate_limited replies inside the window flip to cooldown exactly once", func(t *testing.T) {
		c := newClock()
		h := newHealthTracker(3, time.Hour)
		var changes int
		for i := 0; i < 5; i++ {
			c.advance(time.Second)
			if status, changed := h.fail(tenant, transport, store.ErrorClassRateLimited, c.now()); changed {
				changes++
				if status != store.TransportCooldown {
					t.Fatalf("status = %s, want cooldown", status)
				}
			}
		}
		if changes != 1 {
			t.Fatalf("cooldown fired %d times, want exactly 1", changes)
		}
	})

	t.Run("a success while rate_limited instants are still inside the window does not recover", func(t *testing.T) {
		c := newClock()
		h := newHealthTracker(3, time.Hour)
		for i := 0; i < 3; i++ {
			c.advance(time.Second)
			h.fail(tenant, transport, store.ErrorClassRateLimited, c.now())
		}
		c.advance(time.Second)
		if _, changed := h.success(tenant, transport, c.now()); changed {
			t.Fatal("recovered although the rate_limited window is still open")
		}
	})

	t.Run("a success after the window has passed recovers, once", func(t *testing.T) {
		c := newClock()
		h := newHealthTracker(3, time.Hour)
		for i := 0; i < 3; i++ {
			c.advance(time.Second)
			h.fail(tenant, transport, store.ErrorClassRateLimited, c.now())
		}
		c.advance(h.rateLimitWindow + time.Second)
		status, changed := h.success(tenant, transport, c.now())
		if !changed || status != store.TransportHealthy {
			t.Fatalf("success after the window passed = (%s, %v), want (healthy, true)", status, changed)
		}
		// A further success is a no-op: already healthy.
		if _, changed := h.success(tenant, transport, c.now()); changed {
			t.Fatal("a second success after recovery changed the status again")
		}
	})

	t.Run("a burst of interleaved rate_limited failures and successes flips at most once each way", func(t *testing.T) {
		c := newClock()
		h := newHealthTracker(3, time.Hour)
		var cooldowns, healthys int
		for i := 0; i < 50; i++ {
			c.advance(time.Second)
			if status, changed := h.fail(tenant, transport, store.ErrorClassRateLimited, c.now()); changed {
				if status != store.TransportCooldown {
					t.Fatalf("fail transition = %s, want cooldown", status)
				}
				cooldowns++
			}
			c.advance(time.Second)
			if status, changed := h.success(tenant, transport, c.now()); changed {
				if status != store.TransportHealthy {
					t.Fatalf("success transition = %s, want healthy", status)
				}
				healthys++
			}
		}
		if cooldowns > 1 || healthys > 1 {
			t.Fatalf("cooldown transitions=%d healthy transitions=%d, want at most 1 each (interleaved successes stay inside the %s window)",
				cooldowns, healthys, h.rateLimitWindow)
		}
	})

	t.Run("auth behaviour is unchanged", func(t *testing.T) {
		c := newClock()
		h := newHealthTracker(3, time.Hour)
		var changes []store.TransportStatus
		for i := 0; i < 3; i++ {
			c.advance(time.Second)
			if status, changed := h.fail(tenant, transport, store.ErrorClassAuth, c.now()); changed {
				changes = append(changes, status)
			}
		}
		if len(changes) != 1 || changes[0] != store.TransportUnhealthy {
			t.Fatalf("auth transitions = %v, want exactly one unhealthy", changes)
		}
		// success on an unhealthy transport recovers unconditionally, same as
		// before this change (it does not go through the rate_limited window).
		c.advance(time.Millisecond)
		status, changed := h.success(tenant, transport, c.now())
		if !changed || status != store.TransportHealthy {
			t.Fatalf("recovery after auth failure = (%s, %v), want (healthy, true)", status, changed)
		}
	})
}

// statusUntil gives every non-healthy status an expiry and healthy none.
func TestStatusUntil(t *testing.T) {
	now := time.Now()
	s := &Sender{cfg: Config{TransportProbeInterval: time.Minute}}
	if got := s.statusUntil(store.TransportHealthy, now); !got.IsZero() {
		t.Errorf("healthy StatusUntil = %v, want zero", got)
	}
	for _, status := range []store.TransportStatus{store.TransportCooldown, store.TransportUnhealthy} {
		if got := s.statusUntil(status, now); !got.Equal(now.Add(time.Minute)) {
			t.Errorf("%s StatusUntil = %v, want %v", status, got, now.Add(time.Minute))
		}
	}
}
