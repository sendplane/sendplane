package sender

import (
	"context"
	"sync"
	"time"

	"github.com/sendplane/sendplane/store"
)

// healthTracker is the transport circuit of architecture 8.3.
//
// Consecutive auth/TLS/connect failures take a transport out of rotation; a
// probe every TransportProbeInterval puts it back. A rate_limited reply is not
// a fault, it is a signal to slow down, so it moves the transport to cooldown
// (visible in the UI) while the rate limiter does the actual work.
type healthTracker struct {
	threshold  int
	probeEvery time.Duration

	mu sync.Mutex
	m  map[string]*transportHealth
}

type transportHealth struct {
	tenantID    string
	transportID string
	failures    int
	status      store.TransportStatus
	nextProbe   time.Time
}

// probeTarget names a transport whose probe is due.
type probeTarget struct{ tenantID, transportID string }

func newHealthTracker(threshold int, probeEvery time.Duration) *healthTracker {
	return &healthTracker{threshold: threshold, probeEvery: probeEvery, m: map[string]*transportHealth{}}
}

func healthKey(tenantID, transportID string) string { return tenantID + "/" + transportID }

func (h *healthTracker) state(tenantID, transportID string) *transportHealth {
	k := healthKey(tenantID, transportID)
	th, ok := h.m[k]
	if !ok {
		th = &transportHealth{tenantID: tenantID, transportID: transportID}
		h.m[k] = th
	}
	return th
}

// usable reports whether this replica should route through the transport right
// now. An unhealthy transport is skipped until its probe is due; the probe
// itself goes through probeTransports, not through a delivery.
func (h *healthTracker) usable(tenantID, transportID string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	th := h.state(tenantID, transportID)
	return th.status != store.TransportUnhealthy || !now.Before(th.nextProbe)
}

// fail records a failure and returns the status that should be persisted, if
// it changed.
func (h *healthTracker) fail(tenantID, transportID string, class store.ErrorClass, now time.Time) (store.TransportStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	th := h.state(tenantID, transportID)
	switch class {
	case store.ErrorClassAuth:
		th.failures++
		if th.failures < h.threshold || th.status == store.TransportUnhealthy {
			if th.status == store.TransportUnhealthy {
				th.nextProbe = now.Add(h.probeEvery)
			}
			return 0, false
		}
		th.status = store.TransportUnhealthy
		th.nextProbe = now.Add(h.probeEvery)
		return store.TransportUnhealthy, true
	case store.ErrorClassRateLimited:
		if th.status == store.TransportHealthy {
			th.status = store.TransportCooldown
			return store.TransportCooldown, true
		}
		return 0, false
	default:
		// A per-recipient rejection says nothing about the transport.
		return 0, false
	}
}

// success clears the failure run and returns the status to persist when the
// transport was not healthy before.
func (h *healthTracker) success(tenantID, transportID string, _ time.Time) (store.TransportStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	th := h.state(tenantID, transportID)
	th.failures = 0
	if th.status == store.TransportHealthy {
		return 0, false
	}
	th.status = store.TransportHealthy
	return store.TransportHealthy, true
}

// due lists the unhealthy transports whose probe interval has elapsed, and
// pushes their next probe out so that two passes do not probe twice.
func (h *healthTracker) due(now time.Time) []probeTarget {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []probeTarget
	for _, th := range h.m {
		if th.status == store.TransportUnhealthy && !now.Before(th.nextProbe) {
			th.nextProbe = now.Add(h.probeEvery)
			out = append(out, probeTarget{th.tenantID, th.transportID})
		}
	}
	return out
}

// recover marks a transport healthy after a successful probe.
func (h *healthTracker) recover(ctx context.Context, t *tenantState, tr *store.Transport, s *Sender) {
	now := s.cfg.Clock()
	if status, changed := h.success(t.id, tr.ID, now); changed {
		s.setTransportStatus(ctx, t, tr.ID, status, "probe succeeded", s.statusUntil(status, now))
	}
}

// transportUsable decides whether this replica may route a delivery through a
// transport right now. It combines the persisted status with this replica's
// own circuit, and the persisted StatusUntil breaks the tie.
//
// Neither half is enough on its own. A replica that never saw the failure
// would route through a transport the rest of the cluster knows is broken, so
// the stored unhealthy status has to block it. But a replica that wrote
// unhealthy and then died never comes back to clear it, so an expired
// StatusUntil has to make the transport eligible again whatever this replica's
// own circuit says — the next delivery is the probe (architecture 8.3).
func (s *Sender) transportUsable(t *tenantState, tr *store.Transport, now time.Time) bool {
	// An elapsed window only overrides a status that has one. A healthy row
	// carries no window, so a stale StatusUntil left on one by an API edit
	// must not smuggle a locally failing transport back into rotation.
	if tr.Status != store.TransportHealthy && !tr.StatusUntil.IsZero() && now.After(tr.StatusUntil) {
		return true
	}
	if tr.Status == store.TransportUnhealthy {
		return false
	}
	return s.health.usable(t.id, tr.ID, now)
}

// statusUntil is how long a non-healthy transport status is trusted before any
// replica may try the transport again. Healthy has no expiry: there is nothing
// to recover from.
func (s *Sender) statusUntil(status store.TransportStatus, now time.Time) time.Time {
	if status == store.TransportHealthy {
		return time.Time{}
	}
	return now.Add(s.cfg.TransportProbeInterval)
}

// setTransportStatus persists a transport status transition. It re-reads the
// row first: the cached copy may be stale, and Update is optimistic on
// Version, so writing the cached row would fight the API.
func (s *Sender) setTransportStatus(ctx context.Context, t *tenantState, transportID string, status store.TransportStatus, reason string, until time.Time) {
	for attempt := 0; attempt < 2; attempt++ {
		tr, err := t.st.Transports().Get(ctx, transportID)
		if err != nil {
			return
		}
		// Re-asserting a non-healthy status is not a no-op: it pushes the
		// window out, which is what keeps a transport that is still failing
		// out of rotation. Only an identical row is skipped, and the stored
		// instant is compared at the resolution the store keeps (store/doc.go).
		until = store.TruncateTime(until)
		if tr.Status == status && tr.StatusUntil.Equal(until) {
			return
		}
		tr.Status = status
		tr.StatusReason = reason
		tr.StatusChangedAt = s.cfg.Clock()
		tr.StatusUntil = until
		if err := t.st.Transports().Update(ctx, tr); err == nil {
			t.transports.invalidate(transportID)
			s.cfg.Metrics.Count(MetricTransport, 1, "transport", transportID, "status", status.String())
			if status == store.TransportUnhealthy {
				s.pool.CloseTransport(transportID)
			}
			s.cfg.Logger.Warn("sendplane: transport status changed",
				"tenant", t.id, "transport", transportID, "status", status.String(), "reason", reason)
			return
		}
		// A concurrent edit bumped Version: read it again and retry once.
		t.transports.invalidate(transportID)
	}
}
