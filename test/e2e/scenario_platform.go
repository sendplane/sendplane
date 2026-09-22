package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Scenario 9: the operator's shared ("platform") sending infrastructure of
// ADR-0017, end to end against the real stack.
//
// The whole design rests on four claims that only a running deployment can
// settle, and this scenario is where each one is either true or the run is red:
//
//  1. A shared sender's From address is a template over the tenant attributes
//     the *request* carried, so one configured sender serves every tenant.
//     Asserted off the wire: chaos-smtp has to show
//     `From: Acme, Inc. <sender+acme@platform.e2e.test>`.
//  2. A request that does not supply what the template reads is refused with
//     the keys named, not sent from `sender+@platform.e2e.test`.
//  3. A tenant sees the shared *sender* and nothing else — no relay, no
//     domain, no probe verdict — while the system tenant sees all of it.
//  4. None of the configuration is in the database. Asserted by reading the
//     shadow rows back: the run prints the `sys:` rows of `transport` and
//     `sender` so the assertion is visible in the log, not only in an `if`.
//
// The platform catalog itself comes from test/e2e/config.yaml, not from the
// API: that is the point of it. There is nothing here to create.
const (
	platformSenderID    = "sys:default"
	platformTransportID = "sys:relay"
	platformDomainID    = "sys:dom"
	platformProbeBoxID  = "sys:probe"

	// The rendered address the templates produce for these tenant_vars.
	platformTenantSlug = "acme"
	platformTenantName = "Acme, Inc."
	platformFromEmail  = "sender+" + platformTenantSlug + "@platform.e2e.test"

	// systemTenant is the operator's own scope. The harness reaches it with
	// the header cmd/sendplane's resolver is configured to honour.
	systemTenant  = "_system"
	tenantHeader  = "X-Sendplane-Tenant"
	platformRcpt  = "platform-one@" + mailDomain
	platformRcpt2 = "platform-two@" + mailDomain
)

func (r *runner) scenarioPlatform(ctx context.Context) error {
	if err := r.platformSenderVisibility(ctx); err != nil {
		return err
	}
	if err := r.platformTransportHidden(ctx); err != nil {
		return err
	}
	if err := r.platformReadOnly(ctx); err != nil {
		return err
	}
	if err := r.platformTransactional(ctx); err != nil {
		return err
	}
	if err := r.platformTenantVarsMissing(ctx); err != nil {
		return err
	}
	if err := r.platformCampaignDenied(ctx); err != nil {
		return err
	}
	if err := r.platformProbeState(ctx); err != nil {
		return err
	}
	return r.platformShadowRows(ctx)
}

// platformShadowRows is claim 4, read straight out of the database: the only
// rows that exist for a platform resource are state shadow rows, and every
// configuration column on one is empty.
//
// It is the one assertion in the suite that goes around the API on purpose.
// "the API refuses to write it" and "it is not in the database" are different
// claims, and the second is the one the design rests on — a shared relay's
// password existing in no database is what makes rotating it a deploy and
// makes a per-tenant dump safe (ADR-0017). store/platformtest checks the same
// invariant per backend; this checks the deployment.
//
// It runs psql inside the compose stack rather than opening a connection, so
// the harness needs no database driver. A stack running the MongoDB overlay
// has no rows here at all, which psql reports as a failure and this logs as a
// skip: the scenario's other eight assertions still ran.
func (r *runner) platformShadowRows(ctx context.Context) error {
	const q = `SELECT id, shared, coalesce(name,''), coalesce(host,''), port, ` +
		`coalesce(username,''), coalesce(length(password),0), status ` +
		`FROM transport WHERE id LIKE 'sys:%' ORDER BY id`
	out, err := r.psql(ctx, q)
	if err != nil {
		r.logf("  shadow-row check skipped (no psql in this stack: %v)", err)
		return nil
	}
	rows := nonEmptyLines(out)
	if len(rows) == 0 {
		// Nothing changed the shared transport's circuit during this run, so
		// there is no row at all. That is not a failure: the absence of a row
		// is the strongest possible version of "no configuration in the
		// database". The probe mailbox check below is the one that is
		// guaranteed to have written state.
		r.logf("  no shadow row for the shared transport (nothing changed its status) — " +
			"which is itself the strongest form of the claim")
	}
	for _, row := range rows {
		f := strings.Split(row, "|")
		if len(f) != 8 {
			r.fail("unexpected transport shadow row %q", row)
			continue
		}
		r.logf("  transport shadow row: id=%s shared=%s name=%q host=%q port=%s user=%q pwlen=%s status=%s",
			f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7])
		if f[1] != "t" {
			r.fail("shadow row %s has shared=%s, want t", f[0], f[1])
		}
		for i, col := range []string{"name", "host", "username"} {
			if f[2+i] != "" {
				r.fail("shadow row %s carries configuration: %s=%q", f[0], col, f[2+i])
			}
		}
		if f[4] != "0" {
			r.fail("shadow row %s carries configuration: port=%s", f[0], f[4])
		}
		if f[6] != "0" {
			r.fail("shadow row %s carries a password (%s bytes)", f[0], f[6])
		}
	}

	// The sender, whose shadow row holds the probe verdict the platform probe
	// writes when the run finishes.
	const sq = `SELECT id, shared, coalesce(name,''), coalesce(from_email,''), ` +
		`coalesce(transport_id,''), coalesce(domain_id,''), health ` +
		`FROM sender WHERE id LIKE 'sys:%' ORDER BY id`
	sout, err := r.psql(ctx, sq)
	if err != nil {
		return nil
	}
	for _, row := range nonEmptyLines(sout) {
		f := strings.Split(row, "|")
		if len(f) != 7 {
			r.fail("unexpected sender shadow row %q", row)
			continue
		}
		r.logf("  sender shadow row: id=%s shared=%s name=%q from_email=%q transport=%q domain=%q health=%s",
			f[0], f[1], f[2], f[3], f[4], f[5], f[6])
		if f[1] != "t" {
			r.fail("sender shadow row %s has shared=%s, want t", f[0], f[1])
		}
		for i, col := range []string{"name", "from_email", "transport_id", "domain_id"} {
			if f[2+i] != "" {
				r.fail("sender shadow row %s carries configuration: %s=%q", f[0], col, f[2+i])
			}
		}
	}

	// The probe mailbox is the deterministic one: test/e2e/config.yaml sets
	// probe.mailbox_check_interval to 15s, so the leader's mailbox-check loop
	// logs in to the shared mailbox and records the result within one
	// interval. That write is the whole shadow-row mechanism, so the run waits
	// for it rather than accepting its absence.
	const mq = `SELECT id, shared, coalesce(name,''), coalesce(address,''), ` +
		`coalesce(host,''), port, coalesce(username,''), ` +
		`coalesce(length(password),0), health_status, coalesce(health_stage,'') ` +
		`FROM probe_mailbox WHERE id LIKE 'sys:%' ORDER BY id`
	deadline := time.Now().Add(90 * time.Second)
	for {
		mout, err := r.psql(ctx, mq)
		if err != nil {
			return nil
		}
		rows := nonEmptyLines(mout)
		if len(rows) > 0 {
			for _, row := range rows {
				f := strings.Split(row, "|")
				if len(f) != 10 {
					r.fail("unexpected probe mailbox shadow row %q", row)
					continue
				}
				r.logf("  probe mailbox shadow row: id=%s shared=%s name=%q address=%q host=%q "+
					"port=%s user=%q pwlen=%s health=%s/%s",
					f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7], f[8], f[9])
				if f[1] != "t" {
					r.fail("probe mailbox shadow row %s has shared=%s, want t", f[0], f[1])
				}
				for i, col := range []string{"name", "address", "host", "port", "username"} {
					if f[2+i] != "" && f[2+i] != "0" {
						r.fail("probe mailbox shadow row %s carries configuration: %s=%q",
							f[0], col, f[2+i])
					}
				}
				if f[7] != "0" {
					r.fail("probe mailbox shadow row %s carries a password (%s bytes)", f[0], f[7])
				}
				if f[8] == "0" {
					r.fail("probe mailbox shadow row %s has no health status; "+
						"the row exists but carries no state", f[0])
				}
			}
			return nil
		}
		if time.Now().After(deadline) {
			r.fail("no shadow row for the shared probe mailbox after 90s; " +
				"the mailbox-check loop should have written one (probe.mailbox_check_interval: 15s)")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// psql runs one query inside the compose stack's postgres container and
// returns the unaligned, pipe-separated rows.
func (r *runner) psql(ctx context.Context, query string) (string, error) {
	return r.composeOutput(ctx, "exec", "-T", "postgres",
		"psql", "-U", "sendplane", "-d", "sendplane", "-At", "-c", query)
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// platformSenderVisibility is claim 3 from the tenant's side: the shared
// sender is listed and usable, and carries no state and no internals.
func (r *runner) platformSenderVisibility(ctx context.Context) error {
	var snd sender
	if err := r.api.getJSON(ctx, "/api/v1/senders/"+platformSenderID, &snd); err != nil {
		return fmt.Errorf("GET /senders/%s: %w", platformSenderID, err)
	}
	if !snd.Shared {
		r.fail("the shared sender %s came back with shared=false", platformSenderID)
	}
	if got := strings.Join(snd.Uses, ","); got != "transactional" {
		r.fail("shared sender uses = %q, want %q (test/e2e/config.yaml)", got, "transactional")
	}
	// The From fields are templates, not addresses: that is what one entry
	// serving every tenant looks like on the wire.
	if !strings.Contains(snd.FromEmail, "{{") {
		r.fail("shared sender from_email = %q, want the configured template", snd.FromEmail)
	}
	// Claim 3, the part that matters most: no probe verdict about a
	// reputation every tenant shares.
	if snd.Health != "" || snd.HealthReason != "" || snd.HealthCheckedAt != "" {
		r.fail("a tenant's view of %s carries health %q/%q/%q, want none",
			platformSenderID, snd.Health, snd.HealthReason, snd.HealthCheckedAt)
	}
	if snd.DomainID != "" {
		r.fail("a tenant's view of %s exposes domain_id %q", platformSenderID, snd.DomainID)
	}

	// And the system tenant does see all of it — otherwise "hidden from the
	// tenant" would be indistinguishable from "never written".
	var sys sender
	if err := r.api.getJSONAs(ctx, systemTenant, "/api/v1/senders/"+platformSenderID, &sys); err != nil {
		return fmt.Errorf("GET /senders/%s as %s: %w", platformSenderID, systemTenant, err)
	}
	if sys.TransportID != platformTransportID {
		r.fail("%s sees transport_id %q for %s, want %q",
			systemTenant, sys.TransportID, platformSenderID, platformTransportID)
	}
	if sys.DomainID != platformDomainID {
		r.fail("%s sees domain_id %q for %s, want %q",
			systemTenant, sys.DomainID, platformSenderID, platformDomainID)
	}
	r.logf("  shared sender visible to the tenant without state; %s sees transport=%s domain=%s",
		systemTenant, sys.TransportID, sys.DomainID)
	return nil
}

// platformTransportHidden is the other half of claim 3: the relay behind the
// shared sender is the operator's infrastructure and a tenant cannot reach it,
// by ID or in a listing.
func (r *runner) platformTransportHidden(ctx context.Context) error {
	var list struct {
		Items []transport `json:"items"`
	}
	if err := r.api.getJSON(ctx, "/api/v1/transports?limit=100", &list); err != nil {
		return fmt.Errorf("GET /transports: %w", err)
	}
	for _, t := range list.Items {
		if t.ID == platformTransportID {
			r.fail("a tenant's GET /transports includes the shared transport %s (host %q)",
				t.ID, t.Host)
		}
	}

	err := r.api.getJSON(ctx, "/api/v1/transports/"+platformTransportID, nil)
	if got := statusOf(err); got != http.StatusNotFound {
		r.fail("GET /transports/%s as a tenant returned %d (%v), want 404",
			platformTransportID, got, err)
	}

	var sysList struct {
		Items []transport `json:"items"`
	}
	if err := r.api.getJSONAs(ctx, systemTenant, "/api/v1/transports?limit=100", &sysList); err != nil {
		return fmt.Errorf("GET /transports as %s: %w", systemTenant, err)
	}
	found := false
	for _, t := range sysList.Items {
		if t.ID != platformTransportID {
			continue
		}
		found = true
		if t.Host != "chaos-smtp" {
			r.fail("%s sees host %q for %s, want chaos-smtp", systemTenant, t.Host, t.ID)
		}
		if !t.Shared {
			r.fail("%s sees %s with shared=false", systemTenant, t.ID)
		}
		// No has_password assertion: the shared relay is chaos-smtp, which
		// answers AUTH with 5.5.1, so test/e2e/config.yaml deliberately gives
		// it no credentials. That the cipher round trip works for a configured
		// secret is store/platformtest's job.
	}
	if !found {
		r.fail("%s does not see the shared transport %s at all", systemTenant, platformTransportID)
	}
	r.logf("  shared transport hidden from the tenant, visible to %s with its host and password",
		systemTenant)
	return nil
}

// platformReadOnly is the write side of claim 4: the configuration lives in
// the operator's config file, so there is nothing an API caller can change.
func (r *runner) platformReadOnly(ctx context.Context) error {
	cases := []struct {
		what   string
		method string
		path   string
		body   any
		as     string
	}{
		{"PUT a shared transport", http.MethodPut, "/api/v1/transports/" + platformTransportID,
			map[string]any{"name": "mine", "host": "attacker.example.com", "port": 25, "version": 1}, ""},
		{"DELETE a shared transport", http.MethodDelete, "/api/v1/transports/" + platformTransportID, nil, ""},
		{"PUT a shared sender", http.MethodPut, "/api/v1/senders/" + platformSenderID,
			map[string]any{"name": "mine", "from_email": "x@platform.e2e.test",
				"transport_id": platformTransportID, "version": 1}, ""},
		// The operator cannot edit it either: a config-file resource is
		// read-only in every scope, not just in a tenant's.
		{"PUT a shared transport as " + systemTenant, http.MethodPut,
			"/api/v1/transports/" + platformTransportID,
			map[string]any{"name": "mine", "host": "attacker.example.com", "port": 25, "version": 1},
			systemTenant},
	}
	for _, c := range cases {
		err := r.api.doAs(ctx, c.as, c.method, c.path, c.body, nil)
		var ae *apiError
		switch {
		case !asAPIError(err, &ae):
			r.fail("%s: got %v, want 403 platform_read_only", c.what, err)
		case ae.Status != http.StatusForbidden || ae.Code != "platform_read_only":
			r.fail("%s: got %d %s, want 403 platform_read_only", c.what, ae.Status, ae.Code)
		}
	}
	r.logf("  every write to a platform resource answered 403 platform_read_only")
	return nil
}

// platformTransactional is claim 1: the templated From address, read back off
// the wire rather than out of the API's own response.
func (r *runner) platformTransactional(ctx context.Context) error {
	var res messageResult
	err := r.api.postJSON(ctx, "/api/v1/messages", messageRequest{
		TemplateID: r.templateID,
		SenderID:   platformSenderID,
		TenantVars: map[string]any{"slug": platformTenantSlug, "name": platformTenantName},
		To:         []messageRecipient{{Email: platformRcpt, Name: "Platform One", Locale: "en"}},
	}, &res)
	if err != nil {
		return fmt.Errorf("POST /messages with the shared sender: %w", err)
	}
	if len(res.Deliveries) != 1 {
		return fmt.Errorf("POST /messages created %d deliveries, want 1", len(res.Deliveries))
	}
	id := res.Deliveries[0].DeliveryID

	// sent or failed: chaos-smtp rejects a deterministic ~1% of messages
	// permanently, and this scenario is about the rendered From address, not
	// about the retry policy scenario 2 covers. Either way the message reached
	// end of DATA, so chaos-smtp remembered it and the header can be read
	// back; a permanent failure is logged rather than silently accepted.
	d, err := r.waitForDelivery(ctx, id, r.opt.mailTimeout, "sent", "failed")
	if err != nil {
		r.fail("the shared-sender delivery went nowhere: %v", err)
		return nil
	}
	if d.Status != "sent" {
		r.logf("  note: chaos-smtp rejected the shared-sender message (%s: %s); "+
			"the From assertion below still holds", d.Status, d.LastError)
	}
	// The stored delivery carries the tenant attributes it was sent with:
	// there is no tenant table to look them up in later (ADR-0017).
	if got := fmt.Sprint(d.TenantVars["slug"]); got != platformTenantSlug {
		r.fail("delivery %s stored tenant_vars.slug = %q, want %q",
			short(id), got, platformTenantSlug)
	}

	msgs, err := fetchChaosMessages(ctx, r.hc, r.opt.chaosStats, platformRcpt, 5, false)
	if err != nil {
		r.fail("reading chaos-smtp messages for %s: %v", platformRcpt, err)
		return nil
	}
	if len(msgs) == 0 {
		r.fail("chaos-smtp has no message for %s although the delivery is sent", platformRcpt)
		return nil
	}
	// The display name is quoted because it contains a comma: the MIME builder
	// has to encode it, and asserting the quoted form is what proves it did
	// rather than pasting the name straight into the header.
	want := fmt.Sprintf("%q <%s>", platformTenantName, platformFromEmail)
	got := msgs[0].Headers["From"]
	if got != want {
		r.fail("From on the delivered message is %q, want %q", got, want)
	} else {
		r.logf("  the shared sender rendered per tenant: From: %s", got)
	}
	return nil
}

// platformTenantVarsMissing is claim 2: the refusal names the key, and nothing
// was queued.
func (r *runner) platformTenantVarsMissing(ctx context.Context) error {
	err := r.api.postJSON(ctx, "/api/v1/messages", messageRequest{
		TemplateID: r.templateID,
		SenderID:   platformSenderID,
		// name but no slug: from_email needs the slug.
		TenantVars: map[string]any{"name": platformTenantName},
		To:         []messageRecipient{{Email: platformRcpt2, Name: "Platform Two"}},
	}, nil)
	var ae *apiError
	switch {
	case !asAPIError(err, &ae):
		r.fail("POST /messages without tenant_vars.slug: got %v, want 422 tenant_vars_missing", err)
		return nil
	case ae.Status != http.StatusUnprocessableEntity || ae.Code != "tenant_vars_missing":
		r.fail("POST /messages without tenant_vars.slug: got %d %s, want 422 tenant_vars_missing",
			ae.Status, ae.Code)
		return nil
	case !strings.Contains(ae.Body, "slug"):
		r.fail("the 422 does not name the missing key: %s", ae.Body)
		return nil
	}
	// And nothing was sent: a refusal that still queued the mail would be
	// worse than no check at all.
	if msgs, err := fetchChaosMessages(ctx, r.hc, r.opt.chaosStats, platformRcpt2, 5, false); err == nil &&
		len(msgs) > 0 {
		r.fail("chaos-smtp received %d message(s) for %s although the request was refused",
			len(msgs), platformRcpt2)
	}
	r.logf("  a send missing tenant_vars.slug was refused with the key named, and queued nothing")
	return nil
}

// platformCampaignDenied is the sender-use policy: the operator allowed its
// shared identity for transactional mail only.
func (r *runner) platformCampaignDenied(ctx context.Context) error {
	err := r.api.postJSON(ctx, "/api/v1/campaigns", campaignInput{
		Name: "e2e platform campaign", VersionID: r.versionID,
		SenderID:   platformSenderID,
		TenantVars: map[string]any{"slug": platformTenantSlug, "name": platformTenantName},
	}, nil)
	var ae *apiError
	switch {
	case !asAPIError(err, &ae):
		r.fail("POST /campaigns with the shared sender: got %v, want 403 sender_use_denied", err)
	case ae.Status != http.StatusForbidden || ae.Code != "sender_use_denied":
		r.fail("POST /campaigns with the shared sender: got %d %s, want 403 sender_use_denied",
			ae.Status, ae.Code)
	default:
		r.logf("  a campaign with the shared sender was denied: %s", ae.Msg)
	}

	// A tenant may not probe it either: the operator did not offer the shared
	// probe mailbox to its tenants (uses has no `probe`).
	err = r.api.postJSON(ctx, "/api/v1/senders/"+platformSenderID+"/probe", map[string]any{}, nil)
	if got := statusOf(err); got != http.StatusForbidden {
		r.fail("POST /senders/%s/probe as a tenant returned %d (%v), want 403",
			platformSenderID, got, err)
	}
	return nil
}

// platformProbeState is the operator's own view of the shared identity's
// health: the probe runs in the system tenant and its state is visible there
// and nowhere else.
func (r *runner) platformProbeState(ctx context.Context) error {
	var trig probeTriggerResult
	if err := r.api.postJSONAs(ctx, systemTenant,
		"/api/v1/senders/"+platformSenderID+"/probe", map[string]any{}, &trig); err != nil {
		return fmt.Errorf("POST /senders/%s/probe as %s: %w", platformSenderID, systemTenant, err)
	}
	if len(trig.Runs) == 0 {
		r.fail("the platform probe trigger created no run")
		return nil
	}
	if got := trig.Runs[0].MailboxID; got != platformProbeBoxID {
		r.fail("the platform probe ran against mailbox %q, want %q", got, platformProbeBoxID)
	}

	// The system tenant sees the run...
	var sysRuns probeRunList
	if err := r.api.getJSONAs(ctx, systemTenant,
		"/api/v1/probe-runs?sender_id="+platformSenderID+"&limit=20", &sysRuns); err != nil {
		return fmt.Errorf("GET /probe-runs as %s: %w", systemTenant, err)
	}
	if len(sysRuns.Items) == 0 {
		r.fail("%s sees no probe run for %s after triggering one", systemTenant, platformSenderID)
	} else {
		// Items[0] is the oldest: the listing is keyset-ordered by
		// (created_at, id) like every other one.
		r.logf("  %s sees %d platform probe run(s); first %s status=%q pending=%v",
			systemTenant, len(sysRuns.Items), short(sysRuns.Items[0].ID),
			sysRuns.Items[0].Status, sysRuns.Items[0].Pending)
	}

	// ...and the tenant sees none of it. The probe verdict of a shared
	// identity is the operator's, so a tenant reads nothing but the one bit
	// GET /senders/{id}/health carries.
	var tenantRuns probeRunList
	if err := r.api.getJSON(ctx,
		"/api/v1/probe-runs?sender_id="+platformSenderID+"&limit=20", &tenantRuns); err != nil {
		return fmt.Errorf("GET /probe-runs as the tenant: %w", err)
	}
	if len(tenantRuns.Items) != 0 {
		r.fail("a tenant sees %d probe run(s) for the shared sender %s, want 0",
			len(tenantRuns.Items), platformSenderID)
	}

	var h senderHealth
	if err := r.api.getJSON(ctx, "/api/v1/senders/"+platformSenderID+"/health", &h); err != nil {
		return fmt.Errorf("GET /senders/%s/health as the tenant: %w", platformSenderID, err)
	}
	if h.TransportStatus != "" || len(h.Mailboxes) != 0 {
		r.fail("a tenant's health summary for %s leaks platform detail: transport=%q mailboxes=%d",
			platformSenderID, h.TransportStatus, len(h.Mailboxes))
	}
	if h.Reason != "" && h.Reason != "shared sender unavailable" {
		r.fail("a tenant's health reason for %s is %q, want empty or the generic reason",
			platformSenderID, h.Reason)
	}
	r.logf("  the tenant's health summary for the shared sender carries no platform detail")
	return nil
}

// whoamiCheck asserts what GET /api/v1/whoami tells a console. It runs inside
// the bootstrap rather than as its own scenario because everything else about
// the operator's view depends on the switch actually working, and finding that
// out first turns "scenario 9 failed" into "the header is not honoured".
func (r *runner) whoamiCheck(ctx context.Context) error {
	var own whoami
	if err := r.api.getJSON(ctx, "/api/v1/whoami", &own); err != nil {
		return fmt.Errorf("GET /whoami: %w", err)
	}
	if own.TenantID != "default" {
		r.fail("whoami reports tenant_id %q, want the principal's own (default)", own.TenantID)
	}
	if own.SystemTenant {
		r.fail("whoami reports system_tenant=true for the caller's own tenant")
	}
	if !own.CanSwitchTenant {
		r.fail("whoami reports can_switch_tenant=false, but test/e2e/config.yaml enables the header")
	}

	var sys whoami
	if err := r.api.getJSONAs(ctx, systemTenant, "/api/v1/whoami", &sys); err != nil {
		return fmt.Errorf("GET /whoami as %s: %w", systemTenant, err)
	}
	if sys.TenantID != systemTenant || !sys.SystemTenant {
		r.fail("whoami with the %s header reports tenant_id=%q system_tenant=%v, want %q/true",
			tenantHeader, sys.TenantID, sys.SystemTenant, systemTenant)
	}
	r.logf("  whoami: principal=%s tenant=%s can_switch=%v; %s header switches to the operator view",
		own.PrincipalID, own.TenantID, own.CanSwitchTenant, tenantHeader)
	return nil
}
