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

	Mode ContentMode // mjml | html
	Body string
	I18n I18nBundle

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type LayoutRepo interface {
	Create(ctx context.Context, l *Layout) error
	Get(ctx context.Context, id string) (*Layout, error)
	Update(ctx context.Context, l *Layout) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[Layout], error)
}
