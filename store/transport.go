package store

import (
	"context"
	"time"
)

// TLSMode is how a transport negotiates TLS.
type TLSMode string

const (
	TLSNone     TLSMode = "none"
	TLSSTARTTLS TLSMode = "starttls"
	TLSImplicit TLSMode = "tls"
)

// Transport is an SMTP account.
type Transport struct {
	ID       string
	TenantID string
	Name     string

	Host     string
	Port     int
	TLS      TLSMode
	Username string
	// Password is encrypted at rest by the host's SecretCipher.
	Password []byte

	// MaxConns is the connection pool size per sender replica.
	MaxConns int
	// RatePerSecond is the cluster-wide target rate, divided across active
	// sender replicas (architecture 8.2). Zero means unlimited.
	RatePerSecond float64
	// DomainRatePerSecond caps the rate towards specific recipient domains,
	// keyed by the recipient domain ("gmail.com").
	DomainRatePerSecond map[string]float64

	Status          TransportStatus
	StatusReason    string
	StatusChangedAt time.Time

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type TransportRepo interface {
	Create(ctx context.Context, t *Transport) error
	Get(ctx context.Context, id string) (*Transport, error)
	Update(ctx context.Context, t *Transport) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[Transport], error)
}
