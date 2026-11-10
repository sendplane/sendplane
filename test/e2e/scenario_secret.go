package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Scenario 10: a sensitive transactional send (ADR-0019). One recipient gets a
// one-time code through secret_vars, and the run checks the claims the design
// rests on from outside the process:
//
//  1. The code reaches the mailbox, and the message has no open pixel and no
//     rewritten link even though the tenant has both switched on.
//  2. The delivery reports sensitive=true and secret_state=consumed once sent,
//     and nothing the API returns (the send, the delivery, the listing, the
//     attempts) carries the code.
//  3. A retry is refused with 409 not_retryable.
//  4. The database row has no ciphertext left (postgres only; psql runs in
//     the compose stack the way scenario 9 does it).
const (
	secretRcpt = "secret-one@" + mailDomain
	// secretCode is distinctive enough that finding it anywhere is a finding.
	//nolint:gosec // G101: a fixed, public, test-only code; the stack is torn down after every run.
	secretCode = "E2E-OTP-739154"
)

func (r *runner) scenarioSecret(ctx context.Context) error {
	var tpl idOnly
	if err := r.api.postJSON(ctx, "/api/v1/templates", templateInput{
		Name: "e2e otp", Subject: "Your sign-in code", Mode: "html",
		Body: `<html><body><p>Your code is <b>{{ secret.code }}</b>.</p>` +
			`<p><a href="` + offerURL + `">Open the app</a></p></body></html>`,
		DefaultLocale: "en",
	}, &tpl); err != nil {
		return fmt.Errorf("create the otp template: %w", err)
	}
	var version messageVersion
	if err := r.api.postJSON(ctx, "/api/v1/templates/"+tpl.ID+"/publish", map[string]any{}, &version); err != nil {
		return fmt.Errorf("publish the otp template: %w", err)
	}

	var res messageResult
	if err := r.api.postJSON(ctx, "/api/v1/messages", messageRequest{
		VersionID: version.ID, SenderID: r.senderMailID,
		To: []messageRecipient{{
			Email: secretRcpt, Name: "Secret One", Locale: "en",
			SecretVars: map[string]any{"code": secretCode},
		}},
		SecretTTLSeconds: 300,
	}, &res); err != nil {
		return fmt.Errorf("POST /messages with secret_vars: %w", err)
	}
	if len(res.Deliveries) != 1 {
		return fmt.Errorf("POST /messages created %d deliveries, want 1", len(res.Deliveries))
	}
	id := res.Deliveries[0].DeliveryID

	d, err := r.waitForDelivery(ctx, id, r.opt.mailTimeout, "sent")
	if err != nil {
		r.fail("%v", err)
	} else {
		r.logf("  %s -> sent (%s)", secretRcpt, short(d.ID))
		if !d.Sensitive || d.SecretState != "consumed" {
			r.fail("delivery %s: sensitive=%v secret_state=%q, want true and consumed", short(id), d.Sensitive, d.SecretState)
		} else {
			r.logf("  delivery: sensitive=true secret_state=consumed")
		}
		if d.SecretExpiresAt == nil {
			r.fail("delivery %s has no secret_expires_at", short(id))
		}
	}

	// 1. The message.
	msgs, err := r.gm.waitForMessage(ctx, secretRcpt, 1, r.opt.mailTimeout)
	if err != nil {
		r.fail("reading %s: %v", secretRcpt, err)
	} else {
		html, err := msgs[0].htmlPart()
		switch {
		case err != nil:
			r.fail("decoding the otp message: %v", err)
		case !strings.Contains(html, secretCode):
			r.fail("the delivered message does not contain the code")
		default:
			r.logf("  the code reached the mailbox")
		}
		if strings.Contains(html, "/t/o/") {
			r.fail("the sensitive message has an open pixel")
		}
		if strings.Contains(html, "/t/c/") || !strings.Contains(html, `href="`+offerURL+`"`) {
			r.fail("the sensitive message had its link rewritten for click tracking")
		}
		if err == nil && !strings.Contains(html, "/t/o/") && !strings.Contains(html, "/t/c/") {
			r.logf("  no open pixel, no rewritten link (tracking is on for the tenant)")
		}
	}

	// 2. Nothing the API returns carries the code.
	for _, path := range []string{
		"/api/v1/deliveries/" + id,
		"/api/v1/deliveries/" + id + "/attempts",
		"/api/v1/deliveries?lane=transactional&limit=200",
	} {
		resp, err := r.api.rawGet(ctx, path, nil)
		switch {
		case err != nil:
			r.fail("GET %s: %v", path, err)
		case resp.Status != http.StatusOK:
			r.fail("GET %s: status %d", path, resp.Status)
		case strings.Contains(string(resp.Body), secretCode):
			r.fail("GET %s returned the secret", path)
		case strings.Contains(string(resp.Body), `"secret_vars"`):
			r.fail("GET %s returned a secret_vars field", path)
		}
	}
	r.logf("  delivery, attempts and listing carry no secret")

	// 3. No resend.
	err = r.api.postJSON(ctx, "/api/v1/deliveries/"+id+"/retry", map[string]any{}, nil)
	r.expectAPIError(err, http.StatusConflict, "not_retryable", "retrying the sensitive delivery")

	// 4. The row itself.
	out, err := r.psql(ctx, `SELECT secret_state, secret_ct IS NULL FROM delivery WHERE id = '`+id+`'`)
	if err != nil {
		r.logf("  secret column check skipped (no psql in this stack: %v)", err)
	} else {
		row := strings.TrimSpace(out)
		// secret_state 2 is consumed (store.SecretConsumed).
		if row != "2|t" {
			r.fail("delivery row secret_state|secret_ct IS NULL = %q, want 2|t", row)
		} else {
			r.logf("  delivery row: secret_state=consumed, secret_ct IS NULL")
		}
	}

	// Nothing was sent a second time.
	time.Sleep(2 * time.Second)
	if again, err := r.gm.messages(ctx, secretRcpt); err == nil && len(again) != 1 {
		r.fail("%s holds %d messages, want 1", secretRcpt, len(again))
	}
	return nil
}
