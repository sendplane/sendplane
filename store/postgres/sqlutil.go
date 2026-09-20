package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sendplane/sendplane/store"
)

// --- parameter building ------------------------------------------------
//
// SQL is written by hand (no query builder, ADR-0007): args collects the
// placeholders so a predicate can be appended without renumbering by hand.

type args struct{ v []any }

// add records one value and returns its placeholder ("$3").
func (a *args) add(x any) string {
	a.v = append(a.v, x)
	return "$" + strconv.Itoa(len(a.v))
}

// --- nullability -------------------------------------------------------
//
// A zero time.Time is SQL NULL (store/doc.go), and Go has no nullable string,
// so the optional string columns are NOT NULL DEFAULT ''.

// tsIn maps a Go time to a nullable timestamptz parameter, truncated to the
// contract's millisecond resolution (store.TruncateTime). Every instant that
// reaches the database goes through it, including the ones used only as a
// comparison bound, so a stored value and a bound derived from the same
// time.Time compare exactly.
func tsIn(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := store.TruncateTime(t)
	return &u
}

// tsInNN is tsIn for a NOT NULL column.
func tsInNN(t time.Time) time.Time { return store.TruncateTime(t) }

// tsOut maps a scanned nullable timestamptz back to a Go time.
func tsOut(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.UTC()
}

// idIn maps an empty ID to NULL; it is only used for delivery.campaign_id,
// where "no campaign" must stay out of the partial unique index.
func idIn(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func idOut(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// jsonIn marshals a value into a jsonb parameter. A nil/empty value is NULL so
// that "absent" and "the JSON null literal" do not have to be told apart.
func jsonIn(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		if len(x) == 0 {
			return nil, nil
		}
		return x, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: encode json: %v", store.ErrInvalid, err)
	}
	if string(b) == "null" {
		return nil, nil
	}
	return b, nil
}

// jsonOut unmarshals a scanned jsonb column into out, tolerating NULL.
func jsonOut(b []byte, out any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func rawOut(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// --- enums -------------------------------------------------------------

func i16[T ~int8](v T) int16 { return int16(v) }

func i16s[T ~int8](vs []T) []int16 {
	out := make([]int16, len(vs))
	for i, v := range vs {
		out[i] = int16(v)
	}
	return out
}

// enumOut narrows a scanned smallint back to the int8 an enum is stored in. A
// value outside the int8 range cannot come from this package, so it is mapped
// to the zero enum rather than wrapping around.
func enumOut[T ~int8](v int16) T {
	if v < math.MinInt8 || v > math.MaxInt8 {
		return T(0)
	}
	return T(v)
}

// smtpCode narrows a response code to the width of the column. SMTP codes are
// three digits; anything that does not fit is recorded as "no code".
func smtpCode(v int) int32 {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0
	}
	return int32(v)
}

// --- limits ------------------------------------------------------------

// allRows is the LIMIT used when a caller passes limit <= 0 ("every match").
const allRows = math.MaxInt32

func limitOrAll(limit int) int {
	if limit <= 0 {
		return allRows
	}
	return limit
}

// --- retention ---------------------------------------------------------

// deleteBefore is the shared retention delete: the oldest rows of one table
// first, at most limit of them, optionally narrowed by extra (an already-safe
// SQL fragment, never caller input). The CTE picks the ids under the list
// index and the DELETE matches on the primary key, so a chunk never degrades
// into a table scan and never holds a lock on rows it is not deleting.
func deleteBefore(ctx context.Context, p *Provider, table, tenant string,
	before time.Time, limit int, extra string) (int, error) {
	if err := p.check(); err != nil {
		return 0, err
	}
	a := &args{}
	q := "WITH c AS (SELECT id FROM " + table + " WHERE tenant_id = " + a.add(tenant) +
		" AND created_at < " + a.add(tsInNN(before)) + extra +
		" ORDER BY created_at, id LIMIT " + a.add(limitOrAll(limit)) + ") " +
		"DELETE FROM " + table + " WHERE id IN (SELECT id FROM c)"
	tag, err := p.pool.Exec(ctx, q, a.v...)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// --- cursors -----------------------------------------------------------
//
// Keyset pagination on (created_at, id): rows inserted after a page was read
// sort after the cursor, so they can neither repeat nor skip an existing row
// (store/doc.go). The encoding is opaque on purpose.

type cursor struct {
	at  time.Time
	key string
}

func encodeCursor(at time.Time, key string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(at.UTC().Format(time.RFC3339Nano) + "\x00" + key))
}

func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: bad cursor", store.ErrInvalid)
	}
	at, key, ok := strings.Cut(string(raw), "\x00")
	if !ok {
		return cursor{}, fmt.Errorf("%w: bad cursor", store.ErrInvalid)
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: bad cursor", store.ErrInvalid)
	}
	return cursor{at: t, key: key}, nil
}

// keysetWhere appends the "after the cursor" predicate for a (created_at, key)
// keyset, or nothing for the first page.
func keysetWhere(a *args, p store.Page, keyCol string) (string, error) {
	c, err := decodeCursor(p.Cursor)
	if err != nil {
		return "", err
	}
	if p.Cursor == "" {
		return "", nil
	}
	return fmt.Sprintf(" AND (created_at, %s) > (%s, %s)",
		keyCol, a.add(c.at), a.add(c.key)), nil
}

// --- errors ------------------------------------------------------------

const pgUniqueViolation = "23505"

// mapErr turns driver errors into the sentinel errors of the contract.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no rows", store.ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%w: %s", store.ErrConflict, pgErr.ConstraintName)
	}
	return err
}

// --- small helpers -----------------------------------------------------

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

func joinComma(parts []string) string { return strings.Join(parts, ", ") }

// prefixedList qualifies a comma-separated column list with a table alias,
// which UPDATE ... FROM needs when the CTE also exposes an id.
func prefixedList(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// sortByID restores a deterministic order for statements whose RETURNING
// clause has none.
func sortByID[T any](rows []T, id func(*T) string) {
	sort.Slice(rows, func(i, j int) bool { return id(&rows[i]) < id(&rows[j]) })
}
