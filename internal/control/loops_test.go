package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

func TestSchedulerStartsDueCampaignsOnly(t *testing.T) {
	_, st, _, c := newFixture(t, host.Hooks{})
	ctx := context.Background()
	now := baseTime

	due := seedCampaign(t, st, store.CampaignScheduled, func(c *store.Campaign) {
		c.ScheduleAt = now.Add(-time.Minute)
	})
	later := seedCampaign(t, st, store.CampaignScheduled, func(c *store.Campaign) {
		c.ScheduleAt = now.Add(time.Hour)
	})
	// A zero ScheduleAt means "start now" (store/campaign.go).
	immediate := seedCampaign(t, st, store.CampaignScheduled)
	draft := seedCampaign(t, st, store.CampaignDraft)

	loop := &scheduler{st: st, log: discardLogger(), page: c.cfg.batches.CampaignPage}
	if err := loop.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	for _, id := range []string{due.ID, immediate.ID} {
		got := getCampaign(t, st, id)
		if got.Status != store.CampaignRunning {
			t.Errorf("campaign %s: status %s, want running", id, got.Status)
		}
		if !got.StartedAt.Equal(now) {
			t.Errorf("campaign %s: StartedAt %v, want %v", id, got.StartedAt, now)
		}
	}
	if got := getCampaign(t, st, later.ID); got.Status != store.CampaignScheduled {
		t.Errorf("future campaign: status %s, want scheduled", got.Status)
	}
	if got := getCampaign(t, st, draft.ID); got.Status != store.CampaignDraft {
		t.Errorf("draft campaign: status %s, want draft (drafts never auto-start)", got.Status)
	}

	events := listOutbox(t, st, store.OutboxPending)
	if len(events) != 2 {
		t.Fatalf("outbox has %d events, want 2", len(events))
	}
	for _, e := range events {
		if e.Type != EventCampaignStarted {
			t.Errorf("event type %q, want %q", e.Type, EventCampaignStarted)
		}
	}
}

func TestFinalizerCachesStatsAndCompletes(t *testing.T) {
	_, st, _, c := newFixture(t, host.Hooks{})
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignRunning)
	seedDeliveries(t, st, cam.ID, store.DeliverySent, 3)
	seedDeliveries(t, st, cam.ID, store.DeliveryQueued, 2, func(d *store.Delivery) {
		d.Email = "q" + d.Email
		d.EmailNorm = "q" + d.EmailNorm
	})

	loop := &finalizer{st: st, log: discardLogger(), cfg: &c.cfg, skips: map[string]int{}}
	if err := loop.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := getCampaign(t, st, cam.ID)
	if got.Status != store.CampaignRunning {
		t.Fatalf("status %s, want running while 2 are queued", got.Status)
	}
	if got.Stats.ByStatus[store.DeliverySent] != 3 || got.Stats.ByStatus[store.DeliveryQueued] != 2 {
		t.Fatalf("stats %v, want sent=3 queued=2", got.Stats.ByStatus)
	}
	if !got.Stats.ComputedAt.Equal(baseTime) {
		t.Errorf("ComputedAt %v, want %v", got.Stats.ComputedAt, baseTime)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdateStats did not refresh UpdatedAt")
	}
	if n := len(listOutbox(t, st, store.OutboxPending)); n != 0 {
		t.Fatalf("outbox has %d events, want none before completion", n)
	}

	// Drain the queue: now nothing is in flight.
	if _, err := st.Deliveries().BulkTransition(ctx, cam.ID,
		[]store.DeliveryStatus{store.DeliveryQueued}, store.DeliveryFailed, 100); err != nil {
		t.Fatalf("BulkTransition: %v", err)
	}
	later := baseTime.Add(time.Minute)
	if err := loop.Tick(ctx, later); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got = getCampaign(t, st, cam.ID)
	if got.Status != store.CampaignCompleted {
		t.Fatalf("status %s, want completed", got.Status)
	}
	if !got.CompletedAt.Equal(later) {
		t.Errorf("CompletedAt %v, want %v", got.CompletedAt, later)
	}
	events := listOutbox(t, st, store.OutboxPending)
	if len(events) != 1 || events[0].Type != EventCampaignCompleted {
		t.Fatalf("outbox = %+v, want one campaign.completed", events)
	}
	if want := `"by_status"`; !strings.Contains(string(events[0].Payload), want) {
		t.Errorf("payload %s does not carry %s", events[0].Payload, want)
	}
}

func TestFinalizerLeavesEmptyCampaignRunning(t *testing.T) {
	_, st, _, c := newFixture(t, host.Hooks{})
	cam := seedCampaign(t, st, store.CampaignRunning)

	loop := &finalizer{st: st, log: discardLogger(), cfg: &c.cfg, skips: map[string]int{}}
	if err := loop.Tick(context.Background(), baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := getCampaign(t, st, cam.ID); got.Status != store.CampaignRunning {
		t.Fatalf("status %s, want running: a campaign with zero rows is not complete", got.Status)
	}
}

func TestFinalizerThrottlesLargeCampaigns(t *testing.T) {
	// rows=1 makes a 2-delivery campaign "large", every=3 means it is
	// recounted on one tick in three.
	_, st, _, c := newFixture(t, host.Hooks{}, WithLargeCampaign(1, 3))
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignRunning)
	seedDeliveries(t, st, cam.ID, store.DeliveryQueued, 2)

	loop := &finalizer{st: st, log: discardLogger(), cfg: &c.cfg, skips: map[string]int{}}
	if err := loop.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := getCampaign(t, st, cam.ID); !got.Stats.ComputedAt.Equal(baseTime) {
		t.Fatalf("ComputedAt %v, want %v", got.Stats.ComputedAt, baseTime)
	}

	for i := 1; i <= 2; i++ {
		at := baseTime.Add(time.Duration(i) * time.Minute)
		if err := loop.Tick(ctx, at); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if got := getCampaign(t, st, cam.ID); !got.Stats.ComputedAt.Equal(baseTime) {
			t.Fatalf("tick %d recounted a large campaign: ComputedAt %v", i, got.Stats.ComputedAt)
		}
	}

	fourth := baseTime.Add(3 * time.Minute)
	if err := loop.Tick(ctx, fourth); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := getCampaign(t, st, cam.ID); !got.Stats.ComputedAt.Equal(fourth) {
		t.Fatalf("ComputedAt %v, want the 4th tick %v", got.Stats.ComputedAt, fourth)
	}
}

func TestCancellerMovesRowsInChunks(t *testing.T) {
	_, st, _, _ := newFixture(t, host.Hooks{})
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignCancelled)
	seedDeliveries(t, st, cam.ID, store.DeliveryQueued, 5)
	// A leased row is left for the sender to finish (architecture 4.1).
	seedDeliveries(t, st, cam.ID, store.DeliveryLeased, 1, func(d *store.Delivery) {
		d.Email, d.EmailNorm = "leased@example.com", "leased@example.com"
	})

	loop := &canceller{st: st, log: discardLogger(), page: 100, chunk: 2}
	want := []int64{3, 1, 0, 0}
	for i, remaining := range want {
		if err := loop.Tick(ctx, baseTime); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
		counts, err := st.Deliveries().CountByStatus(ctx, cam.ID)
		if err != nil {
			t.Fatalf("CountByStatus: %v", err)
		}
		if counts[store.DeliveryQueued] != remaining {
			t.Fatalf("after tick %d: %d queued, want %d", i, counts[store.DeliveryQueued], remaining)
		}
		if counts[store.DeliveryLeased] != 1 {
			t.Fatalf("after tick %d: leased count %d, want the leased row untouched", i, counts[store.DeliveryLeased])
		}
	}
	counts, _ := st.Deliveries().CountByStatus(ctx, cam.ID)
	if counts[store.DeliveryCancelled] != 5 {
		t.Fatalf("%d cancelled, want 5", counts[store.DeliveryCancelled])
	}
}

func TestLeaseReaperReleasesExpired(t *testing.T) {
	_, st, _, _ := newFixture(t, host.Hooks{})
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignRunning)
	seedDeliveries(t, st, cam.ID, store.DeliveryLeased, 1, func(d *store.Delivery) {
		d.LeaseOwner = "dead-worker"
		d.LeaseUntil = baseTime.Add(-time.Second)
	})
	seedDeliveries(t, st, cam.ID, store.DeliveryLeased, 1, func(d *store.Delivery) {
		d.Email, d.EmailNorm = "live@example.com", "live@example.com"
		d.LeaseOwner = "live-worker"
		d.LeaseUntil = baseTime.Add(time.Minute)
	})

	loop := &leaseReaper{st: st, log: discardLogger(), limit: 5000}
	if err := loop.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	counts, _ := st.Deliveries().CountByStatus(ctx, cam.ID)
	if counts[store.DeliveryQueued] != 1 {
		t.Errorf("%d queued, want the expired lease back in the queue", counts[store.DeliveryQueued])
	}
	if counts[store.DeliveryLeased] != 1 {
		t.Errorf("%d leased, want the live lease untouched", counts[store.DeliveryLeased])
	}
}

func TestRetentionDeletesFinishedCampaigns(t *testing.T) {
	_, st, clk, c := newFixture(t, host.Hooks{}, WithBatches(Batches{RetentionChunk: 2}))
	ctx := context.Background()

	settings := store.DefaultTenantSettings(testTenant, baseTime)
	settings.RetentionDays = 30
	if err := st.TenantSettings().Create(ctx, settings); err != nil {
		t.Fatalf("TenantSettings.Create: %v", err)
	}

	// Rows created a hundred days ago, on a campaign completed then too.
	clk.Advance(-100 * 24 * time.Hour)
	old := seedCampaign(t, st, store.CampaignCompleted, func(c *store.Campaign) {
		c.CompletedAt = clk.Now()
	})
	seedDeliveries(t, st, old.ID, store.DeliverySent, 5)
	recent := seedCampaign(t, st, store.CampaignCompleted, func(c *store.Campaign) {
		c.CompletedAt = baseTime.Add(-time.Hour)
	})
	seedDeliveries(t, st, recent.ID, store.DeliverySent, 2)
	clk.Advance(100 * 24 * time.Hour)

	loop := &retention{st: st, tenant: testTenant, log: discardLogger(), cfg: &c.cfg}
	if err := loop.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if counts, _ := st.Deliveries().CountByStatus(ctx, old.ID); len(counts) != 0 {
		t.Errorf("old campaign still has %v", counts)
	}
	if counts, _ := st.Deliveries().CountByStatus(ctx, recent.ID); counts[store.DeliverySent] != 2 {
		t.Errorf("recent campaign lost rows: %v", counts)
	}
}

// Retention covers more than deliveries: tracking and bounce events and
// dispatched outbox rows fall under the same cutoff (architecture 9.4, 16).
// Pending outbox rows never do, however old they are.
func TestRetentionDeletesTrackingBouncesAndDispatchedOutbox(t *testing.T) {
	_, st, _, c := newFixture(t, host.Hooks{}, WithBatches(Batches{RetentionChunk: 2}))
	ctx := context.Background()

	settings := store.DefaultTenantSettings(testTenant, baseTime)
	settings.RetentionDays = 30
	if err := st.TenantSettings().Create(ctx, settings); err != nil {
		t.Fatalf("TenantSettings.Create: %v", err)
	}

	old := baseTime.Add(-100 * 24 * time.Hour)
	cam := seedCampaign(t, st, store.CampaignCompleted, func(c *store.Campaign) {
		c.CompletedAt = old
	})

	evs := make([]store.TrackingEvent, 0, 5)
	for range 5 {
		evs = append(evs, store.TrackingEvent{
			ID: store.NewID(), DeliveryID: store.NewID(), CampaignID: cam.ID,
			Kind: store.TrackingOpen, LinkNo: -1, CreatedAt: old,
		})
	}
	evs = append(evs, store.TrackingEvent{
		ID: store.NewID(), DeliveryID: store.NewID(), CampaignID: cam.ID,
		Kind: store.TrackingOpen, LinkNo: -1, CreatedAt: baseTime.Add(-time.Hour),
	})
	if err := st.Tracking().InsertEvents(ctx, evs); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	for _, at := range []time.Time{old, baseTime.Add(-time.Hour)} {
		if err := st.Bounces().Create(ctx, &store.BounceEvent{
			Type: store.BounceHard, EmailNorm: "a@example.com", CreatedAt: at,
		}); err != nil {
			t.Fatalf("Bounces.Create: %v", err)
		}
	}

	dispatched, pending := store.NewID(), store.NewID()
	if err := st.Outbox().Enqueue(ctx, []store.OutboxEvent{
		{ID: dispatched, Type: "campaign.completed", CreatedAt: old, NextAttemptAt: old},
		{ID: pending, Type: "campaign.started", CreatedAt: old, NextAttemptAt: old},
	}); err != nil {
		t.Fatalf("Outbox.Enqueue: %v", err)
	}
	if err := st.Outbox().MarkDelivered(ctx, dispatched, old); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	loop := &retention{st: st, tenant: testTenant, log: discardLogger(), cfg: &c.cfg}
	if err := loop.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	counts, err := st.Tracking().CountUnique(ctx, cam.ID)
	if err != nil {
		t.Fatalf("CountUnique: %v", err)
	}
	if counts.UniqueOpens != 1 {
		t.Errorf("unique opens = %d, want the one inside the retention window", counts.UniqueOpens)
	}
	bounces, err := st.Bounces().List(ctx, store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("Bounces.List: %v", err)
	}
	if len(bounces.Items) != 1 {
		t.Errorf("%d bounces left, want the one inside the retention window", len(bounces.Items))
	}
	delivered, err := st.Outbox().List(ctx, store.OutboxDelivered, store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("Outbox.List: %v", err)
	}
	if len(delivered.Items) != 0 {
		t.Errorf("%d dispatched outbox rows left, want none", len(delivered.Items))
	}
	still, err := st.Outbox().List(ctx, store.OutboxPending, store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("Outbox.List: %v", err)
	}
	if len(still.Items) != 1 {
		t.Errorf("%d pending outbox rows left, want the undelivered one kept", len(still.Items))
	}
}

// Retention creates the settings row when a tenant has none, rather than
// treating "no row" as "no retention" (store.LoadTenantSettings).
func TestRetentionCreatesMissingSettings(t *testing.T) {
	_, st, _, c := newFixture(t, host.Hooks{})
	ctx := context.Background()

	loop := &retention{st: st, tenant: testTenant, log: discardLogger(), cfg: &c.cfg}
	if err := loop.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got, err := st.TenantSettings().Get(ctx)
	if err != nil {
		t.Fatalf("TenantSettings.Get: %v", err)
	}
	if got.RetentionDays != store.DefaultTenantSettings(testTenant, baseTime).RetentionDays {
		t.Errorf("RetentionDays = %d, want the default", got.RetentionDays)
	}
}

func TestRetentionDisabled(t *testing.T) {
	_, st, _, c := newFixture(t, host.Hooks{})
	ctx := context.Background()

	settings := store.DefaultTenantSettings(testTenant, baseTime)
	settings.RetentionDays = 0
	if err := st.TenantSettings().Create(ctx, settings); err != nil {
		t.Fatalf("TenantSettings.Create: %v", err)
	}
	cam := seedCampaign(t, st, store.CampaignCompleted, func(c *store.Campaign) {
		c.CompletedAt = baseTime.Add(-10000 * time.Hour)
	})
	seedDeliveries(t, st, cam.ID, store.DeliverySent, 2)

	loop := &retention{st: st, tenant: testTenant, log: discardLogger(), cfg: &c.cfg}
	if err := loop.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if counts, _ := st.Deliveries().CountByStatus(ctx, cam.ID); counts[store.DeliverySent] != 2 {
		t.Fatalf("retention ran with RetentionDays=0: %v", counts)
	}
}

// BUG-1 of test/e2e/README.md: a recipient opens the mail after the campaign
// completed, and the cached uniques of architecture 9.3 have to follow. The
// status counts must not be recounted: the campaign is over.
func TestFinalizerRefreshesTrackingOfCompletedCampaigns(t *testing.T) {
	_, st, clk, c := newFixture(t, host.Hooks{})
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignRunning)
	ids := seedDeliveries(t, st, cam.ID, store.DeliverySent, 2)

	loop := &finalizer{st: st, log: discardLogger(), cfg: &c.cfg, skips: map[string]int{}}
	if err := loop.Tick(ctx, clk.Now()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got := getCampaign(t, st, cam.ID)
	if got.Status != store.CampaignCompleted {
		t.Fatalf("status %s, want completed", got.Status)
	}
	if got.Stats.UniqueOpens != 0 {
		t.Fatalf("unique_opens = %d before anyone opened anything", got.Stats.UniqueOpens)
	}

	// The open arrives five minutes after the campaign finished, which is the
	// normal case for every real campaign.
	clk.Advance(5 * time.Minute)
	b := c.Tracking()
	b.Record(trackingEvent(testTenant, ids[0], cam.ID, store.TrackingOpen, clk.Now()))
	b.Record(trackingEvent(testTenant, ids[1], cam.ID, store.TrackingOpen, clk.Now()))
	b.Record(trackingEvent(testTenant, ids[1], cam.ID, store.TrackingUnsubscribed, clk.Now()))
	if err := b.Close(); err != nil {
		t.Fatalf("tracking Close: %v", err)
	}

	if err := loop.Tick(ctx, clk.Now()); err != nil {
		t.Fatalf("refresh Tick: %v", err)
	}
	got = getCampaign(t, st, cam.ID)
	if got.Stats.UniqueOpens != 2 || got.Stats.Unsubscribed != 1 {
		t.Fatalf("stats = %+v, want unique_opens 2 and unsubscribed 1", got.Stats)
	}
	if got.Stats.ByStatus[store.DeliverySent] != 2 {
		t.Errorf("the refresh dropped the status counts: %+v", got.Stats.ByStatus)
	}
	if !got.Stats.ComputedAt.Equal(clk.Now()) {
		t.Errorf("ComputedAt = %v, want %v", got.Stats.ComputedAt, clk.Now())
	}
	if got.Status != store.CampaignCompleted || !got.CompletedAt.Equal(baseTime) {
		t.Errorf("the refresh moved the campaign: status %s completed_at %v", got.Status, got.CompletedAt)
	}
}

// The cadence decays: every tick for the first hour, then every ten minutes,
// and never once the campaign is older than the refresh window.
func TestStatsRefreshDueDecays(t *testing.T) {
	const window = 14 * 24 * time.Hour
	completed := baseTime
	cases := []struct {
		name string
		age  time.Duration
		last time.Duration // ComputedAt relative to completion; -1 = never
		want bool
	}{
		{"just completed", time.Second, 0, true},
		{"inside the hot hour, computed a second ago", 30 * time.Minute, 30*time.Minute - time.Second, true},
		{"cold, computed a minute ago", 2 * time.Hour, 2*time.Hour - time.Minute, false},
		{"cold, computed eleven minutes ago", 2 * time.Hour, 2*time.Hour - 11*time.Minute, true},
		{"cold, never computed", 2 * time.Hour, -1, true},
		{"past the window", window + time.Hour, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &store.Campaign{CompletedAt: completed}
			if tc.last >= 0 {
				c.Stats.ComputedAt = completed.Add(tc.last)
			}
			if got := statsRefreshDue(c, completed.Add(tc.age), window); got != tc.want {
				t.Errorf("statsRefreshDue = %v, want %v", got, tc.want)
			}
		})
	}
}

// The walk is bounded per tick and resumes where it stopped, so a tenant with
// more completed campaigns than the scan budget is covered over several ticks
// instead of being either skipped or paid for in full every ten seconds.
func TestFinalizerRefreshWalkIsBoundedAndResumes(t *testing.T) {
	_, st, clk, c := newFixture(t, host.Hooks{},
		WithBatches(Batches{CampaignPage: 2, StatsRefreshScan: 2}))
	ctx := context.Background()

	const n = 5
	var ids []string
	for i := 0; i < n; i++ {
		cam := seedCampaign(t, st, store.CampaignCompleted, func(c *store.Campaign) {
			c.CompletedAt = clk.Now()
		})
		ids = append(ids, cam.ID)
		d := seedDeliveries(t, st, cam.ID, store.DeliverySent, 1)
		if err := st.Tracking().InsertEvents(ctx, []store.TrackingEvent{
			trackingEvent(testTenant, d[0], cam.ID, store.TrackingOpen, clk.Now()),
		}); err != nil {
			t.Fatalf("InsertEvents: %v", err)
		}
	}

	loop := &finalizer{st: st, log: discardLogger(), cfg: &c.cfg, skips: map[string]int{}}
	refreshed := func() int {
		n := 0
		for _, id := range ids {
			if getCampaign(t, st, id).Stats.UniqueOpens == 1 {
				n++
			}
		}
		return n
	}
	if err := loop.Tick(ctx, clk.Now()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := refreshed(); got != 2 {
		t.Fatalf("%d campaigns refreshed on the first tick, want the 2 the budget allows", got)
	}
	for i := 0; i < 3; i++ {
		if err := loop.Tick(ctx, clk.Now()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	if got := refreshed(); got != n {
		t.Fatalf("%d of %d campaigns refreshed after four ticks, want all of them", got, n)
	}
}
