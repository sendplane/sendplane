package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sendplane/sendplane/store"
)

// maxRetryChunks bounds one RetryCampaign call. With the default chunk size
// that is ten million deliveries, the documented ceiling for one campaign
// (sendplane.DefaultLimits.MaxRecipientsPerCampaign).
const maxRetryChunks = 1000

// defaultRetryStatuses is what a retry means when the caller names no status.
// It is deliberately not "everything": an empty status filter would also match
// the rows the same call just requeued, and each pass would bump their
// retry generation again.
var defaultRetryStatuses = []store.DeliveryStatus{store.DeliveryFailed}

// StartCampaign moves a draft campaign to scheduled or running (architecture
// 7.1). A zero or past `at` starts now; a future one records the schedule and
// leaves the promotion to the scheduler loop.
//
// The preconditions are checked here rather than in the HTTP layer because
// they are the campaign invariants, not request validation: a campaign with no
// recipients or no sender would go running and never finish.
func (c *Control) StartCampaign(ctx context.Context, st store.Store, id string, at time.Time) error {
	now := c.now()
	cam, err := st.Campaigns().Get(ctx, id)
	if err != nil {
		return err
	}
	if cam.Status != store.CampaignDraft {
		return invalidTransition(cam, "start")
	}
	if cam.VersionID == "" && cam.TemplateID == "" {
		return fmt.Errorf("%w: campaign %s", ErrNoVersion, id)
	}
	if cam.SenderID == "" {
		return fmt.Errorf("%w: campaign %s names no sender", ErrNoSender, id)
	}
	if _, err := st.Senders().Get(ctx, cam.SenderID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: sender %s does not exist", ErrNoSender, cam.SenderID)
		}
		return err
	}
	counts, err := st.Deliveries().CountByStatus(ctx, id)
	if err != nil {
		return err
	}
	if total, _ := splitCounts(counts); total == 0 {
		return fmt.Errorf("%w: campaign %s", ErrNoRecipients, id)
	}

	at = store.TruncateTime(at)
	if !at.IsZero() && at.After(now) {
		// A scheduled campaign is pinned when the scheduler promotes it, not
		// now: "the published version at start time" is the version that is
		// published when the mail actually goes out.
		cam.Status = store.CampaignScheduled
		cam.ScheduleAt = at
	} else {
		if err := resolveCampaignVersion(ctx, st, cam); err != nil {
			return err
		}
		cam.Status = store.CampaignRunning
		cam.StartedAt = now
	}
	if err := st.Campaigns().Update(ctx, cam); err != nil {
		return err
	}
	if cam.Status == store.CampaignRunning {
		return enqueueCampaignEvent(ctx, st, cam, EventCampaignStarted, now, nil)
	}
	return nil
}

// resolveCampaignVersion binds a campaign that named a template to that
// template's currently published version. A campaign may be created from a
// template that has never been published; the binding happens here, at start,
// so a template edited and republished in between is the one that goes out
// (architecture 7.1). A campaign that pinned VersionID itself is left alone.
func resolveCampaignVersion(ctx context.Context, st store.Store, cam *store.Campaign) error {
	if cam.VersionID != "" {
		return nil
	}
	if cam.TemplateID == "" {
		return fmt.Errorf("%w: campaign %s", ErrNoVersion, cam.ID)
	}
	tpl, err := st.Templates().Get(ctx, cam.TemplateID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: template %s does not exist", ErrNoVersion, cam.TemplateID)
		}
		return err
	}
	if tpl.PublishedVersionID == "" {
		return fmt.Errorf("%w: template %s has never been published", ErrNoVersion, tpl.ID)
	}
	cam.VersionID = tpl.PublishedVersionID
	return nil
}

// PauseCampaign takes a running campaign out of the sender's running set. It
// is one row write: the delivery rows are not touched (ADR-0002), so a paused
// campaign simply stops being claimable.
func (c *Control) PauseCampaign(ctx context.Context, st store.Store, id string) error {
	now := c.now()
	cam, err := st.Campaigns().Get(ctx, id)
	if err != nil {
		return err
	}
	if cam.Status != store.CampaignRunning {
		return invalidTransition(cam, "pause")
	}
	cam.Status = store.CampaignPaused
	if err := st.Campaigns().Update(ctx, cam); err != nil {
		return err
	}
	return enqueueCampaignEvent(ctx, st, cam, EventCampaignPaused, now, nil)
}

// ResumeCampaign puts a paused campaign back into the running set. There is no
// campaign.resumed event type in architecture 12, so it emits campaign.started.
func (c *Control) ResumeCampaign(ctx context.Context, st store.Store, id string) error {
	now := c.now()
	cam, err := st.Campaigns().Get(ctx, id)
	if err != nil {
		return err
	}
	if cam.Status != store.CampaignPaused {
		return invalidTransition(cam, "resume")
	}
	cam.Status = store.CampaignRunning
	if cam.StartedAt.IsZero() {
		cam.StartedAt = now
	}
	if err := st.Campaigns().Update(ctx, cam); err != nil {
		return err
	}
	return enqueueCampaignEvent(ctx, st, cam, EventCampaignStarted, now, nil)
}

// CancelCampaign marks the campaign cancelled and returns immediately. Moving
// the delivery rows is the canceller loop's job, in chunks: a cancel must not
// block an HTTP request on a million-row UPDATE (architecture 7.2).
//
// CompletedAt is set because for a cancelled campaign that is when it stopped,
// which is what retention keys off.
func (c *Control) CancelCampaign(ctx context.Context, st store.Store, id string) error {
	now := c.now()
	cam, err := st.Campaigns().Get(ctx, id)
	if err != nil {
		return err
	}
	switch cam.Status {
	case store.CampaignDraft, store.CampaignScheduled, store.CampaignRunning, store.CampaignPaused:
	default:
		return invalidTransition(cam, "cancel")
	}
	cam.Status = store.CampaignCancelled
	cam.CompletedAt = now
	if err := st.Campaigns().Update(ctx, cam); err != nil {
		return err
	}
	return enqueueCampaignEvent(ctx, st, cam, EventCampaignCancelled, now, nil)
}

// RetryCampaign requeues the deliveries a filter selects and returns how many
// moved. The retry generation is bumped rather than the attempt count reset,
// so the earlier attempts stay in the history (ADR-0003).
//
// A completed campaign goes back to running, because a queued delivery of a
// campaign outside the sender's running set would never be claimed.
func (c *Control) RetryCampaign(ctx context.Context, st store.Store, id string, filter store.RetryFilter) (int, error) {
	now := c.now()
	cam, err := st.Campaigns().Get(ctx, id)
	if err != nil {
		return 0, err
	}
	switch cam.Status {
	case store.CampaignRunning, store.CampaignPaused, store.CampaignCompleted:
	default:
		// draft and scheduled have nothing to retry; cancelled is terminal.
		return 0, invalidTransition(cam, "retry")
	}

	filter.CampaignID = id
	if filter.Now.IsZero() {
		filter.Now = now
	}
	if len(filter.Statuses) == 0 {
		filter.Statuses = defaultRetryStatuses
	}
	if filter.DeliveryIDs != nil && len(filter.DeliveryIDs) == 0 {
		return 0, nil // an explicitly empty selection matches nothing
	}

	chunk := c.cfg.batches.RetryChunk
	total := 0
	for i := 0; i < maxRetryChunks; i++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := st.Deliveries().Requeue(ctx, filter, chunk)
		total += n
		if err != nil {
			return total, err
		}
		if n < chunk {
			break
		}
	}
	if total == 0 || cam.Status != store.CampaignCompleted {
		return total, nil
	}

	cam.Status = store.CampaignRunning
	cam.CompletedAt = time.Time{}
	if err := st.Campaigns().Update(ctx, cam); err != nil {
		return total, err
	}
	return total, enqueueCampaignEvent(ctx, st, cam, EventCampaignStarted, now, nil)
}
