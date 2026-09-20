package memstore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/sendplane/sendplane/store"
)

// meta gives the generic table access to the bookkeeping fields of a model.
// version and updated are nil for immutable aggregates; times is nil for the
// models whose only timestamps are created/updated.
type meta[T any] struct {
	id      func(*T) *string
	tenant  func(*T) *string
	version func(*T) *int64
	created func(*T) *time.Time
	updated func(*T) *time.Time
	// times lists the model's remaining timestamps, the ones a SQL backend
	// would give a column of their own, so they can be truncated on write.
	times func(*T) []*time.Time
}

// truncate reduces every time it is given to the resolution the contract
// stores (store.TruncateTime). A real backend gets this from its column
// types; memstore keeps Go values, so it has to do it explicitly or the
// reference implementation would be more precise than the contract.
func truncate(ps ...*time.Time) {
	for _, p := range ps {
		*p = store.TruncateTime(*p)
	}
}

// truncateRow applies truncate to every timestamp of a row about to be stored.
func (t *table[T]) truncateRow(v *T) {
	if t.m.created != nil {
		truncate(t.m.created(v))
	}
	if t.m.updated != nil {
		truncate(t.m.updated(v))
	}
	if t.m.times != nil {
		truncate(t.m.times(v)...)
	}
}

// table is the shared CRUD implementation. Its method set satisfies the
// uniform repository interfaces (Create/Get/Update/Delete/List) directly.
//
// Stored values are shallow copies: a caller that mutates a slice or map it
// previously handed to Create would also mutate the stored row. Real backends
// serialize, so callers must not rely on that.
type table[T any] struct {
	p      *Provider
	tenant string
	rows   map[string]*T
	m      meta[T]
}

func (t *table[T]) Create(_ context.Context, v *T) error {
	if v == nil {
		return fmt.Errorf("%w: nil value", store.ErrInvalid)
	}
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if err := t.p.check(); err != nil {
		return err
	}
	id := t.m.id(v)
	if *id == "" {
		*id = store.NewID()
	}
	if _, ok := t.rows[*id]; ok {
		return fmt.Errorf("%w: %s already exists", store.ErrConflict, *id)
	}
	*t.m.tenant(v) = t.tenant
	now := t.p.now()
	if t.m.created != nil && t.m.created(v).IsZero() {
		*t.m.created(v) = now
	}
	if t.m.updated != nil {
		*t.m.updated(v) = now
	}
	if t.m.version != nil {
		*t.m.version(v) = 1
	}
	cp := *v
	t.truncateRow(&cp)
	t.rows[*id] = &cp
	return nil
}

func (t *table[T]) Get(_ context.Context, id string) (*T, error) {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if err := t.p.check(); err != nil {
		return nil, err
	}
	cur, ok := t.rows[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", store.ErrNotFound, id)
	}
	cp := *cur
	return &cp, nil
}

func (t *table[T]) Update(_ context.Context, v *T) error {
	if v == nil {
		return fmt.Errorf("%w: nil value", store.ErrInvalid)
	}
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if err := t.p.check(); err != nil {
		return err
	}
	id := *t.m.id(v)
	cur, ok := t.rows[id]
	if !ok {
		return fmt.Errorf("%w: %s", store.ErrNotFound, id)
	}
	if t.m.version != nil {
		if *t.m.version(cur) != *t.m.version(v) {
			return fmt.Errorf("%w: %s version %d, have %d",
				store.ErrConflict, id, *t.m.version(cur), *t.m.version(v))
		}
		*t.m.version(v) = *t.m.version(cur) + 1
	}
	*t.m.tenant(v) = t.tenant
	if t.m.created != nil {
		*t.m.created(v) = *t.m.created(cur)
	}
	if t.m.updated != nil {
		*t.m.updated(v) = t.p.now()
	}
	cp := *v
	t.truncateRow(&cp)
	t.rows[id] = &cp
	return nil
}

func (t *table[T]) Delete(_ context.Context, id string) error {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if err := t.p.check(); err != nil {
		return err
	}
	if _, ok := t.rows[id]; !ok {
		return fmt.Errorf("%w: %s", store.ErrNotFound, id)
	}
	delete(t.rows, id)
	return nil
}

func (t *table[T]) List(_ context.Context, p store.Page) (store.Result[T], error) {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if err := t.p.check(); err != nil {
		return store.Result[T]{}, err
	}
	return paginate(t.rows, p, nil), nil
}

func (t *table[T]) listWhere(p store.Page, keep func(*T) bool) (store.Result[T], error) {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if err := t.p.check(); err != nil {
		return store.Result[T]{}, err
	}
	return paginate(t.rows, p, keep), nil
}

// paginate walks rows in ID order. IDs are UUIDv7, so that is creation order,
// and the cursor is simply the last ID returned: rows inserted later sort
// after it and are never repeated or skipped.
func paginate[T any](rows map[string]*T, p store.Page, keep func(*T) bool) store.Result[T] {
	p = p.Normalize()
	ids := make([]string, 0, len(rows))
	for id := range rows {
		if id <= p.Cursor {
			continue
		}
		if keep != nil && !keep(rows[id]) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	res := store.Result[T]{}
	if len(ids) > p.Limit {
		res.NextCursor = ids[p.Limit-1]
		ids = ids[:p.Limit]
	}
	res.Items = make([]T, 0, len(ids))
	for _, id := range ids {
		res.Items = append(res.Items, *rows[id])
	}
	return res
}
