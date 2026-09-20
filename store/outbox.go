package store

import (
	"context"
	"encoding/json"
	"time"
)

// OutboxStatus is the dispatch state of an event.
type OutboxStatus string

const (
	OutboxPending   OutboxStatus = "pending"
	OutboxDelivered OutboxStatus = "delivered"
	// OutboxFailed is the dead letter state: the host can list and resend.
	OutboxFailed OutboxStatus = "failed"
)

// OutboxEvent is an event queued for the host (architecture 12). It is written
// in the same store as the state transition it describes, which is how
// sendplane gets outbox semantics without cross-repository transactions.
type OutboxEvent struct {
	ID       string
	TenantID string

	Type    string
	Payload json.RawMessage

	Status        OutboxStatus
	Attempts      int
	NextAttemptAt time.Time
	LeaseOwner    string
	LeaseUntil    time.Time
	LastError     string

	CreatedAt   time.Time
	DeliveredAt time.Time
}

type OutboxRepo interface {
	Enqueue(ctx context.Context, evs []OutboxEvent) error
	// ClaimPending leases due pending events for one dispatcher.
	ClaimPending(ctx context.Context, limit int, lease time.Duration, owner string, now time.Time) ([]OutboxEvent, error)
	MarkDelivered(ctx context.Context, id string, at time.Time) error
	// MarkFailed schedules a retry; an empty nextAttempt moves the event to
	// the dead letter state.
	MarkFailed(ctx context.Context, id string, nextAttempt time.Time, errMsg string) error
	List(ctx context.Context, status OutboxStatus, p Page) (Result[OutboxEvent], error)
}
