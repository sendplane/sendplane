package control

import (
	"context"
	"testing"
	"time"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/store"
)

func trackingEvent(tenantID, deliveryID, campaignID string, kind store.TrackingKind, at time.Time) store.TrackingEvent {
	return store.TrackingEvent{
		TenantID:   tenantID,
		DeliveryID: deliveryID,
		CampaignID: campaignID,
		Kind:       kind,
		CreatedAt:  at,
	}
}

func TestTrackingBufferFlushDerivesFirstInteractions(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{})
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignRunning)
	ids := seedDeliveries(t, st, cam.ID, store.DeliverySent, 2)

	b := c.Tracking()
	first := baseTime
	second := baseTime.Add(time.Minute)
	// Two opens for the same delivery: only the first may set the column.
	b.Record(trackingEvent(testTenant, ids[0], cam.ID, store.TrackingOpen, first))
	b.Record(trackingEvent(testTenant, ids[0], cam.ID, store.TrackingOpen, second))
	b.Record(trackingEvent(testTenant, ids[0], cam.ID, store.TrackingClick, second))
	b.Record(trackingEvent(testTenant, ids[1], cam.ID, store.TrackingUnsubscribed, second))
	// unsubscribe_clicked is the unconfirmed GET: it must not mark anyone.
	b.Record(trackingEvent(testTenant, ids[1], cam.ID, store.TrackingUnsubscribeClicked, second))

	if got := b.Stats().Buffered; got != 5 {
		t.Fatalf("Buffered = %d, want 5", got)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stats := b.Stats()
	if stats.Flushed != 5 || stats.Buffered != 0 || stats.Dropped != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 5 flushed and nothing else", stats)
	}

	counts, err := st.Tracking().CountUnique(ctx, cam.ID)
	if err != nil {
		t.Fatalf("CountUnique: %v", err)
	}
	want := store.TrackingCounts{UniqueOpens: 1, UniqueClicks: 1, UnsubscribeClicked: 1, Unsubscribed: 1}
	if counts != want {
		t.Errorf("CountUnique = %+v, want %+v", counts, want)
	}

	d0, err := st.Deliveries().Get(ctx, ids[0])
	if err != nil {
		t.Fatalf("Deliveries.Get: %v", err)
	}
	if !d0.FirstOpenedAt.Equal(first) {
		t.Errorf("FirstOpenedAt = %v, want the first open %v", d0.FirstOpenedAt, first)
	}
	if !d0.FirstClickedAt.Equal(second) {
		t.Errorf("FirstClickedAt = %v, want %v", d0.FirstClickedAt, second)
	}
	if !d0.UnsubscribedAt.IsZero() {
		t.Errorf("UnsubscribedAt = %v, want zero", d0.UnsubscribedAt)
	}

	d1, err := st.Deliveries().Get(ctx, ids[1])
	if err != nil {
		t.Fatalf("Deliveries.Get: %v", err)
	}
	if !d1.UnsubscribedAt.Equal(second) {
		t.Errorf("UnsubscribedAt = %v, want %v", d1.UnsubscribedAt, second)
	}
	if !d1.FirstOpenedAt.IsZero() {
		t.Errorf("unsubscribe_clicked set FirstOpenedAt: %v", d1.FirstOpenedAt)
	}
}

func TestTrackingBufferIgnoresSuspectedBots(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{})
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignRunning)
	ids := seedDeliveries(t, st, cam.ID, store.DeliverySent, 1)

	b := c.Tracking()
	bot := trackingEvent(testTenant, ids[0], cam.ID, store.TrackingOpen, baseTime)
	bot.SuspectedBot = true
	b.Record(bot)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	d, err := st.Deliveries().Get(ctx, ids[0])
	if err != nil {
		t.Fatalf("Deliveries.Get: %v", err)
	}
	if !d.FirstOpenedAt.IsZero() {
		t.Errorf("a suspected bot set FirstOpenedAt to %v", d.FirstOpenedAt)
	}
	// The raw record is still kept (architecture 9.3).
	if got := b.Stats().Flushed; got != 1 {
		t.Errorf("Flushed = %d, want the bot event stored anyway", got)
	}
}

func TestTrackingBufferDropsOldestWhenFull(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{},
		WithTracking(time.Hour, 1_000_000, 3))
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignRunning)
	ids := seedDeliveries(t, st, cam.ID, store.DeliverySent, 5)

	b := c.Tracking()
	for i, id := range ids {
		b.Record(trackingEvent(testTenant, id, cam.ID, store.TrackingOpen, baseTime.Add(time.Duration(i)*time.Second)))
	}
	if got := b.Stats(); got.Buffered != 3 || got.Dropped != 2 {
		t.Fatalf("stats = %+v, want 3 buffered and 2 dropped", got)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The two oldest went; the three newest survived.
	for i, id := range ids {
		d, err := st.Deliveries().Get(ctx, id)
		if err != nil {
			t.Fatalf("Deliveries.Get: %v", err)
		}
		if want := i >= 2; d.FirstOpenedAt.IsZero() == want {
			t.Errorf("delivery %d: FirstOpenedAt zero = %v, want kept = %v", i, d.FirstOpenedAt.IsZero(), want)
		}
	}
}

func TestTrackingBufferFlushesOnSizeThreshold(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{},
		// An hour-long interval: only the size threshold can flush this.
		WithTracking(time.Hour, 3, 1000))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cam := seedCampaign(t, st, store.CampaignRunning)
	ids := seedDeliveries(t, st, cam.ID, store.DeliverySent, 3)

	b := c.Tracking()
	b.start(ctx)
	t.Cleanup(func() { _ = b.Close() })

	for _, id := range ids {
		b.Record(trackingEvent(testTenant, id, cam.ID, store.TrackingOpen, baseTime))
	}
	waitFor(t, time.Second, func() bool { return b.Stats().Flushed == 3 })
	if got := b.Stats().Buffered; got != 0 {
		t.Errorf("Buffered = %d after the threshold flush", got)
	}
}

func TestTrackingBufferFlushesOnInterval(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{},
		WithTracking(5*time.Millisecond, 1_000_000, 1000))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cam := seedCampaign(t, st, store.CampaignRunning)
	ids := seedDeliveries(t, st, cam.ID, store.DeliverySent, 1)

	b := c.Tracking()
	b.start(ctx)
	t.Cleanup(func() { _ = b.Close() })

	b.Record(trackingEvent(testTenant, ids[0], cam.ID, store.TrackingOpen, baseTime))
	waitFor(t, time.Second, func() bool { return b.Stats().Flushed == 1 })
}

func TestTrackingBufferGroupsTenants(t *testing.T) {
	p, st, _, c := newFixture(t, sendplane.Hooks{})
	ctx := context.Background()

	other, err := p.ForTenant(ctx, "t2")
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	cam1 := seedCampaign(t, st, store.CampaignRunning)
	ids1 := seedDeliveries(t, st, cam1.ID, store.DeliverySent, 1)
	cam2 := seedCampaign(t, other, store.CampaignRunning)
	ids2 := seedDeliveries(t, other, cam2.ID, store.DeliverySent, 1)

	b := c.Tracking()
	b.Record(trackingEvent(testTenant, ids1[0], cam1.ID, store.TrackingOpen, baseTime))
	b.Record(trackingEvent("t2", ids2[0], cam2.ID, store.TrackingOpen, baseTime))
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, tc := range []struct {
		st  store.Store
		cam string
	}{{st, cam1.ID}, {other, cam2.ID}} {
		counts, err := tc.st.Tracking().CountUnique(ctx, tc.cam)
		if err != nil {
			t.Fatalf("CountUnique: %v", err)
		}
		if counts.UniqueOpens != 1 {
			t.Errorf("campaign %s: UniqueOpens = %d, want 1", tc.cam, counts.UniqueOpens)
		}
	}
}

func TestTrackingBufferCloseIsIdempotent(t *testing.T) {
	_, _, _, c := newFixture(t, sendplane.Hooks{})
	b := c.Tracking()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Records after Close are counted, not written.
	b.Record(trackingEvent(testTenant, "gone", "c", store.TrackingOpen, baseTime))
	if got := b.Stats(); got.Dropped != 1 || got.Buffered != 0 {
		t.Fatalf("stats after Close = %+v, want 1 dropped", got)
	}
}
