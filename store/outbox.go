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
	// DeleteBefore removes dispatched events created before the cutoff, at
	// most limit of them, and returns how many it deleted.
	//
	// Only delivered and failed rows are eligible. A pending row is still
	// owed to the host however old it is, and a failed one is the dead letter
	// the host lists and replays, so retention is the only thing that ever
	// removes it. A limit <= 0 means "every match".
	DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error)
}
