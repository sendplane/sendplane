package store

import (
	"context"
	"encoding/json"
	"time"
)

// ProbeMailbox is an IMAP account a loopback probe mail is recovered from.
// Registering several (Gmail, Outlook, own MTA) is recommended: the verdict
// depends on the headers the receiving MTA adds (ADR-0012).
type ProbeMailbox struct {
	ID       string
	TenantID string
	Name     string

	// Address is where probe mail is sent.
	Address string

	Host     string
	Port     int
	TLS      TLSMode
	Username string
	// Password is encrypted at rest by the host's SecretCipher.
	Password []byte

	InboxFolder string
	SpamFolder  string
	// AuthServID is the authserv-id of this mailbox's MTA. Only
	// Authentication-Results headers carrying it are trusted (RFC 8601).
	AuthServID string

	Enabled bool

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ProbeMailboxRepo interface {
	Create(ctx context.Context, m *ProbeMailbox) error
	Get(ctx context.Context, id string) (*ProbeMailbox, error)
	Update(ctx context.Context, m *ProbeMailbox) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[ProbeMailbox], error)
}

// ProbeRun is one sender x mailbox loopback result plus the DNS diagnosis
// gathered alongside it (architecture 11.2).
type ProbeRun struct {
	ID        string
	TenantID  string
	SenderID  string
	MailboxID string
	// DeliveryID is the lane=probe delivery that carried the mail.
	DeliveryID string

	Status HealthStatus
	// Reason explains a non-green status ("not delivered", "dkim=fail").
	Reason string

	Delivered bool
	// Folder is where the mail landed: inbox or spam.
	Folder  string
	Latency time.Duration

	// Authentication-Results verdicts, as written by the receiving MTA.
	SPF          string
	DKIM         string
	DMARC        string
	DKIMDomain   string
	DKIMSelector string
	DMARCPolicy  string

	// Observed from the Received chain.
	TLS        bool
	ObservedIP string
	PTR        string
	PTRMatch   bool

	// DNS holds the static SPF/DKIM/DMARC/MX/PTR check results
	// (architecture 11.3).
	DNS json.RawMessage
	// RawHeaders is kept for diagnosis, under the tenant retention period.
	RawHeaders string

	StartedAt  time.Time
	ReceivedAt time.Time
	CreatedAt  time.Time
}

// ProbeRunRepo stores immutable run history.
type ProbeRunRepo interface {
	Create(ctx context.Context, r *ProbeRun) error
	Get(ctx context.Context, id string) (*ProbeRun, error)
	ListBySender(ctx context.Context, senderID string, p Page) (Result[ProbeRun], error)
}
