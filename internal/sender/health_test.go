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
