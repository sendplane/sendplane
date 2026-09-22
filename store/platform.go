package store

import (
	"context"
	"fmt"
	"strings"
)

// Platform resources are the shared transports, sending domains, senders and
// mailboxes an operator runs *for* its tenants: one relay account, one warmed
// domain, one "no-reply@" identity, used by everybody who has not brought
// their own.
//
// They are defined in configuration and resolved in code as virtual entities
// (ADR-0017). Nothing here is ever written to the store: a shared transport's
// host, port and password live in the operator's config file and in nothing
// else, so rotating a password is a deploy and not a migration, and a database
// dump of one tenant can never contain another tenant's relay credentials.
//
// Only *runtime state* about a platform resource is persisted, and only in the
// system tenant's own tables as a shadow row (see WithPlatform): a transport's
// circuit status, a mailbox's reachability, a sender's probe verdict. A shadow
// row carries the virtual ID and the state columns and nothing else; every
// configuration column on it is written empty.
//
// There is deliberately no tenant registry either: sendplane stores no tenant
// attributes (name, slug, plan). A request that needs them carries them as
// tenant variables, which is what a templated shared sender renders its From
// address from (Campaign.TenantVars, Delivery.TenantVars).

// PlatformIDPrefix marks a virtual ID. A store ID sendplane mints is a UUIDv7
// (NewID), and a colon cannot occur in one, so the two can never collide.
const PlatformIDPrefix = "sys:"

// IsPlatformID reports whether id names a platform (virtual) entity rather
// than a row in the store.
func IsPlatformID(id string) bool { return strings.HasPrefix(id, PlatformIDPrefix) }

// PlatformID turns a catalog-local ID ("shared-a") into the virtual ID the API
// and the store contract use ("sys:shared-a"). An ID that already carries the
// prefix is returned unchanged, so the function is idempotent and a config
// file may spell a reference either way.
func PlatformID(local string) string {
	if local == "" || IsPlatformID(local) {
		return local
	}
	return PlatformIDPrefix + local
}

// UseKind is what a sender is being used for. It is the vocabulary of the
// sender-use policy: an operator may allow its shared sender for
// transactional mail and refuse it for campaigns, which is the difference
// between a password reset and a newsletter going out over a reputation
// everybody shares.
type UseKind string

const (
	UseCampaign      UseKind = "campaign"
	UseTransactional UseKind = "transactional"
	UseProbe         UseKind = "probe"
)

// Valid reports whether k is a use this version knows.
func (k UseKind) Valid() bool {
	switch k {
	case UseCampaign, UseTransactional, UseProbe:
		return true
	}
	return false
}

func (k UseKind) String() string { return string(k) }

// DefaultSenderUses is what a platform sender allows when its config names no
// `uses`: campaign and transactional mail, but not the loopback probe, which
// the system tenant runs on its own behalf regardless.
func DefaultSenderUses() []UseKind { return []UseKind{UseCampaign, UseTransactional} }

// SecretCipher encrypts secrets at rest. It is declared here, rather than only
// in package host, because the platform overlay has to encrypt the passwords
// it builds virtual entities from so that the existing decrypt paths in
// internal/sender and internal/bounce work unchanged.
//
// host.SecretCipher is an alias of this type, so a host keeps implementing one
// interface.
type SecretCipher interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// PlatformTransport is a shared SMTP account. The fields mirror Transport
// minus the tenant and the runtime state: state is never configured, and
// configuration is never stored.
type PlatformTransport struct {
	// ID is catalog-local ("shared-a"); Normalize turns it into "sys:shared-a".
	ID   string
	Name string

	Host     string
	Port     int
	TLS      TLSMode
	Username string
	// Password is the plaintext from configuration. WithPlatform encrypts it
	// in memory with the host's cipher before it ever reaches a caller, so the
	// sender's existing decrypt path is unchanged.
	Password string

	MaxConns int
	// RatePerSecond is the cluster-wide rate for the whole transport, shared
	// by every tenant using it.
	RatePerSecond float64
	// PerTenantRatePerSecond is one tenant's share of it, so that a single
	// tenant's campaign cannot consume the shared relay (architecture 8.2).
	// Zero means no per-tenant cap.
	PerTenantRatePerSecond float64
	DomainRatePerSecond    map[string]float64
}

// PlatformDomain is a shared sending domain: the DKIM material and the
// return-path domain every platform sender bound to it signs with.
type PlatformDomain struct {
	ID     string
	Domain string

	DKIMSelector string
	// DKIMPrivateKey is the plaintext PEM from configuration; see
	// PlatformTransport.Password.
	DKIMPrivateKey string

	ReturnPathDomain string
	ExpectedSPF      string
	OutboundIPs      []string
}

// PlatformSender is a shared From identity. FromName, FromEmail and ReplyTo
// are Liquid templates over the request's tenant variables, so one
// configuration entry serves every tenant:
//
//	from_email: "sender+{{ tenant.slug }}@mail.example.com"
//	from_name:  "{{ tenant.name }}"
//
// A tenant variable a template needs and a request did not carry is a 422
// (tenant_vars_missing), not a mail from "sender+@mail.example.com".
type PlatformSender struct {
	ID   string
	Name string

	TransportID string
	DomainID    string

	FromName  string
	FromEmail string
	ReplyTo   string

	// Uses restricts what the sender may be used for. Empty means
	// DefaultSenderUses.
	Uses []UseKind

	// ProbeVars are the tenant variables the loopback probe renders the
	// templates with. A platform sender is probed once, in the system tenant
	// (architecture 11.2), and its From address has to resolve to something
	// deliverable for that one run.
	ProbeVars map[string]any
}

// PlatformProbeMailbox is a shared probe mailbox: the same fields as
// ProbeMailbox minus the tenant and the health.
type PlatformProbeMailbox struct {
	ID   string
	Name string

	Kind    ProbeMailboxKind
	Address string

	Host     string
	Port     int
	TLS      TLSMode
	Username string
	Password string

	InboxFolder string
	SpamFolder  string
	AuthServID  string

	// Enabled defaults to true: a mailbox somebody wrote into the config file
	// is one they want polled.
	Enabled *bool
}

// PlatformBounceMailbox is a shared bounce mailbox. Mail arriving in it may
// belong to any tenant, so the processor finds the tenant from the globally
// unique delivery ID (Provider.LookupDeliveryTenant) instead of assuming the
// mailbox's own (architecture 10).
type PlatformBounceMailbox struct {
	ID   string
	Name string

	Address  string
	Protocol string
	Host     string
	Port     int
	TLS      TLSMode
	Username string
	Password string

	Folder       string
	AfterProcess string

	Enabled *bool
}

// PlatformCatalog is the whole platform configuration. host.Platform is an
// alias of it, so a host writes sendplane.Platform and this package needs no
// second copy of the same structs.
type PlatformCatalog struct {
	Transports      []PlatformTransport
	Domains         []PlatformDomain
	Senders         []PlatformSender
	ProbeMailboxes  []PlatformProbeMailbox
	BounceMailboxes []PlatformBounceMailbox

	// TrackingDomain and UnsubscribeURLTemplate are the platform fallbacks for
	// the matching TenantSettings fields: a tenant that has configured neither
	// still gets working open/click tracking and a working unsubscribe link
	// off the operator's own domain.
	TrackingDomain         string
	UnsubscribeURLTemplate string
}

// Empty reports whether the catalog defines nothing at all, in which case
// WithPlatform is not worth wrapping a Provider in.
func (c PlatformCatalog) Empty() bool {
	return len(c.Transports) == 0 && len(c.Domains) == 0 && len(c.Senders) == 0 &&
		len(c.ProbeMailboxes) == 0 && len(c.BounceMailboxes) == 0 &&
		c.TrackingDomain == "" && c.UnsubscribeURLTemplate == ""
}

// Normalize returns the catalog with every ID and every reference in virtual
// form ("sys:x"), so that nothing downstream has to know whether the config
// file spelled a reference with the prefix or without it.
func (c PlatformCatalog) Normalize() PlatformCatalog {
	out := c
	out.Transports = make([]PlatformTransport, len(c.Transports))
	for i, v := range c.Transports {
		v.ID = PlatformID(v.ID)
		out.Transports[i] = v
	}
	out.Domains = make([]PlatformDomain, len(c.Domains))
	for i, v := range c.Domains {
		v.ID = PlatformID(v.ID)
		out.Domains[i] = v
	}
	out.Senders = make([]PlatformSender, len(c.Senders))
	for i, v := range c.Senders {
		v.ID = PlatformID(v.ID)
		v.TransportID = PlatformID(v.TransportID)
		v.DomainID = PlatformID(v.DomainID)
		out.Senders[i] = v
	}
	out.ProbeMailboxes = make([]PlatformProbeMailbox, len(c.ProbeMailboxes))
	for i, v := range c.ProbeMailboxes {
		v.ID = PlatformID(v.ID)
		out.ProbeMailboxes[i] = v
	}
	out.BounceMailboxes = make([]PlatformBounceMailbox, len(c.BounceMailboxes))
	for i, v := range c.BounceMailboxes {
		v.ID = PlatformID(v.ID)
		out.BounceMailboxes[i] = v
	}
	return out
}

// Validate checks everything that can be checked without a Liquid parser:
// unique IDs, resolvable references, known enum values. The root package
// additionally parses the sender templates, which is the one check that needs
// internal/render (see sendplane.New).
//
// It expects a normalized catalog and normalizes defensively, so a caller that
// forgot is still validated against what it will actually get.
func (c PlatformCatalog) Validate() error {
	c = c.Normalize()
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	transports := map[string]bool{}
	for i, t := range c.Transports {
		checkPlatformID(add, "platform.transports", i, t.ID, transports)
		if strings.TrimSpace(t.Name) == "" {
			add("platform.transports[%d].name is required", i)
		}
		if strings.TrimSpace(t.Host) == "" {
			add("platform.transports[%d].host is required", i)
		}
		if t.Port <= 0 || t.Port > 65535 {
			add("platform.transports[%d].port must be between 1 and 65535", i)
		}
		if !validTLS(t.TLS) {
			add("platform.transports[%d].tls %q is not none, starttls or tls", i, t.TLS)
		}
		if t.RatePerSecond < 0 || t.PerTenantRatePerSecond < 0 || t.MaxConns < 0 {
			add("platform.transports[%d] has a negative rate or connection count", i)
		}
	}

	domains := map[string]bool{}
	for i, d := range c.Domains {
		checkPlatformID(add, "platform.domains", i, d.ID, domains)
		if strings.TrimSpace(d.Domain) == "" {
			add("platform.domains[%d].domain is required", i)
		}
		if d.DKIMPrivateKey != "" && d.DKIMSelector == "" {
			add("platform.domains[%d] has a DKIM key but no selector", i)
		}
	}

	senders := map[string]bool{}
	for i, s := range c.Senders {
		checkPlatformID(add, "platform.senders", i, s.ID, senders)
		if strings.TrimSpace(s.Name) == "" {
			add("platform.senders[%d].name is required", i)
		}
		if strings.TrimSpace(s.FromEmail) == "" {
			add("platform.senders[%d].from_email is required", i)
		}
		if s.TransportID == "" {
			add("platform.senders[%d].transport_id is required", i)
		} else if !transports[s.TransportID] {
			add("platform.senders[%d].transport_id %q names no platform transport", i, s.TransportID)
		}
		if s.DomainID != "" && !domains[s.DomainID] {
			add("platform.senders[%d].domain_id %q names no platform domain", i, s.DomainID)
		}
		for j, u := range s.Uses {
			if !u.Valid() {
				add("platform.senders[%d].uses[%d] %q is not campaign, transactional or probe", i, j, u)
			}
		}
	}

	mailboxes := map[string]bool{}
	for i, m := range c.ProbeMailboxes {
		checkPlatformID(add, "platform.probe_mailboxes", i, m.ID, mailboxes)
		if strings.TrimSpace(m.Address) == "" {
			add("platform.probe_mailboxes[%d].address is required", i)
		}
		if !m.Kind.Valid() {
			add("platform.probe_mailboxes[%d].kind %q is not imap or webhook", i, m.Kind)
		}
		if m.Kind.Normalized() == ProbeMailboxIMAP {
			if strings.TrimSpace(m.Host) == "" {
				add("platform.probe_mailboxes[%d].host is required for an imap mailbox", i)
			}
			if m.Port <= 0 || m.Port > 65535 {
				add("platform.probe_mailboxes[%d].port must be between 1 and 65535", i)
			}
		}
		if !validTLS(m.TLS) {
			add("platform.probe_mailboxes[%d].tls %q is not none, starttls or tls", i, m.TLS)
		}
	}
	for i, m := range c.BounceMailboxes {
		checkPlatformID(add, "platform.bounce_mailboxes", i, m.ID, mailboxes)
		if strings.TrimSpace(m.Host) == "" {
			add("platform.bounce_mailboxes[%d].host is required", i)
		}
		if m.Port <= 0 || m.Port > 65535 {
			add("platform.bounce_mailboxes[%d].port must be between 1 and 65535", i)
		}
		if !validTLS(m.TLS) {
			add("platform.bounce_mailboxes[%d].tls %q is not none, starttls or tls", i, m.TLS)
		}
		switch p := strings.ToLower(strings.TrimSpace(m.Protocol)); p {
		case "", "imap", "pop3":
		default:
			add("platform.bounce_mailboxes[%d].protocol %q is not imap or pop3", i, p)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid platform config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// checkPlatformID validates one catalog ID and records it in seen. The local
// part is restricted so that a virtual ID can be a path segment without
// escaping and cannot be confused with a UUID.
func checkPlatformID(add func(string, ...any), where string, i int, id string, seen map[string]bool) {
	local := strings.TrimPrefix(id, PlatformIDPrefix)
	switch {
	case local == "":
		add("%s[%d].id is required", where, i)
		return
	case !validPlatformLocalID(local):
		add("%s[%d].id %q must be 1-63 characters of a-z, 0-9, '-' or '_', starting with a letter or digit",
			where, i, local)
		return
	case seen[id]:
		add("%s[%d].id %q is already used", where, i, local)
		return
	}
	seen[id] = true
}

func validPlatformLocalID(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

func validTLS(m TLSMode) bool {
	switch m {
	case "", TLSNone, TLSSTARTTLS, TLSImplicit:
		return true
	}
	return false
}

// Sender returns the platform sender with the given virtual ID.
func (c PlatformCatalog) Sender(id string) (PlatformSender, bool) {
	for _, s := range c.Senders {
		if s.ID == id {
			return s, true
		}
	}
	return PlatformSender{}, false
}

// Transport returns the platform transport with the given virtual ID.
func (c PlatformCatalog) Transport(id string) (PlatformTransport, bool) {
	for _, t := range c.Transports {
		if t.ID == id {
			return t, true
		}
	}
	return PlatformTransport{}, false
}

// Domain returns the platform sending domain with the given virtual ID.
func (c PlatformCatalog) Domain(id string) (PlatformDomain, bool) {
	for _, d := range c.Domains {
		if d.ID == id {
			return d, true
		}
	}
	return PlatformDomain{}, false
}

// SenderUses is the effective use list of a platform sender: its own, or
// DefaultSenderUses when it configured none. An unknown sender allows
// nothing, so a stale reference denies rather than permits.
func (c PlatformCatalog) SenderUses(id string) []UseKind {
	s, ok := c.Sender(id)
	if !ok {
		return nil
	}
	if len(s.Uses) == 0 {
		return DefaultSenderUses()
	}
	return s.Uses
}

// PlatformView returns the system-tenant Store, which is the only view that
// sees platform internals: a shared transport's host and password, a shared
// domain's DKIM key, the state shadow rows.
//
// It exists so that the sender process, the probe runner and the bounce
// processor can load what a `sys:` sender actually sends through without the
// tenant-facing visibility rules being weakened to let them (ADR-0017). A
// caller must never hand the result to a request handler.
func PlatformView(ctx context.Context, p Provider) (Store, error) {
	return p.ForTenant(ctx, SystemTenantID)
}
