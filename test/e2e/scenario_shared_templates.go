package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Scenario 9, second half: shared templates with tenant overrides (ADR-0018).
//
// The operator authors a template in the system tenant, keyed "welcome",
// shares it and restricts it to transactional mail. From then on:
//
//  1. A tenant sends by `template_key` and the relay receives the shared
//     content — read through, never copied into the tenant.
//  2. A campaign from it is refused (403 template_use_denied).
//  3. The tenant overrides it, edits the subject, publishes, and the same send
//     by key now carries the tenant's subject.
//  4. Deleting the override returns the tenant to the shared template.
//  5. The database holds no copy but the explicit override: checked with SQL
//     at the end, the same way the shadow rows are.
const (
	sharedKey          = "welcome"
	sharedSubject      = "Shared welcome"
	overrideSubject    = "Tenant welcome"
	sharedRcptShared   = "shared-one@" + mailDomain
	sharedRcptOverride = "shared-two@" + mailDomain
	sharedRcptReverted = "shared-three@" + mailDomain
)

type sharedTemplate struct {
	ID                 string   `json:"id"`
	Key                string   `json:"key"`
	Name               string   `json:"name"`
	Shared             bool     `json:"shared"`
	Overridden         bool     `json:"overridden"`
	Uses               []string `json:"uses"`
	Subject            string   `json:"subject"`
	Mode               string   `json:"mode"`
	Body               string   `json:"body"`
	LayoutID           string   `json:"layout_id,omitempty"`
	PublishedVersionID string   `json:"published_version_id"`
	Version            int64    `json:"version"`
}

func (r *runner) platformSharedTemplates(ctx context.Context) error {
	if err := r.sharedTemplateCleanup(ctx); err != nil {
		return err
	}

	// The operator's side, in the system tenant.
	var tpl sharedTemplate
	if err := r.api.postJSONAs(ctx, systemTenant, "/api/v1/templates", map[string]any{
		"name": "welcome (shared)", "key": sharedKey, "shared": true,
		"uses": []string{"transactional"},
		"mode": "html", "subject": sharedSubject + ", {{ recipient.name }}",
		"body": "<html><body><p>Hello from the platform, {{ recipient.name }}.</p></body></html>",
	}, &tpl); err != nil {
		return fmt.Errorf("create the shared template as %s: %w", systemTenant, err)
	}
	var sharedVersion messageVersion
	if err := r.api.postJSONAs(ctx, systemTenant, "/api/v1/templates/"+tpl.ID+"/publish",
		map[string]any{}, &sharedVersion); err != nil {
		return fmt.Errorf("publish the shared template: %w", err)
	}
	r.logf("  %s shared template %s (key %q, uses transactional), version %s",
		systemTenant, short(tpl.ID), sharedKey, short(sharedVersion.ID))

	// The tenant sees it, marked shared, and cannot change it.
	var seen sharedTemplate
	if err := r.api.getJSON(ctx, "/api/v1/templates/"+tpl.ID, &seen); err != nil {
		return fmt.Errorf("GET the shared template as the tenant: %w", err)
	}
	if !seen.Shared || seen.Key != sharedKey {
		r.fail("the tenant sees the shared template with shared=%v key=%q", seen.Shared, seen.Key)
	}
	err := r.api.doAs(ctx, "", http.MethodPost, "/api/v1/templates/"+tpl.ID+"/publish", map[string]any{}, nil)
	r.expectAPIError(err, http.StatusForbidden, "platform_read_only", "publishing a shared template as a tenant")

	// 1. Send by key: the shared content goes out.
	if v := r.sendByKey(ctx, sharedRcptShared); v != "" && v != sharedVersion.ID {
		r.fail("a send by key used version %s, want the shared %s", short(v), short(sharedVersion.ID))
	}
	r.expectSubject(ctx, sharedRcptShared, sharedSubject)

	// 2. The shared template is for transactional mail only.
	err = r.api.postJSON(ctx, "/api/v1/campaigns", map[string]any{
		"name": "e2e shared template campaign", "template_key": sharedKey, "sender_id": r.senderChaosID,
	}, nil)
	r.expectAPIError(err, http.StatusForbidden, "template_use_denied", "a campaign from the shared template")

	// 3. Override, edit, publish, send by key again.
	var over sharedTemplate
	if err := r.api.postJSON(ctx, "/api/v1/templates/"+tpl.ID+"/override", map[string]any{}, &over); err != nil {
		return fmt.Errorf("override the shared template: %w", err)
	}
	if over.Key != sharedKey || over.Shared || !over.Overridden {
		r.fail("the override came back key=%q shared=%v overridden=%v", over.Key, over.Shared, over.Overridden)
	}
	if err := r.api.putJSON(ctx, "/api/v1/templates/"+over.ID, map[string]any{
		"name": over.Name, "key": over.Key, "uses": over.Uses, "mode": over.Mode,
		"subject": overrideSubject + ", {{ recipient.name }}", "body": over.Body,
		"version": over.Version,
	}, nil); err != nil {
		return fmt.Errorf("edit the override: %w", err)
	}
	var overVersion messageVersion
	if err := r.api.postJSON(ctx, "/api/v1/templates/"+over.ID+"/publish", map[string]any{}, &overVersion); err != nil {
		return fmt.Errorf("publish the override: %w", err)
	}
	if v := r.sendByKey(ctx, sharedRcptOverride); v != "" && v != overVersion.ID {
		r.fail("a send by key after the override used version %s, want the override's %s",
			short(v), short(overVersion.ID))
	}
	r.expectSubject(ctx, sharedRcptOverride, overrideSubject)

	// 4. Delete the override: back to the shared template.
	if err := r.api.doAs(ctx, "", http.MethodDelete, "/api/v1/templates/"+over.ID, nil, nil); err != nil {
		return fmt.Errorf("delete the override: %w", err)
	}
	if v := r.sendByKey(ctx, sharedRcptReverted); v != "" && v != sharedVersion.ID {
		r.fail("a send by key after deleting the override used version %s, want the shared %s",
			short(v), short(sharedVersion.ID))
	}
	r.expectSubject(ctx, sharedRcptReverted, sharedSubject)

	// 5. No copies in the database but the explicit override.
	r.sharedTemplateRows(ctx, tpl.ID, over.ID, overVersion.ID)
	return nil
}

// sharedTemplateCleanup makes the half idempotent against a stack an earlier
// run left behind: the key is unique per tenant, so a second create would be a
// 409 rather than a test of anything.
func (r *runner) sharedTemplateCleanup(ctx context.Context) error {
	for _, tenant := range []string{"", systemTenant} {
		var list struct {
			Items []sharedTemplate `json:"items"`
		}
		if err := r.api.getJSONAs(ctx, tenant, "/api/v1/templates?limit=1000", &list); err != nil {
			return fmt.Errorf("list templates: %w", err)
		}
		for _, t := range list.Items {
			if t.Key != sharedKey || (tenant == "" && t.Shared) {
				continue
			}
			if err := r.api.doAs(ctx, tenant, http.MethodDelete, "/api/v1/templates/"+t.ID, nil, nil); err != nil {
				return fmt.Errorf("delete a leftover %q template: %w", sharedKey, err)
			}
		}
	}
	return nil
}

// sendByKey sends one transactional message by template_key with the tenant's
// own chaos-smtp sender, waits for it to reach the relay, and returns the
// version the API reported.
func (r *runner) sendByKey(ctx context.Context, rcpt string) string {
	var res messageResult
	if err := r.api.postJSON(ctx, "/api/v1/messages", map[string]any{
		"template_key": sharedKey, "sender_id": r.senderChaosID,
		"to": []messageRecipient{{Email: rcpt, Name: "Shared", Locale: "en"}},
	}, &res); err != nil {
		r.fail("POST /messages with template_key %q: %v", sharedKey, err)
		return ""
	}
	if len(res.Deliveries) != 1 {
		r.fail("POST /messages by key created %d deliveries, want 1", len(res.Deliveries))
		return res.VersionID
	}
	// sent or failed, for the reason platformTransactional gives: chaos-smtp
	// remembers the message either way, and the subject is what is asserted.
	d, err := r.waitForDelivery(ctx, res.Deliveries[0].DeliveryID, r.opt.mailTimeout, "sent", "failed")
	if err != nil {
		r.fail("the send by key to %s went nowhere: %v", rcpt, err)
	} else if d.Status != "sent" {
		r.logf("  note: chaos-smtp rejected the message to %s (%s: %s)", rcpt, d.Status, d.LastError)
	}
	return res.VersionID
}

func (r *runner) expectSubject(ctx context.Context, rcpt, wantPrefix string) {
	var msgs []chaosMessage
	var err error
	for range 10 {
		if msgs, err = fetchChaosMessages(ctx, r.hc, r.opt.chaosStats, rcpt, 5, false); err == nil && len(msgs) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || len(msgs) == 0 {
		r.fail("chaos-smtp has no message for %s (%v)", rcpt, err)
		return
	}
	if got := msgs[0].Subject; !strings.HasPrefix(got, wantPrefix) {
		r.fail("the message to %s has subject %q, want it to start with %q", rcpt, got, wantPrefix)
		return
	}
	r.logf("  %s received %q", rcpt, msgs[0].Subject)
}

func (r *runner) expectAPIError(err error, status int, code, what string) {
	var ae *apiError
	switch {
	case !asAPIError(err, &ae):
		r.fail("%s: got %v, want %d %s", what, err, status, code)
	case ae.Status != status || ae.Code != code:
		r.fail("%s: got %d %s, want %d %s", what, ae.Status, ae.Code, status, code)
	default:
		r.logf("  %s: %d %s", what, status, code)
	}
}

// sharedTemplateRows reads the template and message_version tables directly:
// "the tenant uses the shared template" must not mean "the tenant has a copy
// of it". The only rows the tenant may ever have had are the override and its
// one published version, and the override is deleted by now.
func (r *runner) sharedTemplateRows(ctx context.Context, sharedID, overrideID, overrideVersionID string) {
	out, err := r.psql(ctx, `SELECT tenant_id, id, shared FROM template WHERE key = '`+sharedKey+`' ORDER BY tenant_id`)
	if err != nil {
		r.logf("  shared-template row check skipped (no psql in this stack: %v)", err)
		return
	}
	rows := nonEmptyLines(out)
	for _, row := range rows {
		r.logf("  template row with key %q: %s", sharedKey, row)
	}
	if len(rows) != 1 || !strings.HasPrefix(rows[0], systemTenant+"|"+sharedID+"|t") {
		r.fail("template rows with key %q are %v, want exactly the shared one in %s", sharedKey, rows, systemTenant)
	}

	vout, err := r.psql(ctx, `SELECT tenant_id, id, template_id FROM message_version `+
		`WHERE template_id IN ('`+sharedID+`', '`+overrideID+`') ORDER BY tenant_id, created_at`)
	if err != nil {
		return
	}
	vrows := nonEmptyLines(vout)
	for _, row := range vrows {
		r.logf("  message_version row: %s", row)
		f := strings.Split(row, "|")
		if len(f) != 3 {
			r.fail("unexpected message_version row %q", row)
			continue
		}
		switch {
		case f[0] == systemTenant && f[2] == sharedID:
			// The shared template's own version, in the system tenant.
		case f[1] == overrideVersionID && f[2] == overrideID:
			// The explicit override's publish: the one copy the tenant asked for.
		default:
			r.fail("message_version %s of template %s is in tenant %s: a shared template's "+
				"version was copied", f[1], f[2], f[0])
		}
	}
	if len(vrows) != 2 {
		r.fail("message_version rows for the shared template and its override: %d, want 2 "+
			"(one shared publish, one override publish)", len(vrows))
	}
}
