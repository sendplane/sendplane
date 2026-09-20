// Package dnscheck is the diagnostic half of the sending health check
// (docs/architecture.md 11.3, ADR-0012).
//
// It answers "why did the loopback probe fail": the record is missing, the
// SPF record does not authorize the IP the probe was actually seen from, the
// published DKIM key is not the one sendplane signs with, DMARC is still at
// p=none, the PTR does not forward-confirm. It never decides health on its
// own — the probe's Authentication-Results do (internal/probe). A record can
// be perfect and the mail still land in spam.
package dnscheck

import (
	"context"
	"net"
	"time"

	"github.com/sendplane/sendplane/store"
)

// Result is one check's verdict. Details is a flat string map so that it
// survives a round trip through ProbeRun.DNS (jsonb) and can be rendered by
// the console without a per-check type.
type Result struct {
	Check   string             `json:"check"`
	Status  store.HealthStatus `json:"status"`
	Summary string             `json:"summary"`
	Details map[string]string  `json:"details,omitempty"`
}

// Report is one full pass of the diagnostic layer. A check that had no input
// (no selector, no observed IP, no return-path domain) is nil, not red: the
// probe cannot ask a question it has no subject for.
type Report struct {
	Status    store.HealthStatus `json:"status"`
	CheckedAt time.Time          `json:"checked_at"`

	SPF   *Result `json:"spf,omitempty"`
	DKIM  *Result `json:"dkim,omitempty"`
	DMARC *Result `json:"dmarc,omitempty"`
	MX    *Result `json:"mx,omitempty"`
	PTR   *Result `json:"ptr,omitempty"`
}

// Inputs is everything RunAll needs, gathered from the Sender, its
// SendingDomain and the probe's Received chain.
type Inputs struct {
	// Domain is the From domain: the one SPF, DKIM and DMARC are published on.
	Domain string
	// DKIMSelector is empty when the relay signs, which skips the DKIM check.
	DKIMSelector string
	// DKIMKey is the decrypted signing key (PEM private key) or the expected
	// public key in base64. Empty skips the key comparison but still checks
	// that the record parses.
	DKIMKey []byte
	// ReturnPathDomain is the bounce domain whose MX must accept DSNs. Empty
	// falls back to Domain.
	ReturnPathDomain string
	// ObservedIP is the outbound IP the probe's Received chain recorded. Nil
	// means SPF is only checked for syntax and PTR is skipped entirely
	// (architecture 11.3: the probe is what discovers the IP).
	ObservedIP net.IP
}

// Checker runs the checks against one resolver.
type Checker struct {
	r   Resolver
	now func() time.Time
}

// Option configures a Checker.
type Option func(*Checker)

// WithClock replaces time.Now, so a test can assert on Report.CheckedAt.
func WithClock(clock func() time.Time) Option {
	return func(c *Checker) { c.now = clock }
}

// New returns a Checker using r.
func New(r Resolver, opts ...Option) *Checker {
	c := &Checker{r: r, now: time.Now}
	for _, o := range opts {
		o(c)
	}
	return c
}

// RunAll runs every check whose input is present.
func (c *Checker) RunAll(ctx context.Context, in Inputs) Report {
	rep := Report{CheckedAt: c.now().UTC()}
	if in.Domain != "" {
		spf := c.SPF(ctx, in.Domain, in.ObservedIP)
		dmarc := c.DMARC(ctx, in.Domain)
		rep.SPF, rep.DMARC = &spf, &dmarc
		if in.DKIMSelector != "" {
			dkim := c.DKIM(ctx, in.Domain, in.DKIMSelector, in.DKIMKey)
			rep.DKIM = &dkim
		}
	}
	if d := in.ReturnPathDomain; d != "" || in.Domain != "" {
		if d == "" {
			d = in.Domain
		}
		mx := c.MX(ctx, d)
		rep.MX = &mx
	}
	if in.ObservedIP != nil {
		ptr := c.PTR(ctx, in.ObservedIP)
		rep.PTR = &ptr
	}
	rep.Status = rep.worst()
	return rep
}

// worst is the summary of architecture 11.4: the worst of the individual
// checks. HealthStatus is ordered unknown < green < yellow < red, so this is a
// max over the checks that ran.
func (r Report) worst() store.HealthStatus {
	out := store.HealthUnknown
	for _, res := range []*Result{r.SPF, r.DKIM, r.DMARC, r.MX, r.PTR} {
		if res != nil && res.Status > out {
			out = res.Status
		}
	}
	return out
}

// Worst exposes the summary for callers that assembled a Report by hand.
func (r Report) Worst() store.HealthStatus { return r.worst() }

// result builds a Result, dropping empty details so the stored JSON stays
// readable.
func result(check string, status store.HealthStatus, summary string, details map[string]string) Result {
	for k, v := range details {
		if v == "" {
			delete(details, k)
		}
	}
	if len(details) == 0 {
		details = nil
	}
	return Result{Check: check, Status: status, Summary: summary, Details: details}
}
