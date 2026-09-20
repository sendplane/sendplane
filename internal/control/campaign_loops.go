package control

import (
	"context"
	"log/slog"
	"time"

	"github.com/sendplane/sendplane/store"
)

// --- scheduler ---------------------------------------------------------

// scheduler promotes scheduled campaigns whose time has come to running. A
// draft campaign is never touched: only an explicit StartCampaign moves it out
// of draft (architecture 7.1).
//
// Nothing else happens on start. The million delivery rows stay pending and
// the sender picks them up because the campaign entered its running set: a
// pending row is claimable exactly while its campaign is in the claimer's
// ClaimRequest.CampaignIDs (store.DeliveryRepo.Claim). That is why start and
// pause are one-row writes and ADR-0002 needed no bulk update.
type scheduler struct {
	st   store.Store
	log  *slog.Logger
	page int
}

func (s *scheduler) Tick(ctx context.Context, now time.Time) error {
	return eachCampaign(ctx, s.st, []store.CampaignStatus{store.CampaignScheduled}, s.page,
		func(c store.Campaign) error {
			if c.ScheduleAt.After(now) {
				return nil
			}
			// A campaign created from a template is bound to that template's
			// published version here, at the moment it actually starts. A
			// template still unpublished leaves the campaign scheduled so the
			// next tick can pick it up once somebody publishes it, rather than
			// starting a send with no version.
			if err := resolveCampaignVersion(ctx, s.st, &c); err != nil {
				s.log.Error("control: cannot start scheduled campaign",
					"campaign", c.ID, "err", err)
				return nil
			}
			c.Status = store.CampaignRunning
			if c.StartedAt.IsZero() {
				c.StartedAt = now
			}
			if err := s.st.Campaigns().Update(ctx, &c); err != nil {
				if raced(err) {
					return nil
				}
				return err
			}
			s.log.Info("control: campaign started", "campaign", c.ID, "scheduled_at", c.ScheduleAt)
			if err := enqueueCampaignEvent(ctx, s.st, &c, EventCampaignStarted, now, nil); err != nil {
				s.log.Error("control: cannot enqueue campaign.started", "campaign", c.ID, "err", err)
			}
			return nil
		})
}

// --- finalizer ---------------------------------------------------------

// finalizer recomputes the cached campaign stats and detects completion
// (architecture 7.3). Delivery rows never increment a counter on the campaign
// row: that hot-row update is exactly what ADR-0003 rejected, so the aggregate
// is recomputed here instead.
type finalizer struct {
	st  store.Store
	log *slog.Logger
	cfg *config

	// skips throttles large campaigns: counting a million rows every ten
	// seconds is the one query in this package that can hurt a real database,
	// so a campaign over cfg.largeCampaignRows is only recounted every
	// cfg.largeCampaignEvery ticks.
	skips map[string]int
}

func (f *finalizer) Tick(ctx context.Context, now time.Time) error {
	live := map[string]bool{}
	err := eachCampaign(ctx, f.st, []store.CampaignStatus{store.CampaignRunning}, f.cfg.batches.CampaignPage,
		func(c store.Campaign) error {
			live[c.ID] = true
			if f.skips[c.ID] > 0 {
				f.skips[c.ID]--
				return nil
			}
			counts, err := f.st.Deliveries().CountByStatus(ctx, c.ID)
			if err != nil {
				return err
			}
			total, inflight := splitCounts(counts)
			if total > f.cfg.largeCampaignRows && f.cfg.largeCampaignEvery > 1 {
				f.skips[c.ID] = f.cfg.largeCampaignEvery - 1
			} else {
				delete(f.skips, c.ID)
			}

			stats := store.CampaignStats{ByStatus: counts, ComputedAt: now}
			if tc, err := f.st.Tracking().CountUnique(ctx, c.ID); err != nil {
				// Tracking is a nice-to-have on the cache; the status counts
				// and the completion verdict are not, so do not fail the tick.
				f.log.Error("control: cannot count tracking uniques", "campaign", c.ID, "err", err)
			} else {
				stats.UniqueOpens = tc.UniqueOpens
				stats.UniqueClicks = tc.UniqueClicks
				stats.Unsubscribed = tc.Unsubscribed
				stats.UnsubscribeClicked = tc.UnsubscribeClicked
			}
			if err := f.st.Campaigns().UpdateStats(ctx, c.ID, stats); err != nil {
				if raced(err) {
					return nil
				}
				return err
			}

			// total == 0 is not completion: it is a campaign whose rows a
			// retention pass already removed, or one that raced ingest. Only
			// a campaign that had work and has none left is done.
			if total == 0 || inflight > 0 {
				return nil
			}
			c.Stats = stats
			c.Status = store.CampaignCompleted
			c.CompletedAt = now
			if err := f.st.Campaigns().Update(ctx, &c); err != nil {
				if raced(err) {
					return nil
				}
				return err
			}
			delete(f.skips, c.ID)
			f.log.Info("control: campaign completed", "campaign", c.ID, "deliveries", total)
			if err := enqueueCampaignEvent(ctx, f.st, &c, EventCampaignCompleted, now, counts); err != nil {
				f.log.Error("control: cannot enqueue campaign.completed", "campaign", c.ID, "err", err)
			}
			return nil
		})
	for id := range f.skips {
		if !live[id] {
			delete(f.skips, id)
		}
	}
	return err
}

// --- canceller ---------------------------------------------------------

// canceller finishes a cancel that the API only recorded on the campaign row.
// Cancelling a million recipients cannot be one UPDATE, so the rows are moved
// one chunk per tick until nothing matches (architecture 7.2).
//
// Leased deliveries are deliberately left alone: a sender already has them in
// flight, and racing its Complete would either lose the attempt record or
// resurrect a cancelled row. They finish naturally (architecture 4.1).
type canceller struct {
	st    store.Store
	log   *slog.Logger
	page  int
	chunk int
}

// cancellable are the statuses a cancel may still take away from a sender.
var cancellableStatuses = []store.DeliveryStatus{
	store.DeliveryPending, store.DeliveryQueued, store.DeliveryDeferred,
}

func (c *canceller) Tick(ctx context.Context, _ time.Time) error {
	return eachCampaign(ctx, c.st, []store.CampaignStatus{store.CampaignCancelled}, c.page,
		func(cam store.Campaign) error {
			n, err := c.st.Deliveries().BulkTransition(ctx, cam.ID,
				cancellableStatuses, store.DeliveryCancelled, c.chunk)
			if err != nil {
				return err
			}
			if n > 0 {
				c.log.Info("control: cancelled deliveries", "campaign", cam.ID, "count", n)
			}
			return nil
		})
}
