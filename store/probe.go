package store

import (
	"context"
	"encoding/json"
	"time"
)

// ProbeMailboxKind is how a probe mail gets back to sendplane (ADR-0016).
//
// The empty value means ProbeMailboxIMAP: the kind was added after the model
// shipped, and every row that predates it is an IMAP account.
type ProbeMailboxKind string

const (
	// ProbeMailboxIMAP is an account the probe collector logs in to and
	// searches. It is the only kind that can tell inbox from spam.
	ProbeMailboxIMAP ProbeMailboxKind = "imap"
	// ProbeMailboxWebhook is an address whose provider posts the delivered
	// mail to sendplane's global inbound endpoint. Only Address, AuthServID,
	// Enabled and Health are meaningful; there is nothing to log in to.
	ProbeMailboxWebhook ProbeMailboxKind = "webhook"
)

// Normalized resolves the empty value to imap.
func (k ProbeMailboxKind) Normalized() ProbeMailboxKind {
	if k == "" {
		return ProbeMailboxIMAP
	}
	return k
}

// Valid reports whether k is a kind this version knows.
func (k ProbeMailboxKind) Valid() bool {
	switch k.Normalized() {
	case ProbeMailboxIMAP, ProbeMailboxWebhook:
		return true
	}
	return false
}

func (k ProbeMailboxKind) String() string { return string(k.Normalized()) }

// ProbeMailbox is a mailbox a loopback probe mail is recovered from, either by
// polling it over IMAP or by the provider posting it to sendplane's inbound
// webhook (Kind). Registering several (Gmail, Outlook, own MTA) is
// recommended: the verdict depends on the headers the receiving MTA adds
// (ADR-0012).
type ProbeMailbox struct {
	ID       string
	TenantID string
	Name     string

	// Kind is imap (the default and the zero value) or webhook. For a webhook
	// mailbox the IMAP block below is unused and rejected on write.
	Kind ProbeMailboxKind

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

	// Health is the last reachability check of the account. It is written by
	// UpdateHealth, never by Update: see MailboxHealth.
	Health MailboxHealth

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
	// UpdateHealth writes the reachability of the account and nothing else.
	// It takes no part in optimistic concurrency and does not bump Version,
	// like the sender's transport status writes: the writers are background
	// loops, and an operator editing the row must neither lose their edit to
	// a health observation nor make one fail.
	UpdateHealth(ctx context.Context, id string, h MailboxHealth) error
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
	// GroupID ties together the runs one trigger created, one per mailbox, so
	// a caller can ask for "the result of this trigger" without guessing from
	// StartedAt.
	GroupID string

	// Pending is true while the run is still waiting for its mail. A finished
	// run carries one of green/yellow/red; Pending is the explicit marker
	// rather than an inference from Status == unknown, because a DNS-only run
	// can legitimately finish as unknown.
	Pending bool

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

// ProbeRunRepo stores run history. A run is written pending and updated once,
// when its mail arrives or its timeout passes; nothing else ever changes it.
type ProbeRunRepo interface {
	Create(ctx context.Context, r *ProbeRun) error
	// Update writes the pending -> finished transition. It takes no part in
	// optimistic concurrency: only the collector writes a run, and it holds
	// the control leader lease.
	Update(ctx context.Context, r *ProbeRun) error
	Get(ctx context.Context, id string) (*ProbeRun, error)
	ListBySender(ctx context.Context, senderID string, p Page) (Result[ProbeRun], error)
	// ListPending returns the runs still waiting for their mail, oldest
	// first, so the collector does not page through finished history to find
	// them.
	ListPending(ctx context.Context, p Page) (Result[ProbeRun], error)
}
