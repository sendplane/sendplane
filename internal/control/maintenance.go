package control

import (
	"context"
	"fmt"
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

// retention enforces the tenant's RetentionDays on every per-recipient row:
// the deliveries of campaigns that finished long enough ago, plus the tracking
// events (architecture 9.4), bounce events and dispatched outbox rows of the
// whole tenant (architecture 16). ADR-0003 accepted one row per recipient, so
// deleting them again is a required operational feature, not housekeeping.
//
// Only campaign deliveries are covered. Transactional deliveries (the ones
// with no campaign) have no "finished" campaign to key off and are left alone;
// see the package README.
type retention struct {
	st     store.Store
	tenant string
	log    *slog.Logger
	cfg    *config
}

func (r *retention) Tick(ctx context.Context, now time.Time) error {
	settings, err := store.LoadTenantSettings(ctx, r.st, r.tenant, now)
	if err != nil {
		return err
	}
	if settings.RetentionDays <= 0 {
		return nil // retention disabled for this tenant
	}
	cutoff := now.AddDate(0, 0, -settings.RetentionDays)

	if err := r.deleteCampaignDeliveries(ctx, cutoff); err != nil {
		return err
	}
	return r.deleteTenantWide(ctx, cutoff)
}

// deleteTenantWide drops the rows whose retention does not depend on a
// campaign: tracking and bounce events, and outbox rows that were already
// dispatched. Each is chunked the same way the delivery walk is, so one tick
// never holds a long delete open.
func (r *retention) deleteTenantWide(ctx context.Context, cutoff time.Time) error {
	for _, target := range []struct {
		name   string
		delete func(context.Context, time.Time, int) (int, error)
	}{
		{"tracking events", r.st.Tracking().DeleteBefore},
		{"bounce events", r.st.Bounces().DeleteBefore},
		{"outbox events", r.st.Outbox().DeleteBefore},
	} {
		deleted, err := chunkedDelete(ctx, target.delete, cutoff,
			r.cfg.batches.RetentionChunk, r.cfg.batches.RetentionMaxChunks)
		if err != nil {
			return fmt.Errorf("retention: %s: %w", target.name, err)
		}
		if deleted > 0 {
			r.log.Info("control: retention deleted rows",
				"what", target.name, "count", deleted, "before", cutoff)
		}
	}
	return nil
}

// chunkedDelete calls one DeleteBefore repeatedly until it comes back short or
// the tick's budget runs out; whatever is left waits for the next tick.
func chunkedDelete(ctx context.Context, del func(context.Context, time.Time, int) (int, error),
	cutoff time.Time, chunk, maxChunks int) (int, error) {
	total := 0
	for i := 0; i < maxChunks; i++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := del(ctx, cutoff, chunk)
		total += n
		if err != nil {
			return total, err
		}
		if n < chunk {
			break
		}
	}
	return total, nil
}

func (r *retention) deleteCampaignDeliveries(ctx context.Context, cutoff time.Time) error {
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
			deleted, err := chunkedDelete(ctx, func(ctx context.Context, before time.Time, limit int) (int, error) {
				return r.st.Deliveries().DeleteBefore(ctx, c.ID, before, limit)
			}, cutoff, r.cfg.batches.RetentionChunk, r.cfg.batches.RetentionMaxChunks)
			if err != nil {
				return err
			}
			if deleted > 0 {
				r.log.Info("control: retention deleted deliveries",
					"campaign", c.ID, "count", deleted, "before", cutoff)
			}
			return nil
		})
}
