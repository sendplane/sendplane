package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

	// Key identifies the template across tenants (ADR-0018). It is optional:
	// a template without one cannot be shared, cannot be overridden and is
	// never found by key. When set it is unique within the tenant
	// (ValidContentKey).
	Key string
	// Shared makes a system-tenant template usable, read-only, by every
	// tenant. It requires a Key, and it means nothing outside the system
	// tenant: a tenant's own template is never shared.
	Shared bool
	// Uses restricts what a shared template may be used for (campaign,
	// transactional). Empty means everything. It is enforced by the template
	// policy (host.Hooks.TemplatePolicy), whose default applies it to shared
	// templates only.
	Uses []UseKind
	// OverriddenFromVersion is set on a tenant's override of a shared
	// template: the shared template's PublishedVersionID at the moment the
	// copy was made. It is how "the shared template changed since you
	// overrode it" is told without keeping any history.
	OverriddenFromVersion string

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

	// The fields below are computed by the platform overlay (WithPlatform)
	// on a normal tenant's read and are never persisted.

	// Overridden is true on a tenant's own template whose Key equals a
	// shared template's: the tenant's copy shadows the shared one.
	Overridden bool
	// SharedPublishedVersionID is, on an overriding template, the shared
	// template's current PublishedVersionID. Compared with
	// OverriddenFromVersion it says whether the shared original has been
	// republished since the override was made.
	SharedPublishedVersionID string
}

// TemplateRepo is the template aggregate. GetByKey finds a template by its
// Key and is ErrNotFound for an empty key.
type TemplateRepo interface {
	Create(ctx context.Context, t *Template) error
	Get(ctx context.Context, id string) (*Template, error)
	GetByKey(ctx context.Context, key string) (*Template, error)
	Update(ctx context.Context, t *Template) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[Template], error)
}

// contentKeyRE is the shape of a template or layout key: short, lower case,
// URL- and YAML-safe, so it can travel in a request body, a path or a config
// file without escaping.
var contentKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidContentKey reports whether key is acceptable as a Template.Key or a
// Layout.Key. The empty key is valid: it means "no key".
func ValidContentKey(key string) error {
	if key == "" || contentKeyRE.MatchString(key) {
		return nil
	}
	return fmt.Errorf("%w: key %q must be 1-64 characters of a-z, 0-9, '.', '_' or '-', "+
		"starting with a letter or digit", ErrInvalid, key)
}

// TemplateUses are the uses a template may be restricted to. A template is
// never the probe's: the loopback probe renders a built-in message.
func TemplateUses() []UseKind { return []UseKind{UseCampaign, UseTransactional} }
