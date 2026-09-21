package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sendplane/sendplane/store"
)

// The specs below are the whole mapping between a model and its table: the
// data columns, the values that go into them, and the scan that reads them
// back. Everything else (create, get, update with optimistic concurrency,
// delete, keyset pagination) is in crud.go.

// --- transport ---------------------------------------------------------

var transportSpec = spec[store.Transport]{
	table: "transport",
	cols: []string{
		"name", "host", "port", "tls", "username", "password", "max_conns",
		"rate_per_second", "domain_rate_per_second", "status", "status_reason",
		"status_changed_at", "status_until",
	},
	args: func(v *store.Transport) ([]any, error) {
		rates, err := jsonIn(v.DomainRatePerSecond)
		if err != nil {
			return nil, err
		}
		return []any{
			v.Name, v.Host, v.Port, string(v.TLS), v.Username, v.Password,
			v.MaxConns, v.RatePerSecond, rates, i16(v.Status), v.StatusReason,
			tsIn(v.StatusChangedAt), tsIn(v.StatusUntil),
		}, nil
	},
	scan: func(r rowScanner) (*store.Transport, error) {
		var v store.Transport
		var tls string
		var rates []byte
		var status int16
		var changed, until *time.Time
		if err := r.Scan(&v.ID, &v.TenantID, &v.Name, &v.Host, &v.Port, &tls,
			&v.Username, &v.Password, &v.MaxConns, &v.RatePerSecond, &rates,
			&status, &v.StatusReason, &changed, &until,
			&v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, err
		}
		v.TLS = store.TLSMode(tls)
		v.Status = enumOut[store.TransportStatus](status)
		v.StatusChangedAt = tsOut(changed)
		v.StatusUntil = tsOut(until)
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		return &v, jsonOut(rates, &v.DomainRatePerSecond)
	},
	id:       func(v *store.Transport) *string { return &v.ID },
	tenantID: func(v *store.Transport) *string { return &v.TenantID },
	version:  func(v *store.Transport) *int64 { return &v.Version },
	created:  func(v *store.Transport) *time.Time { return &v.CreatedAt },
	updated:  func(v *store.Transport) *time.Time { return &v.UpdatedAt },
}

// --- sender ------------------------------------------------------------

var senderSpec = spec[store.Sender]{
	table: "sender",
	cols: []string{
		"name", "from_name", "from_email", "reply_to", "transport_id",
		"domain_id", "health", "health_reason", "health_checked_at",
	},
	args: func(v *store.Sender) ([]any, error) {
		return []any{
			v.Name, v.FromName, v.FromEmail, v.ReplyTo, v.TransportID,
			v.DomainID, i16(v.Health), v.HealthReason, tsIn(v.HealthCheckedAt),
		}, nil
	},
	scan: func(r rowScanner) (*store.Sender, error) {
		var v store.Sender
		var health int16
		var checked *time.Time
		if err := r.Scan(&v.ID, &v.TenantID, &v.Name, &v.FromName, &v.FromEmail,
			&v.ReplyTo, &v.TransportID, &v.DomainID, &health, &v.HealthReason,
			&checked, &v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, err
		}
		v.Health = enumOut[store.HealthStatus](health)
		v.HealthCheckedAt = tsOut(checked)
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		return &v, nil
	},
	id:       func(v *store.Sender) *string { return &v.ID },
	tenantID: func(v *store.Sender) *string { return &v.TenantID },
	version:  func(v *store.Sender) *int64 { return &v.Version },
	created:  func(v *store.Sender) *time.Time { return &v.CreatedAt },
	updated:  func(v *store.Sender) *time.Time { return &v.UpdatedAt },
}

// --- sending domain ----------------------------------------------------

var domainSpec = spec[store.SendingDomain]{
	table: "sending_domain",
	cols: []string{
		"domain", "dkim_selector", "dkim_private_key", "return_path_domain",
		"expected_spf", "outbound_ips", "health", "health_reason",
		"health_checked_at",
	},
	args: func(v *store.SendingDomain) ([]any, error) {
		return []any{
			v.Domain, v.DKIMSelector, v.DKIMPrivateKey, v.ReturnPathDomain,
			v.ExpectedSPF, v.OutboundIPs, i16(v.Health), v.HealthReason,
			tsIn(v.HealthCheckedAt),
		}, nil
	},
	scan: func(r rowScanner) (*store.SendingDomain, error) {
		var v store.SendingDomain
		var health int16
		var checked *time.Time
		if err := r.Scan(&v.ID, &v.TenantID, &v.Domain, &v.DKIMSelector,
			&v.DKIMPrivateKey, &v.ReturnPathDomain, &v.ExpectedSPF,
			&v.OutboundIPs, &health, &v.HealthReason, &checked,
			&v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, err
		}
		v.Health = enumOut[store.HealthStatus](health)
		v.HealthCheckedAt = tsOut(checked)
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		return &v, nil
	},
	id:       func(v *store.SendingDomain) *string { return &v.ID },
	tenantID: func(v *store.SendingDomain) *string { return &v.TenantID },
	version:  func(v *store.SendingDomain) *int64 { return &v.Version },
	created:  func(v *store.SendingDomain) *time.Time { return &v.CreatedAt },
	updated:  func(v *store.SendingDomain) *time.Time { return &v.UpdatedAt },
}

// --- mailbox health ----------------------------------------------------

// healthCols is the six-column MailboxHealth block probe_mailbox and
// bounce_mailbox share. The two specs scan into its fields by name, because a
// Scan argument list cannot be spliced.
type healthCols struct {
	status              int16
	stage, reason       string
	checkedAt, lastOKAt *time.Time
	failures            int
}

func (h healthCols) health() store.MailboxHealth {
	return store.MailboxHealth{
		Status:              enumOut[store.MailboxStatus](h.status),
		Stage:               h.stage,
		Reason:              h.reason,
		CheckedAt:           tsOut(h.checkedAt),
		LastOKAt:            tsOut(h.lastOKAt),
		ConsecutiveFailures: h.failures,
	}
}

// healthColumns is the column list, in the order healthArgs produces values.
var healthColumns = []string{
	"health_status", "health_stage", "health_reason",
	"health_checked_at", "health_last_ok_at", "health_failures",
}

func healthArgs(h store.MailboxHealth) []any {
	return []any{
		i16(h.Status), h.Stage, h.Reason,
		tsIn(h.CheckedAt), tsIn(h.LastOKAt), h.ConsecutiveFailures,
	}
}

// updateHealth is the shared UpdateHealth statement. It writes the health
// block alone: no version check, no version bump and no updated_at, so a
// background check never fights an operator's edit (store.MailboxHealth).
func updateHealth(ctx context.Context, p *Provider, table, tenant, id string, h store.MailboxHealth) error {
	if err := p.check(); err != nil {
		return err
	}
	a := &args{}
	vals := healthArgs(h)
	sets := make([]string, 0, len(healthColumns))
	for i, col := range healthColumns {
		sets = append(sets, col+" = "+a.add(vals[i]))
	}
	q := fmt.Sprintf("UPDATE %s SET %s WHERE id = %s AND tenant_id = %s",
		table, strings.Join(sets, ", "), a.add(id), a.add(tenant))
	tag, err := p.pool.Exec(ctx, q, a.v...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s %s", store.ErrNotFound, table, id)
	}
	return nil
}

// --- bounce mailbox ----------------------------------------------------

var bounceMailboxSpec = spec[store.BounceMailbox]{
	table: "bounce_mailbox",
	cols: append([]string{
		"name", "address", "protocol", "host", "port", "tls", "username",
		"password", "folder", "after_process", "enabled",
	}, healthColumns...),
	args: func(v *store.BounceMailbox) ([]any, error) {
		return append([]any{
			v.Name, v.Address, v.Protocol, v.Host, v.Port, string(v.TLS),
			v.Username, v.Password, v.Folder, v.AfterProcess, v.Enabled,
		}, healthArgs(v.Health)...), nil
	},
	scan: func(r rowScanner) (*store.BounceMailbox, error) {
		var v store.BounceMailbox
		var tls string
		var h healthCols
		if err := r.Scan(&v.ID, &v.TenantID, &v.Name, &v.Address, &v.Protocol,
			&v.Host, &v.Port, &tls, &v.Username, &v.Password, &v.Folder,
			&v.AfterProcess, &v.Enabled,
			&h.status, &h.stage, &h.reason, &h.checkedAt, &h.lastOKAt, &h.failures,
			&v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, err
		}
		v.TLS = store.TLSMode(tls)
		v.Health = h.health()
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		return &v, nil
	},
	id:       func(v *store.BounceMailbox) *string { return &v.ID },
	tenantID: func(v *store.BounceMailbox) *string { return &v.TenantID },
	version:  func(v *store.BounceMailbox) *int64 { return &v.Version },
	created:  func(v *store.BounceMailbox) *time.Time { return &v.CreatedAt },
	updated:  func(v *store.BounceMailbox) *time.Time { return &v.UpdatedAt },
}

type bounceMailboxRepo struct{ *crud[store.BounceMailbox] }

// ListEnabled returns the whole enabled set in one call; the partial index
// bounce_mailbox_enabled covers it.
func (r *bounceMailboxRepo) ListEnabled(ctx context.Context) ([]store.BounceMailbox, error) {
	if err := r.p.check(); err != nil {
		return nil, err
	}
	a := &args{}
	q := "SELECT " + r.s.selectList() + " FROM bounce_mailbox WHERE tenant_id = " +
		a.add(r.tenant) + " AND enabled ORDER BY created_at, id"
	rows, err := r.p.pool.Query(ctx, q, a.v...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.BounceMailbox
	for rows.Next() {
		v, err := r.s.scan(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, *v)
	}
	return out, mapErr(rows.Err())
}

func (r *bounceMailboxRepo) UpdateHealth(ctx context.Context, id string, h store.MailboxHealth) error {
	return updateHealth(ctx, r.p, "bounce_mailbox", r.tenant, id, h)
}

// --- probe mailbox -----------------------------------------------------

var mailboxSpec = spec[store.ProbeMailbox]{
	table: "probe_mailbox",
	cols: append([]string{
		"name", "address", "host", "port", "tls", "username", "password",
		"inbox_folder", "spam_folder", "authserv_id", "enabled",
	}, healthColumns...),
	args: func(v *store.ProbeMailbox) ([]any, error) {
		return append([]any{
			v.Name, v.Address, v.Host, v.Port, string(v.TLS), v.Username,
			v.Password, v.InboxFolder, v.SpamFolder, v.AuthServID, v.Enabled,
		}, healthArgs(v.Health)...), nil
	},
	scan: func(r rowScanner) (*store.ProbeMailbox, error) {
		var v store.ProbeMailbox
		var tls string
		var h healthCols
		if err := r.Scan(&v.ID, &v.TenantID, &v.Name, &v.Address, &v.Host,
			&v.Port, &tls, &v.Username, &v.Password, &v.InboxFolder,
			&v.SpamFolder, &v.AuthServID, &v.Enabled,
			&h.status, &h.stage, &h.reason, &h.checkedAt, &h.lastOKAt, &h.failures,
			&v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, err
		}
		v.TLS = store.TLSMode(tls)
		v.Health = h.health()
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		return &v, nil
	},
	id:       func(v *store.ProbeMailbox) *string { return &v.ID },
	tenantID: func(v *store.ProbeMailbox) *string { return &v.TenantID },
	version:  func(v *store.ProbeMailbox) *int64 { return &v.Version },
	created:  func(v *store.ProbeMailbox) *time.Time { return &v.CreatedAt },
	updated:  func(v *store.ProbeMailbox) *time.Time { return &v.UpdatedAt },
}

type probeMailboxRepo struct{ *crud[store.ProbeMailbox] }

func (r *probeMailboxRepo) UpdateHealth(ctx context.Context, id string, h store.MailboxHealth) error {
	return updateHealth(ctx, r.p, "probe_mailbox", r.tenant, id, h)
}

// --- probe run (immutable) ---------------------------------------------

var probeRunSpec = spec[store.ProbeRun]{
	table: "probe_run",
	cols: []string{
		"sender_id", "mailbox_id", "delivery_id", "group_id", "pending",
		"status", "reason",
		"delivered", "folder", "latency", "spf", "dkim", "dmarc",
		"dkim_domain", "dkim_selector", "dmarc_policy", "tls", "observed_ip",
		"ptr", "ptr_match", "dns", "raw_headers", "started_at", "received_at",
	},
	args: func(v *store.ProbeRun) ([]any, error) {
		dns, err := jsonIn(v.DNS)
		if err != nil {
			return nil, err
		}
		return []any{
			v.SenderID, v.MailboxID, v.DeliveryID, v.GroupID, v.Pending,
			i16(v.Status), v.Reason,
			v.Delivered, v.Folder, int64(v.Latency), v.SPF, v.DKIM, v.DMARC,
			v.DKIMDomain, v.DKIMSelector, v.DMARCPolicy, v.TLS, v.ObservedIP,
			v.PTR, v.PTRMatch, dns, v.RawHeaders,
			tsIn(v.StartedAt), tsIn(v.ReceivedAt),
		}, nil
	},
	scan: func(r rowScanner) (*store.ProbeRun, error) {
		var v store.ProbeRun
		var status int16
		var latency int64
		var dns []byte
		var started, received *time.Time
		if err := r.Scan(&v.ID, &v.TenantID, &v.SenderID, &v.MailboxID,
			&v.DeliveryID, &v.GroupID, &v.Pending,
			&status, &v.Reason, &v.Delivered, &v.Folder,
			&latency, &v.SPF, &v.DKIM, &v.DMARC, &v.DKIMDomain,
			&v.DKIMSelector, &v.DMARCPolicy, &v.TLS, &v.ObservedIP, &v.PTR,
			&v.PTRMatch, &dns, &v.RawHeaders, &started, &received,
			&v.CreatedAt); err != nil {
			return nil, err
		}
		v.Status = enumOut[store.HealthStatus](status)
		v.Latency = time.Duration(latency)
		v.DNS = rawOut(dns)
		v.StartedAt, v.ReceivedAt = tsOut(started), tsOut(received)
		v.CreatedAt = v.CreatedAt.UTC()
		return &v, nil
	},
	id:       func(v *store.ProbeRun) *string { return &v.ID },
	tenantID: func(v *store.ProbeRun) *string { return &v.TenantID },
	created:  func(v *store.ProbeRun) *time.Time { return &v.CreatedAt },
	// A probe run is written pending and rewritten once when it finishes. It
	// has no version column: only the control leader's collector updates it,
	// so there is nothing to race with (store.ProbeRunRepo).
	mutableWithoutVersion: true,
}

type probeRunRepo struct{ *crud[store.ProbeRun] }

func (r *probeRunRepo) ListBySender(ctx context.Context, senderID string, p store.Page) (store.Result[store.ProbeRun], error) {
	return r.listWhere(ctx, p, func(a *args) string {
		return " AND sender_id = " + a.add(senderID)
	})
}

func (r *probeRunRepo) ListPending(ctx context.Context, p store.Page) (store.Result[store.ProbeRun], error) {
	return r.listWhere(ctx, p, func(*args) string { return " AND pending" })
}

// --- layout ------------------------------------------------------------

var layoutSpec = spec[store.Layout]{
	table: "layout",
	cols:  []string{"name", "mode", "body", "i18n"},
	args: func(v *store.Layout) ([]any, error) {
		i18n, err := jsonIn(v.I18n)
		if err != nil {
			return nil, err
		}
		return []any{v.Name, string(v.Mode), v.Body, i18n}, nil
	},
	scan: func(r rowScanner) (*store.Layout, error) {
		var v store.Layout
		var mode string
		var i18n []byte
		if err := r.Scan(&v.ID, &v.TenantID, &v.Name, &mode, &v.Body, &i18n,
			&v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, err
		}
		v.Mode = store.ContentMode(mode)
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		return &v, jsonOut(i18n, &v.I18n)
	},
	id:       func(v *store.Layout) *string { return &v.ID },
	tenantID: func(v *store.Layout) *string { return &v.TenantID },
	version:  func(v *store.Layout) *int64 { return &v.Version },
	created:  func(v *store.Layout) *time.Time { return &v.CreatedAt },
	updated:  func(v *store.Layout) *time.Time { return &v.UpdatedAt },
}

// --- template ----------------------------------------------------------

var templateSpec = spec[store.Template]{
	table: "template",
	cols: []string{
		"name", "layout_id", "subject", "preheader", "mode", "body", "blocks",
		"text_body", "i18n", "default_locale", "published_version_id",
	},
	args: func(v *store.Template) ([]any, error) {
		blocks, err := jsonIn(v.Blocks)
		if err != nil {
			return nil, err
		}
		i18n, err := jsonIn(v.I18n)
		if err != nil {
			return nil, err
		}
		return []any{
			v.Name, v.LayoutID, v.Subject, v.Preheader, string(v.Mode), v.Body,
			blocks, v.Text, i18n, v.DefaultLocale, v.PublishedVersionID,
		}, nil
	},
	scan: func(r rowScanner) (*store.Template, error) {
		var v store.Template
		var mode string
		var blocks, i18n []byte
		if err := r.Scan(&v.ID, &v.TenantID, &v.Name, &v.LayoutID, &v.Subject,
			&v.Preheader, &mode, &v.Body, &blocks, &v.Text, &i18n,
			&v.DefaultLocale, &v.PublishedVersionID,
			&v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, err
		}
		v.Mode = store.ContentMode(mode)
		v.Blocks = rawOut(blocks)
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		return &v, jsonOut(i18n, &v.I18n)
	},
	id:       func(v *store.Template) *string { return &v.ID },
	tenantID: func(v *store.Template) *string { return &v.TenantID },
	version:  func(v *store.Template) *int64 { return &v.Version },
	created:  func(v *store.Template) *time.Time { return &v.CreatedAt },
	updated:  func(v *store.Template) *time.Time { return &v.UpdatedAt },
}

// --- message version (immutable) ---------------------------------------

var messageVersionSpec = spec[store.MessageVersion]{
	table: "message_version",
	cols: []string{
		"template_id", "layout_id", "subject_tpl", "html_tpl", "text_tpl",
		"i18n", "default_locale", "links", "checksum",
	},
	args: func(v *store.MessageVersion) ([]any, error) {
		i18n, err := jsonIn(v.I18n)
		if err != nil {
			return nil, err
		}
		return []any{
			v.TemplateID, v.LayoutID, v.SubjectTpl, v.HTMLTpl, v.TextTpl,
			i18n, v.DefaultLocale, v.Links, v.Checksum,
		}, nil
	},
	scan: func(r rowScanner) (*store.MessageVersion, error) {
		var v store.MessageVersion
		var i18n []byte
		if err := r.Scan(&v.ID, &v.TenantID, &v.TemplateID, &v.LayoutID,
			&v.SubjectTpl, &v.HTMLTpl, &v.TextTpl, &i18n, &v.DefaultLocale,
			&v.Links, &v.Checksum, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.CreatedAt = v.CreatedAt.UTC()
		return &v, jsonOut(i18n, &v.I18n)
	},
	id:       func(v *store.MessageVersion) *string { return &v.ID },
	tenantID: func(v *store.MessageVersion) *string { return &v.TenantID },
	created:  func(v *store.MessageVersion) *time.Time { return &v.CreatedAt },
}

type versionRepo struct{ *crud[store.MessageVersion] }

func (r *versionRepo) ListByTemplate(ctx context.Context, templateID string, p store.Page) (store.Result[store.MessageVersion], error) {
	return r.listWhere(ctx, p, func(a *args) string {
		return " AND template_id = " + a.add(templateID)
	})
}

// --- campaign ----------------------------------------------------------

var campaignSpec = spec[store.Campaign]{
	table: "campaign",
	cols: []string{
		"name", "template_id", "version_id", "sender_id", "default_locale",
		"vars", "status", "schedule_at", "started_at", "completed_at", "stats",
	},
	args: func(v *store.Campaign) ([]any, error) {
		vars, err := jsonIn(v.Vars)
		if err != nil {
			return nil, err
		}
		stats, err := jsonIn(v.Stats)
		if err != nil {
			return nil, err
		}
		return []any{
			v.Name, v.TemplateID, v.VersionID, v.SenderID, v.DefaultLocale, vars,
			i16(v.Status), tsIn(v.ScheduleAt), tsIn(v.StartedAt),
			tsIn(v.CompletedAt), stats,
		}, nil
	},
	scan: func(r rowScanner) (*store.Campaign, error) {
		var v store.Campaign
		var status int16
		var vars, stats []byte
		var schedule, started, completed *time.Time
		if err := r.Scan(&v.ID, &v.TenantID, &v.Name, &v.TemplateID, &v.VersionID,
			&v.SenderID,
			&v.DefaultLocale, &vars, &status, &schedule, &started, &completed,
			&stats, &v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, err
		}
		v.Status = enumOut[store.CampaignStatus](status)
		v.ScheduleAt, v.StartedAt = tsOut(schedule), tsOut(started)
		v.CompletedAt = tsOut(completed)
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		if err := jsonOut(vars, &v.Vars); err != nil {
			return nil, err
		}
		return &v, jsonOut(stats, &v.Stats)
	},
	id:       func(v *store.Campaign) *string { return &v.ID },
	tenantID: func(v *store.Campaign) *string { return &v.TenantID },
	version:  func(v *store.Campaign) *int64 { return &v.Version },
	created:  func(v *store.Campaign) *time.Time { return &v.CreatedAt },
	updated:  func(v *store.Campaign) *time.Time { return &v.UpdatedAt },
}

type campaignRepo struct{ *crud[store.Campaign] }

func (r *campaignRepo) ListByStatus(ctx context.Context, statuses []store.CampaignStatus, p store.Page) (store.Result[store.Campaign], error) {
	return r.listWhere(ctx, p, func(a *args) string {
		return " AND status = ANY(" + a.add(i16s(statuses)) + "::smallint[])"
	})
}

// UpdateStats refreshes the cached aggregate without taking part in
// optimistic concurrency: the control loop must not fight API edits.
func (r *campaignRepo) UpdateStats(ctx context.Context, campaignID string, s store.CampaignStats) error {
	if err := r.p.check(); err != nil {
		return err
	}
	stats, err := jsonIn(s)
	if err != nil {
		return err
	}
	const q = `UPDATE campaign SET stats = $3, updated_at = $4
	            WHERE id = $1 AND tenant_id = $2`
	tag, err := r.p.pool.Exec(ctx, q, campaignID, r.tenant, stats, r.p.now())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: campaign %s", store.ErrNotFound, campaignID)
	}
	return nil
}

// --- bounce event (immutable) ------------------------------------------

var bounceSpec = spec[store.BounceEvent]{
	table: "bounce_event",
	cols: []string{
		"delivery_id", "type", "source", "verified", "recipient", "email_norm",
		"smtp_status", "diagnostic_code", "message_id", "raw", "received_at",
	},
	args: func(v *store.BounceEvent) ([]any, error) {
		raw, err := jsonIn(v.Raw)
		if err != nil {
			return nil, err
		}
		return []any{
			v.DeliveryID, i16(v.Type), string(v.Source), v.Verified,
			v.Recipient, v.EmailNorm, v.SMTPStatus, v.DiagnosticCode,
			v.MessageID, raw, tsIn(v.ReceivedAt),
		}, nil
	},
	scan: func(r rowScanner) (*store.BounceEvent, error) {
		var v store.BounceEvent
		var typ int16
		var source string
		var raw []byte
		var received *time.Time
		if err := r.Scan(&v.ID, &v.TenantID, &v.DeliveryID, &typ, &source,
			&v.Verified, &v.Recipient, &v.EmailNorm, &v.SMTPStatus,
			&v.DiagnosticCode, &v.MessageID, &raw, &received,
			&v.CreatedAt); err != nil {
			return nil, err
		}
		v.Type = enumOut[store.BounceType](typ)
		v.Source = store.BounceSource(source)
		v.Raw = rawOut(raw)
		v.ReceivedAt = tsOut(received)
		v.CreatedAt = v.CreatedAt.UTC()
		return &v, nil
	},
	id:       func(v *store.BounceEvent) *string { return &v.ID },
	tenantID: func(v *store.BounceEvent) *string { return &v.TenantID },
	created:  func(v *store.BounceEvent) *time.Time { return &v.CreatedAt },
}

type bounceRepo struct{ *crud[store.BounceEvent] }

func (r *bounceRepo) ListByDelivery(ctx context.Context, deliveryID string, p store.Page) (store.Result[store.BounceEvent], error) {
	return r.listWhere(ctx, p, func(a *args) string {
		return " AND delivery_id = " + a.add(deliveryID)
	})
}

func (r *bounceRepo) DeleteBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	return deleteBefore(ctx, r.p, "bounce_event", r.tenant, before, limit, "")
}
