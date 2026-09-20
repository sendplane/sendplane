package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func testSuppressions(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	r := s.Suppressions()
	now := time.Now().UTC()

	ok, _, err := r.IsSuppressed(ctx, "nobody@example.com", now)
	must(t, "IsSuppressed unknown", err)
	eq(t, "unknown is not suppressed", ok, false)

	must(t, "Upsert", r.Upsert(ctx, &store.Suppression{
		EmailNorm: "hard@example.com", Reason: store.SuppressionHardBounce,
		SourceDeliveryID: store.NewID(), CreatedAt: now,
	}))
	must(t, "Upsert temporary", r.Upsert(ctx, &store.Suppression{
		EmailNorm: "temp@example.com", Reason: store.SuppressionManual,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}))

	ok, got, err := r.IsSuppressed(ctx, "hard@example.com", now)
	must(t, "IsSuppressed", err)
	eq(t, "suppressed", ok, true)
	eq(t, "reason", got.Reason, store.SuppressionHardBounce)
	eq(t, "tenant", got.TenantID, tenantID)

	ok, _, err = r.IsSuppressed(ctx, "temp@example.com", now.Add(time.Minute))
	must(t, "IsSuppressed before expiry", err)
	eq(t, "still suppressed", ok, true)
	ok, _, err = r.IsSuppressed(ctx, "temp@example.com", now.Add(2*time.Hour))
	must(t, "IsSuppressed after expiry", err)
	eq(t, "expired", ok, false)

	// Upsert replaces rather than duplicating.
	must(t, "Upsert again", r.Upsert(ctx, &store.Suppression{
		EmailNorm: "hard@example.com", Reason: store.SuppressionComplaint, CreatedAt: now,
	}))
	_, got, err = r.IsSuppressed(ctx, "hard@example.com", now)
	must(t, "IsSuppressed after upsert", err)
	eq(t, "reason replaced", got.Reason, store.SuppressionComplaint)

	list, err := r.List(ctx, store.Page{Limit: 100})
	must(t, "List", err)
	eq(t, "list size", len(list.Items), 2)

	// DeleteBefore is the ADR-0008 retention path: only entries that actually
	// expire are eligible, and "never expires" survives any cutoff.
	n, err := r.DeleteBefore(ctx, now.Add(24*time.Hour), 10)
	must(t, "DeleteBefore", err)
	eq(t, "only the expiring entry", n, 1)
	ok, _, err = r.IsSuppressed(ctx, "hard@example.com", now)
	must(t, "IsSuppressed after DeleteBefore", err)
	eq(t, "permanent entry survived", ok, true)
	list, err = r.List(ctx, store.Page{Limit: 100})
	must(t, "List after DeleteBefore", err)
	eq(t, "one row left", len(list.Items), 1)

	n, err = r.DeleteBefore(ctx, now.Add(24*time.Hour), 10)
	must(t, "DeleteBefore again", err)
	eq(t, "nothing left to expire", n, 0)

	must(t, "Delete", r.Delete(ctx, "hard@example.com"))
	ok, _, err = r.IsSuppressed(ctx, "hard@example.com", now)
	must(t, "IsSuppressed after delete", err)
	eq(t, "not suppressed after delete", ok, false)
	mustBe(t, "Delete twice", r.Delete(ctx, "hard@example.com"), store.ErrNotFound)
}

func testBounces(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	r := s.Bounces()
	now := time.Now().UTC()
	deliveryID := store.NewID()

	b := &store.BounceEvent{
		DeliveryID: deliveryID, Type: store.BounceHard, Source: store.BounceSourceVERP,
		Verified: true, Recipient: "a@example.com", EmailNorm: "a@example.com",
		SMTPStatus: "5.1.1", DiagnosticCode: "smtp; 550 5.1.1 unknown",
		Raw: []byte(`{"raw":"..."}`), ReceivedAt: now,
	}
	must(t, "Create", r.Create(ctx, b))
	eq(t, "tenant", b.TenantID, tenantID)
	must(t, "Create unverified", r.Create(ctx, &store.BounceEvent{
		Type: store.BounceComplaint, Source: store.BounceSourceHeuristic, Verified: false,
	}))

	got, err := r.Get(ctx, b.ID)
	must(t, "Get", err)
	eq(t, "type", got.Type, store.BounceHard)
	eq(t, "source", got.Source, store.BounceSourceVERP)
	_, err = r.Get(ctx, store.NewID())
	mustBe(t, "Get unknown", err, store.ErrNotFound)

	list, err := r.List(ctx, store.Page{Limit: 100})
	must(t, "List", err)
	eq(t, "list size", len(list.Items), 2)

	list, err = r.ListByDelivery(ctx, deliveryID, store.Page{Limit: 100})
	must(t, "ListByDelivery", err)
	eq(t, "by delivery", len(list.Items), 1)

	// Retention (architecture 16). CreatedAt is stamped by the store, so the
	// cutoff is relative to "now": nothing is old enough yet, then everything
	// is, one chunk at a time.
	n, err := r.DeleteBefore(ctx, now.Add(-time.Hour), 10)
	must(t, "DeleteBefore cutoff in the past", err)
	eq(t, "nothing that old", n, 0)

	n, err = r.DeleteBefore(ctx, now.Add(time.Hour), 1)
	must(t, "DeleteBefore chunk", err)
	eq(t, "one chunk", n, 1)
	list, err = r.List(ctx, store.Page{Limit: 100})
	must(t, "List after chunk", err)
	eq(t, "one left", len(list.Items), 1)

	n, err = r.DeleteBefore(ctx, now.Add(time.Hour), 10)
	must(t, "DeleteBefore rest", err)
	eq(t, "the rest", n, 1)
	n, err = r.DeleteBefore(ctx, now.Add(time.Hour), 10)
	must(t, "DeleteBefore empty", err)
	eq(t, "nothing left", n, 0)
}

func testTracking(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Tracking()
	now := time.Now().UTC()
	campaignID := store.NewID()
	a, b := store.NewID(), store.NewID()

	ev := func(d string, kind store.TrackingKind, url string, linkNo int, bot bool) store.TrackingEvent {
		return store.TrackingEvent{
			ID: store.NewID(), DeliveryID: d, CampaignID: campaignID, Kind: kind,
			URL: url, LinkNo: linkNo, UserAgent: "test", SuspectedBot: bot, CreatedAt: now,
		}
	}
	must(t, "InsertEvents", r.InsertEvents(ctx, []store.TrackingEvent{
		ev(a, store.TrackingOpen, "", -1, false),
		ev(a, store.TrackingOpen, "", -1, false), // same delivery: still one unique open
		ev(b, store.TrackingOpen, "", -1, false),
		ev(a, store.TrackingClick, "https://example.com/a", 0, false),
		ev(b, store.TrackingClick, "https://example.com/a", 0, false),
		ev(b, store.TrackingClick, "https://example.com/b", 1, false),
		ev(a, store.TrackingUnsubscribeClicked, "", -1, false),
		ev(a, store.TrackingUnsubscribed, "", -1, false),
		// A scanner's click counts for nothing.
		ev(store.NewID(), store.TrackingClick, "https://example.com/a", 0, true),
		ev(store.NewID(), store.TrackingOpen, "", -1, true),
	}))
	// Another campaign must not leak into the counts.
	must(t, "InsertEvents other", r.InsertEvents(ctx, []store.TrackingEvent{{
		ID: store.NewID(), DeliveryID: store.NewID(), CampaignID: store.NewID(),
		Kind: store.TrackingOpen, CreatedAt: now,
	}}))

	counts, err := r.CountUnique(ctx, campaignID)
	must(t, "CountUnique", err)
	eq(t, "unique opens", counts.UniqueOpens, int64(2))
	eq(t, "unique clicks", counts.UniqueClicks, int64(2))
	eq(t, "unsubscribe clicked", counts.UnsubscribeClicked, int64(1))
	eq(t, "unsubscribed", counts.Unsubscribed, int64(1))

	links, err := r.LinkClicks(ctx, campaignID)
	must(t, "LinkClicks", err)
	eq(t, "links", len(links), 2)
	eq(t, "link 0 url", links[0].URL, "https://example.com/a")
	eq(t, "link 0 clicks", links[0].Clicks, int64(2))
	eq(t, "link 0 unique", links[0].UniqueClicks, int64(2))
	eq(t, "link 1 url", links[1].URL, "https://example.com/b")
	eq(t, "link 1 clicks", links[1].Clicks, int64(1))

	empty, err := r.CountUnique(ctx, store.NewID())
	must(t, "CountUnique unknown", err)
	eq(t, "unknown campaign opens", empty.UniqueOpens, int64(0))

	// Retention: tracking events live as long as the deliveries they describe
	// (architecture 9.4), so the whole tenant is walked, not one campaign.
	n, err := r.DeleteBefore(ctx, now.Add(-time.Hour), 100)
	must(t, "DeleteBefore cutoff in the past", err)
	eq(t, "nothing that old", n, 0)

	n, err = r.DeleteBefore(ctx, now.Add(time.Hour), 4)
	must(t, "DeleteBefore chunk", err)
	eq(t, "one chunk", n, 4)

	total := 4
	for {
		n, err = r.DeleteBefore(ctx, now.Add(time.Hour), 4)
		must(t, "DeleteBefore rest", err)
		total += n
		if n < 4 {
			break
		}
	}
	eq(t, "every event deleted", total, 11)
	counts, err = r.CountUnique(ctx, campaignID)
	must(t, "CountUnique after retention", err)
	eq(t, "counts cleared", counts.UniqueOpens, int64(0))
}

func testOutbox(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	r := s.Outbox()
	now := time.Now().UTC()

	must(t, "Enqueue", r.Enqueue(ctx, []store.OutboxEvent{
		{ID: store.NewID(), Type: "delivery.failed", Payload: []byte(`{"id":"1"}`), CreatedAt: now, NextAttemptAt: now},
		{ID: store.NewID(), Type: "campaign.completed", Payload: []byte(`{"id":"2"}`), CreatedAt: now, NextAttemptAt: now},
	}))

	claimed, err := r.ClaimPending(ctx, 10, time.Minute, "d1", now)
	must(t, "ClaimPending", err)
	eq(t, "claimed", len(claimed), 2)
	eq(t, "tenant", claimed[0].TenantID, tenantID)

	// A second dispatcher gets nothing while the lease holds.
	again, err := r.ClaimPending(ctx, 10, time.Minute, "d2", now)
	must(t, "ClaimPending again", err)
	eq(t, "leased away", len(again), 0)

	must(t, "MarkDelivered", r.MarkDelivered(ctx, claimed[0].ID, now.Add(time.Second)))
	must(t, "MarkFailed", r.MarkFailed(ctx, claimed[1].ID, now.Add(time.Minute), "502 bad gateway"))

	none, err := r.ClaimPending(ctx, 10, time.Minute, "d2", now.Add(10*time.Second))
	must(t, "ClaimPending before retry", err)
	eq(t, "backoff respected", len(none), 0)

	retry, err := r.ClaimPending(ctx, 10, time.Minute, "d2", now.Add(2*time.Minute))
	must(t, "ClaimPending after retry", err)
	eq(t, "retried", len(retry), 1)
	eq(t, "attempts counted", retry[0].Attempts, 1)
	eq(t, "last error", retry[0].LastError, "502 bad gateway")

	// An empty next attempt means dead-letter.
	must(t, "MarkFailed final", r.MarkFailed(ctx, retry[0].ID, time.Time{}, "gave up"))
	dead, err := r.List(ctx, store.OutboxFailed, store.Page{Limit: 10})
	must(t, "List failed", err)
	eq(t, "dead letters", len(dead.Items), 1)
	delivered, err := r.List(ctx, store.OutboxDelivered, store.Page{Limit: 10})
	must(t, "List delivered", err)
	eq(t, "delivered", len(delivered.Items), 1)

	mustBe(t, "MarkDelivered unknown", r.MarkDelivered(ctx, store.NewID(), now), store.ErrNotFound)
	mustBe(t, "MarkFailed unknown", r.MarkFailed(ctx, store.NewID(), now, "x"), store.ErrNotFound)

	// Get reads one row by ID; Reset is the replay verb and gives a dead
	// letter a full attempt budget again.
	one, err := r.Get(ctx, dead.Items[0].ID)
	must(t, "Get", err)
	eq(t, "Get id", one.ID, dead.Items[0].ID)
	eq(t, "Get status", one.Status, store.OutboxFailed)
	eq(t, "Get tenant", one.TenantID, tenantID)
	if one.Attempts == 0 {
		t.Fatalf("Get attempts = 0, want the failed count")
	}
	_, err = r.Get(ctx, store.NewID())
	mustBe(t, "Get unknown", err, store.ErrNotFound)

	replayAt := now.Add(3 * time.Minute)
	must(t, "Reset", r.Reset(ctx, one.ID, replayAt))
	one, err = r.Get(ctx, one.ID)
	must(t, "Get after Reset", err)
	eq(t, "Reset status", one.Status, store.OutboxPending)
	eq(t, "Reset attempts", one.Attempts, 0)
	eq(t, "Reset last error", one.LastError, "")
	eqTime(t, "Reset next attempt", one.NextAttemptAt, replayAt)
	replayed, err := r.ClaimPending(ctx, 10, time.Minute, "d3", replayAt)
	must(t, "ClaimPending after Reset", err)
	eq(t, "replayed event is claimable", len(replayed), 1)
	must(t, "MarkFailed replayed", r.MarkFailed(ctx, replayed[0].ID, time.Time{}, "gave up again"))
	mustBe(t, "Reset unknown", r.Reset(ctx, store.NewID(), now), store.ErrNotFound)

	// Retention only ever removes dispatched rows: a pending event is still
	// owed to the host however old it is.
	must(t, "Enqueue pending", r.Enqueue(ctx, []store.OutboxEvent{
		{ID: store.NewID(), Type: "delivery.sent", Payload: []byte(`{"id":"3"}`), CreatedAt: now, NextAttemptAt: now},
	}))

	n, err := r.DeleteBefore(ctx, now.Add(-time.Hour), 10)
	must(t, "DeleteBefore cutoff in the past", err)
	eq(t, "nothing that old", n, 0)

	n, err = r.DeleteBefore(ctx, now.Add(time.Hour), 1)
	must(t, "DeleteBefore chunk", err)
	eq(t, "one chunk", n, 1)

	n, err = r.DeleteBefore(ctx, now.Add(time.Hour), 10)
	must(t, "DeleteBefore rest", err)
	eq(t, "the other dispatched row", n, 1)

	n, err = r.DeleteBefore(ctx, now.Add(time.Hour), 10)
	must(t, "DeleteBefore again", err)
	eq(t, "pending rows are never deleted", n, 0)

	pending, err := r.List(ctx, store.OutboxPending, store.Page{Limit: 10})
	must(t, "List pending", err)
	eq(t, "pending survived", len(pending.Items), 1)
	gone, err := r.List(ctx, store.OutboxDelivered, store.Page{Limit: 10})
	must(t, "List delivered", err)
	eq(t, "delivered gone", len(gone.Items), 0)
	gone, err = r.List(ctx, store.OutboxFailed, store.Page{Limit: 10})
	must(t, "List failed", err)
	eq(t, "dead letters gone", len(gone.Items), 0)
}

func testLocks(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Locks()
	now := time.Now().UTC()
	const name = "scheduler"

	ok, err := r.Acquire(ctx, name, "w1", time.Minute, now)
	must(t, "Acquire", err)
	eq(t, "acquired", ok, true)

	ok, err = r.Acquire(ctx, name, "w2", time.Minute, now.Add(time.Second))
	must(t, "Acquire contended", err)
	eq(t, "second owner blocked", ok, false)

	ok, err = r.Renew(ctx, name, "w2", time.Minute, now.Add(time.Second))
	must(t, "Renew wrong owner", err)
	eq(t, "renew by non-owner", ok, false)

	ok, err = r.Renew(ctx, name, "w1", time.Minute, now.Add(30*time.Second))
	must(t, "Renew", err)
	eq(t, "renewed", ok, true)

	// Still held after the original TTL because of the renewal.
	ok, err = r.Acquire(ctx, name, "w2", time.Minute, now.Add(70*time.Second))
	must(t, "Acquire after renew", err)
	eq(t, "renewal respected", ok, false)

	// Taken over once the lease really expired.
	ok, err = r.Acquire(ctx, name, "w2", time.Minute, now.Add(5*time.Minute))
	must(t, "Acquire after expiry", err)
	eq(t, "expired lock taken over", ok, true)

	ok, err = r.Renew(ctx, name, "w1", time.Minute, now.Add(5*time.Minute))
	must(t, "Renew after takeover", err)
	eq(t, "old owner cannot renew", ok, false)

	got, err := r.Get(ctx, name)
	must(t, "Get", err)
	eq(t, "owner", got.Owner, "w2")

	// Releasing as a non-owner does nothing.
	must(t, "Release non-owner", r.Release(ctx, name, "w1"))
	got, err = r.Get(ctx, name)
	must(t, "Get after non-owner release", err)
	eq(t, "still held", got.Owner, "w2")

	must(t, "Release", r.Release(ctx, name, "w2"))
	_, err = r.Get(ctx, name)
	mustBe(t, "Get after release", err, store.ErrNotFound)

	ok, err = r.Acquire(ctx, name, "w3", time.Minute, now.Add(6*time.Minute))
	must(t, "Acquire after release", err)
	eq(t, "free again", ok, true)
}

func testWorkers(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	r := s.Workers()
	now := time.Now().UTC()

	must(t, "Heartbeat w1", r.Heartbeat(ctx, store.Worker{
		ID: "w1", Role: store.WorkerRoleSender, Lanes: []store.Lane{store.LaneBulk, store.LaneTransactional},
		Concurrency: 64, StartedAt: now, LastSeenAt: now,
	}))
	must(t, "Heartbeat w2", r.Heartbeat(ctx, store.Worker{
		ID: "w2", Role: store.WorkerRoleSender, Concurrency: 32, StartedAt: now, LastSeenAt: now.Add(5 * time.Minute),
	}))

	active, err := r.ListActive(ctx, now)
	must(t, "ListActive all", err)
	eq(t, "two active", len(active), 2)
	eq(t, "tenant", active[0].TenantID, tenantID)
	eq(t, "lanes", len(active[0].Lanes), 2)

	active, err = r.ListActive(ctx, now.Add(time.Minute))
	must(t, "ListActive recent", err)
	eq(t, "one recent", len(active), 1)
	eq(t, "recent worker", active[0].ID, "w2")

	// A heartbeat refreshes rather than duplicating.
	must(t, "Heartbeat w1 again", r.Heartbeat(ctx, store.Worker{
		ID: "w1", Role: store.WorkerRoleSender, Concurrency: 64, LastSeenAt: now.Add(10 * time.Minute),
	}))
	active, err = r.ListActive(ctx, now.Add(6*time.Minute))
	must(t, "ListActive after refresh", err)
	eq(t, "one active", len(active), 1)
	eq(t, "refreshed worker", active[0].ID, "w1")

	eq(t, "role", active[0].Role, store.WorkerRoleSender)

	mustBe(t, "Heartbeat without ID", r.Heartbeat(ctx, store.Worker{Role: store.WorkerRoleSender}), store.ErrInvalid)

}
