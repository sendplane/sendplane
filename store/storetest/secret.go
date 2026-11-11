package storetest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

// The secret_vars contract of sensitive deliveries (ADR-0019, store.Delivery):
// the ciphertext exists only while SecretState is pending, every write that
// moves it on drops the ciphertext in the same write, and nothing moves a
// delivery back to pending or retries it.

// eqSecret checks a delivery's secret state and ciphertext. A nil want means
// the ciphertext must be exactly nil: an empty-but-present value is not a
// wiped secret.
func eqSecret(t *testing.T, what string, d *store.Delivery, state store.SecretState, want []byte) {
	t.Helper()
	if d.SecretState != state {
		t.Fatalf("%s: secret state %s, want %s", what, d.SecretState, state)
	}
	if want == nil {
		if d.SecretCiphertext != nil {
			t.Fatalf("%s: ciphertext %q kept, want it dropped", what, d.SecretCiphertext)
		}
		return
	}
	if !bytes.Equal(d.SecretCiphertext, want) {
		t.Fatalf("%s: ciphertext %q, want %q", what, d.SecretCiphertext, want)
	}
}

func getSecret(t *testing.T, r store.DeliveryRepo, id, what string, state store.SecretState, want []byte) *store.Delivery {
	t.Helper()
	d, err := r.Get(context.Background(), id)
	must(t, "Get "+what, err)
	eqSecret(t, what, d, state, want)
	return d
}

// sensitive builds a transactional delivery with a pending secret. ct is
// distinct per row so that a mix-up between rows shows.
func sensitive(email string, st store.DeliveryStatus, nextAt, expiresAt time.Time) store.Delivery {
	return store.Delivery{
		ID: store.NewID(), Lane: store.LaneTransactional, Status: st,
		Email: email, EmailNorm: email, NextAttemptAt: nextAt,
		SecretCiphertext: []byte("ct:" + email), SecretExpiresAt: expiresAt,
		SecretState: store.SecretPending,
	}
}

// plain is the same delivery without a secret.
func plain(email string, st store.DeliveryStatus, nextAt time.Time) store.Delivery {
	return store.Delivery{
		ID: store.NewID(), Lane: store.LaneTransactional, Status: st,
		Email: email, EmailNorm: email, NextAttemptAt: nextAt,
	}
}

func claimTx(t *testing.T, r store.DeliveryRepo, owner string, now time.Time) []store.Delivery {
	t.Helper()
	batch, err := r.Claim(context.Background(), store.ClaimRequest{
		Lane: store.LaneTransactional, Limit: 100, LeaseFor: time.Minute,
		WorkerID: owner, Now: now,
	})
	must(t, "Claim", err)
	return batch
}

// testSecretRoundTrip: the three fields survive InsertBatch and come back from
// every read, Claim included (the sender needs the ciphertext). The insert
// itself already keeps the invariant: a row inserted terminal has no secret
// to keep, and only a pending secret has a ciphertext.
func testSecretRoundTrip(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	expires := now.Add(10 * time.Minute)

	pending := sensitive("a@example.com", store.DeliveryQueued, now.Add(-time.Minute), expires)
	ordinary := plain("b@example.com", store.DeliveryQueued, now.Add(-time.Minute))
	suppressed := sensitive("c@example.com", store.DeliverySuppressed, now, expires)
	// Not pending, so the ciphertext has no business being there.
	consumed := sensitive("d@example.com", store.DeliveryQueued, now.Add(time.Hour), expires)
	consumed.SecretState = store.SecretConsumed
	stray := plain("e@example.com", store.DeliveryQueued, now.Add(time.Hour))
	stray.SecretCiphertext = []byte("stray")
	seed(t, s, []store.Delivery{pending, ordinary, suppressed, consumed, stray})

	got := getSecret(t, r, pending.ID, "pending", store.SecretPending, []byte("ct:a@example.com"))
	eqTime(t, "secret_expires_at", got.SecretExpiresAt, expires)
	eq(t, "pending is sensitive", got.Sensitive(), true)
	got = getSecret(t, r, ordinary.ID, "ordinary", store.SecretNone, nil)
	eq(t, "ordinary is not sensitive", got.Sensitive(), false)
	eq(t, "ordinary has no expiry", got.SecretExpiresAt.IsZero(), true)
	got = getSecret(t, r, suppressed.ID, "inserted suppressed", store.SecretDiscarded, nil)
	eq(t, "suppressed status kept", got.Status, store.DeliverySuppressed)
	getSecret(t, r, consumed.ID, "inserted consumed", store.SecretConsumed, nil)
	getSecret(t, r, stray.ID, "ciphertext without a secret state", store.SecretNone, nil)

	// The listings carry the fields too.
	none := ""
	for name, list := range map[string]func() (store.Result[store.Delivery], error){
		"List": func() (store.Result[store.Delivery], error) {
			return r.List(ctx, store.DeliveryFilter{CampaignID: &none}, store.Page{Limit: 100})
		},
		"ListByCampaign": func() (store.Result[store.Delivery], error) {
			return r.ListByCampaign(ctx, "", store.DeliveryFilter{}, store.Page{Limit: 100})
		},
	} {
		res, err := list()
		must(t, name, err)
		eq(t, name+" count", len(res.Items), 5)
		for i := range res.Items {
			d := &res.Items[i]
			switch d.ID {
			case pending.ID:
				eqSecret(t, name+" pending", d, store.SecretPending, []byte("ct:a@example.com"))
				eqTime(t, name+" secret_expires_at", d.SecretExpiresAt, expires)
			case ordinary.ID:
				eqSecret(t, name+" ordinary", d, store.SecretNone, nil)
			case suppressed.ID:
				eqSecret(t, name+" suppressed", d, store.SecretDiscarded, nil)
			}
		}
	}

	// Claim hands the ciphertext to the sender.
	batch := claimTx(t, r, "w1", now)
	eq(t, "claimed", len(batch), 2)
	for i := range batch {
		d := &batch[i]
		switch d.ID {
		case pending.ID:
			eqSecret(t, "claimed pending", d, store.SecretPending, []byte("ct:a@example.com"))
			eqTime(t, "claimed secret_expires_at", d.SecretExpiresAt, expires)
		case ordinary.ID:
			eqSecret(t, "claimed ordinary", d, store.SecretNone, nil)
		default:
			t.Fatalf("claimed unexpected delivery %s", d.ID)
		}
	}
}

// testSecretMarkSent: SMTP 250 consumes the secret in the same write, and the
// Complete that follows does not bring anything back.
func testSecretMarkSent(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()

	sec := sensitive("a@example.com", store.DeliveryQueued, now.Add(-time.Minute), now.Add(time.Hour))
	ord := plain("b@example.com", store.DeliveryQueued, now.Add(-time.Minute))
	seed(t, s, []store.Delivery{sec, ord})
	eq(t, "claimed", len(claimTx(t, r, "w1", now)), 2)

	for _, id := range []string{sec.ID, ord.ID} {
		must(t, "MarkSent", r.MarkSent(ctx, id, "w1", "<m@example.com>", now))
	}
	getSecret(t, r, sec.ID, "sent", store.SecretConsumed, nil)
	getSecret(t, r, ord.ID, "ordinary sent", store.SecretNone, nil)

	must(t, "Complete after MarkSent", r.Complete(ctx, []store.DeliveryResult{
		{DeliveryID: sec.ID, LeaseOwner: "w1", NewStatus: store.DeliverySent, MessageID: "<m@example.com>"},
		{DeliveryID: ord.ID, LeaseOwner: "w1", NewStatus: store.DeliverySent, MessageID: "<m@example.com>"},
	}))
	got := getSecret(t, r, sec.ID, "completed", store.SecretConsumed, nil)
	eq(t, "status", got.Status, store.DeliverySent)
	getSecret(t, r, ord.ID, "ordinary completed", store.SecretNone, nil)

	// A bounce or complaint on a sent row leaves the secret fields alone.
	changed, err := r.MarkBounced(ctx, sec.ID, now.Add(time.Hour))
	must(t, "MarkBounced", err)
	eq(t, "bounced", changed, true)
	getSecret(t, r, sec.ID, "bounced", store.SecretConsumed, nil)
	changed, err = r.MarkComplained(ctx, ord.ID, now.Add(time.Hour))
	must(t, "MarkComplained", err)
	eq(t, "complained", changed, true)
	getSecret(t, r, ord.ID, "ordinary complained", store.SecretNone, nil)
}

// testSecretComplete: a terminal result settles the secret against the
// store's clock (consumed, expired or discarded), a deferral keeps it for the
// retry.
func testSecretComplete(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	failed := sensitive("a@example.com", store.DeliveryQueued, due, future)
	lapsed := sensitive("b@example.com", store.DeliveryQueued, due, now.Add(-time.Second))
	sent := sensitive("c@example.com", store.DeliveryQueued, due, future)
	deferred := sensitive("d@example.com", store.DeliveryQueued, due, future)
	noExpiry := sensitive("e@example.com", store.DeliveryQueued, due, time.Time{})
	ordFailed := plain("f@example.com", store.DeliveryQueued, due)
	ordDeferred := plain("g@example.com", store.DeliveryQueued, due)
	seed(t, s, []store.Delivery{failed, lapsed, sent, deferred, noExpiry, ordFailed, ordDeferred})
	eq(t, "claimed", len(claimTx(t, r, "w1", now)), 7)

	perm := func(id string) store.DeliveryResult {
		return store.DeliveryResult{
			DeliveryID: id, LeaseOwner: "w1", NewStatus: store.DeliveryFailed,
			ErrorClass: store.ErrorClassPermanent, SMTPCode: 550, Error: "no such user",
		}
	}
	defer1 := func(id string) store.DeliveryResult {
		return store.DeliveryResult{
			DeliveryID: id, LeaseOwner: "w1", NewStatus: store.DeliveryDeferred,
			NextAttemptAt: future, ErrorClass: store.ErrorClassTransient,
			SMTPCode: 451, IncrementAttempt: true,
		}
	}
	// A stale result settles nothing.
	stale := perm(deferred.ID)
	stale.LeaseOwner = "w2"
	must(t, "Complete stale", r.Complete(ctx, []store.DeliveryResult{stale}))
	getSecret(t, r, deferred.ID, "after stale result", store.SecretPending, []byte("ct:d@example.com"))

	must(t, "Complete", r.Complete(ctx, []store.DeliveryResult{
		perm(failed.ID), perm(lapsed.ID), perm(noExpiry.ID), perm(ordFailed.ID),
		{DeliveryID: sent.ID, LeaseOwner: "w1", NewStatus: store.DeliverySent, MessageID: "<m@example.com>"},
		defer1(deferred.ID), defer1(ordDeferred.ID),
	}))
	got := getSecret(t, r, failed.ID, "failed before expiry", store.SecretDiscarded, nil)
	eq(t, "failed status", got.Status, store.DeliveryFailed)
	getSecret(t, r, lapsed.ID, "failed after expiry", store.SecretExpired, nil)
	getSecret(t, r, noExpiry.ID, "failed without expiry", store.SecretDiscarded, nil)
	got = getSecret(t, r, sent.ID, "sent by Complete", store.SecretConsumed, nil)
	eq(t, "sent status", got.Status, store.DeliverySent)
	got = getSecret(t, r, deferred.ID, "deferred", store.SecretPending, []byte("ct:d@example.com"))
	eq(t, "deferred status", got.Status, store.DeliveryDeferred)
	getSecret(t, r, ordFailed.ID, "ordinary failed", store.SecretNone, nil)
	getSecret(t, r, ordDeferred.ID, "ordinary deferred", store.SecretNone, nil)

	// The deferred secret is still there for the retry's claim, and a
	// terminal result then settles it like any other.
	batch := claimTx(t, r, "w3", future)
	eq(t, "reclaimed", len(batch), 2)
	for i := range batch {
		if batch[i].ID == deferred.ID {
			eqSecret(t, "reclaimed deferred", &batch[i], store.SecretPending, []byte("ct:d@example.com"))
		}
	}
	res := perm(deferred.ID)
	res.LeaseOwner = "w3"
	must(t, "Complete retry", r.Complete(ctx, []store.DeliveryResult{res}))
	getSecret(t, r, deferred.ID, "failed on retry", store.SecretDiscarded, nil)
}

// testSecretBulkTransition: a cancel discards a pending secret in the same
// write; a non-terminal move keeps it.
func testSecretBulkTransition(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	later := now.Add(time.Hour)

	// lapsed is past its expiry: a cancel still discards rather than expires.
	sec := sensitive("a@example.com", store.DeliveryQueued, later, later)
	lapsed := sensitive("b@example.com", store.DeliveryQueued, later, now.Add(-time.Minute))
	ord := plain("c@example.com", store.DeliveryQueued, later)
	seed(t, s, []store.Delivery{sec, lapsed, ord})

	n, err := r.BulkTransition(ctx, "", []store.DeliveryStatus{store.DeliveryQueued}, store.DeliveryDeferred, 100)
	must(t, "BulkTransition to deferred", err)
	eq(t, "deferred", n, 3)
	getSecret(t, r, sec.ID, "moved to deferred", store.SecretPending, []byte("ct:a@example.com"))

	n, err = r.BulkTransition(ctx, "", nil, store.DeliveryCancelled, 100)
	must(t, "BulkTransition to cancelled", err)
	eq(t, "cancelled", n, 3)
	got := getSecret(t, r, sec.ID, "cancelled", store.SecretDiscarded, nil)
	eq(t, "status", got.Status, store.DeliveryCancelled)
	getSecret(t, r, lapsed.ID, "cancelled after expiry", store.SecretDiscarded, nil)
	getSecret(t, r, ord.ID, "ordinary cancelled", store.SecretNone, nil)

	// Nothing brings a settled secret back: moving the row on again leaves
	// it discarded.
	_, err = r.BulkTransition(ctx, "", []store.DeliveryStatus{store.DeliveryCancelled}, store.DeliveryQueued, 100)
	must(t, "BulkTransition back to queued", err)
	getSecret(t, r, sec.ID, "un-cancelled", store.SecretDiscarded, nil)
}

// testSecretRequeue: a sensitive delivery is never retried, in any secret
// state, while an ordinary one the same filter matches is.
func testSecretRequeue(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()

	mk := func(email string, st store.DeliveryStatus, state store.SecretState) store.Delivery {
		d := sensitive(email, st, now, now.Add(time.Hour))
		d.SecretState = state
		d.LastErrorClass = store.ErrorClassTransient
		return d
	}
	all := []store.Delivery{
		mk("a@example.com", store.DeliveryDeferred, store.SecretPending),
		mk("b@example.com", store.DeliverySent, store.SecretConsumed),
		mk("c@example.com", store.DeliveryFailed, store.SecretDiscarded),
		mk("d@example.com", store.DeliveryFailed, store.SecretExpired),
	}
	ord := plain("e@example.com", store.DeliveryFailed, now)
	ord.LastErrorClass = store.ErrorClassTransient
	seed(t, s, append(all, ord))

	at := now.Add(time.Minute)
	n, err := r.Requeue(ctx, store.RetryFilter{Now: at}, 100)
	must(t, "Requeue all", err)
	eq(t, "only the ordinary row requeued", n, 1)
	got, err := r.Get(ctx, ord.ID)
	must(t, "Get ordinary", err)
	eq(t, "ordinary requeued", got.Status, store.DeliveryQueued)
	eqSecret(t, "ordinary requeued", got, store.SecretNone, nil)

	ids := make([]string, 0, len(all))
	for _, d := range all {
		ids = append(ids, d.ID)
	}
	n, err = r.Requeue(ctx, store.RetryFilter{DeliveryIDs: ids, Now: at}, 100)
	must(t, "Requeue by id", err)
	eq(t, "sensitive rows requeued by id", n, 0)
	for _, d := range all {
		got, err := r.Get(ctx, d.ID)
		must(t, "Get sensitive", err)
		eq(t, "sensitive status untouched "+d.Email, got.Status, d.Status)
		eq(t, "sensitive retry gen "+d.Email, got.RetryGen, 0)
	}
}

// testSecretReleaseLease: lease recovery keeps a pending secret (the
// documented at-least-once window).
func testSecretReleaseLease(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()

	sec := sensitive("a@example.com", store.DeliveryQueued, now.Add(-time.Minute), now.Add(time.Hour))
	ord := plain("b@example.com", store.DeliveryQueued, now.Add(-time.Minute))
	seed(t, s, []store.Delivery{sec, ord})
	eq(t, "claimed", len(claimTx(t, r, "w1", now)), 2)

	n, err := r.ReleaseExpiredLeases(ctx, now.Add(10*time.Minute), 10)
	must(t, "ReleaseExpiredLeases", err)
	eq(t, "released", n, 2)
	got := getSecret(t, r, sec.ID, "released", store.SecretPending, []byte("ct:a@example.com"))
	eq(t, "released status", got.Status, store.DeliveryQueued)
	getSecret(t, r, ord.ID, "ordinary released", store.SecretNone, nil)
}

// testExpireSecrets: only due, pending, non-leased rows expire; the call
// honours its limit and is idempotent once drained.
func testExpireSecrets(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Deliveries()
	now := time.Now().UTC()
	later := now.Add(time.Hour)
	past := now.Add(-time.Minute)

	// NextAttemptAt later keeps every row but the leased one out of the claim.
	dueQueued := sensitive("a@example.com", store.DeliveryQueued, later, past)
	dueDeferred := sensitive("b@example.com", store.DeliveryDeferred, later, now)
	dueLeased := sensitive("c@example.com", store.DeliveryQueued, past, past)
	notDue := sensitive("d@example.com", store.DeliveryQueued, later, later)
	noExpiry := sensitive("e@example.com", store.DeliveryQueued, later, time.Time{})
	settled := sensitive("f@example.com", store.DeliveryQueued, later, past)
	settled.SecretState = store.SecretDiscarded
	ord := plain("g@example.com", store.DeliveryQueued, later)
	seed(t, s, []store.Delivery{dueQueued, dueDeferred, dueLeased, notDue, noExpiry, settled, ord})
	batch := claimTx(t, r, "w1", now)
	eqIDs(t, "claimed", claimIDs(batch), []string{dueLeased.ID})

	n, err := r.ExpireSecrets(ctx, now, 1)
	must(t, "ExpireSecrets limit 1", err)
	eq(t, "first chunk", n, 1)
	n, err = r.ExpireSecrets(ctx, now, 1)
	must(t, "ExpireSecrets limit 1 again", err)
	eq(t, "second chunk", n, 1)
	n, err = r.ExpireSecrets(ctx, now, 10)
	must(t, "ExpireSecrets drained", err)
	eq(t, "nothing left", n, 0)
	n, err = r.ExpireSecrets(ctx, now, 10)
	must(t, "ExpireSecrets repeated", err)
	eq(t, "still nothing", n, 0)

	for _, d := range []store.Delivery{dueQueued, dueDeferred} {
		got := getSecret(t, r, d.ID, "expired "+d.Email, store.SecretExpired, nil)
		eq(t, "expired status", got.Status, store.DeliveryFailed)
		eq(t, "expired error class", got.LastErrorClass, store.ErrorClassPermanent)
		eq(t, "expired error", got.LastError, store.SecretExpiredError)
		eqTime(t, "expired finished_at", got.FinishedAt, now)
		eqTime(t, "expired updated_at", got.UpdatedAt, now)
	}
	got := getSecret(t, r, dueLeased.ID, "leased", store.SecretPending, []byte("ct:c@example.com"))
	eq(t, "leased status", got.Status, store.DeliveryLeased)
	got = getSecret(t, r, notDue.ID, "not due", store.SecretPending, []byte("ct:d@example.com"))
	eq(t, "not due status", got.Status, store.DeliveryQueued)
	getSecret(t, r, noExpiry.ID, "no expiry", store.SecretPending, []byte("ct:e@example.com"))
	got = getSecret(t, r, settled.ID, "already discarded", store.SecretDiscarded, nil)
	eq(t, "discarded status", got.Status, store.DeliveryQueued)
	got = getSecret(t, r, ord.ID, "ordinary", store.SecretNone, nil)
	eq(t, "ordinary status", got.Status, store.DeliveryQueued)

	// An expired secret is not retryable either.
	n, err = r.Requeue(ctx, store.RetryFilter{DeliveryIDs: []string{dueQueued.ID}, Now: later}, 10)
	must(t, "Requeue expired", err)
	eq(t, "expired not requeued", n, 0)

	// The sender that holds the leased row finds it expired and fails it;
	// Complete records that as expired too, against the store's clock.
	must(t, "Complete leased", r.Complete(ctx, []store.DeliveryResult{{
		DeliveryID: dueLeased.ID, LeaseOwner: "w1", NewStatus: store.DeliveryFailed,
		ErrorClass: store.ErrorClassPermanent, Error: store.SecretExpiredError,
	}}))
	getSecret(t, r, dueLeased.ID, "leased then failed", store.SecretExpired, nil)

	// Later calls move the rows whose time has come.
	n, err = r.ExpireSecrets(ctx, later, 10)
	must(t, "ExpireSecrets later", err)
	eq(t, "not-due row now due", n, 1)
	getSecret(t, r, notDue.ID, "expired later", store.SecretExpired, nil)
	getSecret(t, r, noExpiry.ID, "never expires", store.SecretPending, []byte("ct:e@example.com"))
}
