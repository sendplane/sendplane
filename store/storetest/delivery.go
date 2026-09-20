package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func seed(t *testing.T, s store.Store, ds []store.Delivery) {
	t.Helper()
	n, err := s.Deliveries().InsertBatch(context.Background(), ds)
	must(t, "InsertBatch", err)
	eq(t, "InsertBatch count", n, len(ds))
}

func claimIDs(batch []store.Delivery) []string {
	ids := make([]string, len(batch))
	for i := range batch {
		ids[i] = batch[i].ID
	}
	return ids
}

func eqIDs(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v (%d), want %v (%d)", what, got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

func testInsertBatchIdempotent(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	campaignID := store.NewID()

	mk := func(email string) store.Delivery {
		return store.Delivery{
			ID: store.NewID(), CampaignID: campaignID, VersionID: "v", SenderID: "s",
			Lane: store.LaneBulk, Status: store.DeliveryQueued,
			Email: email, EmailNorm: email, NextAttemptAt: now,
		}
	}
	batch := []store.Delivery{mk("a@example.com"), mk("b@example.com"), mk("c@example.com")}

	n, err := r.InsertBatch(ctx, batch)
	must(t, "InsertBatch", err)
	eq(t, "first insert", n, 3)

	// Re-sending the same chunk inserts nothing and changes no counts.
	again := []store.Delivery{mk("a@example.com"), mk("b@example.com"), mk("c@example.com")}
	n, err = r.InsertBatch(ctx, again)
	must(t, "InsertBatch again", err)
	eq(t, "second insert", n, 0)

	counts, err := r.CountByStatus(ctx, campaignID)
	must(t, "CountByStatus", err)
	eq(t, "total after retry", counts[store.DeliveryQueued], int64(3))

	// A partially overlapping chunk inserts only the new rows.
	n, err = r.InsertBatch(ctx, []store.Delivery{mk("c@example.com"), mk("d@example.com")})
	must(t, "InsertBatch overlap", err)
	eq(t, "overlap insert", n, 1)

	// Duplicates inside one batch are collapsed too.
	n, err = r.InsertBatch(ctx, []store.Delivery{mk("e@example.com"), mk("e@example.com")})
	must(t, "InsertBatch intra-batch dup", err)
	eq(t, "intra-batch insert", n, 1)

	// The same address in another campaign is a different row.
	other := mk("a@example.com")
	other.CampaignID = store.NewID()
	n, err = r.InsertBatch(ctx, []store.Delivery{other})
	must(t, "InsertBatch other campaign", err)
	eq(t, "other campaign insert", n, 1)

	// Deliveries without a campaign (transactional, probe) are never deduped.
	tx := func() store.Delivery {
		d := mk("t@example.com")
		d.CampaignID = ""
		d.Lane = store.LaneTransactional
		return d
	}
	n, err = r.InsertBatch(ctx, []store.Delivery{tx(), tx()})
	must(t, "InsertBatch transactional", err)
	eq(t, "transactional insert", n, 2)
	n, err = r.InsertBatch(ctx, []store.Delivery{tx()})
	must(t, "InsertBatch transactional again", err)
	eq(t, "transactional insert again", n, 1)

	counts, err = r.CountByStatus(ctx, "")
	must(t, "CountByStatus transactional", err)
	eq(t, "transactional total", counts[store.DeliveryQueued], int64(3))

	_, err = r.InsertBatch(ctx, []store.Delivery{{ID: store.NewID(), Email: "x@example.com"}})
	mustBe(t, "InsertBatch without email_norm", err, store.ErrInvalid)
}

func testClaim(t *testing.T, p store.Provider) {
	ctx := context.Background()
	now := time.Now().UTC()
	campaignID := store.NewID()

	seq := 0
	mk := func(prio int, at time.Duration, status store.DeliveryStatus) store.Delivery {
		seq++
		email := fmt.Sprintf("u%d@example.com", seq)
		return store.Delivery{
			ID: store.NewID(), CampaignID: campaignID, Lane: store.LaneBulk,
			Priority: prio, Status: status,
			Email:         email,
			EmailNorm:     email,
			NextAttemptAt: now.Add(at),
		}
	}

	t.Run("OrderAndGating", func(t *testing.T) {
		s, _ := fresh(t, p)
		low := mk(0, -5*time.Minute, store.DeliveryQueued)
		high := mk(9, -time.Minute, store.DeliveryQueued)
		oldest := mk(0, -9*time.Minute, store.DeliveryDeferred)
		future := mk(0, time.Hour, store.DeliveryQueued)
		done := mk(0, -3*time.Minute, store.DeliverySent)
		seed(t, s, []store.Delivery{low, high, oldest, future, done})

		batch, err := s.Deliveries().Claim(ctx, store.ClaimRequest{
			Lane: store.LaneBulk, Limit: 10, LeaseFor: time.Minute,
			WorkerID: "w1", Now: now,
		})
		must(t, "Claim", err)
		eqIDs(t, "Claim order", claimIDs(batch), []string{high.ID, oldest.ID, low.ID})

		for i := range batch {
			eq(t, "claimed status", batch[i].Status, store.DeliveryLeased)
			eq(t, "claimed owner", batch[i].LeaseOwner, "w1")
			if !batch[i].LeaseUntil.Equal(now.Add(time.Minute)) {
				t.Fatalf("lease_until: got %v, want %v", batch[i].LeaseUntil, now.Add(time.Minute))
			}
		}
		stored, err := s.Deliveries().Get(ctx, high.ID)
		must(t, "Get after Claim", err)
		eq(t, "stored status", stored.Status, store.DeliveryLeased)

		empty, err := s.Deliveries().Claim(ctx, store.ClaimRequest{
			Lane: store.LaneBulk, Limit: 10, LeaseFor: time.Minute, WorkerID: "w2", Now: now,
		})
		must(t, "Claim again", err)
		eq(t, "nothing left to claim", len(empty), 0)

		// The gated row becomes claimable once its time comes.
		later, err := s.Deliveries().Claim(ctx, store.ClaimRequest{
			Lane: store.LaneBulk, Limit: 10, LeaseFor: time.Minute,
			WorkerID: "w2", Now: now.Add(2 * time.Hour),
		})
		must(t, "Claim later", err)
		eqIDs(t, "gated row", claimIDs(later), []string{future.ID})
	})

	t.Run("CampaignFilter", func(t *testing.T) {
		other := store.NewID()
		build := func(t *testing.T) (store.Store, store.Delivery, store.Delivery, store.Delivery) {
			s, _ := fresh(t, p)
			in := mk(0, -time.Minute, store.DeliveryQueued)
			out := mk(0, -time.Minute, store.DeliveryQueued)
			out.CampaignID = other
			none := mk(0, -time.Minute, store.DeliveryQueued)
			none.CampaignID = ""
			seed(t, s, []store.Delivery{in, out, none})
			return s, in, out, none
		}
		claim := func(t *testing.T, s store.Store, ids []string) []string {
			batch, err := s.Deliveries().Claim(ctx, store.ClaimRequest{
				Lane: store.LaneBulk, CampaignIDs: ids, Limit: 10,
				LeaseFor: time.Minute, WorkerID: "w1", Now: now,
			})
			must(t, "Claim", err)
			got := map[string]bool{}
			for _, id := range claimIDs(batch) {
				got[id] = true
			}
			out := make([]string, 0, len(got))
			for id := range got {
				out = append(out, id)
			}
			return out
		}
		has := func(t *testing.T, got []string, want ...string) {
			t.Helper()
			eq(t, "claimed count", len(got), len(want))
			for _, w := range want {
				found := false
				for _, g := range got {
					if g == w {
						found = true
					}
				}
				if !found {
					t.Fatalf("claimed %v, missing %s", got, w)
				}
			}
		}

		t.Run("Nil", func(t *testing.T) {
			s, in, out, none := build(t)
			has(t, claim(t, s, nil), in.ID, out.ID, none.ID)
		})
		t.Run("Empty", func(t *testing.T) {
			s, _, _, none := build(t)
			has(t, claim(t, s, []string{}), none.ID)
		})
		t.Run("List", func(t *testing.T) {
			s, in, _, none := build(t)
			has(t, claim(t, s, []string{campaignID}), in.ID, none.ID)
		})
	})

	t.Run("LaneFilter", func(t *testing.T) {
		s, _ := fresh(t, p)
		bulk := mk(0, -time.Minute, store.DeliveryQueued)
		tx := mk(0, -time.Minute, store.DeliveryQueued)
		tx.CampaignID, tx.Lane = "", store.LaneTransactional
		probe := mk(0, -time.Minute, store.DeliveryQueued)
		probe.CampaignID, probe.Lane = "", store.LaneProbe
		seed(t, s, []store.Delivery{bulk, tx, probe})

		for _, tc := range []struct {
			lane store.Lane
			want string
		}{
			{store.LaneTransactional, tx.ID},
			{store.LaneProbe, probe.ID},
			{store.LaneBulk, bulk.ID},
		} {
			batch, err := s.Deliveries().Claim(ctx, store.ClaimRequest{
				Lane: tc.lane, Limit: 10, LeaseFor: time.Minute, WorkerID: "w1", Now: now,
			})
			must(t, "Claim "+tc.lane.String(), err)
			eqIDs(t, "lane "+tc.lane.String(), claimIDs(batch), []string{tc.want})
		}
	})
}

func testClaimConcurrent(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	now := time.Now().UTC()
	campaignID := store.NewID()

	const total = 2000
	const workers = 16
	batch := make([]store.Delivery, 0, total)
	for i := range total {
		batch = append(batch, store.Delivery{
			ID: store.NewID(), CampaignID: campaignID, Lane: store.LaneBulk,
			Status: store.DeliveryQueued,
			Email:  fmt.Sprintf("u%d@example.com", i), EmailNorm: fmt.Sprintf("u%d@example.com", i),
			NextAttemptAt: now.Add(-time.Minute),
		})
	}
	for i := 0; i < total; i += 500 {
		seed(t, s, batch[i:i+500])
	}

	var mu sync.Mutex
	claimed := map[string]string{}
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			ws, err := p.ForTenant(ctx, tenantID)
			if err != nil {
				t.Errorf("ForTenant: %v", err)
				return
			}
			for {
				got, err := ws.Deliveries().Claim(ctx, store.ClaimRequest{
					Lane: store.LaneBulk, Limit: 50, LeaseFor: time.Minute,
					WorkerID: worker, Now: now,
				})
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for i := range got {
					if prev, dup := claimed[got[i].ID]; dup {
						mu.Unlock()
						t.Errorf("delivery %s claimed twice, by %s and %s", got[i].ID, prev, worker)
						return
					}
					claimed[got[i].ID] = worker
				}
				mu.Unlock()
			}
		}(fmt.Sprintf("w%d", w))
	}
	wg.Wait()
	eq(t, "claimed total", len(claimed), total)

	counts, err := s.Deliveries().CountByStatus(ctx, campaignID)
	must(t, "CountByStatus", err)
	eq(t, "all leased", counts[store.DeliveryLeased], int64(total))
	eq(t, "none queued", counts[store.DeliveryQueued], int64(0))
}

// claimOne seeds a single queued delivery and claims it for owner.
func claimOne(t *testing.T, p store.Provider, owner string, now time.Time) (store.Store, store.Delivery) {
	t.Helper()
	s, _ := fresh(t, p)
	d := store.Delivery{
		ID: store.NewID(), CampaignID: store.NewID(), Lane: store.LaneBulk,
		Status: store.DeliveryQueued, Email: "a@example.com", EmailNorm: "a@example.com",
		NextAttemptAt: now.Add(-time.Minute),
	}
	seed(t, s, []store.Delivery{d})
	batch, err := s.Deliveries().Claim(context.Background(), store.ClaimRequest{
		Lane: store.LaneBulk, Limit: 10, LeaseFor: time.Minute, WorkerID: owner, Now: now,
	})
	must(t, "Claim", err)
	eq(t, "claimed one", len(batch), 1)
	return s, batch[0]
}

func testCompleteCAS(t *testing.T, p store.Provider) {
	ctx := context.Background()
	now := time.Now().UTC()
	s, d := claimOne(t, p, "w1", now)
	r := s.Deliveries()

	// A result from a worker that no longer holds the lease is dropped, and so
	// is its attempt row.
	must(t, "Complete stale", r.Complete(ctx, []store.DeliveryResult{{
		DeliveryID: d.ID, LeaseOwner: "w2", NewStatus: store.DeliveryFailed,
		ErrorClass: store.ErrorClassPermanent, SMTPCode: 550,
		Attempt: &store.DeliveryAttempt{DeliveryID: d.ID, AttemptNo: 1},
	}}))
	got, err := r.Get(ctx, d.ID)
	must(t, "Get after stale Complete", err)
	eq(t, "stale Complete ignored", got.Status, store.DeliveryLeased)
	att, err := s.Attempts().ListByDelivery(ctx, d.ID, store.Page{Limit: 10})
	must(t, "ListByDelivery", err)
	eq(t, "no attempt from stale result", len(att.Items), 0)

	// The lease holder's result applies, together with its attempt.
	must(t, "Complete", r.Complete(ctx, []store.DeliveryResult{{
		DeliveryID: d.ID, LeaseOwner: "w1", NewStatus: store.DeliveryDeferred,
		NextAttemptAt: now.Add(time.Minute), ErrorClass: store.ErrorClassTransient,
		SMTPCode: 451, Error: "try again", IncrementAttempt: true,
		Attempt: &store.DeliveryAttempt{
			DeliveryID: d.ID, AttemptNo: 1, TransportID: "tr",
			SMTPCode: 451, ErrorClass: store.ErrorClassTransient, Error: "try again",
		},
	}}))
	got, err = r.Get(ctx, d.ID)
	must(t, "Get after Complete", err)
	eq(t, "status", got.Status, store.DeliveryDeferred)
	eq(t, "attempt count", got.AttemptCount, 1)
	eq(t, "error class", got.LastErrorClass, store.ErrorClassTransient)
	eq(t, "smtp code", got.LastSMTPCode, 451)
	eq(t, "lease released", got.LeaseOwner, "")
	if !got.NextAttemptAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("next_attempt_at: got %v, want %v", got.NextAttemptAt, now.Add(time.Minute))
	}
	att, err = s.Attempts().ListByDelivery(ctx, d.ID, store.Page{Limit: 10})
	must(t, "ListByDelivery", err)
	eq(t, "attempt recorded", len(att.Items), 1)

	// Replaying the same batch is a no-op: the lease is gone.
	must(t, "Complete replay", r.Complete(ctx, []store.DeliveryResult{{
		DeliveryID: d.ID, LeaseOwner: "w1", NewStatus: store.DeliveryFailed,
		IncrementAttempt: true,
		Attempt:          &store.DeliveryAttempt{DeliveryID: d.ID, AttemptNo: 2},
	}}))
	got, err = r.Get(ctx, d.ID)
	must(t, "Get after replay", err)
	eq(t, "replay ignored", got.Status, store.DeliveryDeferred)
	eq(t, "replay kept attempt count", got.AttemptCount, 1)
	att, err = s.Attempts().ListByDelivery(ctx, d.ID, store.Page{Limit: 10})
	must(t, "ListByDelivery after replay", err)
	eq(t, "no extra attempt", len(att.Items), 1)

	// It is claimable again once next_attempt_at passes.
	batch, err := r.Claim(ctx, store.ClaimRequest{
		Lane: store.LaneBulk, Limit: 10, LeaseFor: time.Minute,
		WorkerID: "w3", Now: now.Add(2 * time.Minute),
	})
	must(t, "Claim after defer", err)
	eqIDs(t, "deferred row is claimable", claimIDs(batch), []string{d.ID})
}

func testMarkSent(t *testing.T, p store.Provider) {
	ctx := context.Background()
	now := time.Now().UTC()
	s, d := claimOne(t, p, "w1", now)
	r := s.Deliveries()

	mustBe(t, "MarkSent unknown", r.MarkSent(ctx, store.NewID(), "w1", "m", now), store.ErrNotFound)
	mustBe(t, "MarkSent wrong owner", r.MarkSent(ctx, d.ID, "w2", "m", now), store.ErrLeaseLost)

	sentAt := now.Add(time.Second)
	must(t, "MarkSent", r.MarkSent(ctx, d.ID, "w1", "<msg@example.com>", sentAt))
	got, err := r.Get(ctx, d.ID)
	must(t, "Get after MarkSent", err)
	eq(t, "status", got.Status, store.DeliverySent)
	eq(t, "message id", got.MessageID, "<msg@example.com>")
	if !got.SentAt.Equal(sentAt) {
		t.Fatalf("sent_at: got %v, want %v", got.SentAt, sentAt)
	}

	// The batched Complete still attaches the attempt afterwards.
	must(t, "Complete after MarkSent", r.Complete(ctx, []store.DeliveryResult{{
		DeliveryID: d.ID, LeaseOwner: "w1", NewStatus: store.DeliverySent,
		MessageID: "<msg@example.com>", IncrementAttempt: true,
		Attempt: &store.DeliveryAttempt{DeliveryID: d.ID, AttemptNo: 1, TransportID: "tr", SMTPCode: 250},
	}}))
	att, err := s.Attempts().ListByDelivery(ctx, d.ID, store.Page{Limit: 10})
	must(t, "ListByDelivery", err)
	eq(t, "attempt recorded", len(att.Items), 1)

	// A sent row is never recycled by lease expiry.
	n, err := r.ReleaseExpiredLeases(ctx, now.Add(time.Hour), 10)
	must(t, "ReleaseExpiredLeases", err)
	eq(t, "sent row not released", n, 0)
	got, err = r.Get(ctx, d.ID)
	must(t, "Get after release", err)
	eq(t, "still sent", got.Status, store.DeliverySent)
}

func testReleaseExpiredLeases(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	campaignID := store.NewID()

	mk := func(email string) store.Delivery {
		return store.Delivery{
			ID: store.NewID(), CampaignID: campaignID, Lane: store.LaneBulk,
			Status: store.DeliveryQueued, Email: email, EmailNorm: email,
			NextAttemptAt: now.Add(-time.Minute),
		}
	}
	first, second := mk("a@example.com"), mk("b@example.com")
	seed(t, s, []store.Delivery{first, second})

	batch, err := r.Claim(ctx, store.ClaimRequest{
		Lane: store.LaneBulk, Limit: 10, LeaseFor: time.Minute, WorkerID: "w1", Now: now,
	})
	must(t, "Claim", err)
	eq(t, "claimed", len(batch), 2)

	// A delivery that already burned an attempt comes back as deferred.
	must(t, "Complete", r.Complete(ctx, []store.DeliveryResult{{
		DeliveryID: first.ID, LeaseOwner: "w1", NewStatus: store.DeliveryDeferred,
		NextAttemptAt: now, ErrorClass: store.ErrorClassTransient, IncrementAttempt: true,
	}}))
	_, err = r.Claim(ctx, store.ClaimRequest{
		Lane: store.LaneBulk, Limit: 10, LeaseFor: time.Minute, WorkerID: "w1", Now: now.Add(time.Second),
	})
	must(t, "Claim again", err)

	n, err := r.ReleaseExpiredLeases(ctx, now.Add(30*time.Second), 10)
	must(t, "ReleaseExpiredLeases early", err)
	eq(t, "nothing expired yet", n, 0)

	// limit is honoured so a million rows can be walked in chunks.
	n, err = r.ReleaseExpiredLeases(ctx, now.Add(10*time.Minute), 1)
	must(t, "ReleaseExpiredLeases limit", err)
	eq(t, "limited release", n, 1)
	n, err = r.ReleaseExpiredLeases(ctx, now.Add(10*time.Minute), 10)
	must(t, "ReleaseExpiredLeases rest", err)
	eq(t, "rest released", n, 1)
	n, err = r.ReleaseExpiredLeases(ctx, now.Add(10*time.Minute), 10)
	must(t, "ReleaseExpiredLeases done", err)
	eq(t, "nothing left", n, 0)

	got, err := r.Get(ctx, first.ID)
	must(t, "Get first", err)
	eq(t, "attempted row deferred", got.Status, store.DeliveryDeferred)
	eq(t, "attempt count kept", got.AttemptCount, 1)
	eq(t, "lease cleared", got.LeaseOwner, "")

	got, err = r.Get(ctx, second.ID)
	must(t, "Get second", err)
	eq(t, "untouched row queued", got.Status, store.DeliveryQueued)
	eq(t, "attempt count still zero", got.AttemptCount, 0)
}

func testRequeue(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	campaignID := store.NewID()

	mk := func(email string, st store.DeliveryStatus, cl store.ErrorClass) store.Delivery {
		return store.Delivery{
			ID: store.NewID(), CampaignID: campaignID, Lane: store.LaneBulk,
			Status: st, LastErrorClass: cl, AttemptCount: 6,
			Email: email, EmailNorm: email, NextAttemptAt: now,
			FinishedAt: now,
		}
	}
	transient := mk("a@example.com", store.DeliveryFailed, store.ErrorClassTransient)
	permanent := mk("b@example.com", store.DeliveryFailed, store.ErrorClassPermanent)
	sent := mk("c@example.com", store.DeliverySent, store.ErrorClassNone)
	seed(t, s, []store.Delivery{transient, permanent, sent})

	at := now.Add(time.Minute)
	n, err := r.Requeue(ctx, store.RetryFilter{
		CampaignID: campaignID,
		Statuses:   []store.DeliveryStatus{store.DeliveryFailed},
		ErrorClasses: []store.ErrorClass{
			store.ErrorClassTransient, store.ErrorClassRateLimited,
		},
		Now: at,
	}, 100)
	must(t, "Requeue", err)
	eq(t, "requeued", n, 1)

	got, err := r.Get(ctx, transient.ID)
	must(t, "Get requeued", err)
	eq(t, "status", got.Status, store.DeliveryQueued)
	eq(t, "retry generation bumped", got.RetryGen, 1)
	eq(t, "attempt count kept", got.AttemptCount, 6)
	if !got.NextAttemptAt.Equal(at) {
		t.Fatalf("next_attempt_at: got %v, want %v", got.NextAttemptAt, at)
	}

	other, err := r.Get(ctx, permanent.ID)
	must(t, "Get permanent", err)
	eq(t, "permanent untouched", other.Status, store.DeliveryFailed)
	eq(t, "permanent retry gen", other.RetryGen, 0)

	// Single-delivery retry.
	n, err = r.Requeue(ctx, store.RetryFilter{
		CampaignID: campaignID, DeliveryIDs: []string{permanent.ID}, Now: at,
	}, 100)
	must(t, "Requeue one", err)
	eq(t, "requeued one", n, 1)
	other, err = r.Get(ctx, permanent.ID)
	must(t, "Get permanent after retry", err)
	eq(t, "permanent requeued", other.Status, store.DeliveryQueued)
	eq(t, "permanent retry gen", other.RetryGen, 1)

	still, err := r.Get(ctx, sent.ID)
	must(t, "Get sent", err)
	eq(t, "sent untouched", still.Status, store.DeliverySent)
}

func testCountByStatus(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	campaignID := store.NewID()

	mk := func(email string, st store.DeliveryStatus, campaign string) store.Delivery {
		return store.Delivery{
			ID: store.NewID(), CampaignID: campaign, Lane: store.LaneBulk, Status: st,
			Email: email, EmailNorm: email, NextAttemptAt: now,
		}
	}
	seed(t, s, []store.Delivery{
		mk("a@example.com", store.DeliverySent, campaignID),
		mk("b@example.com", store.DeliverySent, campaignID),
		mk("c@example.com", store.DeliveryFailed, campaignID),
		mk("d@example.com", store.DeliveryQueued, campaignID),
		mk("e@example.com", store.DeliverySent, ""),
	})

	counts, err := r.CountByStatus(ctx, campaignID)
	must(t, "CountByStatus", err)
	eq(t, "sent", counts[store.DeliverySent], int64(2))
	eq(t, "failed", counts[store.DeliveryFailed], int64(1))
	eq(t, "queued", counts[store.DeliveryQueued], int64(1))
	eq(t, "bounced", counts[store.DeliveryBounced], int64(0))

	counts, err = r.CountByStatus(ctx, "")
	must(t, "CountByStatus transactional", err)
	eq(t, "transactional sent", counts[store.DeliverySent], int64(1))
}

func testBulkTransition(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	campaignID := store.NewID()

	batch := make([]store.Delivery, 0, 5)
	for i := range 5 {
		email := fmt.Sprintf("u%d@example.com", i)
		batch = append(batch, store.Delivery{
			ID: store.NewID(), CampaignID: campaignID, Lane: store.LaneBulk,
			Status: store.DeliveryPending, Email: email, EmailNorm: email,
			NextAttemptAt: now.Add(-time.Minute),
		})
	}
	seed(t, s, batch)

	// One row is leased and must survive a cancel.
	n, err := r.BulkTransition(ctx, campaignID,
		[]store.DeliveryStatus{store.DeliveryPending}, store.DeliveryQueued, 1)
	must(t, "BulkTransition to queued", err)
	eq(t, "one queued", n, 1)
	leased, err := r.Claim(ctx, store.ClaimRequest{
		Lane: store.LaneBulk, Limit: 10, LeaseFor: time.Minute, WorkerID: "w1", Now: now,
	})
	must(t, "Claim", err)
	eq(t, "one leased", len(leased), 1)

	from := []store.DeliveryStatus{store.DeliveryPending, store.DeliveryQueued, store.DeliveryDeferred}
	total := 0
	for {
		n, err := r.BulkTransition(ctx, campaignID, from, store.DeliveryCancelled, 2)
		must(t, "BulkTransition chunk", err)
		if n == 0 {
			break
		}
		if n > 2 {
			t.Fatalf("BulkTransition returned %d for limit 2", n)
		}
		total += n
		if total > 5 {
			t.Fatal("BulkTransition does not terminate")
		}
	}
	eq(t, "cancelled", total, 4)

	counts, err := r.CountByStatus(ctx, campaignID)
	must(t, "CountByStatus", err)
	eq(t, "cancelled count", counts[store.DeliveryCancelled], int64(4))
	eq(t, "leased row kept", counts[store.DeliveryLeased], int64(1))
}

func testSetFirst(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()

	d := store.Delivery{
		ID: store.NewID(), CampaignID: store.NewID(), Lane: store.LaneBulk,
		Status: store.DeliverySent, Email: "a@example.com", EmailNorm: "a@example.com",
		NextAttemptAt: now,
	}
	seed(t, s, []store.Delivery{d})

	for _, tc := range []struct {
		name string
		set  func(context.Context, string, time.Time) (bool, error)
		read func(*store.Delivery) time.Time
	}{
		{"opened", r.SetFirstOpened, func(v *store.Delivery) time.Time { return v.FirstOpenedAt }},
		{"clicked", r.SetFirstClicked, func(v *store.Delivery) time.Time { return v.FirstClickedAt }},
		{"unsubscribed", r.SetUnsubscribed, func(v *store.Delivery) time.Time { return v.UnsubscribedAt }},
	} {
		first := now.Add(time.Minute)
		changed, err := tc.set(ctx, d.ID, first)
		must(t, "set "+tc.name, err)
		eq(t, "first "+tc.name+" changed", changed, true)

		changed, err = tc.set(ctx, d.ID, now.Add(2*time.Minute))
		must(t, "set "+tc.name+" again", err)
		eq(t, "second "+tc.name+" changed", changed, false)

		got, err := r.Get(ctx, d.ID)
		must(t, "Get", err)
		if !tc.read(got).Equal(first) {
			t.Fatalf("%s: got %v, want %v", tc.name, tc.read(got), first)
		}

		// A token for a delivery retention already removed is dropped, not an
		// error (architecture 9.1).
		changed, err = tc.set(ctx, store.NewID(), first)
		must(t, "set "+tc.name+" unknown", err)
		eq(t, "unknown "+tc.name+" changed", changed, false)
	}
}

func testListByCampaign(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	campaignID := store.NewID()

	batch := make([]store.Delivery, 0, 6)
	for i := range 6 {
		email := fmt.Sprintf("u%d@example.com", i)
		st := store.DeliverySent
		cl := store.ErrorClassNone
		if i%2 == 1 {
			st, cl = store.DeliveryFailed, store.ErrorClassPermanent
		}
		batch = append(batch, store.Delivery{
			ID: store.NewID(), CampaignID: campaignID, Lane: store.LaneBulk,
			Status: st, LastErrorClass: cl, Email: email, EmailNorm: email,
			NextAttemptAt: now,
		})
	}
	seed(t, s, batch)
	seed(t, s, []store.Delivery{{
		ID: store.NewID(), Lane: store.LaneTransactional, Status: store.DeliverySent,
		Email: "tx@example.com", EmailNorm: "tx@example.com", NextAttemptAt: now,
	}})

	res, err := r.ListByCampaign(ctx, campaignID, store.DeliveryFilter{}, store.Page{Limit: 100})
	must(t, "ListByCampaign", err)
	eq(t, "all", len(res.Items), 6)

	res, err = r.ListByCampaign(ctx, campaignID, store.DeliveryFilter{
		Statuses: []store.DeliveryStatus{store.DeliveryFailed},
	}, store.Page{Limit: 100})
	must(t, "ListByCampaign failed", err)
	eq(t, "failed", len(res.Items), 3)

	res, err = r.ListByCampaign(ctx, campaignID, store.DeliveryFilter{
		ErrorClasses: []store.ErrorClass{store.ErrorClassPermanent},
	}, store.Page{Limit: 100})
	must(t, "ListByCampaign class", err)
	eq(t, "permanent", len(res.Items), 3)

	res, err = r.ListByCampaign(ctx, campaignID, store.DeliveryFilter{
		EmailNorm: "u2@example.com",
	}, store.Page{Limit: 100})
	must(t, "ListByCampaign email", err)
	eq(t, "by email", len(res.Items), 1)

	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := r.ListByCampaign(ctx, campaignID, store.DeliveryFilter{}, store.Page{Limit: 2, Cursor: cursor})
		must(t, "ListByCampaign page", err)
		for i := range page.Items {
			if seen[page.Items[i].ID] {
				t.Fatalf("delivery %s listed twice", page.Items[i].ID)
			}
			seen[page.Items[i].ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	eq(t, "paged total", len(seen), 6)
}

func testDeleteBefore(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	campaignID := store.NewID()
	old := now.Add(-100 * 24 * time.Hour)

	mk := func(email string, created time.Time) store.Delivery {
		return store.Delivery{
			ID: store.NewID(), CampaignID: campaignID, Lane: store.LaneBulk,
			Status: store.DeliverySent, Email: email, EmailNorm: email,
			NextAttemptAt: created, CreatedAt: created,
		}
	}
	seed(t, s, []store.Delivery{
		mk("a@example.com", old), mk("b@example.com", old), mk("c@example.com", old),
		mk("keep@example.com", now),
	})

	cutoff := now.Add(-24 * time.Hour)
	n, err := r.DeleteBefore(ctx, campaignID, cutoff, 2)
	must(t, "DeleteBefore", err)
	eq(t, "first chunk", n, 2)
	n, err = r.DeleteBefore(ctx, campaignID, cutoff, 2)
	must(t, "DeleteBefore rest", err)
	eq(t, "second chunk", n, 1)
	n, err = r.DeleteBefore(ctx, campaignID, cutoff, 2)
	must(t, "DeleteBefore done", err)
	eq(t, "nothing left", n, 0)

	counts, err := r.CountByStatus(ctx, campaignID)
	must(t, "CountByStatus", err)
	eq(t, "kept", counts[store.DeliverySent], int64(1))
}

func testAttempts(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Attempts()
	now := time.Now().UTC()

	a, b := store.NewID(), store.NewID()
	as := make([]store.DeliveryAttempt, 0, 4)
	for i := range 3 {
		as = append(as, store.DeliveryAttempt{
			ID: store.NewID(), DeliveryID: a, AttemptNo: i + 1, TransportID: "tr",
			StartedAt: now, FinishedAt: now.Add(time.Second),
			SMTPCode: 451, EnhancedCode: "4.7.0", ErrorClass: store.ErrorClassTransient,
			Error: "deferred",
		})
	}
	as = append(as, store.DeliveryAttempt{ID: store.NewID(), DeliveryID: b, AttemptNo: 1})
	must(t, "Insert", r.Insert(ctx, as))

	res, err := r.ListByDelivery(ctx, a, store.Page{Limit: 100})
	must(t, "ListByDelivery", err)
	eq(t, "attempts", len(res.Items), 3)
	eq(t, "enhanced code", res.Items[0].EnhancedCode, "4.7.0")

	res, err = r.ListByDelivery(ctx, a, store.Page{Limit: 2})
	must(t, "ListByDelivery page", err)
	eq(t, "page size", len(res.Items), 2)
	if res.NextCursor == "" {
		t.Fatal("expected a cursor")
	}
	res, err = r.ListByDelivery(ctx, a, store.Page{Limit: 2, Cursor: res.NextCursor})
	must(t, "ListByDelivery page 2", err)
	eq(t, "page 2 size", len(res.Items), 1)
	eq(t, "page 2 cursor", res.NextCursor, "")

	res, err = r.ListByDelivery(ctx, store.NewID(), store.Page{Limit: 10})
	must(t, "ListByDelivery unknown", err)
	eq(t, "unknown delivery", len(res.Items), 0)
}
