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
