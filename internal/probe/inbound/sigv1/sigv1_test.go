package sigv1

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/probe/inbound"
)

var (
	now  = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	body = []byte(`{"from":"probe@example.com","to":["probe@example.net"]}`)
)

func TestVerifyAcceptsAFreshSignature(t *testing.T) {
	h := Sign("whsec_top", now, body)
	if err := Verify(h, body, []string{"whsec_top"}, now, 0); err != nil {
		t.Fatalf("a signature this package produced did not verify: %v", err)
	}
	// The header is the documented wire format, not just something round-trip
	// compatible with itself: anything else signing for us has to match it.
	if !strings.HasPrefix(h, "t="+strconv.FormatInt(now.Unix(), 10)+",v1=") {
		t.Fatalf("header = %q, want t=<unix>,v1=<hex>", h)
	}
	if hex := strings.TrimPrefix(h, "t="+strconv.FormatInt(now.Unix(), 10)+",v1="); len(hex) != 64 {
		t.Fatalf("v1 is %d hex chars, want 64 (sha256)", len(hex))
	}
}

func TestVerifyRejectsTheWrongSecret(t *testing.T) {
	err := Verify(Sign("right", now, body), body, []string{"wrong"}, now, 0)
	if !errors.Is(err, inbound.ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

// The digest covers "<t>.<body>", so a body that was tampered with after
// signing must not verify even though the timestamp still does.
func TestVerifyRejectsATamperedBody(t *testing.T) {
	h := Sign("s", now, body)
	if err := Verify(h, []byte(`{"from":"attacker@example.com"}`), []string{"s"}, now, 0); !errors.Is(err, inbound.ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsAStaleOrFutureTimestamp(t *testing.T) {
	const tol = 5 * time.Minute
	for name, signedAt := range map[string]time.Time{
		"expired": now.Add(-tol - time.Second),
		// A future timestamp is rejected too: a signature made with a clock
		// far ahead of ours stays replayable for as long as the skew lasts.
		"future": now.Add(tol + time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			err := Verify(Sign("s", signedAt, body), body, []string{"s"}, now, tol)
			if !errors.Is(err, inbound.ErrBadSignature) {
				t.Fatalf("err = %v, want ErrBadSignature", err)
			}
			if !strings.Contains(err.Error(), "tolerance") {
				t.Errorf("err = %v, want it to name the tolerance", err)
			}
		})
	}
	// Just inside the window, both ways.
	for _, d := range []time.Duration{-tol + time.Second, tol - time.Second} {
		if err := Verify(Sign("s", now.Add(d), body), body, []string{"s"}, now, tol); err != nil {
			t.Errorf("a signature %s off was rejected: %v", d, err)
		}
	}
}

func TestVerifyDefaultsTheTolerance(t *testing.T) {
	if err := Verify(Sign("s", now.Add(-DefaultTolerance+time.Second), body), body,
		[]string{"s"}, now, 0); err != nil {
		t.Fatalf("tolerance 0 did not fall back to DefaultTolerance: %v", err)
	}
	if err := Verify(Sign("s", now.Add(-DefaultTolerance-time.Second), body), body,
		[]string{"s"}, now, 0); !errors.Is(err, inbound.ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

// Rotation, sender side: a delivery may carry a v1 for the new secret and one
// for the old, and either matching is enough.
func TestVerifyAcceptsOneOfSeveralV1Elements(t *testing.T) {
	ts := strconv.FormatInt(now.Unix(), 10)
	mine := strings.TrimPrefix(Sign("mine", now, body), "t="+ts+",")
	theirs := strings.TrimPrefix(Sign("theirs", now, body), "t="+ts+",")

	for name, h := range map[string]string{
		"ours first":  "t=" + ts + "," + mine + "," + theirs,
		"ours second": "t=" + ts + "," + theirs + "," + mine,
	} {
		if err := Verify(h, body, []string{"mine"}, now, 0); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// None of them ours is still a rejection.
	h := "t=" + ts + "," + theirs + "," + theirs
	if err := Verify(h, body, []string{"mine"}, now, 0); !errors.Is(err, inbound.ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature", err)
	}
}

// Rotation, our side: both the outgoing and the incoming secret are configured
// for as long as deliveries signed with either may arrive.
func TestVerifyAcceptsAnyConfiguredSecret(t *testing.T) {
	for _, signer := range []string{"old", "new"} {
		if err := Verify(Sign(signer, now, body), body, []string{"new", "old"}, now, 0); err != nil {
			t.Errorf("a delivery signed with the %s secret was rejected: %v", signer, err)
		}
	}
}

// No secret is a refusal, not a "skip verification": an unsigned inbound
// endpoint lets anybody complete anybody's probe run.
func TestVerifyRefusesWithoutASecret(t *testing.T) {
	for name, secrets := range map[string][]string{
		"nil":   nil,
		"empty": {"", " "},
	} {
		err := Verify(Sign("s", now, body), body, secrets, now, 0)
		if !errors.Is(err, inbound.ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func TestVerifyRejectsAMalformedHeader(t *testing.T) {
	ts := strconv.FormatInt(now.Unix(), 10)
	good := strings.TrimPrefix(Sign("s", now, body), "t="+ts+",")
	for name, h := range map[string]string{
		"empty":           "",
		"blank":           "   ",
		"no elements":     "garbage",
		"no timestamp":    good,
		"no v1":           "t=" + ts,
		"timestamp words": "t=yesterday," + good,
		"v1 not hex":      "t=" + ts + ",v1=zzzz",
		"v1 too short":    "t=" + ts + ",v1=abcd",
		"two timestamps":  "t=" + ts + ",t=1," + good,
		// The scheme is bare hex with no prefix; a "sha256=" prefix is a
		// different scheme and must not be guessed at.
		"prefixed v1": "t=" + ts + ",v1=sha256=" + strings.TrimPrefix(good, "v1="),
	} {
		if err := Verify(h, body, []string{"s"}, now, 0); !errors.Is(err, inbound.ErrBadSignature) {
			t.Errorf("%s (%q): err = %v, want ErrBadSignature", name, h, err)
		}
	}
}

// An element this build does not know is ignored rather than rejected, so that
// a future v2= can be added to a delivery without breaking this verifier.
func TestVerifyIgnoresUnknownElements(t *testing.T) {
	h := Sign("s", now, body) + ",v2=whatever,x=1"
	if err := Verify(h, body, []string{"s"}, now, 0); err != nil {
		t.Fatalf("an unknown element broke verification: %v", err)
	}
}

// Whitespace around the elements is tolerated: the scheme does not forbid it
// and a hand-written forwarder will produce it.
func TestVerifyToleratesWhitespace(t *testing.T) {
	ts := strconv.FormatInt(now.Unix(), 10)
	v1 := strings.TrimPrefix(Sign("s", now, body), "t="+ts+",")
	if err := Verify("  t = "+ts+" , "+v1+" ", body, []string{"s"}, now, 0); err != nil {
		t.Fatalf("whitespace broke verification: %v", err)
	}
}

// The separator is what stops a timestamp's digits from being confused with a
// body's leading bytes. Without it, t=1 over "23{...}" and t=12 over "3{...}"
// would produce the same digest, and a signature could be moved between them.
func TestSignSeparatesTheTimestampFromTheBody(t *testing.T) {
	a := Sign("s", time.Unix(1, 0), []byte("23x"))
	b := Sign("s", time.Unix(12, 0), []byte("3x"))
	if strings.TrimPrefix(a, "t=1,") == strings.TrimPrefix(b, "t=12,") {
		t.Fatal("the digest does not separate the timestamp from the body")
	}
}

// The secret is used verbatim. A sender that prefixes its secrets signs with
// the prefix included, so trimming anything would break every signature.
func TestSignUsesTheSecretVerbatim(t *testing.T) {
	if Sign("whsec_abc", now, body) == Sign("abc", now, body) {
		t.Fatal("the secret prefix was stripped")
	}
}
