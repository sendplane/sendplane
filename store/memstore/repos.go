package memstore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/sendplane/sendplane/store"
)

// --- tenant settings ---------------------------------------------------

type settingsRepo struct{ s *tenantStore }

func (r *settingsRepo) Get(_ context.Context) (*store.TenantSettings, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	if r.s.d.settings == nil {
		return nil, fmt.Errorf("%w: tenant settings", store.ErrNotFound)
	}
	cp := *r.s.d.settings
	return &cp, nil
}

func (r *settingsRepo) Create(_ context.Context, v *store.TenantSettings) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	if r.s.d.settings != nil {
		return fmt.Errorf("%w: tenant settings already exist", store.ErrConflict)
	}
	v.TenantID = r.s.tenant
	now := r.s.p.now()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	v.Version = 1
	cp := *v
	truncate(&cp.CreatedAt, &cp.UpdatedAt)
	r.s.d.settings = &cp
	return nil
}

func (r *settingsRepo) Update(_ context.Context, v *store.TenantSettings) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	cur := r.s.d.settings
	if cur == nil {
		return fmt.Errorf("%w: tenant settings", store.ErrNotFound)
	}
	if cur.Version != v.Version {
		return fmt.Errorf("%w: tenant settings version %d, have %d", store.ErrConflict, cur.Version, v.Version)
	}
	v.TenantID = r.s.tenant
	v.CreatedAt = cur.CreatedAt
	v.UpdatedAt = r.s.p.now()
	v.Version = cur.Version + 1
	cp := *v
	truncate(&cp.CreatedAt, &cp.UpdatedAt)
	r.s.d.settings = &cp
	return nil
}

// --- message versions --------------------------------------------------

type versionRepo struct{ table[store.MessageVersion] }

func (r *versionRepo) ListByTemplate(_ context.Context, templateID string, p store.Page) (store.Result[store.MessageVersion], error) {
	return r.listWhere(p, func(v *store.MessageVersion) bool { return v.TemplateID == templateID })
}

// --- bounce mailboxes --------------------------------------------------

type bounceMailboxRepo struct{ table[store.BounceMailbox] }

func (r *bounceMailboxRepo) ListEnabled(_ context.Context) ([]store.BounceMailbox, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if err := r.p.check(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(r.rows))
	for id, m := range r.rows {
		if m.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]store.BounceMailbox, 0, len(ids))
	for _, id := range ids {
		out = append(out, *r.rows[id])
	}
	return out, nil
}

// --- probe runs --------------------------------------------------------

type probeRunRepo struct{ table[store.ProbeRun] }

func (r *probeRunRepo) ListBySender(_ context.Context, senderID string, p store.Page) (store.Result[store.ProbeRun], error) {
	return r.listWhere(p, func(v *store.ProbeRun) bool { return v.SenderID == senderID })
}

func (r *probeRunRepo) ListPending(_ context.Context, p store.Page) (store.Result[store.ProbeRun], error) {
	return r.listWhere(p, func(v *store.ProbeRun) bool { return v.Pending })
}

// --- campaigns ---------------------------------------------------------

type campaignRepo struct{ table[store.Campaign] }

func (r *campaignRepo) ListByStatus(_ context.Context, statuses []store.CampaignStatus, p store.Page) (store.Result[store.Campaign], error) {
	return r.listWhere(p, func(c *store.Campaign) bool {
		for _, s := range statuses {
			if c.Status == s {
				return true
			}
		}
		return false
	})
}

func (r *campaignRepo) UpdateStats(_ context.Context, campaignID string, s store.CampaignStats) error {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if err := r.p.check(); err != nil {
		return err
	}
	c, ok := r.rows[campaignID]
	if !ok {
		return fmt.Errorf("%w: campaign %s", store.ErrNotFound, campaignID)
	}
	c.Stats = s
	c.UpdatedAt = r.p.now()
	return nil
}

// --- recipient chunks --------------------------------------------------

type chunkRepo struct{ s *tenantStore }

func chunkKey(campaignID, key string) string { return campaignID + "\x00" + key }

func (r *chunkRepo) Get(_ context.Context, campaignID, key string) (*store.RecipientChunk, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	c, ok := r.s.d.chunks[chunkKey(campaignID, key)]
	if !ok {
		return nil, fmt.Errorf("%w: chunk %s/%s", store.ErrNotFound, campaignID, key)
	}
	cp := *c
	return &cp, nil
}

func (r *chunkRepo) Put(_ context.Context, c *store.RecipientChunk) error {
	if c == nil || c.CampaignID == "" || c.Key == "" {
		return fmt.Errorf("%w: chunk needs a campaign and a key", store.ErrInvalid)
	}
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	c.TenantID = r.s.tenant
	now := r.s.p.now()
	k := chunkKey(c.CampaignID, c.Key)
	if prev, ok := r.s.d.chunks[k]; ok {
		c.CreatedAt = prev.CreatedAt
	} else if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	cp := *c
	truncate(&cp.CreatedAt, &cp.UpdatedAt)
	r.s.d.chunks[k] = &cp
	return nil
}

// --- suppressions ------------------------------------------------------

type suppressionRepo struct{ s *tenantStore }

func (r *suppressionRepo) Upsert(_ context.Context, v *store.Suppression) error {
	if v == nil || v.EmailNorm == "" {
		return fmt.Errorf("%w: suppression needs email_norm", store.ErrInvalid)
	}
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	v.TenantID = r.s.tenant
	if v.CreatedAt.IsZero() {
		v.CreatedAt = r.s.p.now()
	}
	cp := *v
	truncate(&cp.CreatedAt, &cp.ExpiresAt)
	r.s.d.suppressions[v.EmailNorm] = &cp
	return nil
}

func (r *suppressionRepo) IsSuppressed(_ context.Context, emailNorm string, now time.Time) (bool, *store.Suppression, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return false, nil, err
	}
	v, ok := r.s.d.suppressions[emailNorm]
	if !ok {
		return false, nil, nil
	}
	cp := *v
	if !cp.ExpiresAt.IsZero() && !cp.ExpiresAt.After(store.TruncateTime(now)) {
		return false, &cp, nil
	}
	return true, &cp, nil
}

func (r *suppressionRepo) List(_ context.Context, p store.Page) (store.Result[store.Suppression], error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return store.Result[store.Suppression]{}, err
	}
	// Suppressions are keyed by email_norm, so that is also the cursor.
	return paginate(r.s.d.suppressions, p, nil), nil
}

// DeleteBefore removes expired entries only: a zero ExpiresAt means "never
// expires" and is not eligible whatever the cutoff (store.SuppressionRepo).
func (r *suppressionRepo) DeleteBefore(_ context.Context, before time.Time, limit int) (int, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return 0, err
	}
	before = store.TruncateTime(before)
	keys := make([]string, 0, len(r.s.d.suppressions))
	for k, v := range r.s.d.suppressions {
		if v.ExpiresAt.IsZero() || v.ExpiresAt.After(before) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	for _, k := range keys {
		delete(r.s.d.suppressions, k)
	}
	return len(keys), nil
}

func (r *suppressionRepo) Delete(_ context.Context, emailNorm string) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	if _, ok := r.s.d.suppressions[emailNorm]; !ok {
		return fmt.Errorf("%w: suppression %s", store.ErrNotFound, emailNorm)
	}
	delete(r.s.d.suppressions, emailNorm)
	return nil
}

// --- bounces -----------------------------------------------------------

type bounceRepo struct{ table[store.BounceEvent] }

func (r *bounceRepo) ListByDelivery(_ context.Context, deliveryID string, p store.Page) (store.Result[store.BounceEvent], error) {
	return r.listWhere(p, func(b *store.BounceEvent) bool { return b.DeliveryID == deliveryID })
}

func (r *bounceRepo) DeleteBefore(_ context.Context, before time.Time, limit int) (int, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if err := r.p.check(); err != nil {
		return 0, err
	}
	return deleteBefore(r.rows, store.TruncateTime(before), limit,
		func(b *store.BounceEvent) time.Time { return b.CreatedAt },
		nil), nil
}

// --- tracking ----------------------------------------------------------

type trackingRepo struct{ s *tenantStore }

func (r *trackingRepo) InsertEvents(_ context.Context, evs []store.TrackingEvent) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	now := r.s.p.now()
	for i := range evs {
		e := evs[i]
		if e.ID == "" {
			e.ID = store.NewID()
		}
		e.TenantID = r.s.tenant
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		truncate(&e.CreatedAt)
		r.s.d.tracking[e.ID] = &e
	}
	return nil
}

func (r *trackingRepo) CountUnique(_ context.Context, campaignID string) (store.TrackingCounts, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return store.TrackingCounts{}, err
	}
	seen := map[store.TrackingKind]map[string]bool{}
	for _, e := range r.s.d.tracking {
		if e.CampaignID != campaignID || e.SuspectedBot {
			continue
		}
		if seen[e.Kind] == nil {
			seen[e.Kind] = map[string]bool{}
		}
		seen[e.Kind][e.DeliveryID] = true
	}
	return store.TrackingCounts{
		UniqueOpens:        int64(len(seen[store.TrackingOpen])),
		UniqueClicks:       int64(len(seen[store.TrackingClick])),
		UnsubscribeClicked: int64(len(seen[store.TrackingUnsubscribeClicked])),
		Unsubscribed:       int64(len(seen[store.TrackingUnsubscribed])),
	}, nil
}

func (r *trackingRepo) LinkClicks(_ context.Context, campaignID string) ([]store.LinkClick, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	type agg struct {
		linkNo int
		clicks int64
		uniq   map[string]bool
	}
	byURL := map[string]*agg{}
	for _, e := range r.s.d.tracking {
		if e.CampaignID != campaignID || e.Kind != store.TrackingClick || e.SuspectedBot {
			continue
		}
		a, ok := byURL[e.URL]
		if !ok {
			a = &agg{linkNo: e.LinkNo, uniq: map[string]bool{}}
			byURL[e.URL] = a
		}
		if e.LinkNo < a.linkNo {
			a.linkNo = e.LinkNo
		}
		a.clicks++
		a.uniq[e.DeliveryID] = true
	}
	out := make([]store.LinkClick, 0, len(byURL))
	for url, a := range byURL {
		out = append(out, store.LinkClick{
			LinkNo: a.linkNo, URL: url,
			Clicks: a.clicks, UniqueClicks: int64(len(a.uniq)),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LinkNo != out[j].LinkNo {
			return out[i].LinkNo < out[j].LinkNo
		}
		return out[i].URL < out[j].URL
	})
	return out, nil
}

func (r *trackingRepo) DeleteBefore(_ context.Context, before time.Time, limit int) (int, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return 0, err
	}
	return deleteBefore(r.s.d.tracking, store.TruncateTime(before), limit,
		func(e *store.TrackingEvent) time.Time { return e.CreatedAt },
		nil), nil
}

// --- outbox ------------------------------------------------------------

type outboxRepo struct{ s *tenantStore }

func (r *outboxRepo) Enqueue(_ context.Context, evs []store.OutboxEvent) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	now := r.s.p.now()
	for i := range evs {
		e := evs[i]
		if e.ID == "" {
			e.ID = store.NewID()
		}
		e.TenantID = r.s.tenant
		e.Status = store.OutboxPending
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		if e.NextAttemptAt.IsZero() {
			e.NextAttemptAt = e.CreatedAt
		}
		truncate(&e.CreatedAt, &e.NextAttemptAt, &e.LeaseUntil, &e.DeliveredAt)
		r.s.d.outbox[e.ID] = &e
	}
	return nil
}

func (r *outboxRepo) ClaimPending(_ context.Context, limit int, lease time.Duration, owner string, now time.Time) ([]store.OutboxEvent, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	now = store.TruncateTime(now)
	ids := make([]string, 0, len(r.s.d.outbox))
	for id, e := range r.s.d.outbox {
		if e.Status != store.OutboxPending || e.NextAttemptAt.After(now) {
			continue
		}
		if e.LeaseOwner != "" && e.LeaseUntil.After(now) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]store.OutboxEvent, 0, len(ids))
	for _, id := range ids {
		e := r.s.d.outbox[id]
		e.LeaseOwner = owner
		e.LeaseUntil = store.TruncateTime(now.Add(lease))
		out = append(out, *e)
	}
	return out, nil
}

func (r *outboxRepo) Get(_ context.Context, id string) (*store.OutboxEvent, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	e, ok := r.s.d.outbox[id]
	if !ok {
		return nil, fmt.Errorf("%w: outbox %s", store.ErrNotFound, id)
	}
	cp := *e
	return &cp, nil
}

// Reset is the replay verb: a full attempt budget again, due now.
func (r *outboxRepo) Reset(_ context.Context, id string, now time.Time) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	e, ok := r.s.d.outbox[id]
	if !ok {
		return fmt.Errorf("%w: outbox %s", store.ErrNotFound, id)
	}
	e.Status = store.OutboxPending
	e.Attempts = 0
	e.LastError = ""
	e.NextAttemptAt = store.TruncateTime(now)
	e.LeaseOwner, e.LeaseUntil = "", time.Time{}
	e.DeliveredAt = time.Time{}
	return nil
}

func (r *outboxRepo) MarkDelivered(_ context.Context, id string, at time.Time) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	e, ok := r.s.d.outbox[id]
	if !ok {
		return fmt.Errorf("%w: outbox %s", store.ErrNotFound, id)
	}
	e.Status = store.OutboxDelivered
	e.DeliveredAt = store.TruncateTime(at)
	e.LeaseOwner, e.LeaseUntil = "", time.Time{}
	return nil
}

func (r *outboxRepo) MarkFailed(_ context.Context, id string, nextAttempt time.Time, errMsg string) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	e, ok := r.s.d.outbox[id]
	if !ok {
		return fmt.Errorf("%w: outbox %s", store.ErrNotFound, id)
	}
	e.Attempts++
	e.LastError = errMsg
	e.LeaseOwner, e.LeaseUntil = "", time.Time{}
	if nextAttempt.IsZero() {
		e.Status = store.OutboxFailed
	} else {
		e.Status = store.OutboxPending
		e.NextAttemptAt = store.TruncateTime(nextAttempt)
	}
	return nil
}

func (r *outboxRepo) List(_ context.Context, status store.OutboxStatus, p store.Page) (store.Result[store.OutboxEvent], error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return store.Result[store.OutboxEvent]{}, err
	}
	return paginate(r.s.d.outbox, p, func(e *store.OutboxEvent) bool {
		return status == "" || e.Status == status
	}), nil
}

// DeleteBefore only ever removes dispatched rows: a pending event is still
// owed to the host, and a failed one is the dead letter the host replays.
func (r *outboxRepo) DeleteBefore(_ context.Context, before time.Time, limit int) (int, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return 0, err
	}
	return deleteBefore(r.s.d.outbox, store.TruncateTime(before), limit,
		func(e *store.OutboxEvent) time.Time { return e.CreatedAt },
		func(e *store.OutboxEvent) bool {
			return e.Status == store.OutboxDelivered || e.Status == store.OutboxFailed
		}), nil
}

// --- locks -------------------------------------------------------------

type lockRepo struct{ s *tenantStore }

func (r *lockRepo) Acquire(_ context.Context, name, owner string, ttl time.Duration, now time.Time) (bool, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return false, err
	}
	now = store.TruncateTime(now)
	cur, ok := r.s.d.locks[name]
	if ok && cur.Owner != owner && cur.ExpiresAt.After(now) {
		return false, nil
	}
	l := &store.Lock{
		TenantID: r.s.tenant, Name: name, Owner: owner,
		AcquiredAt: now, ExpiresAt: store.TruncateTime(now.Add(ttl)),
	}
	if ok && cur.Owner == owner {
		l.AcquiredAt = cur.AcquiredAt
	}
	r.s.d.locks[name] = l
	return true, nil
}

func (r *lockRepo) Renew(_ context.Context, name, owner string, ttl time.Duration, now time.Time) (bool, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return false, err
	}
	now = store.TruncateTime(now)
	cur, ok := r.s.d.locks[name]
	if !ok || cur.Owner != owner || !cur.ExpiresAt.After(now) {
		return false, nil
	}
	cur.ExpiresAt = store.TruncateTime(now.Add(ttl))
	return true, nil
}

func (r *lockRepo) Release(_ context.Context, name, owner string) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	if cur, ok := r.s.d.locks[name]; ok && cur.Owner == owner {
		delete(r.s.d.locks, name)
	}
	return nil
}

func (r *lockRepo) Get(_ context.Context, name string) (*store.Lock, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	cur, ok := r.s.d.locks[name]
	if !ok {
		return nil, fmt.Errorf("%w: lock %s", store.ErrNotFound, name)
	}
	cp := *cur
	return &cp, nil
}

// --- workers -----------------------------------------------------------

type workerRepo struct{ s *tenantStore }

func (r *workerRepo) Heartbeat(_ context.Context, w store.Worker) error {
	if w.ID == "" {
		return fmt.Errorf("%w: worker needs an ID", store.ErrInvalid)
	}
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	w.TenantID = r.s.tenant
	if w.LastSeenAt.IsZero() {
		w.LastSeenAt = r.s.p.now()
	}
	if prev, ok := r.s.d.workers[w.ID]; ok && w.StartedAt.IsZero() {
		w.StartedAt = prev.StartedAt
	} else if w.StartedAt.IsZero() {
		w.StartedAt = w.LastSeenAt
	}
	truncate(&w.StartedAt, &w.LastSeenAt)
	r.s.d.workers[w.ID] = &w
	return nil
}

func (r *workerRepo) ListActive(_ context.Context, since time.Time) ([]store.Worker, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	since = store.TruncateTime(since)
	out := make([]store.Worker, 0, len(r.s.d.workers))
	for _, w := range r.s.d.workers {
		if w.LastSeenAt.Before(since) {
			continue
		}
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// deleteBefore is the shared retention delete: the oldest matching rows first
// (IDs are UUIDv7, so ID order is creation order), at most limit of them.
func deleteBefore[T any](rows map[string]*T, before time.Time, limit int,
	created func(*T) time.Time, keep func(*T) bool) int {
	ids := make([]string, 0, len(rows))
	for id, v := range rows {
		if !created(v).Before(before) {
			continue
		}
		if keep != nil && !keep(v) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	for _, id := range ids {
		delete(rows, id)
	}
	return len(ids)
}
