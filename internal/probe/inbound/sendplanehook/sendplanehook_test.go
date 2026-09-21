package sendplanehook

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/probe/inbound"
	"github.com/sendplane/sendplane/internal/probe/inbound/sigv1"
)

const secret = "whsec_sendplanehook-test"

var at = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// sample is the documented body, with the headers a probe verdict actually
// reads (architecture 11.4) and a couple of fields the format does not
// document, which must be ignored rather than refused.
const sample = `{
  "from": "news@example.com",
  "to": ["probe@example.net"],
  "headers": {
    "received": [
      "from mx.example.net by inbox.example.net; Tue, 22 Sep 2026 12:00:30 +0000",
      "from mail.example.com (mail.example.com [203.0.113.7]) by mx.example.net with ESMTPS; Tue, 22 Sep 2026 12:00:10 +0000"
    ],
    "Authentication-Results": ["mx.example.net; spf=pass; dkim=pass header.d=example.com; dmarc=pass"],
    "x-sendplane-probe": ["tenant/run/mac"],
    "Subject": ["[sendplane probe run-1]"]
  },
  "text": "sendplane loopback health probe.\r\n",
  "spool_id": 42,
  "attachments": []
}`

// at() is the provider under test with a fixed clock, so that a timestamp
// rule is asserted rather than raced against the wall clock.
func fixed() Provider { return Provider{now: func() time.Time { return at }} }

func post(t *testing.T, body string, header string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/probe/inbound/sendplane", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if header != "" {
		r.Header.Set(HeaderSignature, header)
	}
	return r
}

func TestVerify(t *testing.T) {
	body := []byte(sample)
	ts := strconv.FormatInt(at.Unix(), 10)
	v1 := func(sec string, signedAt time.Time) string {
		return strings.TrimPrefix(Sign(sec, signedAt, body), "t="+strconv.FormatInt(signedAt.Unix(), 10)+",")
	}

	cases := []struct {
		name    string
		header  string
		secrets []string
		wantErr bool
	}{
		{name: "valid", header: Sign(secret, at, body), secrets: []string{secret}},
		{name: "wrong secret", header: Sign("nope", at, body), secrets: []string{secret}, wantErr: true},
		{name: "no signature header", header: "", secrets: []string{secret}, wantErr: true},
		{name: "no secret configured", header: Sign(secret, at, body), secrets: nil, wantErr: true},
		{
			name: "expired timestamp", wantErr: true,
			header:  Sign(secret, at.Add(-sigv1.DefaultTolerance-time.Minute), body),
			secrets: []string{secret},
		},
		{
			name: "future timestamp beyond tolerance", wantErr: true,
			header:  Sign(secret, at.Add(sigv1.DefaultTolerance+time.Minute), body),
			secrets: []string{secret},
		},
		{
			// Rotation on the sender's side.
			name:    "several v1, one of them ours",
			header:  "t=" + ts + "," + v1("theirs", at) + "," + v1(secret, at),
			secrets: []string{secret},
		},
		{
			// Rotation on ours: both are configured while either may arrive.
			name:    "rotated secrets, signed with the old one",
			header:  Sign("previous", at, body),
			secrets: []string{secret, "previous"},
		},
		{name: "malformed header", header: "t=whenever,v1=zz", secrets: []string{secret}, wantErr: true},
		{name: "no timestamp", header: v1(secret, at), secrets: []string{secret}, wantErr: true},
		{
			name: "tampered body", wantErr: true,
			header: Sign(secret, at, []byte(`{"from":"attacker@example.com"}`)), secrets: []string{secret},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fixed().Verify(post(t, sample, tc.header), body, tc.secrets)
			switch {
			case tc.wantErr && !errors.Is(err, inbound.ErrBadSignature):
				t.Fatalf("err = %v, want ErrBadSignature", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// WithTolerance is what carries `probe.webhooks[].tolerance` into the
// provider; without it the setting would be a number that does nothing.
func TestWithTolerance(t *testing.T) {
	body := []byte(sample)
	old := Sign(secret, at.Add(-30*time.Minute), body)

	if err := fixed().Verify(post(t, sample, old), body, []string{secret}); !errors.Is(err, inbound.ErrBadSignature) {
		t.Fatalf("a 30m old signature passed the default tolerance: %v", err)
	}
	wide := fixed().WithTolerance(time.Hour)
	if err := wide.Verify(post(t, sample, old), body, []string{secret}); err != nil {
		t.Fatalf("a 30m old signature failed a 1h tolerance: %v", err)
	}
	// And the default instance is unchanged: WithTolerance returns a copy.
	if err := fixed().Verify(post(t, sample, old), body, []string{secret}); err == nil {
		t.Fatal("WithTolerance mutated the receiver")
	}
	if _, ok := any(Provider{}).(inbound.ToleranceSetter); !ok {
		t.Error("Provider does not implement inbound.ToleranceSetter, so tolerance cannot be configured")
	}
}

func TestParse(t *testing.T) {
	msgs, err := Provider{}.Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]

	if m.From != "news@example.com" || len(m.To) != 1 || m.To[0] != "probe@example.net" {
		t.Errorf("from/to = %q/%v", m.From, m.To)
	}
	if !strings.HasPrefix(m.Text, "sendplane loopback") {
		t.Errorf("text = %q", m.Text)
	}

	// Header names are canonicalized, so a forwarder's capitalization cannot
	// hide a header from the parsers.
	for _, name := range []string{"Received", "Authentication-Results", "X-Sendplane-Probe"} {
		if len(m.Headers[name]) == 0 {
			t.Errorf("header %q is missing; keys are %v", name, keys(m.Headers))
		}
	}
	if got := m.Header("x-sendplane-probe"); got != "tenant/run/mac" {
		t.Errorf("Header is not case-insensitive: %q", got)
	}
	if got := m.Subject(); got != "[sendplane probe run-1]" {
		t.Errorf("Subject = %q", got)
	}

	// The order of the values within one name is the contract: the Received
	// chain is read bottom-up, and the *first* Authentication-Results is the
	// trust decision.
	if !strings.HasPrefix(m.Headers["Received"][0], "from mx.example.net") {
		t.Errorf("the Received chain was reordered: %q", m.Headers["Received"][0])
	}

	// No transaction ID exists in this format, so the delivery identity is
	// the body digest: stable for a redelivery, distinct between two mails.
	again, err := Provider{}.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if m.ProviderID != again[0].ProviderID || len(m.ProviderID) != 64 {
		t.Errorf("ProviderID = %q, want a stable 64-char sha256 hex", m.ProviderID)
	}
	other, err := Provider{}.Parse([]byte(strings.Replace(sample, "tenant/run/mac", "tenant/run2/mac", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if other[0].ProviderID == m.ProviderID {
		t.Error("two different probe mails share a ProviderID; the dedupe memo would drop one")
	}
}

func TestParseRejectsABodyThatIsNotTheFormat(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `not json at all`,
		"json array":    `[]`,
		"empty object":  `{}`,
		"no from":       `{"to":["probe@example.net"],"headers":{}}`,
		"blank from":    `{"from":"   ","headers":{}}`,
		"from is a nul": `{"from":null,"headers":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (Provider{}).Parse([]byte(body)); !errors.Is(err, inbound.ErrBadPayload) {
				t.Fatalf("err = %v, want ErrBadPayload", err)
			}
		})
	}
}

// `to`, `text` and `headers` are all optional: a forwarded mail with no
// plain-text part is still a probe, and the missing pieces are the operator's
// problem to see in the verdict, not a 400.
func TestParseAcceptsTheMinimalBody(t *testing.T) {
	msgs, err := Provider{}.Parse([]byte(`{"from":"a@example.com"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msgs) != 1 || msgs[0].From != "a@example.com" {
		t.Fatalf("got %+v", msgs)
	}
	if msgs[0].Headers == nil {
		t.Error("Headers is nil; Header() must be safe to call on any parsed message")
	}
}

// A key that cannot have come from a header block is dropped rather than
// canonicalized into something that looks like a real header.
func TestParseDropsImpossibleHeaderNames(t *testing.T) {
	msgs, err := Provider{}.Parse([]byte(
		`{"from":"a@example.com","headers":{"":["x"],"Subject: x":["y"],"a b":["z"],"Ok":["v"]}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := keys(msgs[0].Headers); len(got) != 1 || got[0] != "Ok" {
		t.Fatalf("headers = %v, want only [Ok]", got)
	}
}

func TestParseRefusesAnOversizedBody(t *testing.T) {
	big := `{"from":"a@example.com","text":"` + strings.Repeat("x", inbound.MaxBodyBytes) + `"}`
	if _, err := (Provider{}).Parse([]byte(big)); !errors.Is(err, inbound.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestRegisteredByDefault(t *testing.T) {
	p, ok := inbound.Lookup("sendplane")
	if !ok {
		t.Fatalf("sendplane did not register itself; registered: %v", inbound.Names())
	}
	if _, isOurs := p.(Provider); !isOurs {
		t.Fatalf("the registered provider is %T", p)
	}
	if got := inbound.DefaultPath(p.Name()); got != "/probe/inbound/sendplane" {
		t.Errorf("default path = %q", got)
	}
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
