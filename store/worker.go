package store

import (
	"context"
	"time"
)

// Worker is a sender replica's heartbeat. The active worker count is how the
// cluster-wide transport rate is divided without a central lock
// (architecture 8.2).
type Worker struct {
	ID       string
	TenantID string
	Role     string // sender | control | bounce
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
