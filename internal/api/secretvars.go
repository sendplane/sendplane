package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sendplane/sendplane/internal/render"
	"github.com/sendplane/sendplane/store"
)

// secret_vars (ADR-0019): per-recipient values that are encrypted before they
// are stored, rendered as `{{ secret.* }}` and dropped once the delivery is
// sent or finishes.
//
// Nothing in this file may put a secret value into an error, a log line or a
// response. Errors name the recipient's index and the key, never the value.
const (
	// maxSecretVarsBytes caps one recipient's secret_vars as serialized JSON.
	// It is fixed rather than a Limits field: a secret is a code or a link,
	// and anything near this size is not one.
	maxSecretVarsBytes = 8 << 10

	defaultSecretTTL    = 600 * time.Second
	minSecretTTLSeconds = 30
	maxSecretTTLSeconds = 86400
)

// secretPlan is what checkSecretVars decided for one request: whether it
// carries secrets at all, and how long they live.
type secretPlan struct {
	enabled bool
	ttl     time.Duration
}

// checkSecretVars validates a request's secret_vars against the version it
// will render, before a single row is written:
//
//   - secret_ttl_seconds is in range, and only given alongside secret_vars;
//   - secret_vars need a SecretCipher: there is no plaintext fallback for a
//     secret, unlike the stored transport passwords;
//   - a version that reads `secret.*` gets every top-level key it reads from
//     every recipient, because a mail with an empty one-time code is the worst
//     outcome there is;
//   - a version that reads no `secret.*` gets no secret_vars, because a
//     secret nothing renders is a secret stored for nothing;
//   - each recipient's secret_vars fit maxSecretVarsBytes.
func (s *server) checkSecretVars(in *MessageRequest, v *store.MessageVersion) (secretPlan, error) {
	plan := secretPlan{ttl: defaultSecretTTL}
	for _, r := range in.To {
		if r.SecretVars != nil {
			plan.enabled = true
			break
		}
	}
	if in.SecretTtlSeconds != nil {
		if !plan.enabled {
			return plan, errInvalid("secret_ttl_seconds is only allowed when a recipient has secret_vars")
		}
		n := *in.SecretTtlSeconds
		if n < minSecretTTLSeconds || n > maxSecretTTLSeconds {
			return plan, errInvalid("secret_ttl_seconds must be between %d and %d, got %d",
				minSecretTTLSeconds, maxSecretTTLSeconds, n)
		}
		plan.ttl = time.Duration(n) * time.Second
	}
	if plan.enabled && s.deps.Secrets == nil {
		return plan, newErr(http.StatusUnprocessableEntity, ErrorCodeSecretCipherRequired,
			"secret_vars need a secret cipher, and this server has none configured")
	}

	keys := render.SecretKeys(v)
	if len(keys) == 0 {
		if plan.enabled {
			return plan, errInvalid("the message version reads no secret.* variable, so secret_vars would never be used")
		}
		return plan, nil
	}
	for i, r := range in.To {
		var have map[string]any
		if r.SecretVars != nil {
			have = *r.SecretVars
		}
		for _, k := range keys {
			if _, ok := have[k]; !ok {
				e := errInvalid("to[%d].secret_vars has no %q, which the message version reads as secret.%s", i, k, k)
				e.details = []ErrorDetail{{
					Field:   ptr(fmt.Sprintf("to[%d].secret_vars.%s", i, k)),
					Message: "required by the message version",
				}}
				return plan, e
			}
		}
	}
	for i, r := range in.To {
		if r.SecretVars == nil {
			continue
		}
		b, err := json.Marshal(*r.SecretVars)
		n := len(b)
		clear(b)
		if err != nil {
			return plan, errInvalid("to[%d].secret_vars is not serializable", i)
		}
		if n > maxSecretVarsBytes {
			return plan, newErr(http.StatusUnprocessableEntity, ErrorCodeLimitExceeded,
				"to[%d].secret_vars is %d bytes of JSON, the maximum is %d", i, n, maxSecretVarsBytes)
		}
	}
	return plan, nil
}

// sealSecret makes d sensitive. A delivery that will be sent gets its
// secret_vars encrypted under DeliverySecretContext, which binds the tenant,
// the delivery ID and the recipient, so d must already carry all three. A
// suppressed one is never sent, so nothing is encrypted or stored for it at
// all: it is recorded as discarded from the start.
//
// The serialized plaintext is zeroed once it has been encrypted. The decoded
// request still holds the values until it is collected; that is the most a
// garbage-collected handler can promise.
func (s *server) sealSecret(ctx context.Context, d *store.Delivery, index int, vars map[string]any, plan secretPlan, now time.Time) error {
	if d.Status == store.DeliverySuppressed {
		d.SecretState = store.SecretDiscarded
		return nil
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return errInvalid("to[%d].secret_vars is not serializable", index)
	}
	ct, err := s.deps.Secrets.Encrypt(ctx, b, store.DeliverySecretContext(d))
	clear(b)
	if err != nil {
		// The cipher's error, never the input: an implementation that echoed
		// its plaintext would otherwise put it in the request log.
		return fmt.Errorf("api: encrypting to[%d].secret_vars: %w", index, err)
	}
	d.SecretCiphertext = ct
	d.SecretState = store.SecretPending
	d.SecretExpiresAt = now.Add(plan.ttl)
	return nil
}

func secretStateOut(v store.SecretState) SecretState { return SecretState(v.String()) }
