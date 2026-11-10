package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
)

// SecretCipher encrypts secrets at rest. It is declared here, rather than only
// in package host, because the platform overlay has to encrypt the passwords
// it builds virtual entities from so that the existing decrypt paths in
// internal/sender and internal/bounce work unchanged.
//
// host.SecretCipher is an alias of this type, so a host keeps implementing one
// interface.
//
// Every call carries the SecretContext of the row the secret belongs to, and
// an implementation must bind the ciphertext to it: Decrypt with any field of
// sc different from the one Encrypt was given has to fail. That is what stops
// somebody who can write to the database but does not hold the key from
// copying one row's ciphertext into another row — tenant A's SMTP password
// into tenant B's transport — and having sendplane decrypt and use it there.
// An AEAD takes sc.AAD() as its associated data; the reference AES-256-GCM
// cipher (cmd/sendplane) does exactly that.
//
// There is no fallback to decrypting without the context. A deployment that
// has ciphertexts written before the context existed rewraps them once with
// sendplane.RewrapSecrets before the new version needs them.
type SecretCipher interface {
	Encrypt(ctx context.Context, plaintext []byte, sc SecretContext) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte, sc SecretContext) ([]byte, error)
}

// SecretContext names the row a secret belongs to. It is built by the
// constructors below (TransportPasswordContext and friends), never assembled
// by hand, so that the encrypting and the decrypting side cannot disagree on
// a field.
//
// Every field must be immutable for the lifetime of the row: a field that
// changed would make the stored ciphertext undecryptable. That is why the
// context carries IDs and never a name, a host or a username.
//
// A host whose SecretCipher is a KMS with an "encryption context" (a string
// map bound to the ciphertext, as AWS KMS and Google Cloud KMS have) may map
// the four fields to that map's entries instead of using AAD. Whatever it
// does, the binding has to cover all four fields.
type SecretContext struct {
	// Purpose is what the secret is ("transport.password"), one of the
	// Purpose* constants.
	Purpose string
	// TenantID is the tenant the row belongs to; SystemTenantID for a
	// platform entity.
	TenantID string
	// ObjectID is the row's ID. A platform entity uses its virtual
	// "sys:<id>".
	ObjectID string
	// Bind is extra binding for a secret that needs more than the row.
	// DeliverySecretContext binds the recipient here; every other secret
	// leaves it empty.
	Bind string
}

// The purposes of the secrets sendplane encrypts. They are part of every
// stored ciphertext's associated data, so a value here can never be renamed.
const (
	PurposeTransportPassword     = "transport.password"
	PurposeDomainDKIMPrivateKey  = "sending_domain.dkim_private_key"
	PurposeBounceMailboxPassword = "bounce_mailbox.password"
	PurposeProbeMailboxPassword  = "probe_mailbox.password"
	// PurposeMailboxTestPassword is a password typed into a "test this
	// mailbox" form. It is encrypted only so that the tester sees ciphertext
	// like it does for a stored row, and is never stored.
	PurposeMailboxTestPassword = "mailbox_test.password"
	// PurposeDeliverySecretVars is a sensitive delivery's secret_vars
	// (ADR-0019).
	PurposeDeliverySecretVars = "delivery.secret_vars"
)

// secretAADVersion is the first element of every AAD. A future encoding gets
// a new tag rather than a reinterpretation of this one.
const secretAADVersion = "sendplane/secret/v1"

// AAD returns the canonical associated-data encoding of the context: the
// version tag and then each field, in declaration order, as a 4-byte
// big-endian length followed by the bytes. Length-prefixing every element is
// what makes the encoding injective; a separator-joined string would let
// {TenantID: "a/b", ObjectID: "c"} and {TenantID: "a", ObjectID: "b/c"}
// collide.
func (c SecretContext) AAD() []byte {
	fields := [...]string{secretAADVersion, c.Purpose, c.TenantID, c.ObjectID, c.Bind}
	n := 0
	for _, f := range fields {
		n += 4 + len(f)
	}
	out := make([]byte, 0, n)
	for _, f := range fields {
		// IDs and purposes are short; a 4 GiB field is a bug, and truncating
		// its length would break injectivity, so it is refused outright.
		if len(f) > math.MaxUint32 {
			panic("store: SecretContext field longer than 4 GiB")
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(f))) //nolint:gosec // bounded just above
		out = append(out, f...)
	}
	return out
}

// TransportPasswordContext is the context of Transport.Password.
func TransportPasswordContext(t *Transport) SecretContext {
	return SecretContext{Purpose: PurposeTransportPassword, TenantID: t.TenantID, ObjectID: t.ID}
}

// DomainDKIMKeyContext is the context of SendingDomain.DKIMPrivateKey.
func DomainDKIMKeyContext(d *SendingDomain) SecretContext {
	return SecretContext{Purpose: PurposeDomainDKIMPrivateKey, TenantID: d.TenantID, ObjectID: d.ID}
}

// BounceMailboxPasswordContext is the context of BounceMailbox.Password.
func BounceMailboxPasswordContext(m *BounceMailbox) SecretContext {
	return SecretContext{Purpose: PurposeBounceMailboxPassword, TenantID: m.TenantID, ObjectID: m.ID}
}

// ProbeMailboxPasswordContext is the context of ProbeMailbox.Password.
func ProbeMailboxPasswordContext(m *ProbeMailbox) SecretContext {
	return SecretContext{Purpose: PurposeProbeMailboxPassword, TenantID: m.TenantID, ObjectID: m.ID}
}

// MailboxTestPasswordContext is the context of a transient password a mailbox
// test was given in its request body. testID is fresh per test (NewID): the
// ciphertext is valid for that one test and for no stored row.
func MailboxTestPasswordContext(tenantID, testID string) SecretContext {
	return SecretContext{Purpose: PurposeMailboxTestPassword, TenantID: tenantID, ObjectID: testID}
}

// DeliverySecretContext is the context of Delivery.SecretCiphertext.
//
// Besides the row it binds the recipient: Bind is "sha256:" and the lowercase
// hex SHA-256 of EmailNorm. A ciphertext copied onto another delivery fails on
// ObjectID already; binding the address as well means that even rewriting a
// row's email columns cannot make sendplane mail a secret to an address it was
// not sealed for. The address goes in as a hash because a KMS that maps the
// context to an encryption context writes it to its audit log (CloudTrail and
// the like) in clear, and a recipient list is personal data; the binding is
// the same either way.
//
// EmailNorm, like the other fields, never changes for the life of a delivery.
func DeliverySecretContext(d *Delivery) SecretContext {
	sum := sha256.Sum256([]byte(d.EmailNorm))
	return SecretContext{
		Purpose:  PurposeDeliverySecretVars,
		TenantID: d.TenantID,
		ObjectID: d.ID,
		Bind:     "sha256:" + hex.EncodeToString(sum[:]),
	}
}
