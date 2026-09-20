package store

import (
	"context"
	"encoding/json"
	"time"
)

// BounceSource records which of the three correlation paths matched
// (architecture 10).
type BounceSource string

const (
	BounceSourceVERP      BounceSource = "verp"
	BounceSourceHeader    BounceSource = "header"
	BounceSourceMessageID BounceSource = "message_id"
	BounceSourceHeuristic BounceSource = "heuristic"
)

// BounceEvent is one parsed DSN (RFC 3464) or feedback report (RFC 5965).
type BounceEvent struct {
	ID       string
	TenantID string
	// DeliveryID is empty when correlation failed.
	DeliveryID string

	Type   BounceType
	Source BounceSource
	// Verified is false when the VERP HMAC did not match; such events are
	// recorded but never change a delivery.
	Verified bool

	Recipient      string
	EmailNorm      string
	SMTPStatus     string
	DiagnosticCode string
	MessageID      string

	// Raw is the original message, kept only when the tenant opted in.
	Raw json.RawMessage

	ReceivedAt time.Time
	CreatedAt  time.Time
}

// BounceRepo stores immutable events.
type BounceRepo interface {
	Create(ctx context.Context, b *BounceEvent) error
	Get(ctx context.Context, id string) (*BounceEvent, error)
	List(ctx context.Context, p Page) (Result[BounceEvent], error)
	ListByDelivery(ctx context.Context, deliveryID string, p Page) (Result[BounceEvent], error)
	// DeleteBefore removes events created before the cutoff, at most limit of
	// them, and returns how many it deleted. Bounce events fall under the
	// tenant's retention period like every other per-recipient row
	// (architecture 16). A limit <= 0 means "every match".
	DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error)
}
