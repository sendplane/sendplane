package mongo

import (
	"context"
	"fmt"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sendplane/sendplane/store"
)

// Base is the bookkeeping every document shares. Version and UpdatedAt stay
// unset for immutable aggregates.
//
// It is exported only because the BSON codec skips unexported embedded
// fields, which would silently drop every inlined _id and tenant_id.
type Base struct {
	ID        string `bson:"_id"`
	TenantID  string `bson:"tenant_id"`
	Version   int64  `bson:"version,omitempty"`
	CreatedAt int64  `bson:"created_at"`
	UpdatedAt *int64 `bson:"updated_at"`
}

func (b *Base) baseOf() *Base { return b }

// docPtr constrains a document type to something the generic helpers can read
// the pagination key from.
type docPtr[D any] interface {
	*D
	baseOf() *Base
}

// meta gives the generic table access to the bookkeeping fields of a model and
// to its document codec. version and updated are nil for immutable aggregates.
type meta[T any, D any] struct {
	kind    string
	id      func(*T) *string
	tenant  func(*T) *string
	version func(*T) *int64
	created func(*T) *time.Time
	updated func(*T) *time.Time
	enc     func(*T) *D
	dec     func(*D) *T
}

// table is the shared CRUD implementation. Its method set satisfies the
// uniform repository interfaces (Create/Get/Update/Delete/List) directly.
type table[T any, D any, PD docPtr[D]] struct {
	s    *tenantStore
	name string
	m    meta[T, D]
}

func newTable[T any, D any, PD docPtr[D]](s *tenantStore, name string, m meta[T, D]) *table[T, D, PD] {
	return &table[T, D, PD]{s: s, name: name, m: m}
}

func (t *table[T, D, PD]) coll() *mongo.Collection { return t.s.coll(t.name) }

func (t *table[T, D, PD]) Create(ctx context.Context, v *T) error {
	if v == nil {
		return fmt.Errorf("%w: nil %s", store.ErrInvalid, t.m.kind)
	}
	id := t.m.id(v)
	if *id == "" {
		*id = store.NewID()
	}
	*t.m.tenant(v) = t.s.tenant
	now := t.s.p.now()
	if t.m.created(v).IsZero() {
		*t.m.created(v) = now
	}
	if t.m.updated != nil {
		*t.m.updated(v) = now
	}
	if t.m.version != nil {
		*t.m.version(v) = 1
	}
	if _, err := t.coll().InsertOne(ctx, t.m.enc(v)); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return conflict(t.m.kind, *id)
		}
		return wrap("create "+t.m.kind, err)
	}
	return nil
}

func (t *table[T, D, PD]) Get(ctx context.Context, id string) (*T, error) {
	var d D
	err := t.coll().FindOne(ctx, t.s.scope(bson.E{Key: "_id", Value: id})).Decode(&d)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, notFound(t.m.kind, id)
		}
		return nil, wrap("get "+t.m.kind, err)
	}
	return t.m.dec(&d), nil
}

func (t *table[T, D, PD]) Update(ctx context.Context, v *T) error {
	if v == nil {
		return fmt.Errorf("%w: nil %s", store.ErrInvalid, t.m.kind)
	}
	id := *t.m.id(v)
	filter := t.s.scope(bson.E{Key: "_id", Value: id})

	// Build the next revision on a copy so a rejected update leaves the
	// caller's struct untouched.
	next := *v
	*t.m.tenant(&next) = t.s.tenant
	if t.m.updated != nil {
		*t.m.updated(&next) = t.s.p.now()
	}
	if t.m.version != nil {
		want := *t.m.version(v)
		filter = append(filter, bson.E{Key: "version", Value: want})
		*t.m.version(&next) = want + 1
	}
	// created_at is owned by the row, not by the caller's copy of it.
	set, err := setFields(t.m.enc(&next), "_id", "created_at")
	if err != nil {
		return wrap("update "+t.m.kind, err)
	}
	res, err := t.coll().UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return wrap("update "+t.m.kind, err)
	}
	if res.MatchedCount == 0 {
		return t.missOrConflict(ctx, id)
	}
	*v = next
	return nil
}

// missOrConflict tells a failed compare-and-set apart from a row that is not
// there at all (or belongs to another tenant).
func (t *table[T, D, PD]) missOrConflict(ctx context.Context, id string) error {
	n, err := t.coll().CountDocuments(ctx, t.s.scope(bson.E{Key: "_id", Value: id}),
		options.Count().SetLimit(1))
	if err != nil {
		return wrap("update "+t.m.kind, err)
	}
	if n == 0 {
		return notFound(t.m.kind, id)
	}
	return conflict(t.m.kind, id)
}

func (t *table[T, D, PD]) Delete(ctx context.Context, id string) error {
	res, err := t.coll().DeleteOne(ctx, t.s.scope(bson.E{Key: "_id", Value: id}))
	if err != nil {
		return wrap("delete "+t.m.kind, err)
	}
	if res.DeletedCount == 0 {
		return notFound(t.m.kind, id)
	}
	return nil
}

func (t *table[T, D, PD]) List(ctx context.Context, p store.Page) (store.Result[T], error) {
	return t.listWhere(ctx, nil, p)
}

func (t *table[T, D, PD]) listWhere(ctx context.Context, extra bson.D, p store.Page) (store.Result[T], error) {
	return listPage[T, D, PD](ctx, t.coll(), and(t.s.scope(), extra), p, t.m.dec)
}

// listPage runs one keyset page of filter, ordered by (created_at, _id).
func listPage[T any, D any, PD docPtr[D]](
	ctx context.Context, coll *mongo.Collection, filter bson.D,
	p store.Page, dec func(*D) *T,
) (store.Result[T], error) {
	var out store.Result[T]
	p = p.Normalize()
	if p.Cursor != "" {
		c, err := decodeCursor(p.Cursor)
		if err != nil {
			return out, err
		}
		filter = and(filter, afterCursor(c))
	}
	cur, err := coll.Find(ctx, filter,
		options.Find().SetSort(listSort).SetLimit(int64(p.Limit)+1))
	if err != nil {
		return out, wrap("list "+coll.Name(), err)
	}
	var docs []D
	if err := cur.All(ctx, &docs); err != nil {
		return out, wrap("list "+coll.Name(), err)
	}
	if len(docs) > p.Limit {
		b := PD(&docs[p.Limit-1]).baseOf()
		out.NextCursor = encodeCursor(cursor{createdAt: b.CreatedAt, id: b.ID})
		docs = docs[:p.Limit]
	}
	out.Items = make([]T, 0, len(docs))
	for i := range docs {
		out.Items = append(out.Items, *dec(&docs[i]))
	}
	return out, nil
}

// setFields renders a document as a $set body, dropping the fields the update
// must not touch.
func setFields(doc any, skip ...string) (bson.D, error) {
	raw, err := bson.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var d bson.D
	if err := bson.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := d[:0]
	for _, e := range d {
		if slices.Contains(skip, e.Key) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// findIDs collects up to limit _ids matching filter, in sortBy order. It is
// the first half of every "update a bounded chunk" operation: MongoDB has no
// UpdateMany limit, so the ids are picked first and the update stays
// conditional on the state they were picked in.
func findIDs(ctx context.Context, coll *mongo.Collection, filter bson.D, sortBy bson.D, limit int) ([]string, error) {
	opt := options.Find().
		SetProjection(bson.D{{Key: "_id", Value: 1}}).
		SetSort(sortBy)
	if limit > 0 {
		opt = opt.SetLimit(int64(limit))
	}
	cur, err := coll.Find(ctx, filter, opt)
	if err != nil {
		return nil, wrap("find "+coll.Name(), err)
	}
	var rows []struct {
		ID string `bson:"_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, wrap("find "+coll.Name(), err)
	}
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids, nil
}

// idSort orders chunked scans deterministically; IDs are UUIDv7, so it is also
// insertion order.
var idSort = bson.D{{Key: "_id", Value: 1}}

func inIDs(ids []string) bson.E {
	return bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}
}
