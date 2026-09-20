package sender

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// Failure is a normalized transport failure: the SMTP reply if there was one,
// and the error class that decides what happens next (architecture 4.2).
type Failure struct {
	Class store.ErrorClass
	// Code is the SMTP reply code, or 0 for a failure that never got one
	// (connection refused, timeout, TLS).
	Code int
	// Enhanced is the RFC 3463 status ("4.7.0"), when the reply carried one.
	Enhanced string
	// Message is the reply text, or the Go error text.
	Message string
	// Rule names the classification rule that matched, for logs and tests.
	Rule string
	Err  error
}

// rule is one row of the classification table. A rule matches when every
// non-empty condition it states matches; inside a condition the entries are
// alternatives. The table is walked top to bottom and the first match wins, so
// the specific rules (auth, rate limit, policy) sit above the range rules.
//
// The table is a Go literal rather than the YAML of architecture 4.2: it has
// to be compiled anyway, the entries are short, and keeping it next to the
// matcher means a new rule and its test move together. Loading the same rows
// from YAML is a drop-in change if a tenant ever needs to override them.
type rule struct {
	name string
	// codes matches the reply code exactly.
	codes []int
	// codeMin/codeMax match a range (inclusive) when codes is empty.
	codeMin, codeMax int
	// enhanced matches a prefix of the enhanced status code.
	enhanced []string
	// contains matches a lowercased substring of the reply text.
	contains []string
	class    store.ErrorClass
}

// rules is the table of architecture 4.2.
var rules = []rule{
	{
		// 530 "authentication required", 535 "credentials invalid", 534/538
		// "mechanism too weak / encryption required". These are the transport's
		// fault, not the recipient's, so they must not consume a retry.
		name:  "auth.code",
		codes: []int{530, 534, 535, 538},
		class: store.ErrorClassAuth,
	}, {
		name:     "auth.enhanced",
		enhanced: []string{"5.7.8", "5.7.9", "4.7.8"},
		class:    store.ErrorClassAuth,
	}, {
		// 421 is "service not available, closing channel": every large
		// provider uses it to shed load.
		name:  "ratelimit.421",
		codes: []int{421},
		class: store.ErrorClassRateLimited,
	}, {
		name:  "ratelimit.4xx.text",
		codes: []int{450, 451, 452},
		contains: []string{
			"rate", "too many", "too quickly", "throttl", "slow down",
			"try again later", "deferred due to",
		},
		class: store.ErrorClassRateLimited,
	}, {
		name:     "ratelimit.enhanced",
		enhanced: []string{"4.7.0", "4.7.28", "4.2.1"},
		class:    store.ErrorClassRateLimited,
	}, {
		// 5.7.x is "security or policy status": blocked, blacklisted, refused
		// for content. It fails the delivery and raises the event severity.
		name:     "policy.enhanced",
		enhanced: []string{"5.7."},
		class:    store.ErrorClassPolicy,
	}, {
		name:     "policy.text",
		codeMin:  500,
		codeMax:  599,
		contains: []string{"spam", "blocked", "blacklist", "blocklist", "reputation", "policy", "abuse"},
		class:    store.ErrorClassPolicy,
	}, {
		name:    "permanent.5xx",
		codeMin: 500,
		codeMax: 599,
		class:   store.ErrorClassPermanent,
	}, {
		name:    "transient.4xx",
		codeMin: 400,
		codeMax: 499,
		class:   store.ErrorClassTransient,
	},
}

// enhancedRE matches the enhanced status code at the start of a reply text.
var enhancedRE = regexp.MustCompile(`^([245])\.([0-9]{1,3})\.([0-9]{1,3})\b`)

// Classify normalizes a send error into one of the five classes. A nil error
// is ErrorClassNone.
func Classify(err error) Failure {
	if err == nil {
		return Failure{Class: store.ErrorClassNone}
	}
	if f, ok := classifyNetwork(err); ok {
		return f
	}

	var pe *textproto.Error
	if !errors.As(err, &pe) {
		// Not an SMTP reply and not a recognized network failure: treat it as
		// transient so an unknown transport fault retries rather than throwing
		// the delivery away.
		return Failure{
			Class: store.ErrorClassTransient, Message: err.Error(),
			Rule: "transient.unknown", Err: err,
		}
	}
	f := Failure{Code: pe.Code, Message: pe.Msg, Enhanced: enhancedOf(pe.Msg), Err: err}
	lower := strings.ToLower(pe.Msg)
	for _, r := range rules {
		if r.match(pe.Code, f.Enhanced, lower) {
			f.Class, f.Rule = r.class, r.name
			return f
		}
	}
	// A 2xx/3xx that reached the error path, or a code outside 4xx/5xx.
	f.Class, f.Rule = store.ErrorClassTransient, "transient.default"
	return f
}

func (r rule) match(code int, enhanced, lowerMsg string) bool {
	if len(r.codes) > 0 {
		if !containsInt(r.codes, code) {
			return false
		}
	} else if r.codeMin > 0 && (code < r.codeMin || code > r.codeMax) {
		return false
	}
	if len(r.enhanced) > 0 {
		if enhanced == "" || !hasAnyPrefix(enhanced, r.enhanced) {
			return false
		}
	}
	if len(r.contains) > 0 && !containsAny(lowerMsg, r.contains) {
		return false
	}
	return true
}

// classifyNetwork covers the failures that never produced a reply code.
func classifyNetwork(err error) (Failure, bool) {
	// TLS and certificate problems are a configuration fault of the transport,
	// not of the recipient: same handling as an auth failure, so the delivery
	// stays queued and the transport is taken out of rotation.
	var certErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	var authErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	switch {
	case errors.As(err, &certErr), errors.As(err, &recErr),
		errors.As(err, &authErr), errors.As(err, &hostErr):
		return Failure{
			Class: store.ErrorClassAuth, Message: err.Error(), Rule: "auth.tls", Err: err,
		}, true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Failure{
			Class: store.ErrorClassTransient, Message: err.Error(), Rule: "transient.timeout", Err: err,
		}, true
	}
	// A relay that hangs up mid-transaction shows up as EOF, a closed
	// connection, or an *net.OpError wrapping ECONNRESET/EPIPE (below).
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) {
		return Failure{
			Class: store.ErrorClassTransient, Message: err.Error(), Rule: "transient.connection", Err: err,
		}, true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return Failure{
			Class: store.ErrorClassTransient, Message: err.Error(), Rule: "transient.connection", Err: err,
		}, true
	}
	return Failure{}, false
}

// enhancedOf extracts the RFC 3463 status from the front of a reply text. Only
// the first line is considered: a multi-line reply repeats the code on every
// line and textproto joins them with newlines.
func enhancedOf(msg string) string {
	first := msg
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	m := enhancedRE.FindString(strings.TrimSpace(first))
	return m
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
