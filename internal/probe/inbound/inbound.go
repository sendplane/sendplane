// Package inbound is the second channel a loopback probe mail can come back
// over: an HTTP webhook that whoever receives the mail calls, instead of an
// IMAP mailbox sendplane polls (architecture 11.1, ADR-0016).
//
// The two channels answer the same question with different evidence. IMAP
// gives the raw RFC 5322 message and the folder it was filed in, so a spam
// verdict is observable. A webhook gives a header block and no folder, which
// is everything the verdict of architecture 11.4 reads except the folder.
//
// A Provider is registered once, process-wide, and configured once, in the
// host's config file: an inbound endpoint is a URL somebody else was pointed
// at, and a URL cannot be per tenant without handing every tenant its own
// hostname. The tenant is resolved from the probe token on the message
// instead (internal/probe.ParseToken), which is one lookup rather than a sweep
// over every tenant — the same trick the public tracking routes use.
//
// The format sendplane ships is inbound/sendplanehook. It is the one to point
// a forwarder at; another provider's native format is a Provider away.
package inbound

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/textproto"
	"sort"
	"sync"
	"time"
)

// MaxBodyBytes is the largest request body any provider will parse. A probe
// mail is a few kilobytes of headers and a two-line body; a megabyte is
// already two orders of magnitude of headroom, and the endpoint is
// unauthenticated until the signature has been checked, which cannot happen
// before the body has been read.
const MaxBodyBytes = 1 << 20

// Errors a Provider returns. The handler turns them into status codes, so no
// provider decides one.
var (
	// ErrBadSignature is returned by Verify when the request did not
	// authenticate: a missing, malformed, stale or wrong signature. It is a
	// 401 and the sender must not retry it unchanged.
	ErrBadSignature = errors.New("inbound: bad signature")
	// ErrBadPayload is returned by Parse when the body is not what the
	// provider documents. It is a 400: the request authenticated, so this
	// really is the provider, and retrying the same bytes cannot help.
	ErrBadPayload = errors.New("inbound: malformed payload")
	// ErrTooLarge is returned when the body is over MaxBodyBytes.
	ErrTooLarge = errors.New("inbound: payload too large")
)

// Message is one delivered mail, normalized across providers. It carries no
// raw RFC 5322 message: a provider that forwards parsed mail does not send
// one, and requiring it would rule the whole channel out (ADR-0016).
// Everything the verdict needs is in Headers.
type Message struct {
	// ProviderID identifies this delivery. It is the idempotency key for the
	// handler's dedupe memo, so it has to be stable across a redelivery of
	// the same mail and distinct between two different mails: a provider's
	// own transaction ID if it has one, BodyID(body) if it does not.
	ProviderID string

	// From and To are the envelope addresses. Nothing in the verdict reads
	// them; they are here because a delivery without a sender is not a mail,
	// and because they are what an operator needs in the log line.
	From string
	To   []string

	// Headers is the message's header block, keyed by
	// textproto.CanonicalMIMEHeaderKey, values in the order they appeared.
	// Order within one name is what matters to the parsers: the Received
	// chain is read bottom-up and "the first Authentication-Results" is a
	// trust decision (internal/probe/headers.go).
	//
	// Whatever forwards the mail MUST preserve Authentication-Results,
	// Received and X-Sendplane-Probe. Without the first two there is no
	// evidence to judge; without the third the delivery cannot be attributed
	// to a tenant at all.
	Headers map[string][]string

	// Text is the plain-text body, when the provider sends one. The probe
	// does not read it — the verdict is made of headers — so it is optional.
	Text string
}

// Header returns the first value of a header, or "".
func (m Message) Header(name string) string {
	v := m.Headers[textproto.CanonicalMIMEHeaderKey(name)]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// Subject is the mail's Subject header. The probe subject carries the run ID,
// which is the only thing left to log when a mail arrives with no probe token.
func (m Message) Subject() string { return m.Header("Subject") }

// BodyID is the ProviderID for a format that has no delivery ID of its own:
// the SHA-256 of the raw body, hex. A redelivery of the same mail is the same
// bytes and therefore the same ID; two different probe mails differ in their
// token and therefore in their ID. It is deliberately *not* over the
// signature, which carries a timestamp and changes on every re-sign.
func BodyID(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Outcome is what sendplane did with one delivered message. It is not an HTTP
// status: the mapping is the handler's (internal/api/probeinbound.go), so that
// every provider answers the same way and none of them can invent a code that
// makes a sender drop probe mail.
type Outcome int

const (
	// OutcomeAccepted: the message was a sendplane probe and its run is now
	// complete, or was already complete (a redelivery). 200.
	OutcomeAccepted Outcome = iota
	// OutcomeIgnored: not a sendplane probe, or not one we can attribute.
	// Nothing was recorded and nothing will be; the sender must not retry,
	// so this is a 200 too and not a 404.
	OutcomeIgnored
	// OutcomeDeferred: sendplane could not record it right now (the store is
	// unavailable). 503, so the sender holds the mail and retries.
	OutcomeDeferred
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAccepted:
		return "accepted"
	case OutcomeIgnored:
		return "ignored"
	case OutcomeDeferred:
		return "deferred"
	}
	return "unknown"
}

// Provider is one webhook body format plus its request authentication.
//
// Adding a format is three methods and an init(). The whole recipe:
//
//	package examplehook
//
//	import (
//		"encoding/json"
//		"fmt"
//		"net/http"
//		"net/textproto"
//
//		"github.com/sendplane/sendplane/internal/probe/inbound"
//	)
//
//	type Provider struct{}
//
//	func init() { inbound.Register(Provider{}) }
//
//	func (Provider) Name() string { return "example" }
//
//	func (Provider) Verify(r *http.Request, body []byte, secrets []string) error {
//		// Raw body bytes, constant-time compare, every secret tried
//		// (rotation). Wrap inbound.ErrBadSignature on failure.
//		return checkHMAC(r.Header.Get("X-Example-Signature"), body, secrets)
//	}
//
//	func (Provider) Parse(body []byte) ([]inbound.Message, error) {
//		var p struct {
//			Sender  string              `json:"sender"`
//			Headers map[string][]string `json:"headers"`
//		}
//		if err := json.Unmarshal(body, &p); err != nil {
//			return nil, fmt.Errorf("%w: %w", inbound.ErrBadPayload, err)
//		}
//		h := make(map[string][]string, len(p.Headers))
//		for name, vs := range p.Headers { // keep the order within one name
//			k := textproto.CanonicalMIMEHeaderKey(name)
//			h[k] = append(h[k], vs...)
//		}
//		return []inbound.Message{{
//			ProviderID: inbound.BodyID(body), // or the provider's own ID
//			From:       p.Sender,
//			Headers:    h,
//		}}, nil
//	}
//
// Then add the package to the side-effect import list in the root probe.go,
// and `probe.webhooks: [{provider: example, secrets: [...]}]` works. A name
// the build does not know is refused at startup with the list of the ones it
// does, never as a route that quietly never fires.
type Provider interface {
	// Name is the provider key used in the config file and in the default
	// route path ("sendplane" -> POST /probe/inbound/sendplane). It must be a
	// plain lowercase word: it goes into a URL path.
	Name() string

	// Verify authenticates the request against the configured secrets, any
	// one of which may match — several is how a secret is rotated without a
	// flag day. body is the raw bytes exactly as they arrived, because that
	// is what a signature covers; nothing may re-marshal them first. It
	// returns an error wrapping ErrBadSignature for anything that does not
	// authenticate, including an empty secret list.
	Verify(r *http.Request, body []byte, secrets []string) error

	// Parse turns one verified body into the messages it carries — one for
	// most formats, several for a batching one. It returns an error wrapping
	// ErrBadPayload for a body the format does not describe. The request is
	// deliberately not passed: a format that keeps the payload in headers
	// cannot be signed as one blob, so everything Parse needs is in body.
	Parse(body []byte) ([]Message, error)
}

// ToleranceSetter is the optional half of Provider, for a signature scheme
// whose header carries a timestamp. Implementing it is what makes
// `probe.webhooks[].tolerance` reach a provider; a provider that does not
// implement it makes that setting a startup error rather than a number that
// quietly does nothing.
type ToleranceSetter interface {
	Provider
	// WithTolerance returns a copy of the provider that accepts a signed
	// timestamp at most d away from the current clock.
	WithTolerance(d time.Duration) Provider
}

// --- registry ----------------------------------------------------------

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}
)

// Register adds a provider to the process-wide registry. It is meant to be
// called from a provider package's init(), so that importing the package for
// its side effect is all a host has to do. Registering the same name twice is
// a programming error and panics.
func Register(p Provider) {
	if p == nil {
		panic("inbound: Register(nil)")
	}
	name := p.Name()
	if name == "" {
		panic("inbound: provider has no name")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("inbound: provider " + name + " is registered twice")
	}
	registry[name] = p
}

// Lookup returns a registered provider by name.
func Lookup(name string) (Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	return p, ok
}

// Names lists the registered providers, sorted, for the error message a
// misspelled config key deserves.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DefaultPath is the route a provider is mounted on when the config names no
// other one.
func DefaultPath(name string) string { return "/probe/inbound/" + name }
