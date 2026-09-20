package control

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sendplane/sendplane/store"
)

// --- lease reaper ------------------------------------------------------

// leaseReaper returns deliveries whose sender died mid-flight to the queue.
// It is the other half of the at-least-once contract of ADR-0002: Claim takes
// a lease, and if nothing ever commits it, this is what makes the work
// claimable again.
type leaseReaper struct {
	st    store.Store
	log   *slog.Logger
	limit int
}

func (r *leaseReaper) Tick(ctx context.Context, now time.Time) error {
	n, err := r.st.Deliveries().ReleaseExpiredLeases(ctx, now, r.limit)
	if err != nil {
		return err
	}
	if n > 0 {
		r.log.Info("control: released expired leases", "count", n)
	}
	return nil
}

// --- retention ---------------------------------------------------------

// retention deletes the delivery rows of campaigns that finished longer ago
// than the tenant's RetentionDays. ADR-0003 accepted one row per recipient, so
// deleting them again is not optional housekeeping but a required operational
// feature (architecture 16).
//
// Only campaign deliveries are covered. Transactional deliveries (the ones
// with no campaign) have no "finished" campaign to key off and are left alone;
// see the package README.
type retention struct {
	st  store.Store
	log *slog.Logger
	cfg *config
}

func (r *retention) Tick(ctx context.Context, now time.Time) error {
	settings, err := r.st.TenantSettings().Get(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // the tenant was never configured; nothing to enforce
		}
		return err
	}
	if settings.RetentionDays <= 0 {
		return nil // retention disabled for this tenant
	}
	cutoff := now.AddDate(0, 0, -settings.RetentionDays)

	return eachCampaign(ctx, r.st,
		[]store.CampaignStatus{store.CampaignCompleted, store.CampaignCancelled},
		r.cfg.batches.CampaignPage,
		func(c store.Campaign) error {
			finishedAt := c.CompletedAt
			if finishedAt.IsZero() {
				finishedAt = c.UpdatedAt
			}
			if !finishedAt.Before(cutoff) {
				return nil
			}
			deleted := 0
			for i := 0; i < r.cfg.batches.RetentionMaxChunks; i++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				n, err := r.st.Deliveries().DeleteBefore(ctx, c.ID, cutoff, r.cfg.batches.RetentionChunk)
				if err != nil {
					return err
				}
				deleted += n
				if n < r.cfg.batches.RetentionChunk {
					break
				}
			}
			if deleted > 0 {
				r.log.Info("control: retention deleted deliveries",
					"campaign", c.ID, "count", deleted, "before", cutoff)
			}
			return nil
		})
}
