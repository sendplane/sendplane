package host

import "time"

// ProbeConfig configures the loopback health probe (architecture 11,
// ADR-0012). It lives here rather than in the root package so that the root
// can hand it to internal/probe and internal/dnscheck without either of them
// importing the root, exactly like Hooks and Limits.
//
// The mailboxes themselves are not configured here: they are the tenant's
// store.ProbeMailbox rows, managed through the API. This is the process-wide
// half, which has to come from the host because it involves a signing key and
// the resolvers the deployment is allowed to query.
type ProbeConfig struct {
	// Enabled turns the probe on. With it off, the control leader runs no
	// probe loops and POST /senders/{id}/probe answers 501: a trigger that
	// nothing will ever collect is worse than an honest refusal.
	Enabled bool

	// HMACKey signs the run ID in the X-Sendplane-Probe header, so a mail
	// somebody else dropped in the probe mailbox cannot produce a verdict.
	// Empty leaves the token unsigned, which is only safe on a mailbox nobody
	// else can write to.
	HMACKey []byte

	// Nameservers are queried directly for the DNS diagnostic layer
	// (architecture 11.3): "what the world sees" must not be answered by a
	// local cache or /etc/hosts. An entry may carry a port ("1.1.1.1:53").
	// Empty uses the system resolver configuration.
	Nameservers []string

	// Interval is how stale a sender's health may get before it is probed
	// again. Zero uses the probe package's default (6h).
	Interval time.Duration
	// MailboxCheckInterval is how often the mailbox-check leader loop logs
	// in to every enabled probe and bounce mailbox, and how stale a mailbox's
	// health may get before it is checked again. Zero uses 15m.
	//
	// It lives here next to the rest of the probe wiring, but the loop it
	// configures runs whether or not Enabled is set: bounce mailboxes need
	// the same watch, and a deployment with no probe at all still has
	// credentials that expire (architecture 11.5).
	MailboxCheckInterval time.Duration

	// Timeout is how long a probe mail may take to arrive before the run is
	// called undelivered. Zero uses the probe package's default (15m).
	Timeout time.Duration

	// Webhooks are the inbound endpoints whatever receives the probe mail posts
	// probe mail to, instead of sendplane polling an IMAP account (ADR-0016).
	//
	// They are process-wide and serve every tenant, which is not an oversight:
	// an inbound endpoint is a URL somebody else was configured to call, and a
	// per-tenant URL would mean a hostname per tenant. The tenant is resolved
	// from the probe token on the message instead, the same way the public
	// tracking routes resolve theirs.
	Webhooks []ProbeWebhook
}

// ProbeWebhook is one configured inbound webhook.
type ProbeWebhook struct {
	// Provider names a registered internal/probe/inbound provider. The one
	// sendplane ships is "sendplane" (internal/probe/inbound/sendplanehook).
	// An unknown name is a configuration error, not a route that answers 404
	// at runtime.
	Provider string
	// Secrets are what the provider's request signature is verified against.
	// At least one is required: an unsigned inbound endpoint would let
	// anybody complete anybody's probe run. Several are accepted and any one
	// matching is enough, which is how a secret is rotated without a window
	// in which deliveries are refused.
	Secrets []string
	// Path is the route to mount. Empty uses "/probe/inbound/<provider>".
	Path string
	// Tolerance is how far a signed timestamp may be from this process's
	// clock, for a provider whose signature carries one
	// (inbound.ToleranceSetter). Zero uses the provider's default (5m), and a
	// non-zero value for a provider with no timestamp is a configuration
	// error rather than a number that quietly does nothing.
	Tolerance time.Duration
}
