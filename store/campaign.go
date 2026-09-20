package store

import (
	"context"
	"time"
)

// CampaignStats is the periodically recomputed aggregate cached on the
// campaign row. Delivery rows are never counted inline (ADR-0003).
type CampaignStats struct {
	ByStatus map[DeliveryStatus]int64

	UniqueOpens        int64
	UniqueClicks       int64
	Unsubscribed       int64
	UnsubscribeClicked int64

	ComputedAt time.Time
}

// Campaign is one bulk send of a MessageVersion.
type Campaign struct {
	ID       string
	TenantID string
	Name     string

	VersionID     string
	SenderID      string
	DefaultLocale string
	// Vars are campaign-wide template variables, merged under each
	// recipient's own vars.
	Vars map[string]any

	Status CampaignStatus
	// ScheduleAt is zero for "start now".
	ScheduleAt  time.Time
	StartedAt   time.Time
	CompletedAt time.Time

	Stats CampaignStats

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type CampaignRepo interface {
	Create(ctx context.Context, c *Campaign) error
	Get(ctx context.Context, id string) (*Campaign, error)
	Update(ctx context.Context, c *Campaign) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[Campaign], error)

	// ListByStatus feeds the sender's running-campaign set and the control
	// scheduler (ADR-0002).
	ListByStatus(ctx context.Context, statuses []CampaignStatus, p Page) (Result[Campaign], error)
	// UpdateStats refreshes the cached aggregate without taking part in
	// optimistic concurrency: the control loop must not fight API edits.
	UpdateStats(ctx context.Context, campaignID string, s CampaignStats) error
}

// ChunkState is the ingest state of a recipient chunk.
type ChunkState string

const (
	ChunkPending   ChunkState = "pending"
	ChunkCompleted ChunkState = "completed"
)

// RecipientChunk records one ingest call so that re-sending the same chunk is
// idempotent (architecture 7.2). Key is the Idempotency-Key header or, when
// absent, a hash of the body.
type RecipientChunk struct {
	TenantID   string
	CampaignID string
	Key        string

	State      ChunkState
	Accepted   int
	Duplicates int
	Invalid    int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RecipientChunkRepo is keyed by (campaign_id, key); Put inserts or replaces.
type RecipientChunkRepo interface {
	Get(ctx context.Context, campaignID, key string) (*RecipientChunk, error)
	Put(ctx context.Context, c *RecipientChunk) error
}
