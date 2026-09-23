package sendplane

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

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
	// Platform is the operator's shared sending infrastructure: the
	// transports, domains, From identities and mailboxes it runs for its
	// tenants (ADR-0017, architecture 5.4).
	//
	// It is configuration and is never written to the store. New validates it
	// — unique IDs, resolvable references, parseable From templates — and
	// wraps Options.Store in the overlay that resolves it, so a shared sender
	// shows up in every tenant's GET /senders while its relay credentials
	// exist only in this struct.
	//
	// The zero value is a deployment with no shared resources, which is every
	// single-tenant one: nothing is wrapped and nothing costs anything.
	Platform Platform
	// Probe configures the loopback health probe (architecture 11). It is off
	// by default: probing needs mailboxes the deployment owns, and a trigger
	// nothing collects is worse than none at all.
	Probe  ProbeConfig
	Logger *slog.Logger // default: slog.Default()
	Clock  func() time.Time
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
	// ProbeConfig is the process-wide half of the loopback probe setup; the
	// mailboxes themselves are tenant rows managed through the API.
	ProbeConfig = host.ProbeConfig

	// Platform is the operator's shared sending infrastructure (ADR-0017).
	Platform = host.Platform
	// PlatformTransport is a shared SMTP account.
	PlatformTransport = host.PlatformTransport
	// PlatformDomain is a shared sending domain.
	PlatformDomain = host.PlatformDomain
	// PlatformSender is a shared From identity with templated addresses.
	PlatformSender = host.PlatformSender
	// PlatformProbeMailbox is a shared loopback probe mailbox.
	PlatformProbeMailbox = host.PlatformProbeMailbox
	// PlatformBounceMailbox is a shared bounce mailbox.
	PlatformBounceMailbox = host.PlatformBounceMailbox
	// UseKind is what a sender is being used for (campaign, transactional,
	// probe): the vocabulary of the sender-use policy.
	UseKind = host.UseKind
	// SenderUse is one request to send something with a sender, handed to
	// Hooks.SenderPolicy.
	SenderUse = host.SenderUse
	// TemplateUse is one request to send with a template, handed to
	// Hooks.TemplatePolicy (ADR-0018).
	TemplateUse = host.TemplateUse

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
	// ErrSenderUseDenied is what a Hooks.SenderPolicy returns to refuse a
	// send; the API answers 403 sender_use_denied with its message.
	ErrSenderUseDenied = host.ErrSenderUseDenied
	// ErrTemplateUseDenied is what a Hooks.TemplatePolicy returns to refuse a
	// send; the API answers 403 template_use_denied with its message.
	ErrTemplateUseDenied = host.ErrTemplateUseDenied
)

// The sender uses a Hooks.SenderPolicy discriminates on.
const (
	UseCampaign      = host.UseCampaign
	UseTransactional = host.UseTransactional
	UseProbe         = host.UseProbe
)

// DefaultSenderPolicy is the sender-use policy applied when Hooks.SenderPolicy
// is nil: a shared sender may only be used for what its configuration's
// `uses` list names. It is exported so that a host hook can chain it.
var DefaultSenderPolicy = host.DefaultSenderPolicy

// DefaultTemplatePolicy is the template-use policy applied when
// Hooks.TemplatePolicy is nil: a shared template may only be used for what
// its `uses` names; a tenant's own templates are unrestricted. It is exported
// so that a host hook can chain it.
var DefaultTemplatePolicy = host.DefaultTemplatePolicy

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
