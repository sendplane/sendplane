package mongo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sendplane/sendplane/store"
)

// key builds the _id of an aggregate that has no ID of its own. It starts with
// the tenant so that one tenant can never address another tenant's row.
func key(parts ...string) string { return strings.Join(parts, "\x00") }

// --- message versions --------------------------------------------------

type versionRepo struct {
	*table[store.MessageVersion, versionDoc, *versionDoc]
}

func (r *versionRepo) ListByTemplate(ctx context.Context, templateID string, p store.Page) (store.Result[store.MessageVersion], error) {
	return r.listWhere(ctx, bson.D{{Key: "template_id", Value: templateID}}, p)
}

// --- templates and layouts ---------------------------------------------

type templateRepo struct {
	*table[store.Template, templateDoc, *templateDoc]
}

func (r *templateRepo) GetByKey(ctx context.Context, key string) (*store.Template, error) {
	return getByKey(ctx, r.table, key)
}

type layoutRepo struct {
	*table[store.Layout, layoutDoc, *layoutDoc]
}

func (r *layoutRepo) GetByKey(ctx context.Context, key string) (*store.Layout, error) {
	return getByKey(ctx, r.table, key)
}

// getByKey reads the document whose key is key. The empty key never matches:
// it means "no key", and the partial unique index exempts it too.
func getByKey[T any, D any, PD docPtr[D]](ctx context.Context, t *table[T, D, PD], key string) (*T, error) {
	if key == "" {
		return nil, notFound(t.m.kind, "with an empty key")
	}
	var d D
	err := t.coll().FindOne(ctx, t.s.scope(bson.E{Key: "key", Value: key})).Decode(&d)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, notFound(t.m.kind, "with key "+key)
		}
		return nil, wrap("get "+t.m.kind+" by key", err)
	}
	return t.m.dec(&d), nil
}

// --- probe runs --------------------------------------------------------

type probeRunRepo struct {
	*table[store.ProbeRun, probeRunDoc, *probeRunDoc]
}

func (r *probeRunRepo) ListBySender(ctx context.Context, senderID string, p store.Page) (store.Result[store.ProbeRun], error) {
	return r.listWhere(ctx, bson.D{{Key: "sender_id", Value: senderID}}, p)
}

func (r *probeRunRepo) ListPending(ctx context.Context, p store.Page) (store.Result[store.ProbeRun], error) {
	return r.listWhere(ctx, bson.D{{Key: "pending", Value: true}}, p)
}

// --- bounce mailboxes --------------------------------------------------

type bounceMailboxRepo struct {
	*table[store.BounceMailbox, bounceMailboxDoc, *bounceMailboxDoc]
}

// ListEnabled returns the whole enabled set in one call; the partial index
// bounce_mailbox_enabled covers it.
func (r *bounceMailboxRepo) ListEnabled(ctx context.Context) ([]store.BounceMailbox, error) {
	cur, err := r.coll().Find(ctx,
		r.s.scope(bson.E{Key: "enabled", Value: true}),
		options.Find().SetSort(listSort))
	if err != nil {
		return nil, wrap("list enabled bounce mailboxes", err)
	}
	var docs []bounceMailboxDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, wrap("list enabled bounce mailboxes", err)
	}
	out := make([]store.BounceMailbox, 0, len(docs))
	for i := range docs {
		out = append(out, *r.m.dec(&docs[i]))
	}
	return out, nil
}

func (r *bounceMailboxRepo) UpdateHealth(ctx context.Context, id string, h store.MailboxHealth) error {
	return updateHealth(ctx, r.s, r.coll(), r.m.kind, id, h)
}

// --- probe mailboxes ---------------------------------------------------

type probeMailboxRepo struct {
	*table[store.ProbeMailbox, probeMailboxDoc, *probeMailboxDoc]
}

func (r *probeMailboxRepo) UpdateHealth(ctx context.Context, id string, h store.MailboxHealth) error {
	return updateHealth(ctx, r.s, r.coll(), r.m.kind, id, h)
}

// updateHealth is the shared UpdateHealth write: the health sub-document
// alone, with no version predicate, no version bump and no updated_at, so a
// background check never fights an operator's edit (store.MailboxHealth).
func updateHealth(ctx context.Context, s *tenantStore, coll *mongo.Collection,
	kind, id string, h store.MailboxHealth) error {
	res, err := coll.UpdateOne(ctx,
		s.scope(bson.E{Key: "_id", Value: id}),
		bson.D{{Key: "$set", Value: bson.D{{Key: "health", Value: encHealth(h)}}}})
	if err != nil {
		return wrap("update "+kind+" health", err)
	}
	if res.MatchedCount == 0 {
		return notFound(kind, id)
	}
	return nil
}

// --- bounces -----------------------------------------------------------

type bounceRepo struct {
	*table[store.BounceEvent, bounceDoc, *bounceDoc]
}

func (r *bounceRepo) ListByDelivery(ctx context.Context, deliveryID string, p store.Page) (store.Result[store.BounceEvent], error) {
	return r.listWhere(ctx, bson.D{{Key: "delivery_id", Value: deliveryID}}, p)
}

func (r *bounceRepo) DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	return deleteBefore(ctx, r.s, r.coll(), before, limit)
}

// deleteBefore is the shared retention delete: the oldest matching ids first
// (_id is UUIDv7, so id order is creation order), at most limit of them, then
// one DeleteMany on those ids. Selecting the ids first is what bounds a chunk;
// DeleteMany alone has no limit.
func deleteBefore(ctx context.Context, s *tenantStore, coll *mongo.Collection,
	before time.Time, limit int, extra ...bson.E) (int, error) {
	filter := s.scope(append([]bson.E{
		{Key: "created_at", Value: bson.D{{Key: "$lt", Value: ts(before)}}},
	}, extra...)...)
	ids, err := findIDs(ctx, coll, filter, idSort, limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	res, err := coll.DeleteMany(ctx, s.scope(inIDs(ids)))
	if err != nil {
		return 0, wrap("delete "+coll.Name(), err)
	}
	return int(res.DeletedCount), nil
}

// --- campaigns ---------------------------------------------------------

type campaignRepo struct {
	*table[store.Campaign, campaignDoc, *campaignDoc]
}

func (r *campaignRepo) ListByStatus(ctx context.Context, statuses []store.CampaignStatus, p store.Page) (store.Result[store.Campaign], error) {
	filter := bson.D{}
	if len(statuses) > 0 {
		filter = bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: campaignStatusInts(statuses)}}}}
	}
	return r.listWhere(ctx, filter, p)
}

// UpdateStats refreshes the cached aggregate without taking part in optimistic
// concurrency: the control loop must not fight API edits.
func (r *campaignRepo) UpdateStats(ctx context.Context, campaignID string, s store.CampaignStats) error {
	res, err := r.coll().UpdateOne(ctx,
		r.s.scope(bson.E{Key: "_id", Value: campaignID}),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "stats", Value: encStats(s)},
			{Key: "updated_at", Value: ts(r.s.p.now())},
		}}})
	if err != nil {
		return wrap("update campaign stats", err)
	}
	if res.MatchedCount == 0 {
		return notFound("campaign", campaignID)
	}
	return nil
}

// --- tenant settings ---------------------------------------------------

type settingsRepo struct{ s *tenantStore }

func (r *settingsRepo) coll() *mongo.Collection { return r.s.coll(collTenantSettings) }

func (r *settingsRepo) Get(ctx context.Context) (*store.TenantSettings, error) {
	var d settingsDoc
	err := r.coll().FindOne(ctx, r.s.scope(bson.E{Key: "_id", Value: r.s.tenant})).Decode(&d)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, notFound("tenant settings", r.s.tenant)
		}
		return nil, wrap("get tenant settings", err)
	}
	return decSettings(&d), nil
}

func (r *settingsRepo) Create(ctx context.Context, v *store.TenantSettings) error {
	if v == nil {
		return fmt.Errorf("%w: nil tenant settings", store.ErrInvalid)
	}
	v.TenantID = r.s.tenant
	now := r.s.p.now()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	v.Version = 1
	if _, err := r.coll().InsertOne(ctx, encSettings(v)); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return conflict("tenant settings", r.s.tenant)
		}
		return wrap("create tenant settings", err)
	}
	return nil
}

func (r *settingsRepo) Update(ctx context.Context, v *store.TenantSettings) error {
	if v == nil {
		return fmt.Errorf("%w: nil tenant settings", store.ErrInvalid)
	}
	next := *v
	next.TenantID = r.s.tenant
	next.UpdatedAt = r.s.p.now()
	next.Version = v.Version + 1
	set, err := setFields(encSettings(&next), "_id", "created_at")
	if err != nil {
		return wrap("update tenant settings", err)
	}
	filter := r.s.scope(
		bson.E{Key: "_id", Value: r.s.tenant},
		bson.E{Key: "version", Value: v.Version},
	)
	res, err := r.coll().UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return wrap("update tenant settings", err)
	}
	if res.MatchedCount == 0 {
		n, err := r.coll().CountDocuments(ctx,
			r.s.scope(bson.E{Key: "_id", Value: r.s.tenant}), options.Count().SetLimit(1))
		if err != nil {
			return wrap("update tenant settings", err)
		}
		if n == 0 {
			return notFound("tenant settings", r.s.tenant)
		}
		return conflict("tenant settings", r.s.tenant)
	}
	*v = next
	return nil
}

// --- recipient chunks --------------------------------------------------

type chunkRepo struct{ s *tenantStore }

func (r *chunkRepo) coll() *mongo.Collection { return r.s.coll(collChunk) }

func (r *chunkRepo) Get(ctx context.Context, campaignID, chunkKey string) (*store.RecipientChunk, error) {
	id := key(r.s.tenant, campaignID, chunkKey)
	var d chunkDoc
	err := r.coll().FindOne(ctx, r.s.scope(bson.E{Key: "_id", Value: id})).Decode(&d)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, notFound("chunk", campaignID+"/"+chunkKey)
		}
		return nil, wrap("get chunk", err)
	}
	return decChunk(&d), nil
}

func (r *chunkRepo) Put(ctx context.Context, c *store.RecipientChunk) error {
	if c == nil || c.CampaignID == "" || c.Key == "" {
		return fmt.Errorf("%w: chunk needs a campaign and a key", store.ErrInvalid)
	}
	c.TenantID = r.s.tenant
	now := r.s.p.now()
	created := c.CreatedAt
	if created.IsZero() {
		created = now
	}
	c.UpdatedAt = now
	id := key(r.s.tenant, c.CampaignID, c.Key)
	_, err := r.coll().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "tenant_id", Value: r.s.tenant},
				{Key: "campaign_id", Value: c.CampaignID},
				{Key: "key", Value: c.Key},
				{Key: "state", Value: string(c.State)},
				{Key: "accepted", Value: int64(c.Accepted)},
				{Key: "duplicates", Value: int64(c.Duplicates)},
				{Key: "invalid", Value: int64(c.Invalid)},
				{Key: "updated_at", Value: ts(now)},
			}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: ts(created)}}},
		},
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		return wrap("put chunk", err)
	}
	return nil
}

// --- suppressions ------------------------------------------------------

type suppressionRepo struct{ s *tenantStore }

func (r *suppressionRepo) coll() *mongo.Collection { return r.s.coll(collSuppression) }

func (r *suppressionRepo) Upsert(ctx context.Context, v *store.Suppression) error {
	if v == nil || v.EmailNorm == "" {
		return fmt.Errorf("%w: suppression needs email_norm", store.ErrInvalid)
	}
	v.TenantID = r.s.tenant
	if v.CreatedAt.IsZero() {
		v.CreatedAt = r.s.p.now()
	}
	id := key(r.s.tenant, v.EmailNorm)
	doc := &suppressionDoc{
		Base:      Base{ID: id, TenantID: r.s.tenant, CreatedAt: ts(v.CreatedAt)},
		EmailNorm: v.EmailNorm, Reason: string(v.Reason),
		SourceDeliveryID: v.SourceDeliveryID, ExpiresAt: encTime(v.ExpiresAt),
	}
	_, err := r.coll().ReplaceOne(ctx, bson.D{{Key: "_id", Value: id}}, doc,
		options.Replace().SetUpsert(true))
	return wrap("upsert suppression", err)
}

func (r *suppressionRepo) IsSuppressed(ctx context.Context, emailNorm string, now time.Time) (bool, *store.Suppression, error) {
	id := key(r.s.tenant, emailNorm)
	var d suppressionDoc
	err := r.coll().FindOne(ctx, r.s.scope(bson.E{Key: "_id", Value: id})).Decode(&d)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return false, nil, nil
		}
		return false, nil, wrap("is suppressed", err)
	}
	s := decSuppression(&d)
	if !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(ts(now)) {
		return false, s, nil
	}
	return true, s, nil
}

func (r *suppressionRepo) List(ctx context.Context, p store.Page) (store.Result[store.Suppression], error) {
	return listPage[store.Suppression, suppressionDoc](ctx, r.coll(), r.s.scope(), p, decSuppression)
}

// DeleteBefore removes expired entries only: a zero ExpiresAt is stored as
// null and never matches (store.SuppressionRepo).
func (r *suppressionRepo) DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	filter := r.s.scope(bson.E{Key: "expires_at", Value: bson.D{
		{Key: "$ne", Value: nil},
		{Key: "$lte", Value: ts(before)},
	}})
	ids, err := findIDs(ctx, r.coll(), filter, idSort, limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	res, err := r.coll().DeleteMany(ctx, r.s.scope(inIDs(ids)))
	if err != nil {
		return 0, wrap("delete suppressions", err)
	}
	return int(res.DeletedCount), nil
}

func (r *suppressionRepo) Delete(ctx context.Context, emailNorm string) error {
	id := key(r.s.tenant, emailNorm)
	res, err := r.coll().DeleteOne(ctx, r.s.scope(bson.E{Key: "_id", Value: id}))
	if err != nil {
		return wrap("delete suppression", err)
	}
	if res.DeletedCount == 0 {
		return notFound("suppression", emailNorm)
	}
	return nil
}

// --- tracking ----------------------------------------------------------

type trackingRepo struct{ s *tenantStore }

func (r *trackingRepo) coll() *mongo.Collection { return r.s.coll(collTracking) }

func (r *trackingRepo) InsertEvents(ctx context.Context, evs []store.TrackingEvent) error {
	if len(evs) == 0 {
		return nil
	}
	now := r.s.p.now()
	docs := make([]any, 0, len(evs))
	for i := range evs {
		e := evs[i]
		if e.ID == "" {
			e.ID = store.NewID()
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		docs = append(docs, &trackingDoc{
			Base:       Base{ID: e.ID, TenantID: r.s.tenant, CreatedAt: ts(e.CreatedAt)},
			DeliveryID: e.DeliveryID, CampaignID: e.CampaignID, Kind: int32(e.Kind),
			URL: e.URL, LinkNo: i32(e.LinkNo), UserAgent: e.UserAgent,
			IPHash: e.IPHash, SuspectedBot: e.SuspectedBot,
		})
	}
	_, err := r.coll().InsertMany(ctx, docs)
	return wrap("insert tracking events", err)
}

// CountUnique counts distinct deliveries per kind, bots excluded.
func (r *trackingRepo) CountUnique(ctx context.Context, campaignID string) (store.TrackingCounts, error) {
	var out store.TrackingCounts
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: r.s.scope(
			bson.E{Key: "campaign_id", Value: campaignID},
			bson.E{Key: "suspected_bot", Value: false},
		)}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: bson.D{
			{Key: "kind", Value: "$kind"},
			{Key: "delivery", Value: "$delivery_id"},
		}}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$_id.kind"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}
	cur, err := r.coll().Aggregate(ctx, pipeline)
	if err != nil {
		return out, wrap("count unique tracking", err)
	}
	var rows []struct {
		Kind int32 `bson:"_id"`
		N    int64 `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return out, wrap("count unique tracking", err)
	}
	for _, row := range rows {
		switch store.TrackingKind(enum8(row.Kind)) {
		case store.TrackingOpen:
			out.UniqueOpens = row.N
		case store.TrackingClick:
			out.UniqueClicks = row.N
		case store.TrackingUnsubscribeClicked:
			out.UnsubscribeClicked = row.N
		case store.TrackingUnsubscribed:
			out.Unsubscribed = row.N
		}
	}
	return out, nil
}

// LinkClicks reports clicks per link URL, ordered by LinkNo then URL.
func (r *trackingRepo) LinkClicks(ctx context.Context, campaignID string) ([]store.LinkClick, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: r.s.scope(
			bson.E{Key: "campaign_id", Value: campaignID},
			bson.E{Key: "kind", Value: int32(store.TrackingClick)},
			bson.E{Key: "suspected_bot", Value: false},
		)}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$url"},
			{Key: "link_no", Value: bson.D{{Key: "$min", Value: "$link_no"}}},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "deliveries", Value: bson.D{{Key: "$addToSet", Value: "$delivery_id"}}},
		}}},
		{{Key: "$project", Value: bson.D{
			{Key: "link_no", Value: 1},
			{Key: "clicks", Value: 1},
			{Key: "unique", Value: bson.D{{Key: "$size", Value: "$deliveries"}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "link_no", Value: 1}, {Key: "_id", Value: 1}}}},
	}
	cur, err := r.coll().Aggregate(ctx, pipeline)
	if err != nil {
		return nil, wrap("link clicks", err)
	}
	var rows []struct {
		URL    string `bson:"_id"`
		LinkNo int32  `bson:"link_no"`
		Clicks int64  `bson:"clicks"`
		Unique int64  `bson:"unique"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, wrap("link clicks", err)
	}
	out := make([]store.LinkClick, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.LinkClick{
			LinkNo: int(row.LinkNo), URL: row.URL,
			Clicks: row.Clicks, UniqueClicks: row.Unique,
		})
	}
	return out, nil
}

// DeleteBefore enforces retention on tracking events in chunks
// (architecture 9.4).
func (r *trackingRepo) DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	return deleteBefore(ctx, r.s, r.coll(), before, limit)
}

// --- outbox ------------------------------------------------------------

type outboxRepo struct{ s *tenantStore }

func (r *outboxRepo) coll() *mongo.Collection { return r.s.coll(collOutbox) }

func (r *outboxRepo) Enqueue(ctx context.Context, evs []store.OutboxEvent) error {
	if len(evs) == 0 {
		return nil
	}
	now := r.s.p.now()
	docs := make([]any, 0, len(evs))
	for i := range evs {
		e := evs[i]
		if e.ID == "" {
			e.ID = store.NewID()
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		if e.NextAttemptAt.IsZero() {
			e.NextAttemptAt = e.CreatedAt
		}
		docs = append(docs, &outboxDoc{
			Base:    Base{ID: e.ID, TenantID: r.s.tenant, CreatedAt: ts(e.CreatedAt)},
			Type:    e.Type,
			Payload: encRaw(e.Payload),
			Status:  string(store.OutboxPending),
			// Attempts and the lease start empty; the dispatcher owns them.
			NextAttemptAt: encTime(e.NextAttemptAt),
		})
	}
	_, err := r.coll().InsertMany(ctx, docs)
	return wrap("enqueue outbox", err)
}

// ClaimPending leases due pending events with the same token handshake Claim
// uses, so two dispatchers never get the same event.
func (r *outboxRepo) ClaimPending(ctx context.Context, limit int, lease time.Duration, owner string, now time.Time) ([]store.OutboxEvent, error) {
	due := r.s.scope(
		bson.E{Key: "status", Value: string(store.OutboxPending)},
		bson.E{Key: "next_attempt_at", Value: bson.D{{Key: "$lte", Value: ts(now)}}},
	)
	free := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "lease_owner", Value: ""}},
		bson.D{{Key: "lease_until", Value: bson.D{{Key: "$lte", Value: ts(now)}}}},
	}}}
	filter := and(due, free)

	ids, err := findIDs(ctx, r.coll(), filter, idSort, limit)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	token := store.NewID()
	if _, err := r.coll().UpdateMany(ctx, and(filter, bson.D{inIDs(ids)}),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "lease_owner", Value: owner},
			{Key: "lease_until", Value: ts(now.Add(lease))},
			{Key: "claim_token", Value: token},
		}}}); err != nil {
		return nil, wrap("claim outbox", err)
	}
	cur, err := r.coll().Find(ctx,
		r.s.scope(inIDs(ids), bson.E{Key: "claim_token", Value: token}),
		options.Find().SetSort(idSort))
	if err != nil {
		return nil, wrap("claim outbox", err)
	}
	var docs []outboxDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, wrap("claim outbox", err)
	}
	out := make([]store.OutboxEvent, 0, len(docs))
	for i := range docs {
		out = append(out, *decOutbox(&docs[i]))
	}
	return out, nil
}

func (r *outboxRepo) Get(ctx context.Context, id string) (*store.OutboxEvent, error) {
	var d outboxDoc
	err := r.coll().FindOne(ctx, r.s.scope(bson.E{Key: "_id", Value: id})).Decode(&d)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, notFound("outbox event", id)
		}
		return nil, wrap("get outbox event", err)
	}
	return decOutbox(&d), nil
}

// Reset is the replay verb: pending again, due now, with a full attempt
// budget.
func (r *outboxRepo) Reset(ctx context.Context, id string, now time.Time) error {
	res, err := r.coll().UpdateOne(ctx, r.s.scope(bson.E{Key: "_id", Value: id}),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: string(store.OutboxPending)},
			{Key: "attempts", Value: int32(0)},
			{Key: "last_error", Value: ""},
			{Key: "next_attempt_at", Value: ts(now)},
			{Key: "delivered_at", Value: nil},
			{Key: "lease_owner", Value: ""},
			{Key: "lease_until", Value: nil},
			{Key: "claim_token", Value: ""},
		}}})
	if err != nil {
		return wrap("reset outbox event", err)
	}
	if res.MatchedCount == 0 {
		return notFound("outbox event", id)
	}
	return nil
}

func (r *outboxRepo) MarkDelivered(ctx context.Context, id string, at time.Time) error {
	res, err := r.coll().UpdateOne(ctx, r.s.scope(bson.E{Key: "_id", Value: id}),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: string(store.OutboxDelivered)},
			{Key: "delivered_at", Value: ts(at)},
			{Key: "lease_owner", Value: ""},
			{Key: "lease_until", Value: nil},
			{Key: "claim_token", Value: ""},
		}}})
	if err != nil {
		return wrap("mark outbox delivered", err)
	}
	if res.MatchedCount == 0 {
		return notFound("outbox event", id)
	}
	return nil
}

func (r *outboxRepo) MarkFailed(ctx context.Context, id string, nextAttempt time.Time, errMsg string) error {
	set := bson.D{
		{Key: "last_error", Value: errMsg},
		{Key: "lease_owner", Value: ""},
		{Key: "lease_until", Value: nil},
		{Key: "claim_token", Value: ""},
	}
	if nextAttempt.IsZero() {
		set = append(set, bson.E{Key: "status", Value: string(store.OutboxFailed)})
	} else {
		set = append(set,
			bson.E{Key: "status", Value: string(store.OutboxPending)},
			bson.E{Key: "next_attempt_at", Value: ts(nextAttempt)})
	}
	res, err := r.coll().UpdateOne(ctx, r.s.scope(bson.E{Key: "_id", Value: id}),
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}},
		})
	if err != nil {
		return wrap("mark outbox failed", err)
	}
	if res.MatchedCount == 0 {
		return notFound("outbox event", id)
	}
	return nil
}

func (r *outboxRepo) List(ctx context.Context, status store.OutboxStatus, p store.Page) (store.Result[store.OutboxEvent], error) {
	filter := r.s.scope()
	if status != "" {
		filter = append(filter, bson.E{Key: "status", Value: string(status)})
	}
	return listPage[store.OutboxEvent, outboxDoc](ctx, r.coll(), filter, p, decOutbox)
}

// DeleteBefore removes dispatched events only: a pending row is still owed to
// the host, whatever its age.
func (r *outboxRepo) DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	return deleteBefore(ctx, r.s, r.coll(), before, limit, bson.E{
		Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(store.OutboxDelivered), string(store.OutboxFailed),
		}}},
	})
}

// --- locks -------------------------------------------------------------

type lockRepo struct{ s *tenantStore }

func (r *lockRepo) coll() *mongo.Collection { return r.s.coll(collLock) }

// Acquire takes the lock, renews it for the same owner, or takes it over once
// the previous lease expired. It is a conditional upsert: when the filter does
// not match an existing row the server rejects the insert on the _id index,
// which is exactly "someone else still holds it".
func (r *lockRepo) Acquire(ctx context.Context, name, owner string, ttl time.Duration, now time.Time) (bool, error) {
	id := key(r.s.tenant, name)
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "owner", Value: owner}},
			bson.D{{Key: "expires_at", Value: bson.D{{Key: "$lte", Value: ts(now)}}}},
		}},
	}
	update := mongo.Pipeline{{{Key: "$set", Value: bson.D{
		{Key: "tenant_id", Value: lit(r.s.tenant)},
		{Key: "name", Value: lit(name)},
		{Key: "owner", Value: lit(owner)},
		// Keep the original acquisition time while the same owner renews.
		{Key: "acquired_at", Value: bson.D{{Key: "$cond", Value: bson.D{
			{Key: "if", Value: bson.D{{Key: "$eq", Value: bson.A{"$owner", lit(owner)}}}},
			{Key: "then", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$acquired_at", ts(now)}}}},
			{Key: "else", Value: ts(now)},
		}}}},
		{Key: "expires_at", Value: ts(now.Add(ttl))},
	}}}}
	_, err := r.coll().UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, wrap("acquire lock", err)
	}
	return true, nil
}

func (r *lockRepo) Renew(ctx context.Context, name, owner string, ttl time.Duration, now time.Time) (bool, error) {
	res, err := r.coll().UpdateOne(ctx, bson.D{
		{Key: "_id", Value: key(r.s.tenant, name)},
		{Key: "owner", Value: owner},
		{Key: "expires_at", Value: bson.D{{Key: "$gt", Value: ts(now)}}},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "expires_at", Value: ts(now.Add(ttl))}}}})
	if err != nil {
		return false, wrap("renew lock", err)
	}
	return res.MatchedCount > 0, nil
}

func (r *lockRepo) Release(ctx context.Context, name, owner string) error {
	_, err := r.coll().DeleteOne(ctx, bson.D{
		{Key: "_id", Value: key(r.s.tenant, name)},
		{Key: "owner", Value: owner},
	})
	return wrap("release lock", err)
}

func (r *lockRepo) Get(ctx context.Context, name string) (*store.Lock, error) {
	var d lockDoc
	err := r.coll().FindOne(ctx, bson.D{{Key: "_id", Value: key(r.s.tenant, name)}}).Decode(&d)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, notFound("lock", name)
		}
		return nil, wrap("get lock", err)
	}
	return &store.Lock{
		TenantID: d.TenantID, Name: d.Name, Owner: d.Owner,
		AcquiredAt: decTime(&d.AcquiredAt), ExpiresAt: decTime(&d.ExpiresAt),
	}, nil
}

// --- workers -----------------------------------------------------------

type workerRepo struct{ s *tenantStore }

func (r *workerRepo) coll() *mongo.Collection { return r.s.coll(collWorker) }

func (r *workerRepo) Heartbeat(ctx context.Context, w store.Worker) error {
	if w.ID == "" {
		return fmt.Errorf("%w: worker needs an ID", store.ErrInvalid)
	}
	lastSeen := w.LastSeenAt
	if lastSeen.IsZero() {
		lastSeen = r.s.p.now()
	}
	set := bson.D{
		{Key: "tenant_id", Value: r.s.tenant},
		{Key: "worker_id", Value: w.ID},
		{Key: "role", Value: w.Role},
		{Key: "lanes", Value: laneInts(w.Lanes)},
		{Key: "concurrency", Value: i32(w.Concurrency)},
		{Key: "last_seen_at", Value: ts(lastSeen)},
	}
	update := bson.D{{Key: "$set", Value: set}}
	if w.StartedAt.IsZero() {
		// An existing row keeps the start it already has.
		update = append(update, bson.E{Key: "$setOnInsert",
			Value: bson.D{{Key: "started_at", Value: ts(lastSeen)}}})
	} else {
		set = append(set, bson.E{Key: "started_at", Value: ts(w.StartedAt)})
		update = bson.D{{Key: "$set", Value: set}}
	}
	_, err := r.coll().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: key(r.s.tenant, w.ID)}}, update,
		options.UpdateOne().SetUpsert(true))
	return wrap("worker heartbeat", err)
}

func (r *workerRepo) ListActive(ctx context.Context, since time.Time) ([]store.Worker, error) {
	cur, err := r.coll().Find(ctx,
		r.s.scope(bson.E{Key: "last_seen_at", Value: bson.D{{Key: "$gte", Value: ts(since)}}}),
		options.Find().SetSort(bson.D{{Key: "worker_id", Value: 1}}))
	if err != nil {
		return nil, wrap("list workers", err)
	}
	var docs []workerDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, wrap("list workers", err)
	}
	out := make([]store.Worker, 0, len(docs))
	for i := range docs {
		out = append(out, decWorker(&docs[i]))
	}
	return out, nil
}
