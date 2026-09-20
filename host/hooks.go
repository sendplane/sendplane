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
