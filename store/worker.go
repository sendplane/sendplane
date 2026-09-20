package store

import (
	"context"
	"time"
)

// The roles a worker row may carry. They are the process roles of
// docs/architecture.md 2 (--roles=control,sender,bounce) and are constants so
// that the writer of a heartbeat and the reader counting replicas cannot drift
// apart over a typo: the sender divides the cluster-wide transport rate by the
// number of rows whose role is WorkerRoleSender (architecture 8.2).
const (
	WorkerRoleSender  = "sender"
	WorkerRoleControl = "control"
	WorkerRoleBounce  = "bounce"
)

// Worker is a sender replica's heartbeat. The active worker count is how the
// cluster-wide transport rate is divided without a central lock
// (architecture 8.2).
type Worker struct {
	ID       string
	TenantID string
	Role     string // WorkerRoleSender | WorkerRoleControl | WorkerRoleBounce
	Lanes    []Lane
	// Concurrency is this replica's worker pool size.
	Concurrency int

	StartedAt  time.Time
	LastSeenAt time.Time
}

type WorkerRepo interface {
	// Heartbeat inserts or refreshes the worker row.
	Heartbeat(ctx context.Context, w Worker) error
	// ListActive returns workers seen at or after since.
	ListActive(ctx context.Context, since time.Time) ([]Worker, error)
}
