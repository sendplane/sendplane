package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/sendplane/sendplane/store"
)

// --- tenant settings ---------------------------------------------------

type settingsRepo struct {
	p      *Provider
	tenant string
}

const settingsCols = `retry, retention_days, suppression_enabled, unsubscribe_mode,
	unsubscribe_url_template, default_locale, tracking, version, created_at, updated_at`

func (r *settingsRepo) Get(ctx context.Context) (*store.TenantSettings, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	const q = "SELECT " + settingsCols + " FROM tenant_settings WHERE tenant_id = $1"
	var v store.TenantSettings
	var retry, tracking []byte
	var mode string
	err := r.p.pool.QueryRow(ctx, q, r.tenant).Scan(&retry, &v.RetentionDays,
		&v.SuppressionEnabled, &mode, &v.UnsubscribeURLTemplate, &v.DefaultLocale,
		&tracking, &v.Version, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("tenant settings %s: %w", r.tenant, mapErr(err))
	}
	v.TenantID = r.tenant
	v.UnsubscribeMode = store.UnsubscribeMode(mode)
	v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
	if err := jsonOut(retry, &v.Retry); err != nil {
		return nil, err
	}
	return &v, jsonOut(tracking, &v.Tracking)
}

func (r *settingsRepo) Create(ctx context.Context, v *store.TenantSettings) error {
	if v == nil {
		return fmt.Errorf("%w: nil settings", store.ErrInvalid)
	}
	if err := r.p.check(); err != nil {
		return err
	}
	retry, err := jsonIn(v.Retry)
	if err != nil {
		return err
	}
	tracking, err := jsonIn(v.Tracking)
	if err != nil {
		return err
	}
	v.TenantID = r.tenant
	now := r.p.now()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	v.Version = 1
	const q = `INSERT INTO tenant_settings (tenant_id, retry, retention_days,
	    suppression_enabled, unsubscribe_mode, unsubscribe_url_template,
	    default_locale, tracking, version, created_at, updated_at)
	  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	_, err = r.p.pool.Exec(ctx, q, r.tenant, retry, v.RetentionDays,
		v.SuppressionEnabled, string(v.UnsubscribeMode), v.UnsubscribeURLTemplate,
		v.DefaultLocale, tracking, v.Version, tsInNN(v.CreatedAt), tsInNN(v.UpdatedAt))
	return mapErr(err)
}

func (r *settingsRepo) Update(ctx context.Context, v *store.TenantSettings) error {
	if v == nil {
		return fmt.Errorf("%w: nil settings", store.ErrInvalid)
	}
	if err := r.p.check(); err != nil {
		return err
	}
	retry, err := jsonIn(v.Retry)
	if err != nil {
		return err
	}
	tracking, err := jsonIn(v.Tracking)
	if err != nil {
		return err
	}
	v.TenantID = r.tenant
	const q = `UPDATE tenant_settings SET retry = $2, retention_days = $3,
	    suppression_enabled = $4, unsubscribe_mode = $5,
	    unsubscribe_url_template = $6, default_locale = $7, tracking = $8,
	    version = version + 1, updated_at = $9
	  WHERE tenant_id = $1 AND version = $10
	  RETURNING created_at, updated_at, version`
	var created, updated time.Time
	var version int64
	err = r.p.pool.QueryRow(ctx, q, r.tenant, retry, v.RetentionDays,
		v.SuppressionEnabled, string(v.UnsubscribeMode), v.UnsubscribeURLTemplate,
		v.DefaultLocale, tracking, r.p.now(), v.Version).
		Scan(&created, &updated, &version)
	if err != nil {
		var exists bool
		if qerr := r.p.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM tenant_settings WHERE tenant_id = $1)`,
			r.tenant).Scan(&exists); qerr != nil {
			return mapErr(qerr)
		}
		if !exists {
			return fmt.Errorf("%w: tenant settings %s", store.ErrNotFound, r.tenant)
		}
		return fmt.Errorf("%w: tenant settings %s version mismatch", store.ErrConflict, r.tenant)
	}
	v.CreatedAt, v.UpdatedAt, v.Version = created.UTC(), updated.UTC(), version
	return nil
}

// --- recipient chunks --------------------------------------------------

type chunkRepo struct {
	p      *Provider
	tenant string
}

func (r *chunkRepo) Get(ctx context.Context, campaignID, key string) (*store.RecipientChunk, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	const q = `SELECT state, accepted, duplicates, invalid, created_at, updated_at
	             FROM recipient_chunk
	            WHERE tenant_id = $1 AND campaign_id = $2 AND key = $3`
	var c store.RecipientChunk
	var state string
	err := r.p.pool.QueryRow(ctx, q, r.tenant, campaignID, key).
		Scan(&state, &c.Accepted, &c.Duplicates, &c.Invalid, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("chunk %s/%s: %w", campaignID, key, mapErr(err))
	}
	c.TenantID, c.CampaignID, c.Key = r.tenant, campaignID, key
	c.State = store.ChunkState(state)
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return &c, nil
}

// Put inserts or replaces the chunk, which is what makes re-sending the same
// ingest call idempotent (architecture 7.2).
func (r *chunkRepo) Put(ctx context.Context, c *store.RecipientChunk) error {
	if c == nil || c.CampaignID == "" || c.Key == "" {
		return fmt.Errorf("%w: chunk needs a campaign and a key", store.ErrInvalid)
	}
	if err := r.p.check(); err != nil {
		return err
	}
	c.TenantID = r.tenant
	now := r.p.now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	const q = `INSERT INTO recipient_chunk (tenant_id, campaign_id, key, state,
	      accepted, duplicates, invalid, created_at, updated_at)
	    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	    ON CONFLICT (tenant_id, campaign_id, key) DO UPDATE SET
	      state = EXCLUDED.state, accepted = EXCLUDED.accepted,
	      duplicates = EXCLUDED.duplicates, invalid = EXCLUDED.invalid,
	      updated_at = EXCLUDED.updated_at
	    RETURNING created_at`
	var created time.Time
	err := r.p.pool.QueryRow(ctx, q, r.tenant, c.CampaignID, c.Key,
		string(c.State), c.Accepted, c.Duplicates, c.Invalid,
		tsInNN(c.CreatedAt), tsInNN(c.UpdatedAt)).Scan(&created)
	if err != nil {
		return mapErr(err)
	}
	c.CreatedAt = created.UTC()
	return nil
}

// --- suppressions ------------------------------------------------------

type suppressionRepo struct {
	p      *Provider
	tenant string
}

func (r *suppressionRepo) Upsert(ctx context.Context, v *store.Suppression) error {
	if v == nil || v.EmailNorm == "" {
		return fmt.Errorf("%w: suppression needs email_norm", store.ErrInvalid)
	}
	if err := r.p.check(); err != nil {
		return err
	}
	v.TenantID = r.tenant
	if v.CreatedAt.IsZero() {
		v.CreatedAt = r.p.now()
	}
	const q = `INSERT INTO suppression (tenant_id, email_norm, reason,
	      source_delivery_id, created_at, expires_at)
	    VALUES ($1, $2, $3, $4, $5, $6)
	    ON CONFLICT (tenant_id, email_norm) DO UPDATE SET
	      reason = EXCLUDED.reason,
	      source_delivery_id = EXCLUDED.source_delivery_id,
	      created_at = EXCLUDED.created_at,
	      expires_at = EXCLUDED.expires_at`
	_, err := r.p.pool.Exec(ctx, q, r.tenant, v.EmailNorm, string(v.Reason),
		v.SourceDeliveryID, tsInNN(v.CreatedAt), tsIn(v.ExpiresAt))
	return mapErr(err)
}

func (r *suppressionRepo) IsSuppressed(ctx context.Context, emailNorm string, now time.Time) (bool, *store.Suppression, error) {
	if err := r.p.check(); err != nil {
		return false, nil, err
	}
	const q = `SELECT reason, source_delivery_id, created_at, expires_at
	             FROM suppression WHERE tenant_id = $1 AND email_norm = $2`
	var s store.Suppression
	var reason string
	var expires *time.Time
	err := r.p.pool.QueryRow(ctx, q, r.tenant, emailNorm).
		Scan(&reason, &s.SourceDeliveryID, &s.CreatedAt, &expires)
	if err != nil {
		if mapped := mapErr(err); isNotFound(mapped) {
			return false, nil, nil
		}
		return false, nil, mapErr(err)
	}
	s.TenantID, s.EmailNorm = r.tenant, emailNorm
	s.Reason = store.SuppressionReason(reason)
	s.CreatedAt = s.CreatedAt.UTC()
	s.ExpiresAt = tsOut(expires)
	if !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(now) {
		return false, &s, nil
	}
	return true, &s, nil
}

func (r *suppressionRepo) List(ctx context.Context, p store.Page) (store.Result[store.Suppression], error) {
	var zero store.Result[store.Suppression]
	if err := r.p.check(); err != nil {
		return zero, err
	}
	p = p.Normalize()
	a := &args{}
	q := `SELECT email_norm, reason, source_delivery_id, created_at, expires_at
	        FROM suppression WHERE tenant_id = ` + a.add(r.tenant)
	// Suppressions have no id, so email_norm is the tiebreaker of the keyset.
	keyset, err := keysetWhere(a, p, "email_norm")
	if err != nil {
		return zero, err
	}
	q += keyset + " ORDER BY created_at, email_norm LIMIT " + a.add(p.Limit+1)

	rows, err := r.p.pool.Query(ctx, q, a.v...)
	if err != nil {
		return zero, mapErr(err)
	}
	defer rows.Close()
	items := make([]store.Suppression, 0, p.Limit)
	for rows.Next() {
		var s store.Suppression
		var reason string
		var expires *time.Time
		if err := rows.Scan(&s.EmailNorm, &reason, &s.SourceDeliveryID,
			&s.CreatedAt, &expires); err != nil {
			return zero, mapErr(err)
		}
		s.TenantID = r.tenant
		s.Reason = store.SuppressionReason(reason)
		s.CreatedAt = s.CreatedAt.UTC()
		s.ExpiresAt = tsOut(expires)
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		return zero, mapErr(err)
	}
	res := store.Result[store.Suppression]{Items: items}
	if len(items) > p.Limit {
		res.NextCursor = encodeCursor(items[p.Limit-1].CreatedAt, items[p.Limit-1].EmailNorm)
		res.Items = items[:p.Limit]
	}
	return res, nil
}

func (r *suppressionRepo) Delete(ctx context.Context, emailNorm string) error {
	if err := r.p.check(); err != nil {
		return err
	}
	tag, err := r.p.pool.Exec(ctx,
		`DELETE FROM suppression WHERE tenant_id = $1 AND email_norm = $2`,
		r.tenant, emailNorm)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: suppression %s", store.ErrNotFound, emailNorm)
	}
	return nil
}

// --- tracking ----------------------------------------------------------

type trackingRepo struct {
	p      *Provider
	tenant string
}

func (r *trackingRepo) InsertEvents(ctx context.Context, evs []store.TrackingEvent) error {
	if err := r.p.check(); err != nil {
		return err
	}
	if len(evs) == 0 {
		return nil
	}
	now := r.p.now()
	a := &args{}
	tuples := make([]string, 0, len(evs))
	for i := range evs {
		e := evs[i]
		if e.ID == "" {
			e.ID = store.NewID()
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		tuples = append(tuples, "("+a.add(e.ID)+", "+a.add(r.tenant)+", "+
			a.add(e.DeliveryID)+", "+a.add(e.CampaignID)+", "+a.add(i16(e.Kind))+", "+
			a.add(e.URL)+", "+a.add(e.LinkNo)+", "+a.add(e.UserAgent)+", "+
			a.add(e.IPHash)+", "+a.add(e.SuspectedBot)+", "+a.add(tsInNN(e.CreatedAt))+")")
	}
	q := `INSERT INTO tracking_event (id, tenant_id, delivery_id, campaign_id,
	        kind, url, link_no, user_agent, ip_hash, suspected_bot, created_at)
	      VALUES ` + joinComma(tuples) + " ON CONFLICT DO NOTHING"
	_, err := r.p.pool.Exec(ctx, q, a.v...)
	return mapErr(err)
}

// CountUnique counts distinct deliveries per kind, excluding suspected bots.
func (r *trackingRepo) CountUnique(ctx context.Context, campaignID string) (store.TrackingCounts, error) {
	var out store.TrackingCounts
	if err := r.p.check(); err != nil {
		return out, err
	}
	const q = `SELECT kind, count(DISTINCT delivery_id) FROM tracking_event
	            WHERE tenant_id = $1 AND campaign_id = $2 AND NOT suspected_bot
	            GROUP BY kind`
	rows, err := r.p.pool.Query(ctx, q, r.tenant, campaignID)
	if err != nil {
		return out, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind int16
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			return out, mapErr(err)
		}
		switch enumOut[store.TrackingKind](kind) {
		case store.TrackingOpen:
			out.UniqueOpens = n
		case store.TrackingClick:
			out.UniqueClicks = n
		case store.TrackingUnsubscribeClicked:
			out.UnsubscribeClicked = n
		case store.TrackingUnsubscribed:
			out.Unsubscribed = n
		}
	}
	return out, mapErr(rows.Err())
}

// LinkClicks reports clicks per link URL, ordered by LinkNo then URL.
func (r *trackingRepo) LinkClicks(ctx context.Context, campaignID string) ([]store.LinkClick, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	const q = `SELECT min(link_no)::int, url, count(*), count(DISTINCT delivery_id)
	             FROM tracking_event
	            WHERE tenant_id = $1 AND campaign_id = $2 AND kind = 1
	              AND NOT suspected_bot
	            GROUP BY url ORDER BY min(link_no), url`
	rows, err := r.p.pool.Query(ctx, q, r.tenant, campaignID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.LinkClick
	for rows.Next() {
		var lc store.LinkClick
		if err := rows.Scan(&lc.LinkNo, &lc.URL, &lc.Clicks, &lc.UniqueClicks); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, lc)
	}
	return out, mapErr(rows.Err())
}

// --- outbox ------------------------------------------------------------

type outboxRepo struct {
	p      *Provider
	tenant string
}

const outboxCols = `id, tenant_id, type, payload, status, attempts,
	next_attempt_at, lease_owner, lease_until, last_error, created_at, delivered_at`

func scanOutbox(r rowScanner) (*store.OutboxEvent, error) {
	var e store.OutboxEvent
	var payload []byte
	var status string
	var leaseUntil, delivered *time.Time
	if err := r.Scan(&e.ID, &e.TenantID, &e.Type, &payload, &status, &e.Attempts,
		&e.NextAttemptAt, &e.LeaseOwner, &leaseUntil, &e.LastError,
		&e.CreatedAt, &delivered); err != nil {
		return nil, err
	}
	e.Payload = rawOut(payload)
	e.Status = store.OutboxStatus(status)
	e.NextAttemptAt = e.NextAttemptAt.UTC()
	e.LeaseUntil = tsOut(leaseUntil)
	e.CreatedAt = e.CreatedAt.UTC()
	e.DeliveredAt = tsOut(delivered)
	return &e, nil
}

func (r *outboxRepo) Enqueue(ctx context.Context, evs []store.OutboxEvent) error {
	if err := r.p.check(); err != nil {
		return err
	}
	if len(evs) == 0 {
		return nil
	}
	now := r.p.now()
	a := &args{}
	tuples := make([]string, 0, len(evs))
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
		payload, err := jsonIn(e.Payload)
		if err != nil {
			return err
		}
		tuples = append(tuples, "("+a.add(e.ID)+", "+a.add(r.tenant)+", "+
			a.add(e.Type)+", "+a.add(payload)+", "+a.add(string(store.OutboxPending))+", "+
			a.add(e.Attempts)+", "+a.add(tsInNN(e.NextAttemptAt))+", "+
			a.add(e.LeaseOwner)+", "+a.add(tsIn(e.LeaseUntil))+", "+
			a.add(e.LastError)+", "+a.add(tsInNN(e.CreatedAt))+")")
	}
	q := `INSERT INTO outbox_event (id, tenant_id, type, payload, status,
	        attempts, next_attempt_at, lease_owner, lease_until, last_error, created_at)
	      VALUES ` + joinComma(tuples) + " ON CONFLICT DO NOTHING"
	_, err := r.p.pool.Exec(ctx, q, a.v...)
	return mapErr(err)
}

// ClaimPending leases due pending events for one dispatcher.
func (r *outboxRepo) ClaimPending(ctx context.Context, limit int, lease time.Duration, owner string, now time.Time) ([]store.OutboxEvent, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	a := &args{}
	q := "WITH c AS (SELECT id FROM outbox_event WHERE tenant_id = " + a.add(r.tenant) +
		" AND status = " + a.add(string(store.OutboxPending)) +
		" AND next_attempt_at <= " + a.add(tsInNN(now)) +
		" AND (lease_owner = '' OR lease_until IS NULL OR lease_until <= " + a.add(tsInNN(now)) + ")" +
		" ORDER BY id LIMIT " + a.add(limitOrAll(limit)) + " FOR UPDATE SKIP LOCKED) " +
		"UPDATE outbox_event e SET lease_owner = " + a.add(owner) +
		", lease_until = " + a.add(tsInNN(now.Add(lease))) +
		" FROM c WHERE e.id = c.id RETURNING " + prefixedList(outboxCols, "e")

	rows, err := r.p.pool.Query(ctx, q, a.v...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.OutboxEvent
	for rows.Next() {
		e, err := scanOutbox(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	sortByID(out, func(e *store.OutboxEvent) string { return e.ID })
	return out, nil
}

func (r *outboxRepo) MarkDelivered(ctx context.Context, id string, at time.Time) error {
	if err := r.p.check(); err != nil {
		return err
	}
	const q = `UPDATE outbox_event SET status = 'delivered', delivered_at = $3,
	      lease_owner = '', lease_until = NULL
	    WHERE id = $1 AND tenant_id = $2`
	tag, err := r.p.pool.Exec(ctx, q, id, r.tenant, tsInNN(at))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: outbox %s", store.ErrNotFound, id)
	}
	return nil
}

// MarkFailed schedules a retry; an empty nextAttempt moves the event to the
// dead letter state.
func (r *outboxRepo) MarkFailed(ctx context.Context, id string, nextAttempt time.Time, errMsg string) error {
	if err := r.p.check(); err != nil {
		return err
	}
	const q = `UPDATE outbox_event SET
	      attempts = attempts + 1,
	      last_error = $3,
	      status = CASE WHEN $4::timestamptz IS NULL THEN 'failed' ELSE 'pending' END,
	      next_attempt_at = COALESCE($4::timestamptz, next_attempt_at),
	      lease_owner = '', lease_until = NULL
	    WHERE id = $1 AND tenant_id = $2`
	tag, err := r.p.pool.Exec(ctx, q, id, r.tenant, errMsg, tsIn(nextAttempt))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: outbox %s", store.ErrNotFound, id)
	}
	return nil
}

func (r *outboxRepo) List(ctx context.Context, status store.OutboxStatus, p store.Page) (store.Result[store.OutboxEvent], error) {
	var zero store.Result[store.OutboxEvent]
	if err := r.p.check(); err != nil {
		return zero, err
	}
	p = p.Normalize()
	a := &args{}
	q := "SELECT " + outboxCols + " FROM outbox_event WHERE tenant_id = " + a.add(r.tenant)
	if status != "" {
		q += " AND status = " + a.add(string(status))
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
	items := make([]store.OutboxEvent, 0, p.Limit)
	for rows.Next() {
		e, err := scanOutbox(rows)
		if err != nil {
			return zero, mapErr(err)
		}
		items = append(items, *e)
	}
	if err := rows.Err(); err != nil {
		return zero, mapErr(err)
	}
	res := store.Result[store.OutboxEvent]{Items: items}
	if len(items) > p.Limit {
		res.NextCursor = encodeCursor(items[p.Limit-1].CreatedAt, items[p.Limit-1].ID)
		res.Items = items[:p.Limit]
	}
	return res, nil
}

// --- locks -------------------------------------------------------------

type lockRepo struct {
	p      *Provider
	tenant string
}

// Acquire takes the lock, renews it if owner already holds it, or takes it
// over if the existing lease expired at or before now.
func (r *lockRepo) Acquire(ctx context.Context, name, owner string, ttl time.Duration, now time.Time) (bool, error) {
	if err := r.p.check(); err != nil {
		return false, err
	}
	const q = `INSERT INTO job_lock (tenant_id, name, owner, acquired_at, expires_at)
	    VALUES ($1, $2, $3, $4, $5)
	    ON CONFLICT (tenant_id, name) DO UPDATE SET
	      owner = EXCLUDED.owner,
	      acquired_at = CASE WHEN job_lock.owner = EXCLUDED.owner
	                         THEN job_lock.acquired_at ELSE EXCLUDED.acquired_at END,
	      expires_at = EXCLUDED.expires_at
	    WHERE job_lock.owner = EXCLUDED.owner OR job_lock.expires_at <= $4
	    RETURNING 1`
	var got int
	err := r.p.pool.QueryRow(ctx, q, r.tenant, name, owner,
		tsInNN(now), tsInNN(now.Add(ttl))).Scan(&got)
	if err != nil {
		if isNotFound(mapErr(err)) {
			return false, nil // somebody else holds a live lease
		}
		return false, mapErr(err)
	}
	return true, nil
}

func (r *lockRepo) Renew(ctx context.Context, name, owner string, ttl time.Duration, now time.Time) (bool, error) {
	if err := r.p.check(); err != nil {
		return false, err
	}
	const q = `UPDATE job_lock SET expires_at = $4
	    WHERE tenant_id = $1 AND name = $2 AND owner = $3 AND expires_at > $5`
	tag, err := r.p.pool.Exec(ctx, q, r.tenant, name, owner,
		tsInNN(now.Add(ttl)), tsInNN(now))
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r *lockRepo) Release(ctx context.Context, name, owner string) error {
	if err := r.p.check(); err != nil {
		return err
	}
	_, err := r.p.pool.Exec(ctx,
		`DELETE FROM job_lock WHERE tenant_id = $1 AND name = $2 AND owner = $3`,
		r.tenant, name, owner)
	return mapErr(err)
}

func (r *lockRepo) Get(ctx context.Context, name string) (*store.Lock, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	const q = `SELECT owner, acquired_at, expires_at FROM job_lock
	            WHERE tenant_id = $1 AND name = $2`
	var l store.Lock
	err := r.p.pool.QueryRow(ctx, q, r.tenant, name).
		Scan(&l.Owner, &l.AcquiredAt, &l.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", name, mapErr(err))
	}
	l.TenantID, l.Name = r.tenant, name
	l.AcquiredAt, l.ExpiresAt = l.AcquiredAt.UTC(), l.ExpiresAt.UTC()
	return &l, nil
}

// --- workers -----------------------------------------------------------

type workerRepo struct {
	p      *Provider
	tenant string
}

func (r *workerRepo) Heartbeat(ctx context.Context, w store.Worker) error {
	if w.ID == "" {
		return fmt.Errorf("%w: worker needs an ID", store.ErrInvalid)
	}
	if err := r.p.check(); err != nil {
		return err
	}
	lastSeen := w.LastSeenAt
	if lastSeen.IsZero() {
		lastSeen = r.p.now()
	}
	started := w.StartedAt
	keepStarted := started.IsZero()
	if keepStarted {
		started = lastSeen
	}
	const q = `INSERT INTO worker (tenant_id, id, role, lanes, concurrency,
	      started_at, last_seen_at)
	    VALUES ($1, $2, $3, $4, $5, $6, $7)
	    ON CONFLICT (tenant_id, id) DO UPDATE SET
	      role = EXCLUDED.role, lanes = EXCLUDED.lanes,
	      concurrency = EXCLUDED.concurrency,
	      started_at = CASE WHEN $8 THEN worker.started_at ELSE EXCLUDED.started_at END,
	      last_seen_at = EXCLUDED.last_seen_at`
	_, err := r.p.pool.Exec(ctx, q, r.tenant, w.ID, w.Role, i16s(w.Lanes),
		w.Concurrency, tsInNN(started), tsInNN(lastSeen), keepStarted)
	return mapErr(err)
}

func (r *workerRepo) ListActive(ctx context.Context, since time.Time) ([]store.Worker, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	const q = `SELECT id, role, lanes, concurrency, started_at, last_seen_at
	             FROM worker WHERE tenant_id = $1 AND last_seen_at >= $2
	            ORDER BY id`
	rows, err := r.p.pool.Query(ctx, q, r.tenant, tsInNN(since))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Worker
	for rows.Next() {
		var w store.Worker
		var lanes []int16
		if err := rows.Scan(&w.ID, &w.Role, &lanes, &w.Concurrency,
			&w.StartedAt, &w.LastSeenAt); err != nil {
			return nil, mapErr(err)
		}
		w.TenantID = r.tenant
		for _, l := range lanes {
			w.Lanes = append(w.Lanes, enumOut[store.Lane](l))
		}
		w.StartedAt, w.LastSeenAt = w.StartedAt.UTC(), w.LastSeenAt.UTC()
		out = append(out, w)
	}
	return out, mapErr(rows.Err())
}
