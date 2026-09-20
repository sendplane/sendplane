package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sendplane/sendplane/store"
)

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// spec describes one table well enough for the shared CRUD implementation:
// the data columns, how to turn a model into their values, and how to scan a
// row back. The column order is fixed as
//
//	id, tenant_id, <cols...>, created_at [, updated_at, version]
//
// and scan must read them in exactly that order.
//
// version and updated are nil for the immutable aggregates (message versions,
// probe runs, bounce events), which have neither optimistic concurrency nor an
// updated_at column.
type spec[T any] struct {
	table    string
	cols     []string
	args     func(*T) ([]any, error)
	scan     func(rowScanner) (*T, error)
	id       func(*T) *string
	tenantID func(*T) *string
	version  func(*T) *int64
	created  func(*T) *time.Time
	updated  func(*T) *time.Time
}

func (s spec[T]) versioned() bool { return s.version != nil }

// selectList is the projection every read uses, in the order scan expects.
func (s spec[T]) selectList() string {
	var b strings.Builder
	b.WriteString("id, tenant_id, ")
	for _, c := range s.cols {
		b.WriteString(c)
		b.WriteString(", ")
	}
	b.WriteString("created_at")
	if s.versioned() {
		b.WriteString(", updated_at, version")
	}
	return b.String()
}

// crud is the shared repository implementation. Its method set satisfies the
// uniform Create/Get/Update/Delete/List interfaces directly.
type crud[T any] struct {
	p      *Provider
	tenant string
	s      spec[T]
}

func newCrud[T any](p *Provider, tenant string, s spec[T]) *crud[T] {
	return &crud[T]{p: p, tenant: tenant, s: s}
}

func (c *crud[T]) Create(ctx context.Context, v *T) error {
	if v == nil {
		return fmt.Errorf("%w: nil value", store.ErrInvalid)
	}
	if err := c.p.check(); err != nil {
		return err
	}
	id := c.s.id(v)
	if *id == "" {
		*id = store.NewID()
	}
	*c.s.tenantID(v) = c.tenant
	now := c.p.now()
	if c.s.created(v).IsZero() {
		*c.s.created(v) = now
	}
	if c.s.updated != nil {
		*c.s.updated(v) = now
	}
	if c.s.versioned() {
		*c.s.version(v) = 1
	}

	vals, err := c.s.args(v)
	if err != nil {
		return err
	}
	cols := append([]string{"id", "tenant_id"}, c.s.cols...)
	cols = append(cols, "created_at")
	params := append([]any{*id, c.tenant}, vals...)
	params = append(params, *c.s.created(v))
	if c.s.versioned() {
		cols = append(cols, "updated_at", "version")
		params = append(params, *c.s.updated(v), *c.s.version(v))
	}

	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		c.s.table, strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	if _, err := c.p.pool.Exec(ctx, q, params...); err != nil {
		return mapErr(err)
	}
	return nil
}

func (c *crud[T]) Get(ctx context.Context, id string) (*T, error) {
	if err := c.p.check(); err != nil {
		return nil, err
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE id = $1 AND tenant_id = $2",
		c.s.selectList(), c.s.table)
	v, err := c.s.scan(c.p.pool.QueryRow(ctx, q, id, c.tenant))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", c.s.table, id, mapErr(err))
	}
	return v, nil
}

func (c *crud[T]) Update(ctx context.Context, v *T) error {
	if v == nil {
		return fmt.Errorf("%w: nil value", store.ErrInvalid)
	}
	if err := c.p.check(); err != nil {
		return err
	}
	if !c.s.versioned() {
		return fmt.Errorf("%w: %s is immutable", store.ErrInvalid, c.s.table)
	}
	id := *c.s.id(v)
	*c.s.tenantID(v) = c.tenant

	vals, err := c.s.args(v)
	if err != nil {
		return err
	}
	a := &args{}
	sets := make([]string, 0, len(c.s.cols)+2)
	for i, col := range c.s.cols {
		sets = append(sets, col+" = "+a.add(vals[i]))
	}
	now := c.p.now()
	sets = append(sets, "updated_at = "+a.add(now), "version = version + 1")
	q := fmt.Sprintf(
		"UPDATE %s SET %s WHERE id = %s AND tenant_id = %s AND version = %s"+
			" RETURNING created_at, updated_at, version",
		c.s.table, strings.Join(sets, ", "),
		a.add(id), a.add(c.tenant), a.add(*c.s.version(v)))

	var created, updated time.Time
	var version int64
	err = c.p.pool.QueryRow(ctx, q, a.v...).Scan(&created, &updated, &version)
	if err != nil {
		return c.conflictOrNotFound(ctx, id, mapErr(err))
	}
	// Write the new bookkeeping back into the caller's struct (ADR-0007).
	*c.s.created(v) = created.UTC()
	*c.s.updated(v) = updated.UTC()
	*c.s.version(v) = version
	return nil
}

// conflictOrNotFound distinguishes "somebody else bumped the version" from
// "this tenant has no such row", which is what a cross-tenant write hits.
func (c *crud[T]) conflictOrNotFound(ctx context.Context, id string, err error) error {
	if err == nil {
		return nil
	}
	var exists bool
	q := fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE id = $1 AND tenant_id = $2)", c.s.table)
	if qerr := c.p.pool.QueryRow(ctx, q, id, c.tenant).Scan(&exists); qerr != nil {
		return mapErr(qerr)
	}
	if !exists {
		return fmt.Errorf("%w: %s %s", store.ErrNotFound, c.s.table, id)
	}
	return fmt.Errorf("%w: %s %s version mismatch", store.ErrConflict, c.s.table, id)
}

func (c *crud[T]) Delete(ctx context.Context, id string) error {
	if err := c.p.check(); err != nil {
		return err
	}
	q := fmt.Sprintf("DELETE FROM %s WHERE id = $1 AND tenant_id = $2", c.s.table)
	tag, err := c.p.pool.Exec(ctx, q, id, c.tenant)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s %s", store.ErrNotFound, c.s.table, id)
	}
	return nil
}

func (c *crud[T]) List(ctx context.Context, p store.Page) (store.Result[T], error) {
	return c.listWhere(ctx, p, nil)
}

// listWhere is the keyset-paginated read. where appends extra predicates and
// registers their parameters; the tenant predicate is always present.
func (c *crud[T]) listWhere(ctx context.Context, p store.Page, where func(a *args) string) (store.Result[T], error) {
	var zero store.Result[T]
	if err := c.p.check(); err != nil {
		return zero, err
	}
	p = p.Normalize()
	a := &args{}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE tenant_id = %s",
		c.s.selectList(), c.s.table, a.add(c.tenant))
	if where != nil {
		q += where(a)
	}
	keyset, err := keysetWhere(a, p, "id")
	if err != nil {
		return zero, err
	}
	q += keyset
	// One row more than asked for: its presence is what produces NextCursor.
	q += " ORDER BY created_at, id LIMIT " + a.add(p.Limit+1)

	rows, err := c.p.pool.Query(ctx, q, a.v...)
	if err != nil {
		return zero, mapErr(err)
	}
	defer rows.Close()
	items := make([]T, 0, p.Limit)
	for rows.Next() {
		v, err := c.s.scan(rows)
		if err != nil {
			return zero, mapErr(err)
		}
		items = append(items, *v)
	}
	if err := rows.Err(); err != nil {
		return zero, mapErr(err)
	}
	res := store.Result[T]{Items: items}
	if len(items) > p.Limit {
		last := &items[p.Limit-1]
		res.NextCursor = encodeCursor(*c.s.created(last), *c.s.id(last))
		res.Items = items[:p.Limit]
	}
	return res, nil
}
