package host

import (
	"context"
	"errors"
	"net/http"

	"github.com/sendplane/sendplane/store"
)

// Errors returned across the embedding API. Hosts match them with errors.Is.
var (
	// ErrUnauthenticated is returned by an Authenticator when the request
	// carries no usable credentials.
	ErrUnauthenticated = errors.New("sendplane: unauthenticated")
	// ErrForbidden is returned by an Authorizer when the principal may not
	// perform the action on the resource.
	ErrForbidden = errors.New("sendplane: forbidden")
)

// Principal is the authenticated caller. Roles and Attrs are host-defined;
// sendplane never interprets them.
type Principal struct {
	ID       string
	TenantID string
	Roles    []string
	Attrs    map[string]any
}

// Authenticator turns an HTTP request into a Principal. It returns an error
// wrapping ErrUnauthenticated when the request cannot be authenticated.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// Action is the closed set of permissions sendplane defines. Hosts map these
// onto their own RBAC.
//
// Every operation in api/openapi.yaml declares the Action it needs through the
// x-sendplane-action extension, and the constants below are exactly the values
// that extension may take. The spec's one extra value, "none", is not an
// Action: it marks a public route where Authorize is never called.
type Action string

const (
	// Tenant settings: retry policy, retention, suppression switch,
	// unsubscribe mode and the tracking configuration.
	ActionSettingsRead  Action = "settings.read"
	ActionSettingsWrite Action = "settings.write"

	// Sending configuration: transports, senders, sending domains, probe
	// mailboxes and probe runs.
	ActionSenderRead  Action = "sender.read"
	ActionSenderWrite Action = "sender.write"

	// Content: layouts, templates, i18n bundles, previews, publishes and
	// message versions.
	ActionTemplateRead  Action = "template.read"
	ActionTemplateWrite Action = "template.write"

	// Campaigns. Managing a campaign and sending it are separate permissions
	// on purpose (the listmonk lesson of architecture 3).
	ActionCampaignRead  Action = "campaign.read"
	ActionCampaignWrite Action = "campaign.write"
	ActionCampaignSend  Action = "campaign.send"

	// Transactional send.
	ActionMessageSend Action = "message.send"

	// Deliveries, attempts and bounces; the write side is a single-delivery
	// retry or unsubscribe notification.
	ActionDeliveryRead  Action = "delivery.read"
	ActionDeliveryWrite Action = "delivery.write"

	// Suppression list (ADR-0008).
	ActionSuppressionRead  Action = "suppression.read"
	ActionSuppressionWrite Action = "suppression.write"

	// Event outbox: listing the queue and replaying a dead letter.
	ActionEventRead  Action = "event.read"
	ActionEventWrite Action = "event.write"

	// ActionProbeRun starts a loopback health probe (ADR-0012). The REST API
	// covers probe runs under sender.read/sender.write; this action exists for
	// a host that wants to gate triggering a probe on its own.
	ActionProbeRun Action = "probe.run"
	// ActionTrackingConfigWrite changes the tenant tracking domain, the
	// open/click switches and the signing keys (architecture 9). The REST API
	// covers it under settings.write.
	ActionTrackingConfigWrite Action = "tracking.config.write"
)

// Resource identifies what an Action is performed on.
type Resource struct{ Kind, ID, TenantID string }

// Authorizer decides whether a principal may perform an action. It returns an
// error wrapping ErrForbidden when it may not.
type Authorizer interface {
	Authorize(ctx context.Context, p *Principal, a Action, r Resource) error
}

// TenantResolver maps a request to a tenant ID.
type TenantResolver interface {
	Resolve(ctx context.Context, r *http.Request, p *Principal) (tenantID string, err error)
}

// TenantSwitcher is the optional half of a TenantResolver that lets some
// callers choose their tenant per request — typically an operator console
// switching between its own tenant and the system tenant (store.SystemTenantID),
// which is the only view that shows platform state (ADR-0017).
//
// It exists so that GET /api/v1/whoami can tell a console whether to render
// the switcher at all, without the API having to know how the host decides.
// A resolver that does not implement it reports can_switch_tenant false, and
// nothing else changes: the switch itself is entirely the resolver's, which is
// where the trust decision belongs.
type TenantSwitcher interface {
	// CanSwitchTenant reports whether Resolve would honour a tenant selection
	// from this principal.
	CanSwitchTenant(ctx context.Context, p *Principal) bool
}

// SecretCipher encrypts secrets (SMTP/IMAP passwords, DKIM private keys)
// before they reach the store.
//
// It is an alias of store.SecretCipher, which is where the interface is
// declared so that the platform overlay (store.WithPlatform) can encrypt the
// secrets it builds virtual entities from without importing this package. A
// host implements one interface either way.
type SecretCipher = store.SecretCipher
