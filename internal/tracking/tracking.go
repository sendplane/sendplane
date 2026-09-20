// Package tracking signs and verifies the stateless tracking tokens of
// architecture 9.1, and builds the public URLs that carry them.
//
// A token is
//
//	kid "." base64url( body ‖ mac )
//
// where body carries tenant_id, delivery_id, kind and link_no (plus the
// destination for unsubscribe tokens) and mac is an HMAC-SHA256 over tenant_id,
// delivery_id, kind, link_no *and* the destination URL. Binding the
// destination into the MAC is what makes the click redirect safe:
// /t/c/{token}?u={url} only redirects to a u the signer chose, so the route is
// not an open redirect (architecture 16).
//
// The tenant ID travels in the token as well, so an unauthenticated public
// route knows whose signing keys to verify against without a lookup.
//
// Nothing is stored per recipient: verification is a pure function of the
// token, the tenant's signing keys and - for clicks - the u parameter.
package tracking

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// Token errors. Callers on the public routes must not tell them apart in
// their responses; they exist for logs and tests.
var (
	// ErrMalformedToken is returned when a token is not kid "." base64url(...)
	// or does not decode into a payload.
	ErrMalformedToken = errors.New("tracking: malformed token")
	// ErrUnknownKID is returned when no configured signing key has the token's
	// key ID. Rotating keys keeps old links alive only as long as the old key
	// stays in TrackingConfig.SigningKeys.
	ErrUnknownKID = errors.New("tracking: unknown key id")
	// ErrBadSignature is returned when the MAC does not cover the payload and
	// destination presented.
	ErrBadSignature = errors.New("tracking: bad signature")
	// ErrNoKey is returned by Sign when the key id or secret is unusable.
	ErrNoKey = errors.New("tracking: no usable signing key")
)

// Kind is what a token authorizes. The numeric values are part of the token
// wire format and must not be renumbered.
type Kind uint8

const (
	// KindOpen is the pixel token of GET /t/o/{token}.
	KindOpen Kind = 1
	// KindClick is the link token of GET /t/c/{token}?u={url}. Its destination
	// travels in the query string, not in the token.
	KindClick Kind = 2
	// KindUnsubscribe is the token of GET/POST /t/u/{token}. Its destination is
	// embedded in the token because the route has to redirect (and one-click
	// POST has to record) without a query string of its own.
	KindUnsubscribe Kind = 3
)

var kindNames = map[Kind]string{
	KindOpen: "open", KindClick: "click", KindUnsubscribe: "unsubscribe",
}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Valid reports whether k is one of the three defined kinds.
func (k Kind) Valid() bool { _, ok := kindNames[k]; return ok }

// embedsDest reports whether the destination travels inside the token.
func (k Kind) embedsDest() bool { return k == KindUnsubscribe }

// TokenPayload is what a token carries. LinkNo is the index into
// MessageVersion.Links for clicks and -1 (or 0) elsewhere; Dest is the
// destination URL, which is always part of the MAC but only embedded for
// KindUnsubscribe.
//
// TenantID travels in the token because the public routes are unauthenticated
// and the signing keys are per tenant: without it a verifier would have to try
// every tenant's keys to find out whose token it is holding. It is covered by
// the MAC like every other field, so it cannot be swapped for another tenant's.
type TokenPayload struct {
	TenantID   string
	DeliveryID string
	Kind       Kind
	LinkNo     int
	Dest       string
}

// macSize is how much of the HMAC-SHA256 output ends up in the token. 16 bytes
// is 128 bits of forgery resistance, which is the usual truncation for
// capability URLs, and keeps the token ~24 characters shorter than the full
// digest would - tokens sit in every href of every mail.
const macSize = 16

// macContext is the domain separator, so a tracking MAC can never be confused
// with the VERP MAC of architecture 10 even if a tenant reuses a secret.
const macContext = "sendplane/tracking/v1\x00"

// kidSep separates the (variable length) key id from the payload.
//
// Architecture 9.1 writes the token as kid ‖ base64url(...) with no separator;
// a separator is required to split a variable-length kid back out, and "." is
// not in the base64url alphabet, so it cannot occur in the second half.
const kidSep = "."

// Signer signs and verifies tracking tokens. The zero value is ready to use
// and is safe for concurrent use: it holds no state, the keys travel with each
// call because they are per tenant.
type Signer struct{}

// NewSigner returns a Signer.
func NewSigner() Signer { return Signer{} }

// Sign returns the token for p, signed with the secret of key kid.
//
// p.Dest is covered by the MAC for every kind, and additionally embedded in
// the token for KindUnsubscribe.
func (Signer) Sign(kid string, secret []byte, p TokenPayload) string {
	tok, err := signToken(kid, secret, p)
	if err != nil {
		// Sign has no error return by design (architecture 9.2 calls it once
		// per link, per recipient). An unusable key produces an empty token,
		// which the caller treats as "no tracking": the sender checks the key
		// once, before rendering.
		return ""
	}
	return tok
}

// SignErr is Sign with the validation error the sender checks up front.
func (Signer) SignErr(kid string, secret []byte, p TokenPayload) (string, error) {
	return signToken(kid, secret, p)
}

func signToken(kid string, secret []byte, p TokenPayload) (string, error) {
	if kid == "" || strings.Contains(kid, kidSep) {
		return "", fmt.Errorf("%w: key id %q", ErrNoKey, kid)
	}
	if len(secret) == 0 {
		return "", fmt.Errorf("%w: empty secret for key %q", ErrNoKey, kid)
	}
	if !p.Kind.Valid() {
		return "", fmt.Errorf("%w: %s", ErrNoKey, p.Kind)
	}
	if p.DeliveryID == "" {
		return "", fmt.Errorf("%w: empty delivery id", ErrNoKey)
	}
	if p.TenantID == "" {
		return "", fmt.Errorf("%w: empty tenant id", ErrNoKey)
	}
	body := encodeBody(p)
	mac := computeMAC(secret, p)
	buf := make([]byte, 0, len(body)+macSize)
	buf = append(buf, body...)
	buf = append(buf, mac...)
	return kid + kidSep + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Verify checks a token against the tenant's keys. It is the entry point for
// the open and unsubscribe routes, where the destination is either irrelevant
// or embedded in the token.
//
// Click tokens carry their destination in the u query parameter, so the click
// route calls VerifyDest with that parameter instead; Verify rejects a click
// token, because verifying one without its destination would accept any u.
func (s Signer) Verify(keys []store.SigningKey, token string) (TokenPayload, error) {
	return s.VerifyDest(keys, token, "")
}

// VerifyDest checks a token against the tenant's keys with an externally
// supplied destination (the u parameter of GET /t/c). For kinds that embed
// their destination, dest must be empty or equal to the embedded one.
func (Signer) VerifyDest(keys []store.SigningKey, token, dest string) (TokenPayload, error) {
	kid, rest, ok := strings.Cut(token, kidSep)
	if !ok || kid == "" || rest == "" {
		return TokenPayload{}, fmt.Errorf("%w: missing key id", ErrMalformedToken)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return TokenPayload{}, fmt.Errorf("%w: %v", ErrMalformedToken, err)
	}
	if len(raw) <= macSize {
		return TokenPayload{}, fmt.Errorf("%w: too short", ErrMalformedToken)
	}
	body, mac := raw[:len(raw)-macSize], raw[len(raw)-macSize:]
	p, err := decodeBody(body)
	if err != nil {
		return TokenPayload{}, err
	}
	if p.Kind.embedsDest() {
		if dest != "" && dest != p.Dest {
			return TokenPayload{}, fmt.Errorf("%w: destination does not match token", ErrBadSignature)
		}
	} else {
		p.Dest = dest
	}

	key, ok := keyByID(keys, kid)
	if !ok {
		return TokenPayload{}, fmt.Errorf("%w: %q", ErrUnknownKID, kid)
	}
	if !hmac.Equal(mac, computeMAC(key.Secret, p)) {
		return TokenPayload{}, ErrBadSignature
	}
	return p, nil
}

// TenantOf reads the tenant ID out of a token *without* checking its MAC.
//
// It exists for one job: the public routes are unauthenticated and the signing
// keys are per tenant, so something has to say which tenant's keys to verify
// against. The value is attacker-controlled until Verify or VerifyDest has
// run, so a caller may use it to look up keys and for nothing else - and since
// the tenant ID is covered by the MAC, a token that then verifies is proof
// that this is the tenant that signed it.
func TenantOf(token string) (string, error) {
	_, rest, ok := strings.Cut(token, kidSep)
	if !ok || rest == "" {
		return "", fmt.Errorf("%w: missing key id", ErrMalformedToken)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformedToken, err)
	}
	if len(raw) <= macSize {
		return "", fmt.Errorf("%w: too short", ErrMalformedToken)
	}
	p, err := decodeBody(raw[:len(raw)-macSize])
	if err != nil {
		return "", err
	}
	if p.TenantID == "" {
		return "", fmt.Errorf("%w: no tenant id", ErrMalformedToken)
	}
	return p.TenantID, nil
}

func keyByID(keys []store.SigningKey, kid string) (store.SigningKey, bool) {
	for _, k := range keys {
		if k.KID == kid && len(k.Secret) > 0 {
			return k, true
		}
	}
	return store.SigningKey{}, false
}

// SelectKey returns the key new tokens are signed with: the newest usable one,
// with the key id breaking ties so that the choice is stable.
func SelectKey(keys []store.SigningKey) (store.SigningKey, bool) {
	var best store.SigningKey
	found := false
	for _, k := range keys {
		if k.KID == "" || len(k.Secret) == 0 {
			continue
		}
		switch {
		case !found,
			k.CreatedAt.After(best.CreatedAt),
			k.CreatedAt.Equal(best.CreatedAt) && k.KID > best.KID:
			best, found = k, true
		}
	}
	return best, found
}

// encodeBody serialises the part of the payload that travels in the token.
func encodeBody(p TokenPayload) []byte {
	buf := make([]byte, 0, len(p.TenantID)+len(p.DeliveryID)+len(p.Dest)+10)
	buf = appendString(buf, p.TenantID)
	buf = appendString(buf, p.DeliveryID)
	buf = append(buf, byte(p.Kind))
	buf = binary.AppendVarint(buf, int64(p.LinkNo))
	if p.Kind.embedsDest() {
		buf = appendString(buf, p.Dest)
	}
	return buf
}

func decodeBody(body []byte) (TokenPayload, error) {
	var p TokenPayload
	tenant, rest, err := takeString(body)
	if err != nil {
		return p, err
	}
	p.TenantID = tenant
	id, rest, err := takeString(rest)
	if err != nil {
		return p, err
	}
	p.DeliveryID = id
	if len(rest) == 0 {
		return p, fmt.Errorf("%w: no kind", ErrMalformedToken)
	}
	p.Kind = Kind(rest[0])
	rest = rest[1:]
	n, used := binary.Varint(rest)
	if used <= 0 {
		return p, fmt.Errorf("%w: bad link number", ErrMalformedToken)
	}
	p.LinkNo = int(n)
	rest = rest[used:]
	if !p.Kind.Valid() {
		return p, fmt.Errorf("%w: unknown kind %d", ErrMalformedToken, byte(p.Kind))
	}
	if p.Kind.embedsDest() {
		dest, tail, err := takeString(rest)
		if err != nil {
			return p, err
		}
		p.Dest, rest = dest, tail
	}
	if len(rest) != 0 {
		return p, fmt.Errorf("%w: %d trailing bytes", ErrMalformedToken, len(rest))
	}
	return p, nil
}

// computeMAC covers every field, including the destination, whichever side it
// travelled on. Every variable-length field is length-prefixed so that no two
// different payloads can produce the same MAC input.
func computeMAC(secret []byte, p TokenPayload) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(macContext))
	var buf []byte
	buf = appendString(buf, p.TenantID)
	buf = appendString(buf, p.DeliveryID)
	buf = append(buf, byte(p.Kind))
	buf = binary.AppendVarint(buf, int64(p.LinkNo))
	buf = appendString(buf, p.Dest)
	m.Write(buf)
	return m.Sum(nil)[:macSize]
}

func appendString(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func takeString(b []byte) (string, []byte, error) {
	n, used := binary.Uvarint(b)
	if used <= 0 || uint64(len(b)-used) < n {
		return "", nil, fmt.Errorf("%w: truncated field", ErrMalformedToken)
	}
	return string(b[used : used+int(n)]), b[used+int(n):], nil
}

// Route paths of architecture 9.1. control mounts these under the tenant
// tracking domain.
const (
	PathOpen        = "/t/o/"
	PathClick       = "/t/c/"
	PathUnsubscribe = "/t/u/"
	// DestParam is the click destination query parameter.
	DestParam = "u"
)

// OpenURL is the pixel URL for a token. It returns an empty string when the
// tenant has no tracking domain or the token is empty, which is the sender's
// signal to leave the mail untouched.
func OpenURL(domain, token string) string {
	b := base(domain)
	if b == "" || token == "" {
		return ""
	}
	return b + PathOpen + url.PathEscape(token)
}

// ClickURL is the redirect URL for a token and its destination. dest is the
// value the token was signed with; it travels in the query string.
func ClickURL(domain, token, dest string) string {
	b := base(domain)
	if b == "" || token == "" {
		return ""
	}
	return b + PathClick + url.PathEscape(token) +
		"?" + DestParam + "=" + url.QueryEscape(dest)
}

// UnsubscribeURL is the URL that goes into the mail body and the
// List-Unsubscribe header in UnsubscribeSendplane mode.
func UnsubscribeURL(domain, token string) string {
	b := base(domain)
	if b == "" || token == "" {
		return ""
	}
	return b + PathUnsubscribe + url.PathEscape(token)
}

// base turns a configured tracking domain into an origin. A bare domain gets
// https, an explicit scheme is kept (http for local testing), and a trailing
// slash or path is trimmed.
func base(domain string) string {
	d := strings.TrimSpace(domain)
	if d == "" {
		return ""
	}
	if !strings.Contains(d, "://") {
		d = "https://" + d
	}
	return strings.TrimRight(d, "/")
}
