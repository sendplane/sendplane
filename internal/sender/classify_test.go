package sender

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"testing"

	"github.com/sendplane/sendplane/store"
)

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		code  int
		msg   string
		class store.ErrorClass
		rule  string
	}{
		// auth: transport faults, never charged to the delivery.
		{530, "5.7.0 Authentication required", store.ErrorClassAuth, "auth.code"},
		{535, "5.7.8 Authentication credentials invalid", store.ErrorClassAuth, "auth.code"},
		{534, "5.7.9 Please log in via your web browser", store.ErrorClassAuth, "auth.code"},
		{538, "5.7.11 Encryption required for requested mechanism", store.ErrorClassAuth, "auth.code"},
		{454, "4.7.8 Temporary authentication failure", store.ErrorClassAuth, "auth.enhanced"},

		// rate limits: short retry plus a transport slowdown.
		{421, "4.7.0 Too many messages, closing connection", store.ErrorClassRateLimited, "ratelimit.421"},
		{421, "Service not available", store.ErrorClassRateLimited, "ratelimit.421"},
		{450, "4.2.1 The user you are trying to contact is receiving mail too quickly", store.ErrorClassRateLimited, "ratelimit.4xx.text"},
		{451, "4.7.1 Rate limit exceeded", store.ErrorClassRateLimited, "ratelimit.4xx.text"},
		{452, "4.5.3 Too many recipients", store.ErrorClassRateLimited, "ratelimit.4xx.text"},
		{450, "4.7.28 Unusual rate of unsolicited mail", store.ErrorClassRateLimited, "ratelimit.4xx.text"},
		{451, "4.7.0 try later", store.ErrorClassRateLimited, "ratelimit.enhanced"},

		// policy: blocked or filtered.
		{550, "5.7.1 Message rejected as spam", store.ErrorClassPolicy, "policy.enhanced"},
		{554, "5.7.1 Service unavailable; Client host blocked using a blocklist", store.ErrorClassPolicy, "policy.enhanced"},
		{550, "Message refused: bad reputation", store.ErrorClassPolicy, "policy.text"},

		// permanent: the recipient is the problem.
		{550, "5.1.1 User unknown", store.ErrorClassPermanent, "permanent.5xx"},
		{552, "5.3.4 Message too big", store.ErrorClassPermanent, "permanent.5xx"},
		{501, "5.5.4 Syntax error", store.ErrorClassPermanent, "permanent.5xx"},

		// transient: everything else in 4xx.
		{451, "4.3.0 Temporary local problem", store.ErrorClassTransient, "transient.4xx"},
		{450, "4.2.0 Mailbox busy", store.ErrorClassTransient, "transient.4xx"},
		{400, "unspecified", store.ErrorClassTransient, "transient.4xx"},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%d %s", tc.code, tc.msg)
		t.Run(name, func(t *testing.T) {
			f := Classify(&textproto.Error{Code: tc.code, Msg: tc.msg})
			if f.Class != tc.class {
				t.Errorf("class = %s, want %s (rule %s)", f.Class, tc.class, f.Rule)
			}
			if f.Rule != tc.rule {
				t.Errorf("rule = %s, want %s", f.Rule, tc.rule)
			}
			if f.Code != tc.code {
				t.Errorf("code = %d", f.Code)
			}
		})
	}
}

func TestClassifyEnhancedExtraction(t *testing.T) {
	f := Classify(&textproto.Error{Code: 550, Msg: "5.1.1 <a@b>: user unknown"})
	if f.Enhanced != "5.1.1" {
		t.Errorf("enhanced = %q", f.Enhanced)
	}
	// A multi-line reply repeats the code; only the first line is parsed.
	f = Classify(&textproto.Error{Code: 451, Msg: "4.7.0 first line\n4.7.0 second line"})
	if f.Enhanced != "4.7.0" {
		t.Errorf("enhanced = %q", f.Enhanced)
	}
	// No enhanced status at all.
	f = Classify(&textproto.Error{Code: 550, Msg: "No such user here"})
	if f.Enhanced != "" {
		t.Errorf("enhanced = %q, want empty", f.Enhanced)
	}
	if f.Class != store.ErrorClassPermanent {
		t.Errorf("class = %s", f.Class)
	}
}

func TestClassifyNil(t *testing.T) {
	if f := Classify(nil); f.Class != store.ErrorClassNone {
		t.Fatalf("class = %s, want none", f.Class)
	}
}

func TestClassifyNetwork(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		class store.ErrorClass
		rule  string
	}{
		{"eof", io.EOF, store.ErrorClassTransient, "transient.connection"},
		{"unexpected eof", fmt.Errorf("data: %w", io.ErrUnexpectedEOF), store.ErrorClassTransient, "transient.connection"},
		{"closed", net.ErrClosed, store.ErrorClassTransient, "transient.connection"},
		{
			"refused",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			store.ErrorClassTransient, "transient.connection",
		},
		{"timeout", timeoutErr{}, store.ErrorClassTransient, "transient.timeout"},
		{
			"unknown authority",
			fmt.Errorf("tls: %w", x509.UnknownAuthorityError{}),
			store.ErrorClassAuth, "auth.tls",
		},
		{
			"record header",
			tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
			store.ErrorClassAuth, "auth.tls",
		},
		{"unknown", errors.New("something else"), store.ErrorClassTransient, "transient.unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := Classify(tc.err)
			if f.Class != tc.class || f.Rule != tc.rule {
				t.Fatalf("got %s/%s, want %s/%s", f.Class, f.Rule, tc.class, tc.rule)
			}
			if f.Code != 0 {
				t.Errorf("code = %d, want 0", f.Code)
			}
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestReusable(t *testing.T) {
	cases := []struct {
		f    Failure
		want bool
	}{
		{Failure{Class: store.ErrorClassNone}, true},
		{Failure{Class: store.ErrorClassPermanent, Code: 550}, true},
		{Failure{Class: store.ErrorClassTransient, Code: 451}, true},
		{Failure{Class: store.ErrorClassRateLimited, Code: 421}, false},
		{Failure{Class: store.ErrorClassTransient, Code: 0}, false},
	}
	for _, tc := range cases {
		if got := Reusable(tc.f); got != tc.want {
			t.Errorf("Reusable(%d/%s) = %v, want %v", tc.f.Code, tc.f.Class, got, tc.want)
		}
	}
}
