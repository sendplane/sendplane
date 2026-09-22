package store

import (
	"context"
	"time"
)

// SendingDomain is the domain mail is sent from: DKIM material, the
// return-path domain used for VERP, and the health derived from probe runs.
type SendingDomain struct {
	ID       string
	TenantID string
	Domain   string

	DKIMSelector string
	// DKIMPrivateKey is encrypted at rest by the host's SecretCipher. Empty
	// when the relay signs.
	DKIMPrivateKey []byte

	ReturnPathDomain string
	ExpectedSPF      string
	// OutboundIPs are observed from probe Received headers (architecture 11.3)
	// or configured.
	OutboundIPs []string

	// Shared is true for a virtual platform entity resolved from configuration
	// rather than read from a row, and for the system tenant's state shadow row
	// of one (store/platform.go). Nothing tenant-facing may write it: it is set
	// by the platform overlay, and a tenant's own row always has it false.
	Shared bool

	Health          HealthStatus
	HealthReason    string
	HealthCheckedAt time.Time

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type DomainRepo interface {
	Create(ctx context.Context, d *SendingDomain) error
	Get(ctx context.Context, id string) (*SendingDomain, error)
	Update(ctx context.Context, d *SendingDomain) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[SendingDomain], error)
}
