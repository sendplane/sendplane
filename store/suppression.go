package store

import (
	"context"
	"time"
)

// SuppressionReason is why an address is suppressed (ADR-0008).
type SuppressionReason string

const (
	SuppressionHardBounce SuppressionReason = "hard_bounce"
	SuppressionComplaint  SuppressionReason = "complaint"
	SuppressionManual     SuppressionReason = "manual"
)

// Suppression is the opt-in built-in do-not-send list. It is keyed by
// EmailNorm and holds nothing else about the recipient: it is not a contact
// database.
type Suppression struct {
	TenantID  string
	EmailNorm string

	Reason           SuppressionReason
	SourceDeliveryID string

	CreatedAt time.Time
	// ExpiresAt is zero for "never expires".
	ExpiresAt time.Time
}

type SuppressionRepo interface {
	// Upsert inserts or replaces the entry for s.EmailNorm.
	Upsert(ctx context.Context, s *Suppression) error
	// IsSuppressed reports whether the address is suppressed at now. An entry
	// whose ExpiresAt has passed is not suppressed.
	IsSuppressed(ctx context.Context, emailNorm string, now time.Time) (bool, *Suppression, error)
	List(ctx context.Context, p Page) (Result[Suppression], error)
	Delete(ctx context.Context, emailNorm string) error
	// DeleteBefore removes entries whose ExpiresAt is non-zero and at or
	// before the cutoff, at most limit of them, and returns how many it
	// deleted. It is what makes ADR-0008's retention period real: without it
	// an ExpiresAt is only read by IsSuppressed and the row lives forever.
	//
	// Entries with a zero ExpiresAt ("never expires") are never deleted,
	// whatever the cutoff. A limit <= 0 means "every match".
	DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error)
}
