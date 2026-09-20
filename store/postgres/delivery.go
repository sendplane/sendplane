package postgres

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sendplane/sendplane/store"
)

// deliveryColumns is the column order every delivery read, INSERT and COPY
// uses.
var deliveryColumns = []string{
	"id", "tenant_id", "campaign_id", "version_id", "sender_id", "lane",
	"priority", "status", "email", "email_norm", "name", "locale", "vars",
	"unsubscribe_url", "attempt_count", "retry_gen",
	"next_attempt_at", "lease_owner", "lease_until",
	"last_error_class", "last_smtp_code", "last_error", "message_id",
	"sent_at", "finished_at",
	"first_opened_at", "first_clicked_at", "unsubscribed_at",
	"created_at", "updated_at",
}

var (
	deliveryCols  = strings.Join(deliveryColumns, ", ")
	deliveryColsD = prefixed(deliveryColumns, "d")
)

// prefixed qualifies a column list with a table alias, which UPDATE ... FROM
// needs because the CTE also exposes an id.
func prefixed(cols []string, alias string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = alias + "." + c
	}
	return strings.Join(out, ", ")
}

func deliveryValues(d *store.Delivery) ([]any, error) {
	vars, err := jsonIn(d.Vars)
	if err != nil {
		return nil, err
	}
	return []any{
		d.ID, d.TenantID, idIn(d.CampaignID), d.VersionID, d.SenderID,
		i16(d.Lane), d.Priority, i16(d.Status), d.Email, d.EmailNorm, d.Name,
		d.Locale, vars, d.UnsubscribeURL, d.AttemptCount, d.RetryGen,
		tsInNN(d.NextAttemptAt), d.LeaseOwner, tsIn(d.LeaseUntil),
		i16(d.LastErrorClass), d.LastSMTPCode, d.LastError, d.MessageID,
		tsIn(d.SentAt), tsIn(d.FinishedAt),
		tsIn(d.FirstOpenedAt), tsIn(d.FirstClickedAt), tsIn(d.UnsubscribedAt),
		tsInNN(d.CreatedAt), tsInNN(d.UpdatedAt),
	}, nil
}

func scanDelivery(r rowScanner) (*store.Delivery, error) {
	var d store.Delivery
	var campaign *string
	var vars []byte
	var lane, status, errClass int16
	var nextAt time.Time
	var leaseUntil, sentAt, finishedAt, opened, clicked, unsubscribed *time.Time
	if err := r.Scan(
		&d.ID, &d.TenantID, &campaign, &d.VersionID, &d.SenderID, &lane,
		&d.Priority, &status, &d.Email, &d.EmailNorm, &d.Name, &d.Locale,
		&vars, &d.UnsubscribeURL, &d.AttemptCount, &d.RetryGen,
		&nextAt, &d.LeaseOwner, &leaseUntil,
		&errClass, &d.LastSMTPCode, &d.LastError, &d.MessageID,
		&sentAt, &finishedAt, &opened, &clicked, &unsubscribed,
		&d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return nil, err
	}
	d.CampaignID = idOut(campaign)
	d.Lane = enumOut[store.Lane](lane)
	d.Status = enumOut[store.DeliveryStatus](status)
	d.LastErrorClass = enumOut[store.ErrorClass](errClass)
	d.NextAttemptAt = nextAt.UTC()
	d.LeaseUntil = tsOut(leaseUntil)
	d.SentAt = tsOut(sentAt)
	d.FinishedAt = tsOut(finishedAt)
	d.FirstOpenedAt = tsOut(opened)
	d.FirstClickedAt = tsOut(clicked)
	d.UnsubscribedAt = tsOut(unsubscribed)
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return &d, jsonOut(vars, &d.Vars)
}

type deliveryRepo struct {
	p      *Provider
	tenant string
}

// campaignPredicate matches one campaign, or the deliveries without a campaign
// for an empty campaignID. "No campaign" is NULL in SQL and "" in Go, which is
// what keeps the partial unique index limited to campaign deliveries.
func campaignPredicate(a *args, campaignID string) string {
	return " AND campaign_id IS NOT DISTINCT FROM " + a.add(idIn(campaignID)) + "::text"
}

// --- ingest ------------------------------------------------------------

// InsertBatch inserts deliveries, skipping campaign deliveries whose
// (campaign_id, email_norm) already exists. Small batches go in as a multi-row
// INSERT; larger ones are streamed with COPY into a temporary table and moved
// across with INSERT ... SELECT ... ON CONFLICT DO NOTHING (architecture 5.2).
func (r *deliveryRepo) InsertBatch(ctx context.Context, ds []store.Delivery) (int, error) {
	if err := r.p.check(); err != nil {
		return 0, err
	}
	// The whole batch is validated before anything is written, so a rejected
	// chunk can be fixed and re-sent as a whole (store/delivery.go).
	for i := range ds {
		if ds[i].EmailNorm == "" {
			return 0, fmt.Errorf("%w: delivery %d has no email_norm", store.ErrInvalid, i)
		}
	}
	now := r.p.now()
	rows := make([][]any, 0, len(ds))
	// Collapse duplicates inside the batch: ON CONFLICT cannot see rows the
	// same statement is still inserting.
	seen := make(map[string]bool, len(ds))
	for i := range ds {
		d := ds[i]
		if d.CampaignID != "" {
			key := d.CampaignID + "\x00" + d.EmailNorm
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		if d.ID == "" {
			d.ID = store.NewID()
		}
		d.TenantID = r.tenant
		if d.CreatedAt.IsZero() {
			d.CreatedAt = now
		}
		d.UpdatedAt = now
		if d.NextAttemptAt.IsZero() {
			d.NextAttemptAt = now
		}
		vals, err := deliveryValues(&d)
		if err != nil {
			return 0, err
		}
		rows = append(rows, vals)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if len(rows) > r.p.copyThreshold {
		return r.insertCopy(ctx, rows)
	}
	return r.insertValues(ctx, rows)
}

func (r *deliveryRepo) insertValues(ctx context.Context, rows [][]any) (int, error) {
	params := make([]any, 0, len(rows)*len(deliveryColumns))
	tuples := make([]string, 0, len(rows))
	var b strings.Builder
	for _, row := range rows {
		b.Reset()
		b.WriteByte('(')
		for i, v := range row {
			if i > 0 {
				b.WriteString(", ")
			}
			params = append(params, v)
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(len(params)))
		}
		b.WriteByte(')')
		tuples = append(tuples, b.String())
	}
	q := "INSERT INTO delivery (" + deliveryCols + ") VALUES " +
		strings.Join(tuples, ", ") + " ON CONFLICT DO NOTHING"
	tag, err := r.p.pool.Exec(ctx, q, params...)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// insertCopy is the 1M-recipient path: COPY into a temporary table, then one
// INSERT ... SELECT that lets the unique index drop the duplicates.
func (r *deliveryRepo) insertCopy(ctx context.Context, rows [][]any) (int, error) {
	tx, err := r.p.pool.Begin(ctx)
	if err != nil {
		return 0, mapErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const tmp = "_sp_delivery_in"
	if _, err := tx.Exec(ctx,
		"CREATE TEMP TABLE "+tmp+" (LIKE delivery) ON COMMIT DROP"); err != nil {
		return 0, mapErr(err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{tmp}, deliveryColumns,
		pgx.CopyFromRows(rows)); err != nil {
		return 0, mapErr(err)
	}
	tag, err := tx.Exec(ctx, "INSERT INTO delivery ("+deliveryCols+") SELECT "+
		deliveryCols+" FROM "+tmp+" ON CONFLICT DO NOTHING")
	if err != nil {
		return 0, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// --- reads -------------------------------------------------------------

func (r *deliveryRepo) Get(ctx context.Context, id string) (*store.Delivery, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	q := "SELECT " + deliveryCols + " FROM delivery WHERE id = $1 AND tenant_id = $2"
	d, err := scanDelivery(r.p.pool.QueryRow(ctx, q, id, r.tenant))
	if err != nil {
		return nil, fmt.Errorf("delivery %s: %w", id, mapErr(err))
	}
	return d, nil
}

func (r *deliveryRepo) ListByCampaign(ctx context.Context, campaignID string, f store.DeliveryFilter, p store.Page) (store.Result[store.Delivery], error) {
	var zero store.Result[store.Delivery]
	if err := r.p.check(); err != nil {
		return zero, err
	}
	p = p.Normalize()
	a := &args{}
	q := "SELECT " + deliveryCols + " FROM delivery WHERE tenant_id = " + a.add(r.tenant)
	q += campaignPredicate(a, campaignID)
	if len(f.Statuses) > 0 {
		q += " AND status = ANY(" + a.add(i16s(f.Statuses)) + "::smallint[])"
	}
	if len(f.ErrorClasses) > 0 {
		q += " AND last_error_class = ANY(" + a.add(i16s(f.ErrorClasses)) + "::smallint[])"
	}
	if f.EmailNorm != "" {
		q += " AND email_norm = " + a.add(f.EmailNorm)
	}
	keyset, err := keysetWhere(a, p, "id")
	if err != nil {
		return zero, err
	}
	q += keyset + " ORDER BY created_at, id LIMIT " + a.add(p.Limit+1)

	rows, err := r.p.pool.Query(ctx, q, a.v...)
	if err != nil {
		return zero, mapErr(err)
	}
	defer rows.Close()
	items := make([]store.Delivery, 0, p.Limit)
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return zero, mapErr(err)
		}
		items = append(items, *d)
	}
	if err := rows.Err(); err != nil {
		return zero, mapErr(err)
	}
	res := store.Result[store.Delivery]{Items: items}
	if len(items) > p.Limit {
		res.NextCursor = encodeCursor(items[p.Limit-1].CreatedAt, items[p.Limit-1].ID)
		res.Items = items[:p.Limit]
	}
	return res, nil
}

func (r *deliveryRepo) CountByStatus(ctx context.Context, campaignID string) (map[store.DeliveryStatus]int64, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	a := &args{}
	q := "SELECT status, count(*) FROM delivery WHERE tenant_id = " + a.add(r.tenant)
	q += campaignPredicate(a, campaignID) + " GROUP BY status"
	rows, err := r.p.pool.Query(ctx, q, a.v...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[store.DeliveryStatus]int64{}
	for rows.Next() {
		var status int16
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, mapErr(err)
		}
		out[enumOut[store.DeliveryStatus](status)] = n
	}
	return out, mapErr(rows.Err())
}

// --- claim -------------------------------------------------------------

// Claim is the queue read (architecture 5.2): the candidate set is locked with
// FOR UPDATE SKIP LOCKED so that concurrent senders never take the same row,
// and the same statement flips it to leased.
func (r *deliveryRepo) Claim(ctx context.Context, req store.ClaimRequest) ([]store.Delivery, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	leaseUntil := req.Now.Add(req.LeaseFor)
	a := &args{}
	where := "tenant_id = " + a.add(r.tenant) +
		" AND lane = " + a.add(i16(req.Lane)) +
		" AND status IN (1, 3)" + // queued, deferred
		" AND next_attempt_at <= " + a.add(tsInNN(req.Now))
	switch {
	case req.CampaignIDs == nil:
		// no campaign filter at all
	case len(req.CampaignIDs) == 0:
		where += " AND campaign_id IS NULL"
	default:
		where += " AND (campaign_id IS NULL OR campaign_id = ANY(" +
			a.add(req.CampaignIDs) + "::text[]))"
	}
	q := "WITH c AS (SELECT id FROM delivery WHERE " + where +
		" ORDER BY priority DESC, next_attempt_at, id LIMIT " +
		a.add(limitOrAll(req.Limit)) + " FOR UPDATE SKIP LOCKED) " +
		"UPDATE delivery d SET status = 2, lease_owner = " + a.add(req.WorkerID) +
		", lease_until = " + a.add(tsIn(leaseUntil)) +
		", updated_at = " + a.add(tsInNN(req.Now)) +
		" FROM c WHERE d.id = c.id RETURNING " + deliveryColsD

	rows, err := r.p.pool.Query(ctx, q, a.v...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.Delivery, 0, 64)
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	// UPDATE ... RETURNING has no defined row order, so the contract's order
	// (priority desc, then next_attempt_at) is restored here.
	sort.Slice(out, func(i, j int) bool {
		a, b := &out[i], &out[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if !a.NextAttemptAt.Equal(b.NextAttemptAt) {
			return a.NextAttemptAt.Before(b.NextAttemptAt)
		}
		return a.ID < b.ID
	})
	return out, nil
}

// --- transitions -------------------------------------------------------

// Complete applies a batch of finished attempts. The CAS is lease_owner: one
// UPDATE ... FROM unnest(...) transitions every row still held by its worker
// and returns the ids that matched, and only those get an attempt row. A
// replayed batch therefore changes nothing.
func (r *deliveryRepo) Complete(ctx context.Context, results []store.DeliveryResult) error {
	if err := r.p.check(); err != nil {
		return err
	}
	if len(results) == 0 {
		return nil
	}
	now := r.p.now()
	n := len(results)
	ids := make([]string, n)
	owners := make([]string, n)
	statuses := make([]int16, n)
	nextAt := make([]*time.Time, n)
	classes := make([]int16, n)
	codes := make([]int32, n)
	errs := make([]string, n)
	msgIDs := make([]string, n)
	incs := make([]bool, n)
	terms := make([]bool, n)
	for i, res := range results {
		ids[i] = res.DeliveryID
		owners[i] = res.LeaseOwner
		statuses[i] = i16(res.NewStatus)
		nextAt[i] = tsIn(res.NextAttemptAt)
		classes[i] = i16(res.ErrorClass)
		codes[i] = smtpCode(res.SMTPCode)
		errs[i] = res.Error
		msgIDs[i] = res.MessageID
		incs[i] = res.IncrementAttempt
		terms[i] = res.NewStatus.Terminal()
	}

	tx, err := r.p.pool.Begin(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
UPDATE delivery d SET
    status           = r.new_status,
    last_error_class = r.error_class,
    last_smtp_code   = r.smtp_code,
    last_error       = r.error_text,
    message_id       = CASE WHEN r.message_id <> '' THEN r.message_id ELSE d.message_id END,
    attempt_count    = d.attempt_count + CASE WHEN r.increment THEN 1 ELSE 0 END,
    next_attempt_at  = COALESCE(r.next_attempt_at, d.next_attempt_at),
    sent_at          = CASE WHEN r.new_status = 4 AND d.sent_at IS NULL THEN $11 ELSE d.sent_at END,
    finished_at      = CASE WHEN r.terminal THEN $11 ELSE d.finished_at END,
    lease_owner      = '',
    lease_until      = NULL,
    updated_at       = $11
FROM unnest($1::text[], $2::text[], $3::smallint[], $4::timestamptz[],
            $5::smallint[], $6::int[], $7::text[], $8::text[], $9::bool[], $10::bool[])
     AS r(delivery_id, lease_owner, new_status, next_attempt_at,
          error_class, smtp_code, error_text, message_id, increment, terminal)
WHERE d.id = r.delivery_id AND d.tenant_id = $12
  AND r.lease_owner <> '' AND d.lease_owner = r.lease_owner
RETURNING d.id`

	rows, err := tx.Query(ctx, q, ids, owners, statuses, nextAt,
		classes, codes, errs, msgIDs, incs, terms,
		tsInNN(now), r.tenant)
	if err != nil {
		return mapErr(err)
	}
	applied := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return mapErr(err)
		}
		applied[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mapErr(err)
	}

	// Only the results that really held the lease record an attempt.
	attempts := make([]store.DeliveryAttempt, 0, len(applied))
	for _, res := range results {
		if res.Attempt == nil || !applied[res.DeliveryID] {
			continue
		}
		a := *res.Attempt
		a.DeliveryID = res.DeliveryID
		attempts = append(attempts, a)
	}
	if err := insertAttempts(ctx, tx, r.tenant, now, attempts); err != nil {
		return err
	}
	return mapErr(tx.Commit(ctx))
}

// MarkSent is the fast path right after SMTP 250 (architecture 8.1). It keeps
// the lease so the batched Complete can still attach the attempt.
func (r *deliveryRepo) MarkSent(ctx context.Context, id, owner, messageID string, at time.Time) error {
	if err := r.p.check(); err != nil {
		return err
	}
	const q = `UPDATE delivery SET status = 4, message_id = $3,
	    sent_at = $4, finished_at = $4, updated_at = $4
	  WHERE id = $1 AND tenant_id = $2 AND status = 2 AND lease_owner <> '' AND lease_owner = $5`
	tag, err := r.p.pool.Exec(ctx, q, id, r.tenant, messageID, tsInNN(at), owner)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	err = r.p.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM delivery WHERE id = $1 AND tenant_id = $2)`,
		id, r.tenant).Scan(&exists)
	if err != nil {
		return mapErr(err)
	}
	if !exists {
		return fmt.Errorf("%w: delivery %s", store.ErrNotFound, id)
	}
	return fmt.Errorf("%w: delivery %s", store.ErrLeaseLost, id)
}

// ReleaseExpiredLeases returns leased deliveries whose lease has expired to
// deferred, or to queued when nothing was attempted. A row that MarkSent
// already moved to sent is not leased any more and is never touched.
func (r *deliveryRepo) ReleaseExpiredLeases(ctx context.Context, now time.Time, limit int) (int, error) {
	if err := r.p.check(); err != nil {
		return 0, err
	}
	a := &args{}
	q := "WITH c AS (SELECT id FROM delivery WHERE tenant_id = " + a.add(r.tenant) +
		" AND status = 2 AND lease_until IS NOT NULL AND lease_until < " + a.add(tsInNN(now)) +
		" ORDER BY id LIMIT " + a.add(limitOrAll(limit)) + " FOR UPDATE SKIP LOCKED) " +
		"UPDATE delivery d SET status = CASE WHEN d.attempt_count > 0 THEN 3 ELSE 1 END," +
		" lease_owner = '', lease_until = NULL," +
		" next_attempt_at = " + a.add(tsInNN(now)) +
		", updated_at = " + a.add(tsInNN(now)) +
		" FROM c WHERE d.id = c.id"
	tag, err := r.p.pool.Exec(ctx, q, a.v...)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// BulkTransition moves up to limit deliveries between statuses, so cancel can
// walk a million rows in chunks.
func (r *deliveryRepo) BulkTransition(ctx context.Context, campaignID string, from []store.DeliveryStatus, to store.DeliveryStatus, limit int) (int, error) {
	if err := r.p.check(); err != nil {
		return 0, err
	}
	now := r.p.now()
	a := &args{}
	where := "tenant_id = " + a.add(r.tenant) + campaignPredicate(a, campaignID) +
		" AND status <> " + a.add(i16(to))
	if len(from) > 0 {
		where += " AND status = ANY(" + a.add(i16s(from)) + "::smallint[])"
	}
	q := "WITH c AS (SELECT id FROM delivery WHERE " + where +
		" ORDER BY id LIMIT " + a.add(limitOrAll(limit)) + " FOR UPDATE SKIP LOCKED) " +
		"UPDATE delivery d SET status = " + a.add(i16(to))
	if to.Terminal() {
		q += ", finished_at = " + a.add(tsInNN(now))
	}
	q += ", updated_at = " + a.add(tsInNN(now)) + " FROM c WHERE d.id = c.id"
	tag, err := r.p.pool.Exec(ctx, q, a.v...)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// Requeue is the manual retry: matching deliveries go back to queued with
// RetryGen bumped and AttemptCount kept.
func (r *deliveryRepo) Requeue(ctx context.Context, f store.RetryFilter, limit int) (int, error) {
	if err := r.p.check(); err != nil {
		return 0, err
	}
	a := &args{}
	where := "tenant_id = " + a.add(r.tenant) + campaignPredicate(a, f.CampaignID)
	if f.DeliveryIDs != nil {
		// A non-nil but empty list selects nothing, like the reference store.
		where += " AND id = ANY(" + a.add(f.DeliveryIDs) + "::text[])"
	}
	if len(f.Statuses) > 0 {
		where += " AND status = ANY(" + a.add(i16s(f.Statuses)) + "::smallint[])"
	}
	if len(f.ErrorClasses) > 0 {
		where += " AND last_error_class = ANY(" + a.add(i16s(f.ErrorClasses)) + "::smallint[])"
	}
	q := "WITH c AS (SELECT id FROM delivery WHERE " + where +
		" ORDER BY id LIMIT " + a.add(limitOrAll(limit)) + " FOR UPDATE SKIP LOCKED) " +
		"UPDATE delivery d SET status = 1, retry_gen = d.retry_gen + 1," +
		" next_attempt_at = " + a.add(tsInNN(f.Now)) +
		", lease_owner = '', lease_until = NULL," +
		" finished_at = NULL, updated_at = " + a.add(tsInNN(f.Now)) +
		" FROM c WHERE d.id = c.id"
	tag, err := r.p.pool.Exec(ctx, q, a.v...)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// setFirst is the NULL-only conditional update behind the first-interaction
// columns: it reports false when the column was already set, which is what
// makes unique tracking counts correct (architecture 9.3).
func (r *deliveryRepo) setFirst(ctx context.Context, col, id string, at time.Time) (bool, error) {
	if err := r.p.check(); err != nil {
		return false, err
	}
	q := fmt.Sprintf(`UPDATE delivery SET %s = $3, updated_at = $3
	    WHERE id = $1 AND tenant_id = $2 AND %s IS NULL`, col, col)
	tag, err := r.p.pool.Exec(ctx, q, id, r.tenant, tsInNN(at))
	if err != nil {
		return false, mapErr(err)
	}
	// A token for a delivery retention already removed is dropped, not an
	// error (architecture 9.1).
	return tag.RowsAffected() > 0, nil
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

// DeleteBefore enforces retention in chunks, dropping each delivery's attempts
// with it.
func (r *deliveryRepo) DeleteBefore(ctx context.Context, campaignID string, before time.Time, limit int) (int, error) {
	if err := r.p.check(); err != nil {
		return 0, err
	}
	a := &args{}
	q := "WITH c AS (SELECT id FROM delivery WHERE tenant_id = " + a.add(r.tenant) +
		campaignPredicate(a, campaignID) +
		" AND created_at < " + a.add(tsInNN(before)) +
		" ORDER BY created_at, id LIMIT " + a.add(limitOrAll(limit)) + "), " +
		"a AS (DELETE FROM delivery_attempt WHERE tenant_id = " + a.add(r.tenant) +
		" AND delivery_id IN (SELECT id FROM c)) " +
		"DELETE FROM delivery WHERE id IN (SELECT id FROM c)"
	tag, err := r.p.pool.Exec(ctx, q, a.v...)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// --- attempts ----------------------------------------------------------

var attemptColumns = []string{
	"id", "tenant_id", "delivery_id", "attempt_no", "retry_gen", "transport_id",
	"started_at", "finished_at", "smtp_code", "enhanced_code", "error_class",
	"error_text", "created_at",
}

var attemptCols = strings.Join(attemptColumns, ", ")

// execer is satisfied by both the pool and a transaction, so attempt rows can
// be written inside Complete's transaction or on their own.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func scanAttempt(r rowScanner) (*store.DeliveryAttempt, error) {
	var a store.DeliveryAttempt
	var class int16
	var started, finished *time.Time
	if err := r.Scan(&a.ID, &a.TenantID, &a.DeliveryID, &a.AttemptNo, &a.RetryGen,
		&a.TransportID, &started, &finished, &a.SMTPCode, &a.EnhancedCode,
		&class, &a.Error, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.ErrorClass = enumOut[store.ErrorClass](class)
	a.StartedAt, a.FinishedAt = tsOut(started), tsOut(finished)
	a.CreatedAt = a.CreatedAt.UTC()
	return &a, nil
}

// insertAttempts writes attempt rows through q, which is either the pool or
// the transaction Complete is running in.
func insertAttempts(ctx context.Context, q execer, tenant string, now time.Time, as []store.DeliveryAttempt) error {
	if len(as) == 0 {
		return nil
	}
	params := make([]any, 0, len(as)*len(attemptColumns))
	tuples := make([]string, 0, len(as))
	var b strings.Builder
	for i := range as {
		a := as[i]
		if a.ID == "" {
			a.ID = store.NewID()
		}
		a.TenantID = tenant
		if a.CreatedAt.IsZero() {
			a.CreatedAt = now
		}
		vals := []any{
			a.ID, a.TenantID, a.DeliveryID, a.AttemptNo, a.RetryGen,
			a.TransportID, tsIn(a.StartedAt), tsIn(a.FinishedAt), a.SMTPCode,
			a.EnhancedCode, i16(a.ErrorClass), a.Error, tsInNN(a.CreatedAt),
		}
		b.Reset()
		b.WriteByte('(')
		for j, v := range vals {
			if j > 0 {
				b.WriteString(", ")
			}
			params = append(params, v)
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(len(params)))
		}
		b.WriteByte(')')
		tuples = append(tuples, b.String())
	}
	sql := "INSERT INTO delivery_attempt (" + attemptCols + ") VALUES " +
		strings.Join(tuples, ", ") + " ON CONFLICT DO NOTHING"
	_, err := q.Exec(ctx, sql, params...)
	return mapErr(err)
}

type attemptRepo struct {
	p      *Provider
	tenant string
}

func (r *attemptRepo) Insert(ctx context.Context, as []store.DeliveryAttempt) error {
	if err := r.p.check(); err != nil {
		return err
	}
	return insertAttempts(ctx, r.p.pool, r.tenant, r.p.now(), as)
}

func (r *attemptRepo) ListByDelivery(ctx context.Context, deliveryID string, p store.Page) (store.Result[store.DeliveryAttempt], error) {
	var zero store.Result[store.DeliveryAttempt]
	if err := r.p.check(); err != nil {
		return zero, err
	}
	p = p.Normalize()
	a := &args{}
	q := "SELECT " + attemptCols + " FROM delivery_attempt WHERE tenant_id = " +
		a.add(r.tenant) + " AND delivery_id = " + a.add(deliveryID)
	keyset, err := keysetWhere(a, p, "id")
	if err != nil {
		return zero, err
	}
	q += keyset + " ORDER BY created_at, id LIMIT " + a.add(p.Limit+1)

	rows, err := r.p.pool.Query(ctx, q, a.v...)
	if err != nil {
		return zero, mapErr(err)
	}
	defer rows.Close()
	items := make([]store.DeliveryAttempt, 0, p.Limit)
	for rows.Next() {
		at, err := scanAttempt(rows)
		if err != nil {
			return zero, mapErr(err)
		}
		items = append(items, *at)
	}
	if err := rows.Err(); err != nil {
		return zero, mapErr(err)
	}
	res := store.Result[store.DeliveryAttempt]{Items: items}
	if len(items) > p.Limit {
		res.NextCursor = encodeCursor(items[p.Limit-1].CreatedAt, items[p.Limit-1].ID)
		res.Items = items[:p.Limit]
	}
	return res, nil
}
