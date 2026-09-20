package memstore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/sendplane/sendplane/store"
)

type deliveryRepo struct{ s *tenantStore }

func emailKey(campaignID, emailNorm string) string { return campaignID + "\x00" + emailNorm }

func (r *deliveryRepo) InsertBatch(_ context.Context, ds []store.Delivery) (int, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return 0, err
	}
	now := r.s.p.now()
	inserted := 0
	for i := range ds {
		d := ds[i]
		if d.EmailNorm == "" {
			return inserted, fmt.Errorf("%w: delivery %d has no email_norm", store.ErrInvalid, i)
		}
		if d.CampaignID != "" {
			k := emailKey(d.CampaignID, d.EmailNorm)
			if _, dup := r.s.d.campaignEmails[k]; dup {
				continue
			}
			if d.ID == "" {
				d.ID = store.NewID()
			}
			r.s.d.campaignEmails[k] = d.ID
		} else if d.ID == "" {
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
		r.s.d.deliveries[d.ID] = &d
		inserted++
	}
	return inserted, nil
}

func (r *deliveryRepo) Get(_ context.Context, id string) (*store.Delivery, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	d, ok := r.s.d.deliveries[id]
	if !ok {
		return nil, fmt.Errorf("%w: delivery %s", store.ErrNotFound, id)
	}
	cp := *d
	return &cp, nil
}

func matchStatus(statuses []store.DeliveryStatus, s store.DeliveryStatus) bool {
	if len(statuses) == 0 {
		return true
	}
	for _, x := range statuses {
		if x == s {
			return true
		}
	}
	return false
}

func matchClass(classes []store.ErrorClass, c store.ErrorClass) bool {
	if len(classes) == 0 {
		return true
	}
	for _, x := range classes {
		if x == c {
			return true
		}
	}
	return false
}

func (r *deliveryRepo) ListByCampaign(_ context.Context, campaignID string, f store.DeliveryFilter, p store.Page) (store.Result[store.Delivery], error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return store.Result[store.Delivery]{}, err
	}
	return paginate(r.s.d.deliveries, p, func(d *store.Delivery) bool {
		return d.CampaignID == campaignID &&
			matchStatus(f.Statuses, d.Status) &&
			matchClass(f.ErrorClasses, d.LastErrorClass) &&
			(f.EmailNorm == "" || d.EmailNorm == f.EmailNorm)
	}), nil
}

func (r *deliveryRepo) Claim(_ context.Context, req store.ClaimRequest) ([]store.Delivery, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	var campaigns map[string]bool
	if req.CampaignIDs != nil {
		campaigns = make(map[string]bool, len(req.CampaignIDs))
		for _, id := range req.CampaignIDs {
			campaigns[id] = true
		}
	}
	cands := make([]*store.Delivery, 0, 64)
	for _, d := range r.s.d.deliveries {
		if d.Lane != req.Lane {
			continue
		}
		if d.Status != store.DeliveryQueued && d.Status != store.DeliveryDeferred {
			continue
		}
		if d.NextAttemptAt.After(req.Now) {
			continue
		}
		if campaigns != nil && d.CampaignID != "" && !campaigns[d.CampaignID] {
			continue
		}
		cands = append(cands, d)
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if !a.NextAttemptAt.Equal(b.NextAttemptAt) {
			return a.NextAttemptAt.Before(b.NextAttemptAt)
		}
		return a.ID < b.ID
	})
	if req.Limit > 0 && len(cands) > req.Limit {
		cands = cands[:req.Limit]
	}
	out := make([]store.Delivery, 0, len(cands))
	for _, d := range cands {
		d.Status = store.DeliveryLeased
		d.LeaseOwner = req.WorkerID
		d.LeaseUntil = req.Now.Add(req.LeaseFor)
		d.UpdatedAt = req.Now
		out = append(out, *d)
	}
	return out, nil
}

func (r *deliveryRepo) Complete(_ context.Context, results []store.DeliveryResult) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	now := r.s.p.now()
	for _, res := range results {
		d, ok := r.s.d.deliveries[res.DeliveryID]
		if !ok || res.LeaseOwner == "" || d.LeaseOwner != res.LeaseOwner {
			continue // stale result: the lease moved on
		}
		d.Status = res.NewStatus
		d.LastErrorClass = res.ErrorClass
		d.LastSMTPCode = res.SMTPCode
		d.LastError = res.Error
		if res.MessageID != "" {
			d.MessageID = res.MessageID
		}
		if res.IncrementAttempt {
			d.AttemptCount++
		}
		if !res.NextAttemptAt.IsZero() {
			d.NextAttemptAt = res.NextAttemptAt
		}
		if res.NewStatus == store.DeliverySent && d.SentAt.IsZero() {
			d.SentAt = now
		}
		if res.NewStatus.Terminal() {
			d.FinishedAt = now
		}
		d.LeaseOwner, d.LeaseUntil = "", time.Time{}
		d.UpdatedAt = now

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
			r.s.d.attempts[a.ID] = &a
		}
	}
	return nil
}

func (r *deliveryRepo) MarkSent(_ context.Context, id, owner, messageID string, at time.Time) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	d, ok := r.s.d.deliveries[id]
	if !ok {
		return fmt.Errorf("%w: delivery %s", store.ErrNotFound, id)
	}
	if d.Status != store.DeliveryLeased || owner == "" || d.LeaseOwner != owner {
		return fmt.Errorf("%w: delivery %s", store.ErrLeaseLost, id)
	}
	d.Status = store.DeliverySent
	d.MessageID = messageID
	d.SentAt = at
	d.FinishedAt = at
	d.UpdatedAt = at
	// The lease is kept so the batched Complete can still attach the attempt.
	return nil
}

func (r *deliveryRepo) ReleaseExpiredLeases(_ context.Context, now time.Time, limit int) (int, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return 0, err
	}
	ids := make([]string, 0, 16)
	for id, d := range r.s.d.deliveries {
		if d.Status != store.DeliveryLeased || d.LeaseUntil.IsZero() || !d.LeaseUntil.Before(now) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	for _, id := range ids {
		d := r.s.d.deliveries[id]
		if d.AttemptCount > 0 {
			d.Status = store.DeliveryDeferred
		} else {
			d.Status = store.DeliveryQueued
		}
		d.LeaseOwner, d.LeaseUntil = "", time.Time{}
		d.NextAttemptAt = now
		d.UpdatedAt = now
	}
	return len(ids), nil
}

func (r *deliveryRepo) CountByStatus(_ context.Context, campaignID string) (map[store.DeliveryStatus]int64, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return nil, err
	}
	out := map[store.DeliveryStatus]int64{}
	for _, d := range r.s.d.deliveries {
		if d.CampaignID != campaignID {
			continue
		}
		out[d.Status]++
	}
	return out, nil
}

func (r *deliveryRepo) BulkTransition(_ context.Context, campaignID string, from []store.DeliveryStatus, to store.DeliveryStatus, limit int) (int, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return 0, err
	}
	ids := make([]string, 0, 16)
	for id, d := range r.s.d.deliveries {
		if d.CampaignID != campaignID || !matchStatus(from, d.Status) || d.Status == to {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	now := r.s.p.now()
	for _, id := range ids {
		d := r.s.d.deliveries[id]
		d.Status = to
		if to.Terminal() {
			d.FinishedAt = now
		}
		d.UpdatedAt = now
	}
	return len(ids), nil
}

func (r *deliveryRepo) Requeue(_ context.Context, f store.RetryFilter, limit int) (int, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return 0, err
	}
	var only map[string]bool
	if f.DeliveryIDs != nil {
		only = make(map[string]bool, len(f.DeliveryIDs))
		for _, id := range f.DeliveryIDs {
			only[id] = true
		}
	}
	ids := make([]string, 0, 16)
	for id, d := range r.s.d.deliveries {
		if d.CampaignID != f.CampaignID || (only != nil && !only[id]) {
			continue
		}
		if !matchStatus(f.Statuses, d.Status) || !matchClass(f.ErrorClasses, d.LastErrorClass) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	for _, id := range ids {
		d := r.s.d.deliveries[id]
		d.Status = store.DeliveryQueued
		d.RetryGen++
		d.NextAttemptAt = f.Now
		d.LeaseOwner, d.LeaseUntil = "", time.Time{}
		d.FinishedAt = time.Time{}
		d.UpdatedAt = f.Now
	}
	return len(ids), nil
}

func (r *deliveryRepo) setFirst(id string, at time.Time, field func(*store.Delivery) *time.Time) (bool, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return false, err
	}
	d, ok := r.s.d.deliveries[id]
	if !ok {
		return false, nil // retention already removed it; drop the record
	}
	p := field(d)
	if !p.IsZero() {
		return false, nil
	}
	*p = at
	d.UpdatedAt = at
	return true, nil
}

func (r *deliveryRepo) SetFirstOpened(_ context.Context, id string, at time.Time) (bool, error) {
	return r.setFirst(id, at, func(d *store.Delivery) *time.Time { return &d.FirstOpenedAt })
}

func (r *deliveryRepo) SetFirstClicked(_ context.Context, id string, at time.Time) (bool, error) {
	return r.setFirst(id, at, func(d *store.Delivery) *time.Time { return &d.FirstClickedAt })
}

func (r *deliveryRepo) SetUnsubscribed(_ context.Context, id string, at time.Time) (bool, error) {
	return r.setFirst(id, at, func(d *store.Delivery) *time.Time { return &d.UnsubscribedAt })
}

func (r *deliveryRepo) DeleteBefore(_ context.Context, campaignID string, before time.Time, limit int) (int, error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return 0, err
	}
	ids := make([]string, 0, 16)
	for id, d := range r.s.d.deliveries {
		if d.CampaignID != campaignID || !d.CreatedAt.Before(before) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	for _, id := range ids {
		d := r.s.d.deliveries[id]
		if d.CampaignID != "" {
			delete(r.s.d.campaignEmails, emailKey(d.CampaignID, d.EmailNorm))
		}
		delete(r.s.d.deliveries, id)
		for aid, a := range r.s.d.attempts {
			if a.DeliveryID == id {
				delete(r.s.d.attempts, aid)
			}
		}
	}
	return len(ids), nil
}

type attemptRepo struct{ s *tenantStore }

func (r *attemptRepo) Insert(_ context.Context, as []store.DeliveryAttempt) error {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return err
	}
	now := r.s.p.now()
	for i := range as {
		a := as[i]
		if a.ID == "" {
			a.ID = store.NewID()
		}
		a.TenantID = r.s.tenant
		if a.CreatedAt.IsZero() {
			a.CreatedAt = now
		}
		r.s.d.attempts[a.ID] = &a
	}
	return nil
}

func (r *attemptRepo) ListByDelivery(_ context.Context, deliveryID string, p store.Page) (store.Result[store.DeliveryAttempt], error) {
	r.s.p.mu.Lock()
	defer r.s.p.mu.Unlock()
	if err := r.s.p.check(); err != nil {
		return store.Result[store.DeliveryAttempt]{}, err
	}
	return paginate(r.s.d.attempts, p, func(a *store.DeliveryAttempt) bool {
		return a.DeliveryID == deliveryID
	}), nil
}
