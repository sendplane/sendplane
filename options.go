package sendplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// ErrNotImplemented marks the parts of the API that are not built yet.
var ErrNotImplemented = errors.New("sendplane: not implemented")

// Options configures New. Store and Auth are required; everything else has a
// default (see New).
type Options struct {
	Store   store.Provider // required
	Auth    Authenticator  // required. request -> Principal
	Authz   Authorizer     // optional. default: allow every authenticated principal
	Tenants TenantResolver // optional. default: Principal.TenantID, else "default"
	Hooks   Hooks
	Secrets SecretCipher // at-rest encryption for SMTP/IMAP passwords and DKIM keys
	Limits  Limits       // zero fields fall back to DefaultLimits
	Logger  *slog.Logger // default: slog.Default()
	Clock   func() time.Time
	// Metrics receives the counters and histograms the sender emits. Default:
	// host.NopMetrics.
	Metrics Metrics
}

// The embedding API of docs/architecture.md 3. Every identifier below is an
// alias for the one in package host, which is where the types actually live so
// that internal/control, internal/sender and internal/api can use them without
// importing this package (that would be a cycle: this package imports them to
// implement Handler, RunControl and RunSender).
//
// Aliases, not wrappers: sendplane.Hooks and host.Hooks are the same type, so
// a host keeps writing sendplane.X and nothing needs adapting at the boundary.
type (
	// Principal is the authenticated caller.
	Principal = host.Principal
	// Authenticator turns an HTTP request into a Principal.
	Authenticator = host.Authenticator
	// Action is the closed set of permissions sendplane defines.
	Action = host.Action
	// Resource identifies what an Action is performed on.
	Resource = host.Resource
	// Authorizer decides whether a principal may perform an action.
	Authorizer = host.Authorizer
	// TenantResolver maps a request to a tenant ID.
	TenantResolver = host.TenantResolver
	// SecretCipher encrypts secrets before they reach the store.
	SecretCipher = host.SecretCipher

	// Hooks are the optional Go escape hatches.
	Hooks = host.Hooks
	// RecipientContext is the per-recipient data available to hooks.
	RecipientContext = host.RecipientContext
	// OutboundMessage is the rendered message BeforeSend may inspect.
	OutboundMessage = host.OutboundMessage
	// UnsubscribeNotice reports a confirmed unsubscribe to the host.
	UnsubscribeNotice = host.UnsubscribeNotice
	// Event is one notification destined for the host.
	Event = host.Event
	// EventSink receives batches of events.
	EventSink = host.EventSink

	// Limits bounds request sizes.
	Limits = host.Limits
	// Metrics is the minimal surface sendplane needs from a metrics backend.
	Metrics = host.Metrics
	// NopMetrics discards every metric.
	NopMetrics = host.NopMetrics
)

// Errors hosts match with errors.Is.
var (
	// ErrUnauthenticated is returned by an Authenticator when the request
	// carries no usable credentials.
	ErrUnauthenticated = host.ErrUnauthenticated
	// ErrForbidden is returned by an Authorizer when the principal may not
	// perform the action on the resource.
	ErrForbidden = host.ErrForbidden
	// ErrSkip is returned by Hooks.BeforeSend to drop a message.
	ErrSkip = host.ErrSkip
)

// The Action constants of architecture 3, one per x-sendplane-action value in
// api/openapi.yaml.
const (
	ActionSettingsRead        = host.ActionSettingsRead
	ActionSettingsWrite       = host.ActionSettingsWrite
	ActionSenderRead          = host.ActionSenderRead
	ActionSenderWrite         = host.ActionSenderWrite
	ActionTemplateRead        = host.ActionTemplateRead
	ActionTemplateWrite       = host.ActionTemplateWrite
	ActionCampaignRead        = host.ActionCampaignRead
	ActionCampaignWrite       = host.ActionCampaignWrite
	ActionCampaignSend        = host.ActionCampaignSend
	ActionMessageSend         = host.ActionMessageSend
	ActionDeliveryRead        = host.ActionDeliveryRead
	ActionDeliveryWrite       = host.ActionDeliveryWrite
	ActionSuppressionRead     = host.ActionSuppressionRead
	ActionSuppressionWrite    = host.ActionSuppressionWrite
	ActionEventRead           = host.ActionEventRead
	ActionEventWrite          = host.ActionEventWrite
	ActionProbeRun            = host.ActionProbeRun
	ActionTrackingConfigWrite = host.ActionTrackingConfigWrite
)

// DefaultLimits are applied to any zero field of Options.Limits.
var DefaultLimits = host.DefaultLimits

// allowAll is the default Authorizer: anything an authenticated principal asks.
type allowAll struct{}

func (allowAll) Authorize(context.Context, *Principal, Action, Resource) error { return nil }

// principalTenant is the default TenantResolver.
type principalTenant struct{}

func (principalTenant) Resolve(_ context.Context, _ *http.Request, p *Principal) (string, error) {
	if p != nil && p.TenantID != "" {
		return p.TenantID, nil
	}
	return DefaultTenantID, nil
}

// DefaultTenantID is used when neither a TenantResolver nor the Principal names
// a tenant.
const DefaultTenantID = "default"
