package store

import (
	"context"
	"encoding/json"
	"time"
)

// BounceSource records which of the three correlation paths matched
// (architecture 10).
type BounceSource string

const (
	BounceSourceVERP      BounceSource = "verp"
	BounceSourceHeader    BounceSource = "header"
	BounceSourceMessageID BounceSource = "message_id"
	BounceSourceHeuristic BounceSource = "heuristic"
)

// BounceEvent is one parsed DSN (RFC 3464) or feedback report (RFC 5965).
type BounceEvent struct {
	ID       string
	TenantID string
	// DeliveryID is empty when correlation failed.
	DeliveryID string

	Type   BounceType
	Source BounceSource
	// Verified is false when the VERP HMAC did not match; such events are
	// recorded but never change a delivery.
	Verified bool

	Recipient      string
	EmailNorm      string
	SMTPStatus     string
	DiagnosticCode string
	MessageID      string

	// Raw is the original message, kept only when the tenant opted in.
	Raw json.RawMessage

	ReceivedAt time.Time
	CreatedAt  time.Time
}

// BounceRepo stores immutable events.
type BounceRepo interface {
	Create(ctx context.Context, b *BounceEvent) error
	Get(ctx context.Context, id string) (*BounceEvent, error)
	List(ctx context.Context, p Page) (Result[BounceEvent], error)
	ListByDelivery(ctx context.Context, deliveryID string, p Page) (Result[BounceEvent], error)
	// DeleteBefore removes events created before the cutoff, at most limit of
	// them, and returns how many it deleted. Bounce events fall under the
	// tenant's retention period like every other per-recipient row
	// (architecture 16). A limit <= 0 means "every match".
	DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error)
}

// BounceMailbox is an IMAP/POP3 account the bounce poller reads DSNs and
// feedback reports from (architecture 10). It has the same shape as
// ProbeMailbox — the two are different accounts with different jobs, and a
// single mailbox row could not carry both an AfterProcess policy and the
// inbox/spam folder mapping a probe verdict needs — plus the post-processing
// policy the poller applies to a handled message.
type BounceMailbox struct {
	ID       string
	TenantID string
	Name     string

	// Address is the mailbox's own address. It is informational: the poller
	// dials Host/Port, and correlation comes from the message, not from here.
	// It is what an operator recognizes the row by next to the return-path
	// domain it belongs to.
	Address string

	// Protocol is "imap" or "pop3"; empty means imap.
	Protocol string
	Host     string
	Port     int
	TLS      TLSMode
	Username string
	// Password is encrypted at rest by the host's SecretCipher.
	Password []byte

	// Folder is the IMAP mailbox to read. Empty means INBOX; POP3 ignores it.
	Folder string
	// AfterProcess is what happens to a handled message: "keep" (the default),
	// "delete" or "move:<folder>" (IMAP only). internal/mailbox.ParseAction
	// validates it.
	AfterProcess string

	Enabled bool

	// Health is the last reachability check of the account. It is written by
	// UpdateHealth, never by Update: see MailboxHealth.
	Health MailboxHealth

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BounceMailboxRepo is the CRUD surface plus the listing the poller needs.
type BounceMailboxRepo interface {
	Create(ctx context.Context, m *BounceMailbox) error
	Get(ctx context.Context, id string) (*BounceMailbox, error)
	Update(ctx context.Context, m *BounceMailbox) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, p Page) (Result[BounceMailbox], error)
	// ListEnabled returns every enabled mailbox of the tenant in one call,
	// unpaginated: the poller re-reads the whole set every RefreshInterval and
	// a tenant has a handful of bounce mailboxes, not a page of them.
	ListEnabled(ctx context.Context) ([]BounceMailbox, error)
	// UpdateHealth writes the reachability of the account and nothing else.
	// It takes no part in optimistic concurrency and does not bump Version;
	// see ProbeMailboxRepo.UpdateHealth.
	UpdateHealth(ctx context.Context, id string, h MailboxHealth) error
}
