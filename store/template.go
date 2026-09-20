package store

import (
	"context"
	"encoding/json"
	"time"
)

// ContentMode is how a template body is authored (architecture 6.3).
type ContentMode string

const (
	// ContentBlocks is the GrapesJS-MJML block editor: Blocks holds the editor
	// project JSON, Body holds the MJML it exported.
	ContentBlocks ContentMode = "blocks"
	ContentMJML   ContentMode = "mjml"
	ContentHTML   ContentMode = "html"
)

// I18nBundle is the translation table of a layout or template. Values are
// Liquid, so a translation may itself interpolate variables.
type I18nBundle struct {
	DefaultLocale string
	// Locales maps a locale tag ("ko", "ko-KR") to key/value pairs. An empty
	// map for a tag means "fall back to the language".
	Locales map[string]map[string]string
}

// Template is the editable source of a message. Sending never uses it
// directly: publishing freezes it into a MessageVersion.
type Template struct {
	ID       string
	TenantID string
	Name     string
	LayoutID string

	Subject   string
	Preheader string
	Mode      ContentMode
	Body      string
	// Blocks is the block editor project, kept so editing can resume. The
	// server only ever compiles Body.
	Blocks json.RawMessage
	// Text is the optional plain-text part. Empty means "derive from HTML".
	Text string

	I18n               I18nBundle
	DefaultLocale      string
	PublishedVersionID string

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type TemplateRepo interface {
	Create(ctx context.Context, t *Template) error
	Get(ctx context.Context, id string) (*Template, error)
	Update(ctx context.Context, t *Template) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[Template], error)
}
