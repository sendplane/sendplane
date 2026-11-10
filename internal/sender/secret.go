package sender

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sendplane/sendplane/store"
)

// Sensitive deliveries (ADR-0019): a delivery created with secret_vars carries
// them encrypted, and the sender is the only place they are ever decrypted.
// What happens around that is kept in this file:
//
//   - openSecret refuses, before anything is rendered or dialled, a secret
//     that is no longer pending, that has expired or that does not decrypt.
//     There is no fallback: a secret that cannot be read is a delivery that
//     fails, never one that goes out with an empty code.
//   - the decrypted values become render.Bindings.Secret and nothing else:
//     not host.RecipientContext.Vars, not the unsubscribe URL template.
//   - settleSensitive scrubs the values out of whatever error text the result
//     carries (a template error, an SMTP reply, a BeforeSend error may all
//     quote the message), and fails a retry that would only be due after the
//     secret has expired.

// Fixed failure texts. They say what went wrong without saying anything about
// the secret.
const (
	secretUnavailableError = "secret unavailable"
	// templateRequiresSecretError fails a delivery with no secret_vars whose
	// version reads secret.*.
	templateRequiresSecretError = "template requires secret_vars"
	//nolint:gosec // G101: an error message that names the failure, not a credential.
	secretDecryptFailedError = "secret decrypt failed"
	// redactedSecret replaces a secret value found in an error text.
	redactedSecret = "[secret]"
	// minRedactLen is the shortest secret value that is scrubbed from error
	// texts. Replacing every "1" or "ab" in an SMTP reply would make it
	// unreadable, and a value that short carries nothing worth hiding.
	minRedactLen = 3
)

// secretVars is the decrypted secret_vars of one delivery, alive for the
// duration of one process call.
type secretVars struct {
	vars map[string]any
	// needles are the textual forms of the values, longest first, that
	// redact removes.
	needles []string
}

// openSecret decrypts a sensitive delivery's secret. ok=false means the
// delivery must not be rendered and res is the terminal result to commit.
func (s *Sender) openSecret(ctx context.Context, d *store.Delivery, now time.Time) (sec *secretVars, res store.DeliveryResult, ok bool) {
	if d.SecretState != store.SecretPending || len(d.SecretCiphertext) == 0 {
		// Consumed, discarded or expired already, or a row whose ciphertext
		// is gone: the message has been sent or must never be. This is the
		// last of the layers that keep a sensitive delivery from going out a
		// second time (ADR-0019 3).
		return nil, s.failResult(*d, store.ErrorClassPermanent, 0, secretUnavailableError, now), false
	}
	if !d.SecretExpiresAt.IsZero() && !d.SecretExpiresAt.After(now) {
		return nil, s.failResult(*d, store.ErrorClassPermanent, 0, store.SecretExpiredError, now), false
	}
	if s.cfg.Secrets == nil {
		// Unlike a transport password there is no "stored as-is" reading: the
		// API never stores secret_vars without a cipher, so bytes that are
		// here without one are not plaintext anybody should send.
		s.cfg.Logger.Error("sendplane: sensitive delivery but no SecretCipher configured", "delivery", d.ID)
		return nil, s.failResult(*d, store.ErrorClassPermanent, 0, secretDecryptFailedError, now), false
	}
	plain, err := s.cfg.Secrets.Decrypt(ctx, d.SecretCiphertext, store.DeliverySecretContext(d))
	if err != nil {
		if ctx.Err() != nil {
			return nil, s.internalDefer(*d, "secret decrypt", ctx.Err()), false
		}
		// The cipher's error is logged without the ciphertext; a ciphertext
		// bound to another row or recipient lands here (ADR-0019 1).
		s.cfg.Logger.Warn("sendplane: sensitive delivery secret did not decrypt",
			"delivery", d.ID, "err", err)
		return nil, s.failResult(*d, store.ErrorClassPermanent, 0, secretDecryptFailedError, now), false
	}
	var vars map[string]any
	err = json.Unmarshal(plain, &vars)
	clear(plain)
	if err != nil {
		// Not the json error: it can quote the input.
		return nil, s.failResult(*d, store.ErrorClassPermanent, 0, secretDecryptFailedError, now), false
	}
	if vars == nil {
		vars = map[string]any{}
	}
	return &secretVars{vars: vars, needles: secretNeedles(vars)}, store.DeliveryResult{}, true
}

// settleSensitive is applied to every result of a sensitive delivery that got
// past openSecret.
func (s *Sender) settleSensitive(d *store.Delivery, sec *secretVars, res store.DeliveryResult) store.DeliveryResult {
	if res.DeliveryID == "" {
		return res // lease lost: nothing is committed
	}
	res.Error = sec.redact(res.Error)
	if res.Attempt != nil {
		a := *res.Attempt
		a.Error = sec.redact(a.Error)
		res.Attempt = &a
	}
	// A retry due after the secret expires could only ever fail with "secret
	// expired"; failing it now saves the claim and says why. The attempt row
	// keeps what really happened (the 4xx), the delivery says why it stops.
	if !res.NewStatus.Terminal() && !d.SecretExpiresAt.IsZero() && res.NextAttemptAt.After(d.SecretExpiresAt) {
		res.NewStatus = store.DeliveryFailed
		res.NextAttemptAt = time.Time{}
		res.ErrorClass = store.ErrorClassPermanent
		res.Error = store.SecretExpiredError
	}
	return res
}

// redact replaces every secret value in msg. It is nil-safe so the send path
// can call it whether or not the delivery is sensitive.
func (sec *secretVars) redact(msg string) string {
	if sec == nil || msg == "" {
		return msg
	}
	for _, n := range sec.needles {
		msg = strings.ReplaceAll(msg, n, redactedSecret)
	}
	return msg
}

// redactErr is redact for an error that is about to be logged or stored.
func (sec *secretVars) redactErr(err error) error {
	if sec == nil || err == nil {
		return err
	}
	return errors.New(sec.redact(err.Error()))
}

// secretNeedles collects the forms a secret value can take in an error text:
// as rendered, HTML-escaped (the HTML part) and query-escaped (a link).
func secretNeedles(vars map[string]any) []string {
	seen := map[string]bool{}
	var walk func(v any)
	add := func(s string) {
		for _, form := range []string{s, html.EscapeString(s), url.QueryEscape(s)} {
			if len(form) >= minRedactLen {
				seen[form] = true
			}
		}
	}
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			add(x)
		case float64:
			add(strconv.FormatFloat(x, 'f', -1, 64))
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(vars)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	// Longest first, so a value that contains another is replaced whole.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}
