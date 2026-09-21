// Package sigv1 implements the timestamped HMAC-SHA256 request signature
// sendplane's own inbound webhook format uses, and that any other provider
// speaking the same scheme can reuse.
//
// The scheme is the one documented for conduit's webhook verifier
// (https://conduit-v2.mintlify.app/webhook-verifier), which is where it was
// taken from, and it is worth copying rather than inventing for three
// reasons: the timestamp inside the signed string makes a replay detectable
// without storing anything, the repeated `v1=` element makes a secret
// rotation a non-event for the sender, and the wire format is one short
// header that `openssl dgst` can produce in a shell one-liner.
//
//	X-Sendplane-Signature: t=<unix seconds>,v1=<hex>[,v1=<hex>...]
//	v1 = hex(HMAC-SHA256(key = secret, msg = "<t>" + "." + <raw body bytes>))
//
// Two rules decide whether an implementation is correct, and both are about
// bytes rather than about crypto:
//
//  1. The digest covers the *raw* body. Re-marshalled JSON is not the bytes
//     that were signed, so the body has to be read and verified before it is
//     parsed.
//  2. The secret is used verbatim, whatever it looks like. A sender that
//     prefixes its secrets (`whsec_...`) signs with the prefix included, so
//     stripping anything here would break every signature.
package sigv1

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sendplane/sendplane/internal/probe/inbound"
)

// DefaultTolerance is how far the signed timestamp may be from our clock. It
// is the value the scheme documents. It bounds a replay of a captured request
// without any server-side state, and it is also the whole allowance for clock
// skew between the two machines, so it cannot be tightened very far.
const DefaultTolerance = 5 * time.Minute

// Element names of the header. Anything else is ignored rather than rejected,
// so that a future `v2=` can be added to a delivery without breaking a
// verifier that only understands v1 — which is the reason the elements are
// named in the first place.
const (
	elemTimestamp = "t"
	elemV1        = "v1"
)

// Sign returns the header value for a body, as a sender writes it. The e2e
// harness, the tests and the `curl` example in internal/probe/README.md all go
// through this, so what is verified is never merely what this package happens
// to produce.
func Sign(secret string, t time.Time, body []byte) string {
	unix := t.Unix()
	return elemTimestamp + "=" + strconv.FormatInt(unix, 10) +
		"," + elemV1 + "=" + digest(secret, unix, body)
}

// Verify checks a header value against every configured secret.
//
// It accepts when *any* `v1` matches *any* secret: two v1 elements is how a
// sender rolls a secret without a flag day, and two configured secrets is how
// a receiver does the same. Rejecting a stale or future timestamp comes first,
// because a signature that verifies is still a replay if it is old.
//
// now and tolerance are parameters rather than reads of the wall clock so that
// the expiry rule is testable; tolerance <= 0 means DefaultTolerance.
func Verify(header string, body []byte, secrets []string, now time.Time, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	keys := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			keys = append(keys, s)
		}
	}
	if len(keys) == 0 {
		// Not "skip verification": an unsigned inbound endpoint would let
		// anybody complete anybody's probe run, so no secret is a refusal.
		return fmt.Errorf("%w: no webhook secret is configured", inbound.ErrBadSignature)
	}

	unix, sums, err := parse(header)
	if err != nil {
		return err
	}
	if skew := now.Sub(time.Unix(unix, 0)); skew > tolerance || skew < -tolerance {
		return fmt.Errorf("%w: timestamp is %s off our clock, over the %s tolerance",
			inbound.ErrBadSignature, skew.Round(time.Second), tolerance)
	}

	for _, key := range keys {
		want, err := hex.DecodeString(digest(key, unix, body))
		if err != nil { // unreachable: digest() returns hex
			return fmt.Errorf("%w: %w", inbound.ErrBadSignature, err)
		}
		for _, got := range sums {
			// Constant time: a byte-at-a-time compare on a value an attacker
			// can retry leaks the expected digest one byte per round.
			if hmac.Equal(got, want) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: no %s= element matches a configured secret", inbound.ErrBadSignature, elemV1)
}

// parse splits the header into its timestamp and its candidate digests.
func parse(header string) (int64, [][]byte, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, nil, fmt.Errorf("%w: no signature header", inbound.ErrBadSignature)
	}
	var (
		unix    int64
		haveTS  bool
		digests [][]byte
	)
	for _, elem := range strings.Split(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(elem), "=")
		if !ok {
			return 0, nil, fmt.Errorf("%w: %q is not name=value", inbound.ErrBadSignature, elem)
		}
		switch strings.TrimSpace(name) {
		case elemTimestamp:
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return 0, nil, fmt.Errorf("%w: t=%q is not unix seconds", inbound.ErrBadSignature, value)
			}
			if haveTS && n != unix {
				return 0, nil, fmt.Errorf("%w: two different t= elements", inbound.ErrBadSignature)
			}
			unix, haveTS = n, true
		case elemV1:
			sum, err := hex.DecodeString(strings.TrimSpace(value))
			if err != nil || len(sum) != sha256.Size {
				return 0, nil, fmt.Errorf("%w: v1= is not a %d-byte hex digest",
					inbound.ErrBadSignature, sha256.Size)
			}
			digests = append(digests, sum)
		default:
			// A scheme this build does not know about. Ignored on purpose so
			// that a sender can add one without breaking this verifier.
		}
	}
	if !haveTS {
		return 0, nil, fmt.Errorf("%w: no t= element", inbound.ErrBadSignature)
	}
	if len(digests) == 0 {
		return 0, nil, fmt.Errorf("%w: no v1= element", inbound.ErrBadSignature)
	}
	return unix, digests, nil
}

// digest is the hex HMAC of "<t>.<body>". The separator is what stops a
// timestamp's trailing digits from being confused with a body's leading
// bytes — without it, t=1 with body "23..." and t=12 with body "3..." would
// sign the same string.
func digest(secret string, unix int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(unix, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
