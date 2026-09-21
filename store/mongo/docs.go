package mongo

import (
	"encoding/json"
	"time"

	"github.com/sendplane/sendplane/store"
)

// This file holds the BSON documents and their codecs. Keeping them explicit
// (instead of tagging the store models) keeps the on-disk names, the enum
// encoding and the timestamp truncation out of the contract package.

// --- shared pieces -----------------------------------------------------

type i18nDoc struct {
	DefaultLocale string                       `bson:"default_locale"`
	Locales       map[string]map[string]string `bson:"locales"`
}

func encI18n(b store.I18nBundle) i18nDoc {
	return i18nDoc{DefaultLocale: b.DefaultLocale, Locales: b.Locales}
}

func decI18n(d i18nDoc) store.I18nBundle {
	return store.I18nBundle{DefaultLocale: d.DefaultLocale, Locales: d.Locales}
}

func encRaw(r json.RawMessage) []byte {
	if len(r) == 0 {
		return nil
	}
	return []byte(r)
}

func decRaw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// --- transport ---------------------------------------------------------

type transportDoc struct {
	Base                `bson:",inline"`
	Name                string             `bson:"name"`
	Host                string             `bson:"host"`
	Port                int32              `bson:"port"`
	TLS                 string             `bson:"tls"`
	Username            string             `bson:"username"`
	Password            []byte             `bson:"password"`
	MaxConns            int32              `bson:"max_conns"`
	RatePerSecond       float64            `bson:"rate_per_second"`
	DomainRatePerSecond map[string]float64 `bson:"domain_rate_per_second"`
	Status              int32              `bson:"status"`
	StatusReason        string             `bson:"status_reason"`
	StatusChangedAt     *time.Time         `bson:"status_changed_at"`
	StatusUntil         *time.Time         `bson:"status_until"`
}

func transportMeta() meta[store.Transport, transportDoc] {
	return meta[store.Transport, transportDoc]{
		kind:    "transport",
		id:      func(v *store.Transport) *string { return &v.ID },
		tenant:  func(v *store.Transport) *string { return &v.TenantID },
		version: func(v *store.Transport) *int64 { return &v.Version },
		created: func(v *store.Transport) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Transport) *time.Time { return &v.UpdatedAt },
		enc: func(v *store.Transport) *transportDoc {
			return &transportDoc{
				Base: Base{ID: v.ID, TenantID: v.TenantID, Version: v.Version,
					CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
				Name: v.Name, Host: v.Host, Port: i32(v.Port), TLS: string(v.TLS),
				Username: v.Username, Password: v.Password,
				MaxConns: i32(v.MaxConns), RatePerSecond: v.RatePerSecond,
				DomainRatePerSecond: v.DomainRatePerSecond,
				Status:              int32(v.Status), StatusReason: v.StatusReason,
				StatusChangedAt: encTime(v.StatusChangedAt), StatusUntil: encTime(v.StatusUntil),
			}
		},
		dec: func(d *transportDoc) *store.Transport {
			return &store.Transport{
				ID: d.ID, TenantID: d.TenantID, Name: d.Name,
				Host: d.Host, Port: int(d.Port), TLS: store.TLSMode(d.TLS),
				Username: d.Username, Password: d.Password,
				MaxConns: int(d.MaxConns), RatePerSecond: d.RatePerSecond,
				DomainRatePerSecond: d.DomainRatePerSecond,
				Status:              store.TransportStatus(enum8(d.Status)), StatusReason: d.StatusReason,
				StatusChangedAt: decTime(d.StatusChangedAt), StatusUntil: decTime(d.StatusUntil),
				Version: d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
			}
		},
	}
}

// --- sender ------------------------------------------------------------

type senderDoc struct {
	Base            `bson:",inline"`
	Name            string     `bson:"name"`
	FromName        string     `bson:"from_name"`
	FromEmail       string     `bson:"from_email"`
	ReplyTo         string     `bson:"reply_to"`
	TransportID     string     `bson:"transport_id"`
	DomainID        string     `bson:"domain_id"`
	Health          int32      `bson:"health"`
	HealthReason    string     `bson:"health_reason"`
	HealthCheckedAt *time.Time `bson:"health_checked_at"`
}

func senderMeta() meta[store.Sender, senderDoc] {
	return meta[store.Sender, senderDoc]{
		kind:    "sender",
		id:      func(v *store.Sender) *string { return &v.ID },
		tenant:  func(v *store.Sender) *string { return &v.TenantID },
		version: func(v *store.Sender) *int64 { return &v.Version },
		created: func(v *store.Sender) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Sender) *time.Time { return &v.UpdatedAt },
		enc: func(v *store.Sender) *senderDoc {
			return &senderDoc{
				Base: Base{ID: v.ID, TenantID: v.TenantID, Version: v.Version,
					CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
				Name: v.Name, FromName: v.FromName, FromEmail: v.FromEmail,
				ReplyTo: v.ReplyTo, TransportID: v.TransportID, DomainID: v.DomainID,
				Health: int32(v.Health), HealthReason: v.HealthReason,
				HealthCheckedAt: encTime(v.HealthCheckedAt),
			}
		},
		dec: func(d *senderDoc) *store.Sender {
			return &store.Sender{
				ID: d.ID, TenantID: d.TenantID, Name: d.Name,
				FromName: d.FromName, FromEmail: d.FromEmail, ReplyTo: d.ReplyTo,
				TransportID: d.TransportID, DomainID: d.DomainID,
				Health: store.HealthStatus(enum8(d.Health)), HealthReason: d.HealthReason,
				HealthCheckedAt: decTime(d.HealthCheckedAt),
				Version:         d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
			}
		},
	}
}

// --- sending domain ----------------------------------------------------

type domainDoc struct {
	Base             `bson:",inline"`
	Domain           string     `bson:"domain"`
	DKIMSelector     string     `bson:"dkim_selector"`
	DKIMPrivateKey   []byte     `bson:"dkim_private_key"`
	ReturnPathDomain string     `bson:"return_path_domain"`
	ExpectedSPF      string     `bson:"expected_spf"`
	OutboundIPs      []string   `bson:"outbound_ips"`
	Health           int32      `bson:"health"`
	HealthReason     string     `bson:"health_reason"`
	HealthCheckedAt  *time.Time `bson:"health_checked_at"`
}

func domainMeta() meta[store.SendingDomain, domainDoc] {
	return meta[store.SendingDomain, domainDoc]{
		kind:    "sending domain",
		id:      func(v *store.SendingDomain) *string { return &v.ID },
		tenant:  func(v *store.SendingDomain) *string { return &v.TenantID },
		version: func(v *store.SendingDomain) *int64 { return &v.Version },
		created: func(v *store.SendingDomain) *time.Time { return &v.CreatedAt },
		updated: func(v *store.SendingDomain) *time.Time { return &v.UpdatedAt },
		enc: func(v *store.SendingDomain) *domainDoc {
			return &domainDoc{
				Base: Base{ID: v.ID, TenantID: v.TenantID, Version: v.Version,
					CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
				Domain: v.Domain, DKIMSelector: v.DKIMSelector, DKIMPrivateKey: v.DKIMPrivateKey,
				ReturnPathDomain: v.ReturnPathDomain, ExpectedSPF: v.ExpectedSPF,
				OutboundIPs: v.OutboundIPs,
				Health:      int32(v.Health), HealthReason: v.HealthReason,
				HealthCheckedAt: encTime(v.HealthCheckedAt),
			}
		},
		dec: func(d *domainDoc) *store.SendingDomain {
			return &store.SendingDomain{
				ID: d.ID, TenantID: d.TenantID, Domain: d.Domain,
				DKIMSelector: d.DKIMSelector, DKIMPrivateKey: d.DKIMPrivateKey,
				ReturnPathDomain: d.ReturnPathDomain, ExpectedSPF: d.ExpectedSPF,
				OutboundIPs: d.OutboundIPs,
				Health:      store.HealthStatus(enum8(d.Health)), HealthReason: d.HealthReason,
				HealthCheckedAt: decTime(d.HealthCheckedAt),
				Version:         d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
			}
		},
	}
}

// --- mailbox health ----------------------------------------------------

// healthDoc is the MailboxHealth block probe and bounce mailboxes share. It is
// a nested document rather than six inlined fields so that UpdateHealth can
// $set it as one key without naming every column.
type healthDoc struct {
	Status    int32      `bson:"status"`
	Stage     string     `bson:"stage"`
	Reason    string     `bson:"reason"`
	CheckedAt *time.Time `bson:"checked_at"`
	LastOKAt  *time.Time `bson:"last_ok_at"`
	Failures  int32      `bson:"failures"`
}

func encHealth(h store.MailboxHealth) healthDoc {
	return healthDoc{
		Status: int32(h.Status), Stage: h.Stage, Reason: h.Reason,
		CheckedAt: encTime(h.CheckedAt), LastOKAt: encTime(h.LastOKAt),
		Failures: i32(h.ConsecutiveFailures),
	}
}

func decHealth(d healthDoc) store.MailboxHealth {
	return store.MailboxHealth{
		Status: store.MailboxStatus(enum8(d.Status)), Stage: d.Stage, Reason: d.Reason,
		CheckedAt: decTime(d.CheckedAt), LastOKAt: decTime(d.LastOKAt),
		ConsecutiveFailures: int(d.Failures),
	}
}

// --- bounce mailbox ----------------------------------------------------

type bounceMailboxDoc struct {
	Base         `bson:",inline"`
	Name         string    `bson:"name"`
	Address      string    `bson:"address"`
	Protocol     string    `bson:"protocol"`
	Host         string    `bson:"host"`
	Port         int32     `bson:"port"`
	TLS          string    `bson:"tls"`
	Username     string    `bson:"username"`
	Password     []byte    `bson:"password"`
	Folder       string    `bson:"folder"`
	AfterProcess string    `bson:"after_process"`
	Enabled      bool      `bson:"enabled"`
	Health       healthDoc `bson:"health"`
}

func bounceMailboxMeta() meta[store.BounceMailbox, bounceMailboxDoc] {
	return meta[store.BounceMailbox, bounceMailboxDoc]{
		kind:    "bounce mailbox",
		id:      func(v *store.BounceMailbox) *string { return &v.ID },
		tenant:  func(v *store.BounceMailbox) *string { return &v.TenantID },
		version: func(v *store.BounceMailbox) *int64 { return &v.Version },
		created: func(v *store.BounceMailbox) *time.Time { return &v.CreatedAt },
		updated: func(v *store.BounceMailbox) *time.Time { return &v.UpdatedAt },
		enc: func(v *store.BounceMailbox) *bounceMailboxDoc {
			return &bounceMailboxDoc{
				Base: Base{ID: v.ID, TenantID: v.TenantID, Version: v.Version,
					CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
				Name: v.Name, Address: v.Address, Protocol: v.Protocol,
				Host: v.Host, Port: i32(v.Port), TLS: string(v.TLS),
				Username: v.Username, Password: v.Password,
				Folder: v.Folder, AfterProcess: v.AfterProcess, Enabled: v.Enabled,
				Health: encHealth(v.Health),
			}
		},
		dec: func(d *bounceMailboxDoc) *store.BounceMailbox {
			return &store.BounceMailbox{
				ID: d.ID, TenantID: d.TenantID, Name: d.Name, Address: d.Address,
				Protocol: d.Protocol, Host: d.Host, Port: int(d.Port),
				TLS: store.TLSMode(d.TLS), Username: d.Username, Password: d.Password,
				Folder: d.Folder, AfterProcess: d.AfterProcess, Enabled: d.Enabled,
				Health:  decHealth(d.Health),
				Version: d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
			}
		},
	}
}

// --- probe mailbox -----------------------------------------------------

type probeMailboxDoc struct {
	Base        `bson:",inline"`
	Name        string    `bson:"name"`
	Kind        string    `bson:"kind"`
	Address     string    `bson:"address"`
	Host        string    `bson:"host"`
	Port        int32     `bson:"port"`
	TLS         string    `bson:"tls"`
	Username    string    `bson:"username"`
	Password    []byte    `bson:"password"`
	InboxFolder string    `bson:"inbox_folder"`
	SpamFolder  string    `bson:"spam_folder"`
	AuthServID  string    `bson:"auth_serv_id"`
	Enabled     bool      `bson:"enabled"`
	Health      healthDoc `bson:"health"`
}

func probeMailboxMeta() meta[store.ProbeMailbox, probeMailboxDoc] {
	return meta[store.ProbeMailbox, probeMailboxDoc]{
		kind:    "probe mailbox",
		id:      func(v *store.ProbeMailbox) *string { return &v.ID },
		tenant:  func(v *store.ProbeMailbox) *string { return &v.TenantID },
		version: func(v *store.ProbeMailbox) *int64 { return &v.Version },
		created: func(v *store.ProbeMailbox) *time.Time { return &v.CreatedAt },
		updated: func(v *store.ProbeMailbox) *time.Time { return &v.UpdatedAt },
		enc: func(v *store.ProbeMailbox) *probeMailboxDoc {
			return &probeMailboxDoc{
				Base: Base{ID: v.ID, TenantID: v.TenantID, Version: v.Version,
					CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
				Name: v.Name, Kind: string(v.Kind),
				Address: v.Address, Host: v.Host, Port: i32(v.Port),
				TLS: string(v.TLS), Username: v.Username, Password: v.Password,
				InboxFolder: v.InboxFolder, SpamFolder: v.SpamFolder,
				AuthServID: v.AuthServID, Enabled: v.Enabled,
				Health: encHealth(v.Health),
			}
		},
		dec: func(d *probeMailboxDoc) *store.ProbeMailbox {
			return &store.ProbeMailbox{
				ID: d.ID, TenantID: d.TenantID, Name: d.Name,
				Kind: store.ProbeMailboxKind(d.Kind), Address: d.Address,
				Host: d.Host, Port: int(d.Port), TLS: store.TLSMode(d.TLS),
				Username: d.Username, Password: d.Password,
				InboxFolder: d.InboxFolder, SpamFolder: d.SpamFolder,
				AuthServID: d.AuthServID, Enabled: d.Enabled,
				Health:  decHealth(d.Health),
				Version: d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
			}
		},
	}
}

// --- layout ------------------------------------------------------------

type layoutDoc struct {
	Base `bson:",inline"`
	Name string  `bson:"name"`
	Mode string  `bson:"mode"`
	Body string  `bson:"body"`
	I18n i18nDoc `bson:"i18n"`
}

func layoutMeta() meta[store.Layout, layoutDoc] {
	return meta[store.Layout, layoutDoc]{
		kind:    "layout",
		id:      func(v *store.Layout) *string { return &v.ID },
		tenant:  func(v *store.Layout) *string { return &v.TenantID },
		version: func(v *store.Layout) *int64 { return &v.Version },
		created: func(v *store.Layout) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Layout) *time.Time { return &v.UpdatedAt },
		enc: func(v *store.Layout) *layoutDoc {
			return &layoutDoc{
				Base: Base{ID: v.ID, TenantID: v.TenantID, Version: v.Version,
					CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
				Name: v.Name, Mode: string(v.Mode), Body: v.Body, I18n: encI18n(v.I18n),
			}
		},
		dec: func(d *layoutDoc) *store.Layout {
			return &store.Layout{
				ID: d.ID, TenantID: d.TenantID, Name: d.Name,
				Mode: store.ContentMode(d.Mode), Body: d.Body, I18n: decI18n(d.I18n),
				Version: d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
			}
		},
	}
}

// --- template ----------------------------------------------------------

type templateDoc struct {
	Base               `bson:",inline"`
	Name               string  `bson:"name"`
	LayoutID           string  `bson:"layout_id"`
	Subject            string  `bson:"subject"`
	Preheader          string  `bson:"preheader"`
	Mode               string  `bson:"mode"`
	Body               string  `bson:"body"`
	Blocks             []byte  `bson:"blocks"`
	Text               string  `bson:"text"`
	I18n               i18nDoc `bson:"i18n"`
	DefaultLocale      string  `bson:"default_locale"`
	PublishedVersionID string  `bson:"published_version_id"`
}

func templateMeta() meta[store.Template, templateDoc] {
	return meta[store.Template, templateDoc]{
		kind:    "template",
		id:      func(v *store.Template) *string { return &v.ID },
		tenant:  func(v *store.Template) *string { return &v.TenantID },
		version: func(v *store.Template) *int64 { return &v.Version },
		created: func(v *store.Template) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Template) *time.Time { return &v.UpdatedAt },
		enc: func(v *store.Template) *templateDoc {
			return &templateDoc{
				Base: Base{ID: v.ID, TenantID: v.TenantID, Version: v.Version,
					CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
				Name: v.Name, LayoutID: v.LayoutID, Subject: v.Subject,
				Preheader: v.Preheader, Mode: string(v.Mode), Body: v.Body,
				Blocks: encRaw(v.Blocks), Text: v.Text, I18n: encI18n(v.I18n),
				DefaultLocale: v.DefaultLocale, PublishedVersionID: v.PublishedVersionID,
			}
		},
		dec: func(d *templateDoc) *store.Template {
			return &store.Template{
				ID: d.ID, TenantID: d.TenantID, Name: d.Name, LayoutID: d.LayoutID,
				Subject: d.Subject, Preheader: d.Preheader, Mode: store.ContentMode(d.Mode),
				Body: d.Body, Blocks: decRaw(d.Blocks), Text: d.Text, I18n: decI18n(d.I18n),
				DefaultLocale: d.DefaultLocale, PublishedVersionID: d.PublishedVersionID,
				Version: d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
			}
		},
	}
}

// --- message version (immutable) ---------------------------------------

type versionDoc struct {
	Base          `bson:",inline"`
	TemplateID    string   `bson:"template_id"`
	LayoutID      string   `bson:"layout_id"`
	SubjectTpl    string   `bson:"subject_tpl"`
	HTMLTpl       string   `bson:"html_tpl"`
	TextTpl       string   `bson:"text_tpl"`
	I18n          i18nDoc  `bson:"i18n"`
	DefaultLocale string   `bson:"default_locale"`
	Links         []string `bson:"links"`
	Checksum      string   `bson:"checksum"`
}

func versionMeta() meta[store.MessageVersion, versionDoc] {
	return meta[store.MessageVersion, versionDoc]{
		kind:    "message version",
		id:      func(v *store.MessageVersion) *string { return &v.ID },
		tenant:  func(v *store.MessageVersion) *string { return &v.TenantID },
		created: func(v *store.MessageVersion) *time.Time { return &v.CreatedAt },
		enc: func(v *store.MessageVersion) *versionDoc {
			return &versionDoc{
				Base:       Base{ID: v.ID, TenantID: v.TenantID, CreatedAt: ts(v.CreatedAt)},
				TemplateID: v.TemplateID, LayoutID: v.LayoutID,
				SubjectTpl: v.SubjectTpl, HTMLTpl: v.HTMLTpl, TextTpl: v.TextTpl,
				I18n: encI18n(v.I18n), DefaultLocale: v.DefaultLocale,
				Links: v.Links, Checksum: v.Checksum,
			}
		},
		dec: func(d *versionDoc) *store.MessageVersion {
			return &store.MessageVersion{
				ID: d.ID, TenantID: d.TenantID, TemplateID: d.TemplateID, LayoutID: d.LayoutID,
				SubjectTpl: d.SubjectTpl, HTMLTpl: d.HTMLTpl, TextTpl: d.TextTpl,
				I18n: decI18n(d.I18n), DefaultLocale: d.DefaultLocale,
				Links: d.Links, Checksum: d.Checksum, CreatedAt: decTime(&d.CreatedAt),
			}
		},
	}
}

// --- probe run (immutable) ---------------------------------------------

type probeRunDoc struct {
	Base         `bson:",inline"`
	SenderID     string     `bson:"sender_id"`
	MailboxID    string     `bson:"mailbox_id"`
	DeliveryID   string     `bson:"delivery_id"`
	GroupID      string     `bson:"group_id"`
	Pending      bool       `bson:"pending"`
	Status       int32      `bson:"status"`
	Reason       string     `bson:"reason"`
	Delivered    bool       `bson:"delivered"`
	Folder       string     `bson:"folder"`
	Latency      int64      `bson:"latency_ns"`
	SPF          string     `bson:"spf"`
	DKIM         string     `bson:"dkim"`
	DMARC        string     `bson:"dmarc"`
	DKIMDomain   string     `bson:"dkim_domain"`
	DKIMSelector string     `bson:"dkim_selector"`
	DMARCPolicy  string     `bson:"dmarc_policy"`
	TLS          bool       `bson:"tls"`
	ObservedIP   string     `bson:"observed_ip"`
	PTR          string     `bson:"ptr"`
	PTRMatch     bool       `bson:"ptr_match"`
	DNS          []byte     `bson:"dns"`
	RawHeaders   string     `bson:"raw_headers"`
	StartedAt    *time.Time `bson:"started_at"`
	ReceivedAt   *time.Time `bson:"received_at"`
}

func probeRunMeta() meta[store.ProbeRun, probeRunDoc] {
	return meta[store.ProbeRun, probeRunDoc]{
		kind:    "probe run",
		id:      func(v *store.ProbeRun) *string { return &v.ID },
		tenant:  func(v *store.ProbeRun) *string { return &v.TenantID },
		created: func(v *store.ProbeRun) *time.Time { return &v.CreatedAt },
		enc: func(v *store.ProbeRun) *probeRunDoc {
			return &probeRunDoc{
				Base:     Base{ID: v.ID, TenantID: v.TenantID, CreatedAt: ts(v.CreatedAt)},
				SenderID: v.SenderID, MailboxID: v.MailboxID, DeliveryID: v.DeliveryID,
				GroupID: v.GroupID, Pending: v.Pending,
				Status: int32(v.Status), Reason: v.Reason, Delivered: v.Delivered,
				Folder: v.Folder, Latency: int64(v.Latency),
				SPF: v.SPF, DKIM: v.DKIM, DMARC: v.DMARC,
				DKIMDomain: v.DKIMDomain, DKIMSelector: v.DKIMSelector, DMARCPolicy: v.DMARCPolicy,
				TLS: v.TLS, ObservedIP: v.ObservedIP, PTR: v.PTR, PTRMatch: v.PTRMatch,
				DNS: encRaw(v.DNS), RawHeaders: v.RawHeaders,
				StartedAt: encTime(v.StartedAt), ReceivedAt: encTime(v.ReceivedAt),
			}
		},
		dec: func(d *probeRunDoc) *store.ProbeRun {
			return &store.ProbeRun{
				ID: d.ID, TenantID: d.TenantID, SenderID: d.SenderID,
				MailboxID: d.MailboxID, DeliveryID: d.DeliveryID,
				GroupID: d.GroupID, Pending: d.Pending,
				Status: store.HealthStatus(enum8(d.Status)), Reason: d.Reason,
				Delivered: d.Delivered, Folder: d.Folder, Latency: time.Duration(d.Latency),
				SPF: d.SPF, DKIM: d.DKIM, DMARC: d.DMARC,
				DKIMDomain: d.DKIMDomain, DKIMSelector: d.DKIMSelector, DMARCPolicy: d.DMARCPolicy,
				TLS: d.TLS, ObservedIP: d.ObservedIP, PTR: d.PTR, PTRMatch: d.PTRMatch,
				DNS: decRaw(d.DNS), RawHeaders: d.RawHeaders,
				StartedAt: decTime(d.StartedAt), ReceivedAt: decTime(d.ReceivedAt),
				CreatedAt: decTime(&d.CreatedAt),
			}
		},
	}
}

// --- campaign ----------------------------------------------------------

type statsDoc struct {
	ByStatus           map[string]int64 `bson:"by_status"`
	UniqueOpens        int64            `bson:"unique_opens"`
	UniqueClicks       int64            `bson:"unique_clicks"`
	Unsubscribed       int64            `bson:"unsubscribed"`
	UnsubscribeClicked int64            `bson:"unsubscribe_clicked"`
	ComputedAt         *time.Time       `bson:"computed_at"`
}

func encStats(s store.CampaignStats) statsDoc {
	return statsDoc{
		ByStatus: encStatusMap(s.ByStatus), UniqueOpens: s.UniqueOpens,
		UniqueClicks: s.UniqueClicks, Unsubscribed: s.Unsubscribed,
		UnsubscribeClicked: s.UnsubscribeClicked, ComputedAt: encTime(s.ComputedAt),
	}
}

func decStats(d statsDoc) store.CampaignStats {
	return store.CampaignStats{
		ByStatus: decStatusMap(d.ByStatus), UniqueOpens: d.UniqueOpens,
		UniqueClicks: d.UniqueClicks, Unsubscribed: d.Unsubscribed,
		UnsubscribeClicked: d.UnsubscribeClicked, ComputedAt: decTime(d.ComputedAt),
	}
}

type campaignDoc struct {
	Base          `bson:",inline"`
	Name          string         `bson:"name"`
	TemplateID    string         `bson:"template_id"`
	VersionID     string         `bson:"version_id"`
	SenderID      string         `bson:"sender_id"`
	DefaultLocale string         `bson:"default_locale"`
	Vars          map[string]any `bson:"vars"`
	Status        int32          `bson:"status"`
	ScheduleAt    *time.Time     `bson:"schedule_at"`
	StartedAt     *time.Time     `bson:"started_at"`
	CompletedAt   *time.Time     `bson:"completed_at"`
	Stats         statsDoc       `bson:"stats"`
}

func campaignMeta() meta[store.Campaign, campaignDoc] {
	return meta[store.Campaign, campaignDoc]{
		kind:    "campaign",
		id:      func(v *store.Campaign) *string { return &v.ID },
		tenant:  func(v *store.Campaign) *string { return &v.TenantID },
		version: func(v *store.Campaign) *int64 { return &v.Version },
		created: func(v *store.Campaign) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Campaign) *time.Time { return &v.UpdatedAt },
		enc: func(v *store.Campaign) *campaignDoc {
			return &campaignDoc{
				Base: Base{ID: v.ID, TenantID: v.TenantID, Version: v.Version,
					CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
				Name: v.Name, TemplateID: v.TemplateID, VersionID: v.VersionID,
				SenderID:      v.SenderID,
				DefaultLocale: v.DefaultLocale, Vars: v.Vars, Status: int32(v.Status),
				ScheduleAt: encTime(v.ScheduleAt), StartedAt: encTime(v.StartedAt),
				CompletedAt: encTime(v.CompletedAt), Stats: encStats(v.Stats),
			}
		},
		dec: func(d *campaignDoc) *store.Campaign {
			return &store.Campaign{
				ID: d.ID, TenantID: d.TenantID, Name: d.Name,
				TemplateID: d.TemplateID, VersionID: d.VersionID, SenderID: d.SenderID,
				DefaultLocale: d.DefaultLocale, Vars: d.Vars,
				Status:     store.CampaignStatus(enum8(d.Status)),
				ScheduleAt: decTime(d.ScheduleAt), StartedAt: decTime(d.StartedAt),
				CompletedAt: decTime(d.CompletedAt), Stats: decStats(d.Stats),
				Version: d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
			}
		},
	}
}

// --- bounce event (immutable) ------------------------------------------

type bounceDoc struct {
	Base           `bson:",inline"`
	DeliveryID     string     `bson:"delivery_id"`
	Type           int32      `bson:"type"`
	Source         string     `bson:"source"`
	Verified       bool       `bson:"verified"`
	Recipient      string     `bson:"recipient"`
	EmailNorm      string     `bson:"email_norm"`
	SMTPStatus     string     `bson:"smtp_status"`
	DiagnosticCode string     `bson:"diagnostic_code"`
	MessageID      string     `bson:"message_id"`
	Raw            []byte     `bson:"raw"`
	ReceivedAt     *time.Time `bson:"received_at"`
}

func bounceMeta() meta[store.BounceEvent, bounceDoc] {
	return meta[store.BounceEvent, bounceDoc]{
		kind:    "bounce",
		id:      func(v *store.BounceEvent) *string { return &v.ID },
		tenant:  func(v *store.BounceEvent) *string { return &v.TenantID },
		created: func(v *store.BounceEvent) *time.Time { return &v.CreatedAt },
		enc: func(v *store.BounceEvent) *bounceDoc {
			return &bounceDoc{
				Base:       Base{ID: v.ID, TenantID: v.TenantID, CreatedAt: ts(v.CreatedAt)},
				DeliveryID: v.DeliveryID, Type: int32(v.Type), Source: string(v.Source),
				Verified: v.Verified, Recipient: v.Recipient, EmailNorm: v.EmailNorm,
				SMTPStatus: v.SMTPStatus, DiagnosticCode: v.DiagnosticCode,
				MessageID: v.MessageID, Raw: encRaw(v.Raw), ReceivedAt: encTime(v.ReceivedAt),
			}
		},
		dec: func(d *bounceDoc) *store.BounceEvent {
			return &store.BounceEvent{
				ID: d.ID, TenantID: d.TenantID, DeliveryID: d.DeliveryID,
				Type: store.BounceType(enum8(d.Type)), Source: store.BounceSource(d.Source),
				Verified: d.Verified, Recipient: d.Recipient, EmailNorm: d.EmailNorm,
				SMTPStatus: d.SMTPStatus, DiagnosticCode: d.DiagnosticCode,
				MessageID: d.MessageID, Raw: decRaw(d.Raw),
				ReceivedAt: decTime(d.ReceivedAt), CreatedAt: decTime(&d.CreatedAt),
			}
		},
	}
}

// --- tenant settings ---------------------------------------------------

type signingKeyDoc struct {
	KID       string     `bson:"kid"`
	Secret    []byte     `bson:"secret"`
	CreatedAt *time.Time `bson:"created_at"`
}

type trackingConfigDoc struct {
	Domain      string          `bson:"domain"`
	Opens       bool            `bson:"opens"`
	Clicks      bool            `bson:"clicks"`
	SigningKeys []signingKeyDoc `bson:"signing_keys"`
}

type settingsDoc struct {
	Base                   `bson:",inline"`
	BackoffNS              []int64           `bson:"retry_backoff_ns"`
	MaxAttempts            int32             `bson:"retry_max_attempts"`
	RetentionDays          int32             `bson:"retention_days"`
	SuppressionEnabled     bool              `bson:"suppression_enabled"`
	BounceRetainRaw        bool              `bson:"bounce_retain_raw"`
	UnsubscribeMode        string            `bson:"unsubscribe_mode"`
	UnsubscribeURLTemplate string            `bson:"unsubscribe_url_template"`
	UnsubscribeOneClick    bool              `bson:"unsubscribe_one_click"`
	DefaultLocale          string            `bson:"default_locale"`
	Tracking               trackingConfigDoc `bson:"tracking"`
	EventTypes             []string          `bson:"event_types"`
}

func encSettings(v *store.TenantSettings) *settingsDoc {
	backoff := make([]int64, len(v.Retry.Backoff))
	for i, d := range v.Retry.Backoff {
		backoff[i] = int64(d)
	}
	keys := make([]signingKeyDoc, len(v.Tracking.SigningKeys))
	for i, k := range v.Tracking.SigningKeys {
		keys[i] = signingKeyDoc{KID: k.KID, Secret: k.Secret, CreatedAt: encTime(k.CreatedAt)}
	}
	return &settingsDoc{
		Base: Base{ID: v.TenantID, TenantID: v.TenantID, Version: v.Version,
			CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
		BackoffNS: backoff, MaxAttempts: i32(v.Retry.MaxAttempts),
		RetentionDays: i32(v.RetentionDays), SuppressionEnabled: v.SuppressionEnabled,
		BounceRetainRaw:        v.BounceRetainRaw,
		UnsubscribeMode:        string(v.UnsubscribeMode),
		UnsubscribeURLTemplate: v.UnsubscribeURLTemplate,
		UnsubscribeOneClick:    v.UnsubscribeOneClick,
		DefaultLocale:          v.DefaultLocale,
		Tracking: trackingConfigDoc{
			Domain: v.Tracking.Domain, Opens: v.Tracking.Opens,
			Clicks: v.Tracking.Clicks, SigningKeys: keys,
		},
		EventTypes: v.EventTypes,
	}
}

func decSettings(d *settingsDoc) *store.TenantSettings {
	backoff := make([]time.Duration, len(d.BackoffNS))
	for i, n := range d.BackoffNS {
		backoff[i] = time.Duration(n)
	}
	keys := make([]store.SigningKey, len(d.Tracking.SigningKeys))
	for i, k := range d.Tracking.SigningKeys {
		keys[i] = store.SigningKey{KID: k.KID, Secret: k.Secret, CreatedAt: decTime(k.CreatedAt)}
	}
	return &store.TenantSettings{
		TenantID:               d.TenantID,
		Retry:                  store.RetryPolicy{Backoff: backoff, MaxAttempts: int(d.MaxAttempts)},
		RetentionDays:          int(d.RetentionDays),
		SuppressionEnabled:     d.SuppressionEnabled,
		BounceRetainRaw:        d.BounceRetainRaw,
		UnsubscribeMode:        store.UnsubscribeMode(d.UnsubscribeMode),
		UnsubscribeURLTemplate: d.UnsubscribeURLTemplate,
		UnsubscribeOneClick:    d.UnsubscribeOneClick,
		DefaultLocale:          d.DefaultLocale,
		Tracking: store.TrackingConfig{
			Domain: d.Tracking.Domain, Opens: d.Tracking.Opens,
			Clicks: d.Tracking.Clicks, SigningKeys: keys,
		},
		EventTypes: d.EventTypes,
		Version:    d.Version, CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
	}
}

// --- recipient chunk ---------------------------------------------------

type chunkDoc struct {
	Base       `bson:",inline"`
	CampaignID string `bson:"campaign_id"`
	Key        string `bson:"key"`
	State      string `bson:"state"`
	Accepted   int64  `bson:"accepted"`
	Duplicates int64  `bson:"duplicates"`
	Invalid    int64  `bson:"invalid"`
}

func decChunk(d *chunkDoc) *store.RecipientChunk {
	return &store.RecipientChunk{
		TenantID: d.TenantID, CampaignID: d.CampaignID, Key: d.Key,
		State: store.ChunkState(d.State), Accepted: int(d.Accepted),
		Duplicates: int(d.Duplicates), Invalid: int(d.Invalid),
		CreatedAt: decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
	}
}

// --- suppression -------------------------------------------------------

type suppressionDoc struct {
	Base             `bson:",inline"`
	EmailNorm        string     `bson:"email_norm"`
	Reason           string     `bson:"reason"`
	SourceDeliveryID string     `bson:"source_delivery_id"`
	ExpiresAt        *time.Time `bson:"expires_at"`
}

func decSuppression(d *suppressionDoc) *store.Suppression {
	return &store.Suppression{
		TenantID: d.TenantID, EmailNorm: d.EmailNorm,
		Reason:           store.SuppressionReason(d.Reason),
		SourceDeliveryID: d.SourceDeliveryID,
		CreatedAt:        decTime(&d.CreatedAt), ExpiresAt: decTime(d.ExpiresAt),
	}
}

// --- tracking event ----------------------------------------------------

type trackingDoc struct {
	Base         `bson:",inline"`
	DeliveryID   string `bson:"delivery_id"`
	CampaignID   string `bson:"campaign_id"`
	Kind         int32  `bson:"kind"`
	URL          string `bson:"url"`
	LinkNo       int32  `bson:"link_no"`
	UserAgent    string `bson:"user_agent"`
	IPHash       string `bson:"ip_hash"`
	SuspectedBot bool   `bson:"suspected_bot"`
}

// --- outbox event ------------------------------------------------------

type outboxDoc struct {
	Base          `bson:",inline"`
	Type          string     `bson:"type"`
	Payload       []byte     `bson:"payload"`
	Status        string     `bson:"status"`
	Attempts      int32      `bson:"attempts"`
	NextAttemptAt *time.Time `bson:"next_attempt_at"`
	LeaseOwner    string     `bson:"lease_owner"`
	LeaseUntil    *time.Time `bson:"lease_until"`
	ClaimToken    string     `bson:"claim_token"`
	LastError     string     `bson:"last_error"`
	DeliveredAt   *time.Time `bson:"delivered_at"`
}

func decOutbox(d *outboxDoc) *store.OutboxEvent {
	return &store.OutboxEvent{
		ID: d.ID, TenantID: d.TenantID, Type: d.Type, Payload: decRaw(d.Payload),
		Status: store.OutboxStatus(d.Status), Attempts: int(d.Attempts),
		NextAttemptAt: decTime(d.NextAttemptAt),
		LeaseOwner:    d.LeaseOwner, LeaseUntil: decTime(d.LeaseUntil),
		LastError: d.LastError, CreatedAt: decTime(&d.CreatedAt),
		DeliveredAt: decTime(d.DeliveredAt),
	}
}

// --- lock --------------------------------------------------------------

type lockDoc struct {
	ID         string    `bson:"_id"`
	TenantID   string    `bson:"tenant_id"`
	Name       string    `bson:"name"`
	Owner      string    `bson:"owner"`
	AcquiredAt time.Time `bson:"acquired_at"`
	ExpiresAt  time.Time `bson:"expires_at"`
}

// --- worker ------------------------------------------------------------

type workerDoc struct {
	ID          string    `bson:"_id"`
	TenantID    string    `bson:"tenant_id"`
	WorkerID    string    `bson:"worker_id"`
	Role        string    `bson:"role"`
	Lanes       []int32   `bson:"lanes"`
	Concurrency int32     `bson:"concurrency"`
	StartedAt   time.Time `bson:"started_at"`
	LastSeenAt  time.Time `bson:"last_seen_at"`
}

func decWorker(d *workerDoc) store.Worker {
	return store.Worker{
		ID: d.WorkerID, TenantID: d.TenantID, Role: d.Role,
		Lanes: lanesFrom(d.Lanes), Concurrency: int(d.Concurrency),
		StartedAt: decTime(&d.StartedAt), LastSeenAt: decTime(&d.LastSeenAt),
	}
}

// --- delivery ----------------------------------------------------------

type deliveryDoc struct {
	Base `bson:",inline"`
	// CampaignID is omitted entirely for transactional and probe mail, which
	// is what keeps those rows out of the unique (campaign_id, email_norm)
	// partial index.
	CampaignID     string            `bson:"campaign_id,omitempty"`
	VersionID      string            `bson:"version_id"`
	SenderID       string            `bson:"sender_id"`
	Lane           int32             `bson:"lane"`
	Priority       int32             `bson:"priority"`
	Status         int32             `bson:"status"`
	Email          string            `bson:"email"`
	EmailNorm      string            `bson:"email_norm"`
	Name           string            `bson:"name"`
	Locale         string            `bson:"locale"`
	Vars           map[string]any    `bson:"vars"`
	UnsubscribeURL string            `bson:"unsubscribe_url"`
	Headers        map[string]string `bson:"headers"`
	AttemptCount   int32             `bson:"attempt_count"`
	RetryGen       int32             `bson:"retry_gen"`
	NextAttemptAt  *time.Time        `bson:"next_attempt_at"`
	LeaseOwner     string            `bson:"lease_owner"`
	LeaseUntil     *time.Time        `bson:"lease_until"`
	ClaimToken     string            `bson:"claim_token"`
	LastErrorClass int32             `bson:"last_error_class"`
	LastSMTPCode   int32             `bson:"last_smtp_code"`
	LastError      string            `bson:"last_error"`
	MessageID      string            `bson:"message_id"`
	SentAt         *time.Time        `bson:"sent_at"`
	FinishedAt     *time.Time        `bson:"finished_at"`
	FirstOpenedAt  *time.Time        `bson:"first_opened_at"`
	FirstClickedAt *time.Time        `bson:"first_clicked_at"`
	UnsubscribedAt *time.Time        `bson:"unsubscribed_at"`
}

func encDelivery(v *store.Delivery) *deliveryDoc {
	return &deliveryDoc{
		Base: Base{ID: v.ID, TenantID: v.TenantID,
			CreatedAt: ts(v.CreatedAt), UpdatedAt: encTime(v.UpdatedAt)},
		CampaignID: v.CampaignID, VersionID: v.VersionID, SenderID: v.SenderID,
		Lane: int32(v.Lane), Priority: i32(v.Priority), Status: int32(v.Status),
		Email: v.Email, EmailNorm: v.EmailNorm, Name: v.Name, Locale: v.Locale,
		Vars: v.Vars, UnsubscribeURL: v.UnsubscribeURL, Headers: v.Headers,
		AttemptCount: i32(v.AttemptCount), RetryGen: i32(v.RetryGen),
		NextAttemptAt: encTime(v.NextAttemptAt),
		LeaseOwner:    v.LeaseOwner, LeaseUntil: encTime(v.LeaseUntil),
		LastErrorClass: int32(v.LastErrorClass), LastSMTPCode: i32(v.LastSMTPCode),
		LastError: v.LastError, MessageID: v.MessageID,
		SentAt: encTime(v.SentAt), FinishedAt: encTime(v.FinishedAt),
		FirstOpenedAt: encTime(v.FirstOpenedAt), FirstClickedAt: encTime(v.FirstClickedAt),
		UnsubscribedAt: encTime(v.UnsubscribedAt),
	}
}

func decDelivery(d *deliveryDoc) *store.Delivery {
	return &store.Delivery{
		ID: d.ID, TenantID: d.TenantID, CampaignID: d.CampaignID,
		VersionID: d.VersionID, SenderID: d.SenderID,
		Lane: store.Lane(enum8(d.Lane)), Priority: int(d.Priority),
		Status: store.DeliveryStatus(enum8(d.Status)),
		Email:  d.Email, EmailNorm: d.EmailNorm, Name: d.Name, Locale: d.Locale,
		Vars: d.Vars, UnsubscribeURL: d.UnsubscribeURL, Headers: d.Headers,
		AttemptCount: int(d.AttemptCount), RetryGen: int(d.RetryGen),
		NextAttemptAt: decTime(d.NextAttemptAt),
		LeaseOwner:    d.LeaseOwner, LeaseUntil: decTime(d.LeaseUntil),
		LastErrorClass: store.ErrorClass(enum8(d.LastErrorClass)), LastSMTPCode: int(d.LastSMTPCode),
		LastError: d.LastError, MessageID: d.MessageID,
		SentAt: decTime(d.SentAt), FinishedAt: decTime(d.FinishedAt),
		FirstOpenedAt: decTime(d.FirstOpenedAt), FirstClickedAt: decTime(d.FirstClickedAt),
		UnsubscribedAt: decTime(d.UnsubscribedAt),
		CreatedAt:      decTime(&d.CreatedAt), UpdatedAt: decTime(d.UpdatedAt),
	}
}

// --- delivery attempt --------------------------------------------------

type attemptDoc struct {
	Base         `bson:",inline"`
	DeliveryID   string     `bson:"delivery_id"`
	AttemptNo    int32      `bson:"attempt_no"`
	RetryGen     int32      `bson:"retry_gen"`
	TransportID  string     `bson:"transport_id"`
	StartedAt    *time.Time `bson:"started_at"`
	FinishedAt   *time.Time `bson:"finished_at"`
	SMTPCode     int32      `bson:"smtp_code"`
	EnhancedCode string     `bson:"enhanced_code"`
	ErrorClass   int32      `bson:"error_class"`
	Error        string     `bson:"error"`
}

func encAttempt(v *store.DeliveryAttempt) *attemptDoc {
	return &attemptDoc{
		Base:       Base{ID: v.ID, TenantID: v.TenantID, CreatedAt: ts(v.CreatedAt)},
		DeliveryID: v.DeliveryID, AttemptNo: i32(v.AttemptNo), RetryGen: i32(v.RetryGen),
		TransportID: v.TransportID,
		StartedAt:   encTime(v.StartedAt), FinishedAt: encTime(v.FinishedAt),
		SMTPCode: i32(v.SMTPCode), EnhancedCode: v.EnhancedCode,
		ErrorClass: int32(v.ErrorClass), Error: v.Error,
	}
}

func decAttempt(d *attemptDoc) *store.DeliveryAttempt {
	return &store.DeliveryAttempt{
		ID: d.ID, TenantID: d.TenantID, DeliveryID: d.DeliveryID,
		AttemptNo: int(d.AttemptNo), RetryGen: int(d.RetryGen), TransportID: d.TransportID,
		StartedAt: decTime(d.StartedAt), FinishedAt: decTime(d.FinishedAt),
		SMTPCode: int(d.SMTPCode), EnhancedCode: d.EnhancedCode,
		ErrorClass: store.ErrorClass(enum8(d.ErrorClass)), Error: d.Error,
		CreatedAt: decTime(&d.CreatedAt),
	}
}
