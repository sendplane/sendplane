package store

import (
	"context"
	"time"
)

// Sender is a From identity bound to a transport and a sending domain.
type Sender struct {
	ID       string
	TenantID string
	Name     string

	FromName    string
	FromEmail   string
	ReplyTo     string
	TransportID string
	DomainID    string

	// Shared is true for a virtual platform entity resolved from configuration
	// rather than read from a row, and for the system tenant's state shadow row
	// of one (store/platform.go). Nothing tenant-facing may write it: it is set
	// by the platform overlay, and a tenant's own row always has it false.
	Shared bool

	// Health is the latest loopback probe verdict (architecture 11.4).
	Health          HealthStatus
	HealthReason    string
	HealthCheckedAt time.Time

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type SenderRepo interface {
	Create(ctx context.Context, s *Sender) error
	Get(ctx context.Context, id string) (*Sender, error)
	Update(ctx context.Context, s *Sender) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[Sender], error)
}
