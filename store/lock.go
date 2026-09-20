package store

import (
	"context"
	"time"
)

// Lock is a named lease used for leader election and singleton jobs
// (scheduler, aggregation, one poller per bounce mailbox).
type Lock struct {
	TenantID   string
	Name       string
	Owner      string
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

type LockRepo interface {
	// Acquire takes the lock, or renews it if owner already holds it, or
	// takes it over if the existing lease expired at or before now. It
	// reports false when someone else holds a live lease.
	Acquire(ctx context.Context, name, owner string, ttl time.Duration, now time.Time) (bool, error)
	// Renew extends a lease the owner still holds; false if it no longer does.
	Renew(ctx context.Context, name, owner string, ttl time.Duration, now time.Time) (bool, error)
	// Release drops the lock if owner holds it, and is a no-op otherwise.
	Release(ctx context.Context, name, owner string) error
	Get(ctx context.Context, name string) (*Lock, error)
}
