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
	filter := r.s.scope(campaignEq(campaignID))
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
		bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: statusInts(claimable)}}},
		bson.E{Key: "next_attempt_at", Value: bson.D{{Key: "$lte", Value: ts(req.Now)}}},
	)
	if req.CampaignIDs != nil {
		noCampaign := bson.D{{Key: "campaign_id", Value: bson.D{{Key: "$exists", Value: false}}}}
		if len(req.CampaignIDs) == 0 {
			filter = append(filter, noCampaign...)
		} else {
			filter = append(filter, bson.E{Key: "$or", Value: bson.A{
				noCampaign,
				bson.D{{Key: "campaign_id", Value: bson.D{{Key: "$in", Value: req.CampaignIDs}}}},
			}})
		}
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
			{Key: "last_smtp_code", Value: int32(res.SMTPCode)},
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
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: int32(store.DeliverySent)},
			{Key: "message_id", Value: messageID},
			{Key: "sent_at", Value: ts(at)},
			{Key: "finished_at", Value: ts(at)},
			{Key: "updated_at", Value: ts(at)},
		}}})
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
		out[store.DeliveryStatus(row.Status)] = row.N
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
		set = append(set, bson.E{Key: "finished_at", Value: ts(now)})
	}
	res, err := r.coll().UpdateMany(ctx, and(filter, bson.D{inIDs(ids)}),
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return 0, wrap("bulk transition", err)
	}
	return int(res.MatchedCount), nil
}

// Requeue is the manual retry: matching deliveries go back to queued with
// RetryGen bumped and AttemptCount kept.
func (r *deliveryRepo) Requeue(ctx context.Context, f store.RetryFilter, limit int) (int, error) {
	filter := r.s.scope(campaignEq(f.CampaignID))
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
