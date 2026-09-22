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
//
// A single rate_limited reply must not flip the status: with a few percent of
// deliveries deferred, one-strike cooldown and one-success recovery flap the
// status (and its DB write and outbox event) forever. Cooldown instead needs
// rateLimitThreshold rate_limited replies within rateLimitWindow, and healthy
// is only restored once the window has been clear of them for a full
// rateLimitWindow.
type healthTracker struct {
	threshold  int
	probeEvery time.Duration

	// rateLimitThreshold and rateLimitWindow are not threaded through Config:
	// they are package constants (defaultRateLimitThreshold,
	// defaultRateLimitWindow) rather than Sender.Config fields, because wiring
	// them through Config means touching sender.go, which is out of scope for
	// this change.
	rateLimitThreshold int
	rateLimitWindow    time.Duration

	mu sync.Mutex
	m  map[string]*transportHealth
}

type transportHealth struct {
	tenantID    string
	transportID string
	failures    int
	status      store.TransportStatus
	nextProbe   time.Time
	// rateLimits holds the instants of recent rate_limited replies, pruned to
	// rateLimitWindow on every touch (fail and success both prune it, so a
	// long-idle transport does not carry a stale window forward).
	rateLimits []time.Time
}

// probeTarget names a transport whose probe is due.
type probeTarget struct{ tenantID, transportID string }

// Defaults for the rate-limited cooldown window (architecture 8.3). Kept as
// package constants rather than Config fields: see the healthTracker comment.
const (
	defaultRateLimitThreshold = 3
	defaultRateLimitWindow    = time.Minute
)

func newHealthTracker(threshold int, probeEvery time.Duration) *healthTracker {
	return &healthTracker{
		threshold:          threshold,
		probeEvery:         probeEvery,
		rateLimitThreshold: defaultRateLimitThreshold,
		rateLimitWindow:    defaultRateLimitWindow,
		m:                  map[string]*transportHealth{},
	}
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
		th.rateLimits = pruneRateLimits(th.rateLimits, now, h.rateLimitWindow)
		th.rateLimits = append(th.rateLimits, now)
		if th.status == store.TransportHealthy && len(th.rateLimits) >= h.rateLimitThreshold {
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
// transport recovered.
//
// An unhealthy transport (auth/TLS/connect) recovers unconditionally: it only
// gets here after a successful probe, which is itself the signal. A cooldown
// transport (rate_limited) recovers only once its rate_limited window is
// clear: a success in the middle of a burst of deferrals says nothing about
// whether the burst is over.
func (h *healthTracker) success(tenantID, transportID string, now time.Time) (store.TransportStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	th := h.state(tenantID, transportID)
	th.failures = 0
	switch th.status {
	case store.TransportHealthy:
		return 0, false
	case store.TransportCooldown:
		th.rateLimits = pruneRateLimits(th.rateLimits, now, h.rateLimitWindow)
		if len(th.rateLimits) > 0 {
			return 0, false
		}
		th.status = store.TransportHealthy
		return store.TransportHealthy, true
	default: // store.TransportUnhealthy
		th.status = store.TransportHealthy
		return store.TransportHealthy, true
	}
}

// pruneRateLimits drops instants older than window, relative to now. Entries
// are appended in call order so they are already non-decreasing; this still
// filters rather than assumes it, since fail and success both call it and a
// clock is free to be handed non-monotonic values in tests.
func pruneRateLimits(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	out := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			out = append(out, t)
		}
	}
	return out
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
	if status, changed := h.success(limitScope(t.id, tr), tr.ID, now); changed {
		s.setTransportStatus(ctx, t, tr, status, "probe succeeded", s.statusUntil(status, now))
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
	return s.health.usable(limitScope(t.id, tr), tr.ID, now)
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
// A shared transport's status belongs to the platform, not to the tenant that
// happened to observe it: the write goes to the system tenant's view, which is
// where the overlay keeps the state shadow row (ADR-0017).
func (s *Sender) setTransportStatus(ctx context.Context, t *tenantState, transport *store.Transport, status store.TransportStatus, reason string, until time.Time) {
	transportID := transport.ID
	st, err := s.configState(ctx, t, transportID)
	if err != nil {
		s.cfg.Logger.Warn("sendplane: cannot reach the platform view to record a transport status",
			"transport", transportID, "err", err)
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		tr, err := st.st.Transports().Get(ctx, transportID)
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
		if err := st.st.Transports().Update(ctx, tr); err == nil {
			st.transports.invalidate(transportID)
			t.transports.invalidate(transportID)
			s.cfg.Metrics.Count(MetricTransport, 1, "transport", transportID, "status", status.String())
			if status == store.TransportUnhealthy {
				s.pool.CloseTransport(transportID)
			}
			s.cfg.Logger.Warn("sendplane: transport status changed",
				"tenant", t.id, "transport", transportID, "shared", transport.Shared,
				"status", status.String(), "reason", reason)
			return
		}
		// A concurrent edit bumped Version: read it again and retry once.
		st.transports.invalidate(transportID)
		t.transports.invalidate(transportID)
	}
}
