package store

import (
	"context"
	"time"
)

// TrackingEvent is one open, click or unsubscribe interaction. Events are
// buffered and inserted in batches (architecture 9.3).
type TrackingEvent struct {
	ID         string
	TenantID   string
	DeliveryID string
	// CampaignID is denormalized so counting does not need the delivery row,
	// which retention may already have deleted.
	CampaignID string

	Kind TrackingKind
	// URL and LinkNo are set for clicks. LinkNo is the link's position in the
	// published MessageVersion; -1 for a link that is not in that list.
	URL    string
	LinkNo int

	// UserAgent is the summarized UA string, IPHash the optionally hashed
	// client IP. Raw IPs are not stored (architecture 9.4).
	UserAgent string
	IPHash    string
	// SuspectedBot events are kept but excluded from counts.
	SuspectedBot bool

	CreatedAt time.Time
}

// TrackingCounts are unique-per-delivery counts, bots excluded.
type TrackingCounts struct {
	UniqueOpens        int64
	UniqueClicks       int64
	UnsubscribeClicked int64
	Unsubscribed       int64
}

// LinkClick is one row of the per-link click report.
type LinkClick struct {
	LinkNo int
	URL    string
	// Clicks counts every click event, UniqueClicks counts distinct
	// deliveries.
	Clicks       int64
	UniqueClicks int64
}

type TrackingRepo interface {
	InsertEvents(ctx context.Context, evs []TrackingEvent) error
	// CountUnique counts distinct deliveries per kind for a campaign,
	// excluding suspected bots.
	CountUnique(ctx context.Context, campaignID string) (TrackingCounts, error)
	// LinkClicks reports clicks per link URL, ordered by LinkNo then URL.
	LinkClicks(ctx context.Context, campaignID string) ([]LinkClick, error)
}
