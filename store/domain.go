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
