// Package sender is the claim → render → SMTP loop of architecture 8.
//
// One Sender is one replica. It polls the delivery queue per tenant and lane
// (ADR-0002), renders each delivery for its recipient (internal/render),
// applies the tracking and unsubscribe transforms of architecture 9.2, builds
// the MIME message of architecture 10, and hands it to a pooled SMTP
// connection. SMTP results are normalized into the five error classes of
// architecture 4.2 and committed in batches, with an immediate MarkSent right
// after 250 so that a crash before the batch commits costs a duplicate at
// most, never a lost send.
//
// The package does not import the root sendplane package: the root package is
// what will call Run, so the types the host sees (Hooks, OutboundMessage) are
// declared here and adapted there. See README.
package sender

import (
	"context"
	"errors"

	"github.com/sendplane/sendplane/store"
)

// ErrSkip is what a BeforeSend hook returns to drop a message; the delivery
// ends up suppressed. The root package's own ErrSkip is translated into this
// one by the adapter that installs the hooks.
var ErrSkip = errors.New("sender: skip")

// SecretCipher decrypts the secrets the store holds encrypted: transport
// passwords and DKIM private keys. It is structurally identical to
// sendplane.SecretCipher, so the host's implementation satisfies it directly.
type SecretCipher interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// RecipientContext is the per-recipient data hooks see.
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
	CampaignID string
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

// Hooks are the optional Go escape hatches the sender calls. Both may be nil.
type Hooks struct {
	// UnsubscribeURL is the last of the three sources of the host destination:
	// recipient variable > tenant URL template > this hook (architecture 9.2).
	UnsubscribeURL func(ctx context.Context, rc RecipientContext) (string, error)
	// BeforeSend may rewrite or reject a message. Returning ErrSkip marks the
	// delivery suppressed.
	BeforeSend func(ctx context.Context, m *OutboundMessage) error
}
