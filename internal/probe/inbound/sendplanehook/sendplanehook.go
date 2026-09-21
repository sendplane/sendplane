// Package sendplanehook is the inbound webhook format sendplane defines
// itself, and the one registered by default.
//
// Importing it for its side effect registers it:
//
//	import _ "github.com/sendplane/sendplane/internal/probe/inbound/sendplanehook"
//
// Why a format of our own rather than a receiving service's native one: the
// probe verdict is made entirely of the headers the receiving MTA wrote
// (architecture 11.4), above all Authentication-Results, and a service that
// does not forward that header cannot serve as a probe channel at all
// (ADR-0016). Rather than depend on any one service's payload, sendplane
// documents the smallest body that carries what the verdict reads, and
// anything that can POST JSON — a mail-receiving service, a Cloudflare Email
// Worker, a procmail recipe, twenty lines of Python on an MX — can produce it.
//
// The body:
//
//	{
//	  "from":    "sender@example.com",
//	  "to":      ["probe@example.net"],
//	  "headers": {"Authentication-Results": ["mx.example.net; spf=pass; ..."],
//	              "Received": ["...newest...", "...oldest..."],
//	              "X-Sendplane-Probe": ["<tenant>/<run>/<mac>"]},
//	  "text":    "optional plain-text body"
//	}
//
// Only `from` and `headers` decide anything; `to` and `text` are recorded and
// optional, and unknown fields are ignored so the format can grow. Whatever
// forwards the mail MUST preserve Authentication-Results, Received and
// X-Sendplane-Probe: without the first two there is no evidence to judge,
// and without the third the delivery cannot be attributed to a tenant.
//
// The request is authenticated with X-Sendplane-Signature, the timestamped
// HMAC-SHA256 scheme of inbound/sigv1.
package sendplanehook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/sendplane/sendplane/internal/probe/inbound"
	"github.com/sendplane/sendplane/internal/probe/inbound/sigv1"
)

// HeaderSignature carries the sigv1 signature of the raw body.
//
//	X-Sendplane-Signature: t=<unix seconds>,v1=<hex>[,v1=<hex>...]
const HeaderSignature = "X-Sendplane-Signature"

// payload is the documented body. Every field is optional to the JSON decoder
// and required-ness is checked in Parse, so that a missing field is a message
// naming it rather than a type error naming a byte offset.
type payload struct {
	From    string              `json:"from"`
	To      []string            `json:"to"`
	Headers map[string][]string `json:"headers"`
	Text    string              `json:"text"`
}

// Provider is sendplane's own inbound webhook format.
type Provider struct {
	// Tolerance is how far the signed timestamp may be from our clock. Zero
	// uses sigv1.DefaultTolerance (5m). It is a field rather than a constant
	// because the only reason to change it is a deployment whose forwarder
	// batches, and that is a property of the deployment.
	Tolerance time.Duration

	// now is the clock the timestamp is checked against. Nil means
	// time.Now; only the tests set it.
	now func() time.Time
}

func init() { inbound.Register(Provider{}) }

// Name is the provider key in `probe.webhooks[].provider` and the last element
// of the default route, POST /probe/inbound/sendplane.
func (Provider) Name() string { return "sendplane" }

// WithTolerance implements inbound.ToleranceSetter.
func (p Provider) WithTolerance(d time.Duration) inbound.Provider {
	p.Tolerance = d
	return p
}

// Verify checks X-Sendplane-Signature over the raw body. The body must not be
// parsed first: the digest covers the bytes as sent, and re-marshalled JSON is
// not those bytes.
func (p Provider) Verify(r *http.Request, body []byte, secrets []string) error {
	return sigv1.Verify(r.Header.Get(HeaderSignature), body, secrets, p.clock(), p.Tolerance)
}

func (p Provider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Parse reads one delivery. The format carries one message per request.
func (Provider) Parse(body []byte) ([]inbound.Message, error) {
	if len(body) > inbound.MaxBodyBytes {
		return nil, inbound.ErrTooLarge
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("%w: %w", inbound.ErrBadPayload, err)
	}
	// `from` is the one field that proves the body is a forwarded mail at all.
	// Without it, a body of `{}` would be answered 200 and silently dropped,
	// which is the failure an operator wiring up a forwarder must not get.
	if strings.TrimSpace(p.From) == "" {
		return nil, fmt.Errorf("%w: `from` is required", inbound.ErrBadPayload)
	}
	return []inbound.Message{{
		// No transaction ID exists in this format, so the body's own digest
		// is the delivery identity: a redelivery is the same bytes, and two
		// probe mails differ at least in their token.
		ProviderID: inbound.BodyID(body),
		From:       strings.TrimSpace(p.From),
		To:         p.To,
		Headers:    canonical(p.Headers),
		Text:       p.Text,
	}}, nil
}

// canonical re-keys the header map with textproto.CanonicalMIMEHeaderKey, so
// that a lookup does not depend on how the forwarder happened to capitalize a
// name, and so that two spellings of one name fold together. The order of the
// values *within* one name is preserved: the Received chain is read bottom-up
// and "the first Authentication-Results" is a trust decision.
//
// A name that is not a valid header token is dropped rather than canonicalized
// into something else — textproto would return it unchanged, and a key with a
// space or a colon in it cannot have come from a header block.
func canonical(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for name, values := range in {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " :\r\n\t") {
			continue
		}
		key := textproto.CanonicalMIMEHeaderKey(name)
		out[key] = append(out[key], values...)
	}
	return out
}

// Sign is the header value a forwarder writes for a body. It is exported so
// that the tests, the e2e harness and anybody wiring up a forwarder sign the
// way the format documents rather than the way this file happens to verify.
func Sign(secret string, t time.Time, body []byte) string { return sigv1.Sign(secret, t, body) }
