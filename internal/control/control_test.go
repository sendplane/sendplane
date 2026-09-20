package control

import (
	"context"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

func TestNewRequiresProvider(t *testing.T) {
	if _, err := New(nil, host.Hooks{}, nil, nil); err == nil {
		t.Fatal("New(nil provider) = nil error")
	}
}

// TestControlRunCompletesCampaignEndToEnd drives the real Run path: leader
// election, the finalizer detecting completion, and the outbox dispatcher
// handing the event to the host's sink.
//
// It also covers the two halves of Provider.ActiveTenants: the finalizer sees
// the tenant after its last delivery went terminal because the campaign is
// still running, and the outbox dispatcher still sees it one round after
// completing the campaign took it out of the active set.
func TestControlRunCompletesCampaignEndToEnd(t *testing.T) {
	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	st, err := p.ForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	cam := seedCampaign(t, st, store.CampaignRunning)
	seedDeliveries(t, st, cam.ID, store.DeliveryQueued, 2)

	sink := &recordingSink{}
	fast := 5 * time.Millisecond
	c, err := New(p, host.Hooks{Events: sink}, discardLogger(), time.Now,
		WithOwner("test"),
		WithLeaderTTL(testTTL),
		WithLeaderRetry(testRetry),
		WithIntervals(Intervals{Scheduler: fast, Finalizer: fast, Canceller: fast,
			LeaseReaper: fast, Retention: time.Hour, Outbox: fast}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// The stats cache fills while the campaign is still running.
	waitFor(t, 2*time.Second, func() bool {
		got, err := st.Campaigns().Get(ctx, cam.ID)
		return err == nil && got.Stats.ByStatus[store.DeliveryQueued] == 2
	})
	if got := getCampaign(t, st, cam.ID); got.Status != store.CampaignRunning {
		t.Fatalf("status %s, want running", got.Status)
	}

	// The sender finishes the work; the tenant now has no non-terminal rows.
	if _, err := st.Deliveries().BulkTransition(ctx, cam.ID,
		[]store.DeliveryStatus{store.DeliveryQueued}, store.DeliverySent, 100); err != nil {
		t.Fatalf("BulkTransition: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		got, err := st.Campaigns().Get(ctx, cam.ID)
		return err == nil && got.Status == store.CampaignCompleted
	})
	waitFor(t, 2*time.Second, func() bool {
		for _, e := range sink.got() {
			if e.Type == EventCampaignCompleted && e.TenantID == testTenant {
				return true
			}
		}
		return false
	})

	stop()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Run closes the tracking buffer on the way out.
	if err := c.Tracking().Close(); err != nil {
		t.Fatalf("Tracking.Close: %v", err)
	}
}

// TestControlRunFlushesTracking checks the buffer is live for the whole of Run
// and drains on shutdown, on every replica rather than only the leader.
func TestControlRunFlushesTracking(t *testing.T) {
	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	st, err := p.ForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	cam := seedCampaign(t, st, store.CampaignRunning)
	ids := seedDeliveries(t, st, cam.ID, store.DeliverySent, 1)

	// Hold the leader lock elsewhere: this replica will never be leader.
	sys, err := p.ForTenant(ctx, SystemTenantID)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	if ok, err := sys.Locks().Acquire(ctx, LeaderLockName, "someone-else", time.Hour, time.Now()); err != nil || !ok {
		t.Fatalf("Acquire = %v, %v", ok, err)
	}

	c, err := New(p, host.Hooks{}, discardLogger(), time.Now,
		WithOwner("follower"),
		WithLeaderRetry(10*time.Millisecond),
		WithTracking(5*time.Millisecond, 1_000_000, 1000),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	c.Tracking().Record(trackingEvent(testTenant, ids[0], cam.ID, store.TrackingOpen, time.Now().UTC()))
	waitFor(t, 2*time.Second, func() bool { return c.Tracking().Stats().Flushed == 1 })

	d, err := st.Deliveries().Get(ctx, ids[0])
	if err != nil {
		t.Fatalf("Deliveries.Get: %v", err)
	}
	if d.FirstOpenedAt.IsZero() {
		t.Error("a non-leader replica did not derive first_opened_at")
	}

	stop()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
