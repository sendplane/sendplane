package store

import (
	"context"
	"time"
)

// Layout wraps template bodies: MJML or HTML containing a {{ content }} slot,
// with its own i18n bundle that is merged into the template's at publish time
// (template keys win).
type Layout struct {
	ID       string
	TenantID string
	Name     string

	// Key and Shared follow the template rules (ADR-0018): an optional,
	// per-tenant unique identifier, and a system-tenant flag that makes the
	// layout readable by every tenant. A shared template's layout must be
	// shared, so that the template renders the same everywhere.
	Key    string
	Shared bool

	Mode ContentMode // mjml | html
	Body string
	I18n I18nBundle

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time

	// Overridden is computed by the platform overlay on a tenant's read and
	// never persisted: the tenant's own layout shadows a shared one with the
	// same Key.
	Overridden bool
}

// LayoutRepo is the layout aggregate. GetByKey finds a layout by its Key and
// is ErrNotFound for an empty key.
type LayoutRepo interface {
	Create(ctx context.Context, l *Layout) error
	Get(ctx context.Context, id string) (*Layout, error)
	GetByKey(ctx context.Context, key string) (*Layout, error)
	Update(ctx context.Context, l *Layout) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[Layout], error)
}
