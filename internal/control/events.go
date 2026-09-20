package control

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/store"
)

// The campaign event types control emits. They are the subset of
// docs/architecture.md 12 that campaign transitions produce. There is no
// campaign.resumed type, so a resume emits campaign.started again.
const (
	EventCampaignStarted   = "campaign.started"
	EventCampaignPaused    = "campaign.paused"
	EventCampaignCompleted = "campaign.completed"
	EventCampaignCancelled = "campaign.cancelled"
)

// campaignEventPayload is the JSON body of a campaign.* outbox event.
// ByStatus is only filled for campaign.completed, where it is the aggregate
// the finalizer just cached.
type campaignEventPayload struct {
	CampaignID string               `json:"campaign_id"`
	Name       string               `json:"name,omitempty"`
	Status     store.CampaignStatus `json:"status"`
	VersionID  string               `json:"version_id,omitempty"`
	SenderID   string               `json:"sender_id,omitempty"`
	OccurredAt time.Time            `json:"occurred_at"`

	ByStatus map[store.DeliveryStatus]int64 `json:"by_status,omitempty"`
}

// enqueueCampaignEvent writes one campaign.* event to the tenant's outbox. It
// is called right after the transition it describes was committed, in the same
// store: that is the outbox pattern of architecture 12, and it is why control
// never needs a transaction spanning two repositories.
func enqueueCampaignEvent(ctx context.Context, st store.Store, c *store.Campaign, typ string, now time.Time, byStatus map[store.DeliveryStatus]int64) error {
	payload, err := json.Marshal(campaignEventPayload{
		CampaignID: c.ID,
		Name:       c.Name,
		Status:     c.Status,
		VersionID:  c.VersionID,
		SenderID:   c.SenderID,
		OccurredAt: now,
		ByStatus:   byStatus,
	})
	if err != nil {
		return err
	}
	return st.Outbox().Enqueue(ctx, []store.OutboxEvent{{
		Type:          typ,
		Payload:       payload,
		Status:        store.OutboxPending,
		CreatedAt:     now,
		NextAttemptAt: now,
	}})
}

// eventFromOutbox adapts a stored outbox row to the host-facing event. The
// outbox row's CreatedAt is the moment the transition happened, which is what
// the host wants as OccurredAt, not the moment dispatch got around to it.
func eventFromOutbox(ev store.OutboxEvent) sendplane.Event {
	return sendplane.Event{
		ID:         ev.ID,
		TenantID:   ev.TenantID,
		Type:       ev.Type,
		OccurredAt: ev.CreatedAt,
		Payload:    ev.Payload,
	}
}
