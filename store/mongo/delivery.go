package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sendplane/sendplane/store"
)

type deliveryRepo struct{ s *tenantStore }

func (r *deliveryRepo) coll() *mongo.Collection     { return r.s.coll(collDelivery) }
func (r *deliveryRepo) attempts() *mongo.Collection { return r.s.coll(collAttempt) }

// lit wraps a value so that an aggregation-pipeline update treats it as data.
// Without it a string that happens to start with '$' would be read as a field
// path.
func lit(v any) bson.D { return bson.D{{Key: "$literal", Value: v}} }

// campaignEq selects one campaign, or - for the empty campaign ID - the
// deliveries that have no campaign at all. Those omit the field entirely,
// which is also what keeps them out of the unique (campaign_id, email_norm)
// index.
func campaignEq(campaignID string) bson.E {
	if campaignID == "" {
		return bson.E{Key: "campaign_id", Value: bson.D{{Key: "$exists", Value: false}}}
	}
	return bson.E{Key: "campaign_id", Value: campaignID}
}

// insertSecret is the secret invariant (store.Delivery) on the way in: only a
// pending secret has a ciphertext, and a row inserted already terminal
// (suppressed at ingest) has nothing left to send it to.
func insertSecret(d *store.Delivery) {
	if d.SecretState == store.SecretPending && d.Status.Terminal() {
		d.SecretState = store.SecretDiscarded
	}
	if d.SecretState != store.SecretPending {
		d.SecretCiphertext = nil
	}
}

// ifPending is an aggregation expression that evaluates to then for a row
// whose secret is still pending and leaves secret_state as it is otherwise.
// Every write that moves a pending secret on pairs it with secret_ct: null in
// the same $set, which is the whole invariant (store.Delivery). A row that
// is not pending has no ciphertext left, so the null is a no-op there.
//
// A document written before the field existed has no secret_state at all;
// "$secret_state" is then missing and $set leaves it missing, which decodes as
// SecretNone.
func ifPending(then any) bson.D {
	return bson.D{{Key: "$cond", Value: bson.D{
		{Key: "if", Value: bson.D{{Key: "$eq", Value: bson.A{"$secret_state", int32(store.SecretPending)}}}},
		{Key: "then", Value: then},
		{Key: "else", Value: "$secret_state"},
	}}}
}

// wasSent is true for a row that already has a sent_at.
var wasSent = bson.D{{Key: "$ne", Value: bson.A{
	bson.D{{Key: "$ifNull", Value: bson.A{"$sent_at", nil}}}, nil,
}}}

// InsertBatch inserts with ordered:false and counts the duplicate-key errors:
// re-sending a chunk inserts only the rows that are new (architecture 7.2).
func (r *deliveryRepo) InsertBatch(ctx context.Context, ds []store.Delivery) (int, error) {
	if len(ds) == 0 {
		return 0, nil
	}
	// The whole batch is validated before anything is written, so a rejected
	// chunk can be fixed and re-sent as a whole (store/delivery.go).
	for i := range ds {
		if ds[i].EmailNorm == "" {
			return 0, fmt.Errorf("%w: delivery %d has no email_norm", store.ErrInvalid, i)
		}
	}
	now := r.s.p.now()
	docs := make([]any, 0, len(ds))
	for i := range ds {
		d := ds[i]
		if d.ID == "" {
			d.ID = store.NewID()
		}
		d.TenantID = r.s.tenant
		if d.CreatedAt.IsZero() {
			d.CreatedAt = now
		}
		d.UpdatedAt = now
		if d.NextAttemptAt.IsZero() {
			d.NextAttemptAt = now
		}
		insertSecret(&d)
		docs = append(docs, encDelivery(&d))
	}
	if _, err := r.coll().InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil {
		dups, rest := countDuplicates(err)
		if rest != nil {
			return 0, wrap("insert deliveries", rest)
		}
		return len(ds) - dups, nil
	}
	return len(ds), nil
}

func (r *deliveryRepo) Get(ctx context.Context, id string) (*store.Delivery, error) {
	var d deliveryDoc
	err := r.coll().FindOne(ctx, r.s.scope(bson.E{Key: "_id", Value: id})).Decode(&d)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, notFound("delivery", id)
		}
		return nil, wrap("get delivery", err)
	}
	return decDelivery(&d), nil
}

func (r *deliveryRepo) ListByCampaign(ctx context.Context, campaignID string, f store.DeliveryFilter, p store.Page) (store.Result[store.Delivery], error) {
	f.CampaignID = &campaignID
	return r.List(ctx, f, p)
}

func (r *deliveryRepo) List(ctx context.Context, f store.DeliveryFilter, p store.Page) (store.Result[store.Delivery], error) {
	filter := r.s.scope()
	if f.CampaignID != nil {
		filter = append(filter, campaignEq(*f.CampaignID))
	}
	if len(f.Lanes) > 0 {
		filter = append(filter, bson.E{Key: "lane",
			Value: bson.D{{Key: "$in", Value: laneInts(f.Lanes)}}})
	}
	// One created_at entry carrying both bounds: a bson.D with the key twice
	// would silently keep only the last.
	if !f.Since.IsZero() || !f.Until.IsZero() {
		rng := bson.D{}
		if !f.Since.IsZero() {
			rng = append(rng, bson.E{Key: "$gte", Value: ts(f.Since)})
		}
		if !f.Until.IsZero() {
			rng = append(rng, bson.E{Key: "$lt", Value: ts(f.Until)})
		}
		filter = append(filter, bson.E{Key: "created_at", Value: rng})
	}
	if len(f.Statuses) > 0 {
		filter = append(filter, bson.E{Key: "status",
			Value: bson.D{{Key: "$in", Value: statusInts(f.Statuses)}}})
	}
	if len(f.ErrorClasses) > 0 {
		filter = append(filter, bson.E{Key: "last_error_class",
			Value: bson.D{{Key: "$in", Value: classInts(f.ErrorClasses)}}})
	}
	if f.EmailNorm != "" {
		filter = append(filter, bson.E{Key: "email_norm", Value: f.EmailNorm})
	}
	return listPage[store.Delivery, deliveryDoc](ctx, r.coll(), filter, p, decDelivery)
}

// claimSort is the queue order: priority first, then the oldest due row.
var claimSort = bson.D{
	{Key: "priority", Value: -1},
	{Key: "next_attempt_at", Value: 1},
	{Key: "_id", Value: 1},
}

// Claim is the three-step, transaction-free handshake of architecture 5.3:
// pick candidates, move them to leased with a claim token that is unique to
// this call, then read back exactly the rows this call won. Losing candidates
// to a concurrent claimer is fine (the next loop picks up whatever is left);
// handing the same row to two claimers is not, which is what the token
// prevents even when both callers use the same worker ID.
func (r *deliveryRepo) Claim(ctx context.Context, req store.ClaimRequest) ([]store.Delivery, error) {
	filter := r.s.scope(
		bson.E{Key: "lane", Value: int32(req.Lane)},
		bson.E{Key: "next_attempt_at", Value: bson.D{{Key: "$lte", Value: ts(req.Now)}}},
	)
	// Queued and deferred are always claimable; pending only for a campaign
	// the caller named, which is what makes starting a campaign a one-row
	// write (store.DeliveryRepo.Claim).
	ready := bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: statusInts(claimable)}}}}
	noCampaign := bson.D{{Key: "campaign_id", Value: bson.D{{Key: "$exists", Value: false}}}}
	switch {
	case req.CampaignIDs == nil:
		filter = append(filter, ready...)
	case len(req.CampaignIDs) == 0:
		filter = append(filter, ready...)
		filter = append(filter, noCampaign...)
	default:
		inCampaigns := bson.D{{Key: "campaign_id", Value: bson.D{{Key: "$in", Value: req.CampaignIDs}}}}
		// Two independent $or clauses, so they go under one $and: a bson.D
		// with the key twice would silently keep only the last.
		filter = append(filter, bson.E{Key: "$and", Value: bson.A{
			bson.D{{Key: "$or", Value: bson.A{noCampaign, inCampaigns}}},
			bson.D{{Key: "$or", Value: bson.A{
				ready,
				and(bson.D{{Key: "status", Value: int32(store.DeliveryPending)}}, inCampaigns),
			}}},
		}})
	}

	ids, err := findIDs(ctx, r.coll(), filter, claimSort, req.Limit)
	if err != nil || len(ids) == 0 {
		return nil, err
	}

	token := store.NewID()
	leaseFilter := and(filter, bson.D{inIDs(ids)})
	if _, err := r.coll().UpdateMany(ctx, leaseFilter, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: int32(store.DeliveryLeased)},
		{Key: "lease_owner", Value: req.WorkerID},
		{Key: "lease_until", Value: ts(req.Now.Add(req.LeaseFor))},
		{Key: "claim_token", Value: token},
		{Key: "updated_at", Value: ts(req.Now)},
	}}}); err != nil {
		return nil, wrap("claim deliveries", err)
	}

	cur, err := r.coll().Find(ctx,
		r.s.scope(inIDs(ids), bson.E{Key: "claim_token", Value: token}),
		options.Find().SetSort(claimSort))
	if err != nil {
		return nil, wrap("claim deliveries", err)
	}
	var docs []deliveryDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, wrap("claim deliveries", err)
	}
	out := make([]store.Delivery, 0, len(docs))
	for i := range docs {
		out = append(out, *decDelivery(&docs[i]))
	}
	return out, nil
}

// Complete applies each result only while its delivery is still held by that
// exact lease owner, which makes a replayed batch a no-op. The attempts of the
// results that applied go in as one insert.
func (r *deliveryRepo) Complete(ctx context.Context, results []store.DeliveryResult) error {
	if len(results) == 0 {
		return nil
	}
	now := r.s.p.now()
	attempts := make([]any, 0, len(results))
	for _, res := range results {
		if res.LeaseOwner == "" {
			continue // an empty owner never holds a lease
		}
		set := bson.D{
			{Key: "status", Value: int32(res.NewStatus)},
			{Key: "last_error_class", Value: int32(res.ErrorClass)},
			{Key: "last_smtp_code", Value: i32(res.SMTPCode)},
			{Key: "last_error", Value: lit(res.Error)},
			{Key: "lease_owner", Value: lit("")},
			{Key: "lease_until", Value: nil},
			{Key: "claim_token", Value: lit("")},
			{Key: "updated_at", Value: ts(now)},
		}
		if res.MessageID != "" {
			set = append(set, bson.E{Key: "message_id", Value: lit(res.MessageID)})
		}
		if res.IncrementAttempt {
			set = append(set, bson.E{Key: "attempt_count", Value: bson.D{{Key: "$add", Value: bson.A{
				bson.D{{Key: "$ifNull", Value: bson.A{"$attempt_count", 0}}}, 1,
			}}}})
		}
		if !res.NextAttemptAt.IsZero() {
			set = append(set, bson.E{Key: "next_attempt_at", Value: ts(res.NextAttemptAt)})
		}
		if res.NewStatus == store.DeliverySent {
			// MarkSent may already have stamped it; keep the earlier time.
			set = append(set, bson.E{Key: "sent_at",
				Value: bson.D{{Key: "$ifNull", Value: bson.A{"$sent_at", ts(now)}}}})
		}
		if res.NewStatus.Terminal() {
			set = append(set, bson.E{Key: "finished_at", Value: ts(now)})
			// Settle a pending secret in this same update. Every expression
			// in one $set stage reads the document as it was before it, so
			// "$sent_at" is the MarkSent stamp, not the one set just above.
			sent := any(wasSent)
			if res.NewStatus == store.DeliverySent {
				sent = true
			}
			expired := bson.D{{Key: "$and", Value: bson.A{
				bson.D{{Key: "$ne", Value: bson.A{
					bson.D{{Key: "$ifNull", Value: bson.A{"$secret_expires_at", nil}}}, nil,
				}}},
				bson.D{{Key: "$lte", Value: bson.A{"$secret_expires_at", ts(now)}}},
			}}}
			set = append(set,
				bson.E{Key: "secret_state", Value: ifPending(bson.D{{Key: "$switch", Value: bson.D{
					{Key: "branches", Value: bson.A{
						bson.D{{Key: "case", Value: sent}, {Key: "then", Value: int32(store.SecretConsumed)}},
						bson.D{{Key: "case", Value: expired}, {Key: "then", Value: int32(store.SecretExpired)}},
					}},
					{Key: "default", Value: int32(store.SecretDiscarded)},
				}}})},
				bson.E{Key: "secret_ct", Value: nil})
		}
		upd, err := r.coll().UpdateOne(ctx,
			r.s.scope(
				bson.E{Key: "_id", Value: res.DeliveryID},
				bson.E{Key: "lease_owner", Value: res.LeaseOwner},
			),
			mongo.Pipeline{{{Key: "$set", Value: set}}})
		if err != nil {
			return wrap("complete deliveries", err)
		}
		if upd.MatchedCount == 0 {
			continue // stale result: the lease moved on, drop its attempt too
		}
		if res.Attempt != nil {
			a := *res.Attempt
			if a.ID == "" {
				a.ID = store.NewID()
			}
			a.TenantID = r.s.tenant
			a.DeliveryID = res.DeliveryID
			if a.CreatedAt.IsZero() {
				a.CreatedAt = now
			}
			attempts = append(attempts, encAttempt(&a))
		}
	}
	if len(attempts) > 0 {
		if _, err := r.attempts().InsertMany(ctx, attempts); err != nil {
			return wrap("insert attempts", err)
		}
	}
	return nil
}

// MarkSent is the fast path recorded right after SMTP 250. It keeps the lease
// so the batched Complete can still attach the attempt.
func (r *deliveryRepo) MarkSent(ctx context.Context, id, owner, messageID string, at time.Time) error {
	res, err := r.coll().UpdateOne(ctx,
		r.s.scope(
			bson.E{Key: "_id", Value: id},
			bson.E{Key: "lease_owner", Value: owner},
			bson.E{Key: "status", Value: int32(store.DeliveryLeased)},
		),
		// A pipeline update, so that the secret is settled in the same
		// write as the status (ADR-0019).
		mongo.Pipeline{{{Key: "$set", Value: bson.D{
			{Key: "status", Value: int32(store.DeliverySent)},
			{Key: "message_id", Value: lit(messageID)},
			{Key: "sent_at", Value: ts(at)},
			{Key: "finished_at", Value: ts(at)},
			{Key: "updated_at", Value: ts(at)},
			{Key: "secret_state", Value: ifPending(int32(store.SecretConsumed))},
			{Key: "secret_ct", Value: nil},
		}}}})
	if err != nil {
		return wrap("mark sent", err)
	}
	if res.MatchedCount == 0 {
		n, err := r.coll().CountDocuments(ctx, r.s.scope(bson.E{Key: "_id", Value: id}),
			options.Count().SetLimit(1))
		if err != nil {
			return wrap("mark sent", err)
		}
		if n == 0 {
			return notFound("delivery", id)
		}
		return fmt.Errorf("%w: delivery %s", store.ErrLeaseLost, id)
	}
	return nil
}

// markTerminal is the asynchronous bounce/complaint transition. A DSN arrives
// long after the send, so there is no lease to authorize it and the CAS is on
// status = sent alone. The lease MarkSent left in place is cleared so that a
// Complete still in flight cannot move the row back to sent.
func (r *deliveryRepo) markTerminal(ctx context.Context, id string, to store.DeliveryStatus, at time.Time) (bool, error) {
	res, err := r.coll().UpdateOne(ctx,
		r.s.scope(
			bson.E{Key: "_id", Value: id},
			bson.E{Key: "status", Value: int32(store.DeliverySent)},
		),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: int32(to)},
			{Key: "finished_at", Value: ts(at)},
			{Key: "updated_at", Value: ts(at)},
			{Key: "lease_owner", Value: ""},
			{Key: "lease_until", Value: nil},
		}}})
	if err != nil {
		return false, wrap("mark "+to.String(), err)
	}
	// Not in status sent (already bounced, never sent, removed by retention)
	// is changed=false, not an error: that is what makes a redelivered DSN
	// idempotent.
	return res.MatchedCount > 0, nil
}

func (r *deliveryRepo) MarkBounced(ctx context.Context, id string, at time.Time) (bool, error) {
	return r.markTerminal(ctx, id, store.DeliveryBounced, at)
}

func (r *deliveryRepo) MarkComplained(ctx context.Context, id string, at time.Time) (bool, error) {
	return r.markTerminal(ctx, id, store.DeliveryComplained, at)
}

// ReleaseExpiredLeases recycles leases that ran out: a row that already burned
// an attempt comes back as deferred, an untouched one as queued. Rows that
// MarkSent already finished are not leased any more and stay untouched.
func (r *deliveryRepo) ReleaseExpiredLeases(ctx context.Context, now time.Time, limit int) (int, error) {
	filter := r.s.scope(
		bson.E{Key: "status", Value: int32(store.DeliveryLeased)},
		bson.E{Key: "lease_until", Value: bson.D{{Key: "$lt", Value: ts(now)}}},
	)
	ids, err := findIDs(ctx, r.coll(), filter, idSort, limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	res, err := r.coll().UpdateMany(ctx, and(filter, bson.D{inIDs(ids)}),
		mongo.Pipeline{{{Key: "$set", Value: bson.D{
			{Key: "status", Value: bson.D{{Key: "$cond", Value: bson.D{
				{Key: "if", Value: bson.D{{Key: "$gt", Value: bson.A{
					bson.D{{Key: "$ifNull", Value: bson.A{"$attempt_count", 0}}}, 0,
				}}}},
				{Key: "then", Value: int32(store.DeliveryDeferred)},
				{Key: "else", Value: int32(store.DeliveryQueued)},
			}}}},
			{Key: "lease_owner", Value: lit("")},
			{Key: "lease_until", Value: nil},
			{Key: "claim_token", Value: lit("")},
			{Key: "next_attempt_at", Value: ts(now)},
			{Key: "updated_at", Value: ts(now)},
		}}}})
	if err != nil {
		return 0, wrap("release expired leases", err)
	}
	return int(res.MatchedCount), nil
}

// ExpireSecrets fails pending secrets whose expiry has passed. The update
// re-applies the filter, so a row a sender claimed between the find and the
// update stays with that sender. A null secret_expires_at ("no expiry") never
// matches $lte on a date.
func (r *deliveryRepo) ExpireSecrets(ctx context.Context, now time.Time, limit int) (int, error) {
	filter := r.s.scope(
		bson.E{Key: "secret_state", Value: int32(store.SecretPending)},
		bson.E{Key: "secret_expires_at", Value: bson.D{{Key: "$lte", Value: ts(now)}}},
		bson.E{Key: "status", Value: bson.D{{Key: "$ne", Value: int32(store.DeliveryLeased)}}},
	)
	ids, err := findIDs(ctx, r.coll(), filter, secretExpirySort, limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	res, err := r.coll().UpdateMany(ctx, and(filter, bson.D{inIDs(ids)}),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: int32(store.DeliveryFailed)},
			{Key: "finished_at", Value: ts(now)},
			{Key: "last_error_class", Value: int32(store.ErrorClassPermanent)},
			{Key: "last_error", Value: store.SecretExpiredError},
			{Key: "secret_state", Value: int32(store.SecretExpired)},
			{Key: "secret_ct", Value: nil},
			{Key: "updated_at", Value: ts(now)},
		}}})
	if err != nil {
		return 0, wrap("expire secrets", err)
	}
	return int(res.MatchedCount), nil
}

// secretExpirySort walks pending secrets oldest expiry first.
var secretExpirySort = bson.D{{Key: "secret_expires_at", Value: 1}, {Key: "_id", Value: 1}}

func (r *deliveryRepo) CountByStatus(ctx context.Context, campaignID string) (map[store.DeliveryStatus]int64, error) {
	cur, err := r.coll().Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: r.s.scope(campaignEq(campaignID))}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$status"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	})
	if err != nil {
		return nil, wrap("count deliveries by status", err)
	}
	var rows []struct {
		Status int32 `bson:"_id"`
		N      int64 `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, wrap("count deliveries by status", err)
	}
	out := make(map[store.DeliveryStatus]int64, len(rows))
	for _, row := range rows {
		out[store.DeliveryStatus(enum8(row.Status))] = row.N
	}
	return out, nil
}

// BulkTransition moves up to limit deliveries between statuses so that cancel
// can walk a million rows in chunks.
func (r *deliveryRepo) BulkTransition(ctx context.Context, campaignID string, from []store.DeliveryStatus, to store.DeliveryStatus, limit int) (int, error) {
	cond := bson.D{{Key: "$ne", Value: int32(to)}}
	if len(from) > 0 {
		cond = append(cond, bson.E{Key: "$in", Value: statusInts(from)})
	}
	filter := r.s.scope(campaignEq(campaignID), bson.E{Key: "status", Value: cond})

	ids, err := findIDs(ctx, r.coll(), filter, idSort, limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	now := r.s.p.now()
	set := bson.D{
		{Key: "status", Value: int32(to)},
		{Key: "updated_at", Value: ts(now)},
	}
	if to.Terminal() {
		// A pending secret is consumed if the row was sent and discarded
		// otherwise; a cancel is never an expiry, even past
		// secret_expires_at.
		sent := bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: "$eq", Value: bson.A{"$status", int32(store.DeliverySent)}}},
			wasSent,
		}}}
		set = append(set,
			bson.E{Key: "finished_at", Value: ts(now)},
			bson.E{Key: "secret_state", Value: ifPending(bson.D{{Key: "$cond", Value: bson.A{
				sent, int32(store.SecretConsumed), int32(store.SecretDiscarded),
			}}})},
			bson.E{Key: "secret_ct", Value: nil})
	}
	res, err := r.coll().UpdateMany(ctx, and(filter, bson.D{inIDs(ids)}),
		mongo.Pipeline{{{Key: "$set", Value: set}}})
	if err != nil {
		return 0, wrap("bulk transition", err)
	}
	return int(res.MatchedCount), nil
}

// Requeue is the manual retry: matching deliveries go back to queued with
// RetryGen bumped and AttemptCount kept. A sensitive delivery is never
// matched (ADR-0019); the null also matches a document written before
// secret_state existed, which is SecretNone.
func (r *deliveryRepo) Requeue(ctx context.Context, f store.RetryFilter, limit int) (int, error) {
	filter := r.s.scope(campaignEq(f.CampaignID),
		bson.E{Key: "secret_state", Value: bson.D{{Key: "$in", Value: bson.A{int32(store.SecretNone), nil}}}})
	if f.DeliveryIDs != nil {
		// A non-nil but empty list matches nothing (store/delivery.go).
		filter = append(filter, inIDs(f.DeliveryIDs))
	}
	if len(f.Statuses) > 0 {
		filter = append(filter, bson.E{Key: "status",
			Value: bson.D{{Key: "$in", Value: statusInts(f.Statuses)}}})
	}
	if len(f.ErrorClasses) > 0 {
		filter = append(filter, bson.E{Key: "last_error_class",
			Value: bson.D{{Key: "$in", Value: classInts(f.ErrorClasses)}}})
	}
	ids, err := findIDs(ctx, r.coll(), filter, idSort, limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	res, err := r.coll().UpdateMany(ctx, and(filter, bson.D{inIDs(ids)}), bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "status", Value: int32(store.DeliveryQueued)},
			{Key: "next_attempt_at", Value: ts(f.Now)},
			{Key: "lease_owner", Value: ""},
			{Key: "lease_until", Value: nil},
			{Key: "claim_token", Value: ""},
			{Key: "finished_at", Value: nil},
			{Key: "updated_at", Value: ts(f.Now)},
		}},
		{Key: "$inc", Value: bson.D{{Key: "retry_gen", Value: 1}}},
	})
	if err != nil {
		return 0, wrap("requeue deliveries", err)
	}
	return int(res.MatchedCount), nil
}

// setFirst is the conditional NULL-only update the unique tracking counts rest
// on: it only matches while the column is still unset.
func (r *deliveryRepo) setFirst(ctx context.Context, field, id string, at time.Time) (bool, error) {
	res, err := r.coll().UpdateOne(ctx,
		r.s.scope(
			bson.E{Key: "_id", Value: id},
			bson.E{Key: field, Value: nil},
		),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: field, Value: ts(at)},
			{Key: "updated_at", Value: ts(at)},
		}}})
	if err != nil {
		return false, wrap("set "+field, err)
	}
	// No match also covers a delivery retention already removed: the token is
	// dropped, not an error (architecture 9.1).
	return res.MatchedCount > 0, nil
}

func (r *deliveryRepo) SetFirstOpened(ctx context.Context, id string, at time.Time) (bool, error) {
	return r.setFirst(ctx, "first_opened_at", id, at)
}

func (r *deliveryRepo) SetFirstClicked(ctx context.Context, id string, at time.Time) (bool, error) {
	return r.setFirst(ctx, "first_clicked_at", id, at)
}

func (r *deliveryRepo) SetUnsubscribed(ctx context.Context, id string, at time.Time) (bool, error) {
	return r.setFirst(ctx, "unsubscribed_at", id, at)
}

// DeleteBefore enforces retention in chunks, dropping the attempts with it.
func (r *deliveryRepo) DeleteBefore(ctx context.Context, campaignID string, before time.Time, limit int) (int, error) {
	filter := r.s.scope(campaignEq(campaignID),
		bson.E{Key: "created_at", Value: bson.D{{Key: "$lt", Value: ts(before)}}})
	ids, err := findIDs(ctx, r.coll(), filter, idSort, limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	res, err := r.coll().DeleteMany(ctx, r.s.scope(inIDs(ids)))
	if err != nil {
		return 0, wrap("delete deliveries", err)
	}
	if _, err := r.attempts().DeleteMany(ctx, r.s.scope(
		bson.E{Key: "delivery_id", Value: bson.D{{Key: "$in", Value: ids}}})); err != nil {
		return 0, wrap("delete attempts", err)
	}
	return int(res.DeletedCount), nil
}

// --- attempts ----------------------------------------------------------

type attemptRepo struct{ s *tenantStore }

func (r *attemptRepo) coll() *mongo.Collection { return r.s.coll(collAttempt) }

func (r *attemptRepo) Insert(ctx context.Context, as []store.DeliveryAttempt) error {
	if len(as) == 0 {
		return nil
	}
	now := r.s.p.now()
	docs := make([]any, 0, len(as))
	for i := range as {
		a := as[i]
		if a.ID == "" {
			a.ID = store.NewID()
		}
		a.TenantID = r.s.tenant
		if a.CreatedAt.IsZero() {
			a.CreatedAt = now
		}
		docs = append(docs, encAttempt(&a))
	}
	_, err := r.coll().InsertMany(ctx, docs)
	return wrap("insert attempts", err)
}

func (r *attemptRepo) ListByDelivery(ctx context.Context, deliveryID string, p store.Page) (store.Result[store.DeliveryAttempt], error) {
	return listPage[store.DeliveryAttempt, attemptDoc](ctx, r.coll(),
		r.s.scope(bson.E{Key: "delivery_id", Value: deliveryID}), p, decAttempt)
}
