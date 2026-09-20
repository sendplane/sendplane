package store

import (
	"context"
	"time"
)

// MessageVersion is an immutable publish snapshot: layout merged, MJML already
// compiled, i18n bundles merged. A running campaign keeps using its own
// version even if the template changes afterwards (architecture 6.3).
type MessageVersion struct {
	ID         string
	TenantID   string
	TemplateID string
	LayoutID   string

	SubjectTpl string
	HTMLTpl    string
	TextTpl    string

	I18n          I18nBundle
	DefaultLocale string

	// Links are the trackable hrefs found in HTMLTpl, in document order; the
	// index is the link_no carried by click tokens (architecture 9.2).
	Links []string
	// Checksum identifies identical publishes.
	Checksum string

	CreatedAt time.Time
}

// MessageVersionRepo has no Update or Delete: versions are immutable and are
// removed only by retention.
type MessageVersionRepo interface {
	Create(ctx context.Context, v *MessageVersion) error
	Get(ctx context.Context, id string) (*MessageVersion, error)
	ListByTemplate(ctx context.Context, templateID string, p Page) (Result[MessageVersion], error)
}
