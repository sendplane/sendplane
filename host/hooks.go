package host

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/sendplane/sendplane/store"
)

// ErrSkip is returned by Hooks.BeforeSend to drop a message. The delivery ends
// up in status suppressed.
var ErrSkip = errors.New("sendplane: skip")

// Hooks are the optional Go escape hatches. Everything they do can also be
// configured without Go (recipient variables, tenant URL templates, webhooks),
// so a non-Go host can leave all of them nil.
type Hooks struct {
	// UnsubscribeURL returns the host destination for a recipient.
	// Precedence: recipient unsubscribe_url variable > tenant URL template > this hook.
	UnsubscribeURL func(ctx context.Context, rc RecipientContext) (string, error)

	// Unsubscribed is called synchronously when a one-click (RFC 8058)
	// unsubscribe reaches sendplane. If it is nil, or if it fails, the host is
	// notified through the event outbox instead.
	Unsubscribed func(ctx context.Context, u UnsubscribeNotice) error

	// BeforeSend may rewrite or reject a message just before it is handed to a
	// transport. Returning ErrSkip marks the delivery suppressed.
	BeforeSend func(ctx context.Context, m *OutboundMessage) error

	// Events receives delivery/campaign/transport events. Default: the outbox
	// dispatches to the tenant's webhook URL.
	Events EventSink

	// TenantVars validates and completes the tenant attributes one request
	// carried (`tenant_vars` on a campaign, a message or a preview). What it
	// returns is what gets stored on the campaign or delivery and bound as
	// `tenant` in every template, including a shared sender's From templates.
	//
	// It exists because sendplane has no tenant registry: it stores no tenant
	// name, slug or plan, on purpose (ADR-0017). The host's own database is
	// the authority, so the host is the only thing that can say whether
	// "slug: acme" is this tenant's slug. A typical implementation ignores
	// `requested` entirely and returns the attributes it looked up itself,
	// which is what stops one tenant from sending as another:
	//
	//	TenantVars: func(ctx context.Context, _ *sendplane.Principal, tenantID string,
	//	    _ map[string]any) (map[string]any, error) {
	//	    t, err := db.Tenant(ctx, tenantID)
	//	    if err != nil {
	//	        return nil, err
	//	    }
	//	    return map[string]any{"name": t.Name, "slug": t.Slug, "plan": t.Plan}, nil
	//	}
	//
	// Default (nil): pass-through, i.e. the request's own variables. That is
	// right for a single-tenant deployment and a trust decision anywhere else.
	// Returning an error rejects the request: wrap ErrForbidden for a 403,
	// anything else is a 422.
	TenantVars func(ctx context.Context, p *Principal, tenantID string, requested map[string]any) (map[string]any, error)

	// SenderPolicy decides whether a sender may be used for this kind of
	// send. It is called on POST /campaigns (create and start), POST /messages
	// and a probe trigger, before anything is queued.
	//
	// Default (nil): DefaultSenderPolicy, which enforces a platform sender's
	// configured `uses` list. A hook replaces that rather than adding to it,
	// so a host that wants both chains them (see DefaultSenderPolicy).
	//
	// Any non-nil error is a denial and becomes 403 sender_use_denied with the
	// error's message, so the message is part of the API and should say what
	// would be allowed.
	SenderPolicy func(ctx context.Context, u SenderUse) error

	// TemplatePolicy decides whether a template may be used for this kind of
	// send. It is called on POST /messages and on POST /campaigns (create and
	// start), after the template has been resolved (by ID, by key, or through
	// a pinned version), before anything is queued.
	//
	// Default (nil): DefaultTemplatePolicy, which enforces a *shared*
	// template's `uses` list and leaves a tenant's own templates alone. A
	// tenant's override of a shared template is its own template: it carries
	// a copy of the `uses` it was made from, but the default does not enforce
	// it — a host that wants overrides held to the original's restrictions
	// says so here. Like SenderPolicy, a hook replaces the default rather than
	// adding to it; chain DefaultTemplatePolicy to keep it.
	//
	// Any non-nil error is a denial and becomes 403 template_use_denied with
	// the error's message.
	TemplatePolicy func(ctx context.Context, u TemplateUse) error
}

// RecipientContext is the per-recipient data available to hooks.
type RecipientContext struct {
	TenantID   string
	CampaignID string // empty for transactional
	DeliveryID string
	Email      string
	EmailNorm  string
	Name       string
	Locale     string
	Vars       map[string]any
}

// OutboundMessage is the rendered message BeforeSend may inspect or modify.
// Changing Headers, Subject, From, ReplyTo or UnsubscribeURL is allowed;
// values containing CR or LF are rejected afterwards (architecture 16).
type OutboundMessage struct {
	TenantID   string
	DeliveryID string
	CampaignID string // empty for transactional
	VersionID  string
	SenderID   string
	Lane       store.Lane

	Recipient RecipientContext

	FromName string
	From     string
	ReplyTo  string

	Subject string
	HTML    string
	Text    string

	// Headers are extra headers to add. Names are checked against the
	// allowlist of architecture 16 unless they start with X-.
	Headers        map[string]string
	UnsubscribeURL string
}

// UnsubscribeNotice reports a confirmed unsubscribe to the host.
type UnsubscribeNotice struct {
	TenantID   string
	CampaignID string
	DeliveryID string
	Email      string
	EmailNorm  string
	Source     string // "one_click" | "host"
	At         time.Time
}

// Event is one notification destined for the host.
type Event struct {
	ID         string
	TenantID   string
	Type       string // delivery.sent, campaign.completed, sender.health_changed, ...
	OccurredAt time.Time
	Payload    json.RawMessage
}

// EventSink receives batches of events. Implementations must be safe for
// concurrent use and should be idempotent: delivery is at-least-once.
type EventSink interface {
	Emit(ctx context.Context, events []Event) error
}
