// The `platform:` section of config.yaml: the operator's shared sending
// infrastructure (ADR-0017). It maps 1:1 onto sendplane.Platform, which is
// configuration and is never written to the store.
package main

import (
	"strings"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// PlatformConfig is the `platform:` block.
//
// Everything in it is configuration in the strict sense: a read of a shared
// transport, domain, sender or mailbox resolves this struct, and only runtime
// state about them is persisted (in the system tenant, as a state-only shadow
// row). Rotating the relay password is therefore a deploy, not a migration,
// and no database anywhere holds it.
//
// Secrets go through `${ENV}` like every other secret in this file:
// LoadConfig runs os.ExpandEnv over the whole document, so
// `password: ${SENDPLANE_RELAY_PASSWORD}` is the intended spelling and a
// literal is only for a local sandbox.
type PlatformConfig struct {
	Transports      []PlatformTransportConfig     `yaml:"transports"`
	Domains         []PlatformDomainConfig        `yaml:"domains"`
	Senders         []PlatformSenderConfig        `yaml:"senders"`
	ProbeMailboxes  []PlatformProbeMailboxConfig  `yaml:"probe_mailboxes"`
	BounceMailboxes []PlatformBounceMailboxConfig `yaml:"bounce_mailboxes"`

	// TrackingDomain and UnsubscribeURLTemplate are the fallbacks for the
	// matching tenant settings: a tenant that has configured neither still
	// gets working opens, clicks and unsubscribe links off the operator's
	// domain. A tenant that sets its own wins.
	TrackingDomain         string `yaml:"tracking_domain"`
	UnsubscribeURLTemplate string `yaml:"unsubscribe_url_template"`
}

// PlatformTransportConfig is one shared SMTP account.
type PlatformTransportConfig struct {
	// ID is the local name; the API and the store see it as `sys:<id>`.
	ID   string `yaml:"id"`
	Name string `yaml:"name"`

	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	TLS      string `yaml:"tls"` // none | starttls (default) | tls
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	MaxConns int `yaml:"max_conns"`
	// RatePerSecond is the whole relay's cluster-wide capacity, shared by
	// every tenant; PerTenantRatePerSecond is one tenant's slice of it, which
	// is what stops a single campaign consuming the relay (architecture 8.2).
	RatePerSecond          float64            `yaml:"rate_per_second"`
	PerTenantRatePerSecond float64            `yaml:"per_tenant_rate_per_second"`
	DomainRatePerSecond    map[string]float64 `yaml:"domain_rate_per_second"`
}

// PlatformDomainConfig is one shared sending domain.
type PlatformDomainConfig struct {
	ID     string `yaml:"id"`
	Domain string `yaml:"domain"`

	DKIMSelector   string `yaml:"dkim_selector"`
	DKIMPrivateKey string `yaml:"dkim_private_key"`

	ReturnPathDomain string   `yaml:"return_path_domain"`
	ExpectedSPF      string   `yaml:"expected_spf"`
	OutboundIPs      []string `yaml:"outbound_ips"`
}

// PlatformSenderConfig is one shared From identity.
//
// FromName, FromEmail and ReplyTo are Liquid over the request's tenant
// variables, which is what lets one entry serve every tenant:
//
//	from_email: "sender+{{ tenant.slug }}@mail.example.com"
//	from_name:  "{{ tenant.name }}"
//
// The templates are parsed at startup; a syntax error is a refusal to start.
type PlatformSenderConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`

	TransportID string `yaml:"transport_id"`
	DomainID    string `yaml:"domain_id"`

	FromName  string `yaml:"from_name"`
	FromEmail string `yaml:"from_email"`
	ReplyTo   string `yaml:"reply_to"`

	// Uses restricts what tenants may use the sender for:
	// campaign, transactional, probe. Empty means campaign + transactional.
	Uses []string `yaml:"uses"`
	// ProbeVars are the tenant variables the loopback probe renders the
	// templates with. The probe runs once, in the system tenant, so the From
	// address needs values from somewhere (architecture 11.2).
	ProbeVars map[string]any `yaml:"probe_vars"`
}

// PlatformProbeMailboxConfig is one shared probe mailbox.
type PlatformProbeMailboxConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`

	Kind    string `yaml:"kind"` // imap (default) | webhook
	Address string `yaml:"address"`

	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	TLS      string `yaml:"tls"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	InboxFolder string `yaml:"inbox_folder"`
	SpamFolder  string `yaml:"spam_folder"`
	AuthServID  string `yaml:"authserv_id"`

	Enabled *bool `yaml:"enabled"` // omitted means true
}

// PlatformBounceMailboxConfig is one shared bounce mailbox. Mail in it may
// belong to any tenant; the tenant is found from the delivery ID in the VERP
// return path (architecture 10).
type PlatformBounceMailboxConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`

	Address  string `yaml:"address"`
	Protocol string `yaml:"protocol"` // imap (default) | pop3
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	TLS      string `yaml:"tls"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	Folder       string `yaml:"folder"`
	AfterProcess string `yaml:"after_process"` // keep (default) | delete | move:<folder>

	Enabled *bool `yaml:"enabled"`
}

// ToHost converts the section to sendplane.Platform. It does not validate:
// sendplane.New does that (unique IDs, resolvable references, parseable
// templates), and Config.Validate calls the same check so a bad `platform:`
// block is reported alongside every other configuration error rather than one
// at a time.
func (p PlatformConfig) ToHost() host.Platform {
	out := host.Platform{
		TrackingDomain:         strings.TrimSpace(p.TrackingDomain),
		UnsubscribeURLTemplate: strings.TrimSpace(p.UnsubscribeURLTemplate),
	}
	for _, t := range p.Transports {
		out.Transports = append(out.Transports, host.PlatformTransport{
			ID: trimLower(t.ID), Name: t.Name,
			Host: strings.TrimSpace(t.Host), Port: t.Port,
			TLS: store.TLSMode(trimLower(t.TLS)), Username: t.Username, Password: t.Password,
			MaxConns: t.MaxConns, RatePerSecond: t.RatePerSecond,
			PerTenantRatePerSecond: t.PerTenantRatePerSecond,
			DomainRatePerSecond:    t.DomainRatePerSecond,
		})
	}
	for _, d := range p.Domains {
		out.Domains = append(out.Domains, host.PlatformDomain{
			ID: trimLower(d.ID), Domain: trimLower(d.Domain),
			DKIMSelector: strings.TrimSpace(d.DKIMSelector), DKIMPrivateKey: d.DKIMPrivateKey,
			ReturnPathDomain: trimLower(d.ReturnPathDomain),
			ExpectedSPF:      strings.TrimSpace(d.ExpectedSPF),
			OutboundIPs:      d.OutboundIPs,
		})
	}
	for _, s := range p.Senders {
		uses := make([]store.UseKind, 0, len(s.Uses))
		for _, u := range s.Uses {
			uses = append(uses, store.UseKind(trimLower(u)))
		}
		out.Senders = append(out.Senders, host.PlatformSender{
			ID: trimLower(s.ID), Name: s.Name,
			TransportID: trimLower(s.TransportID), DomainID: trimLower(s.DomainID),
			FromName: s.FromName, FromEmail: strings.TrimSpace(s.FromEmail),
			ReplyTo: strings.TrimSpace(s.ReplyTo),
			Uses:    uses, ProbeVars: s.ProbeVars,
		})
	}
	for _, m := range p.ProbeMailboxes {
		out.ProbeMailboxes = append(out.ProbeMailboxes, host.PlatformProbeMailbox{
			ID: trimLower(m.ID), Name: m.Name,
			Kind: store.ProbeMailboxKind(trimLower(m.Kind)), Address: trimLower(m.Address),
			Host: strings.TrimSpace(m.Host), Port: m.Port,
			TLS: store.TLSMode(trimLower(m.TLS)), Username: m.Username, Password: m.Password,
			InboxFolder: m.InboxFolder, SpamFolder: m.SpamFolder,
			AuthServID: strings.TrimSpace(m.AuthServID), Enabled: m.Enabled,
		})
	}
	for _, m := range p.BounceMailboxes {
		out.BounceMailboxes = append(out.BounceMailboxes, host.PlatformBounceMailbox{
			ID: trimLower(m.ID), Name: m.Name,
			Address: trimLower(m.Address), Protocol: trimLower(m.Protocol),
			Host: strings.TrimSpace(m.Host), Port: m.Port,
			TLS: store.TLSMode(trimLower(m.TLS)), Username: m.Username, Password: m.Password,
			Folder: m.Folder, AfterProcess: strings.TrimSpace(m.AfterProcess),
			Enabled: m.Enabled,
		})
	}
	return out
}

func trimLower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
