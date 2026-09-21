package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/sendplane/sendplane/internal/probe/inbound/sendplanehook"
)

// Scenario 6b: the same loopback probe, over the inbound webhook of ADR-0016
// instead of IMAP.
//
// The harness plays whatever forwards the mail. There is no such service in
// the compose stack and there could not be: the point of the channel is that
// somebody else takes delivery and posts the message to sendplane, so what
// has to be tested here is sendplane's half — the signature check, the token,
// the tenant resolution, the verdict built from a header *map* rather than a
// raw message, and the idempotency of a redelivery.
//
// So: sendplane really sends the probe through the real sender path to
// `probe-hook@sendplane.test`, GreenMail really takes delivery, and the
// harness then does exactly what a forwarder would — read the delivered
// message, rewrite it as the `{from,to,headers,text}` body of
// internal/probe/inbound/sendplanehook, sign the raw bytes with the shared
// secret from test/e2e/config.yaml and POST it to the control's inbound route.
//
// The one thing the harness has to invent is the `Authentication-Results`
// header: GreenMail writes none, and without a trusted one the verdict is the
// "nothing to judge" yellow that scenario 6 already covers. Synthesizing it
// with the mailbox's own authserv-id is what makes this scenario exercise the
// *parsing* path instead.
const (
	probeHookAddress    = "probe-hook@" + mailDomain
	probeHookAuthServID = "mx." + mailDomain
	probeHookPath       = "/probe/inbound/sendplane"
	// Must match probe.webhooks[0].secret in test/e2e/config.yaml.
	//nolint:gosec // G101: a fixed, public, test-only secret; the stack is torn down after every run.
	probeHookSecret = "whsec_e2e-probe-webhook"
	// probeHookNoTLSReason is the verdict. Everything the synthesized
	// Authentication-Results reports passes and the folder is unknown, which
	// ADR-0016 makes neutral — so the first thing left that is wrong is that
	// GreenMail takes delivery over plain SMTP (internal/probe/verdict.go).
	probeHookNoTLSReason = "TLS 없이 전달되었습니다"
)

func (r *runner) scenarioProbeWebhook(ctx context.Context) error {
	if err := r.createWebhookProbeMailbox(ctx); err != nil {
		return err
	}

	healthBefore, err := r.senderHealthStatus(ctx)
	if err != nil {
		return err
	}
	eventsBefore := len(r.sink.byType("sender.health_changed"))

	runID, err := r.triggerWebhookProbe(ctx)
	if err != nil {
		return err
	}
	payload, err := r.hookPayload(ctx)
	if err != nil {
		return err
	}

	// Before the good delivery: a request that does not authenticate must
	// never be served, whatever the payload says.
	if err := r.assertHookRejects(ctx, payload); err != nil {
		return err
	}

	// First delivery: the run completes.
	if err := r.postHook(ctx, payload, "accepted"); err != nil {
		return err
	}
	run, err := r.assertWebhookRun(ctx, runID)
	if err != nil {
		return err
	}
	if err := r.assertWebhookMailboxHealthy(ctx); err != nil {
		return err
	}
	if err := r.assertWebhookSenderHealth(ctx, run, healthBefore, eventsBefore); err != nil {
		return err
	}

	// Second delivery of the very same payload: a forwarder retries whenever
	// it does not see a 2xx, so a redelivery has to be a 200 that changes
	// nothing. A channel that completed the run twice would double-count the
	// undelivered streak and re-emit sender.health_changed. The bytes are
	// identical but the signature is not — it carries a fresh timestamp —
	// which is exactly why the dedupe key is the body and not the signature.
	if err := r.postHook(ctx, payload, "accepted"); err != nil {
		return err
	}
	again, err := r.probeRunByID(ctx, runID)
	if err != nil {
		return err
	}
	if again.Status != run.Status || again.Reason != run.Reason || again.ReceivedAt == nil ||
		!again.ReceivedAt.Equal(*run.ReceivedAt) {
		r.fail("redelivering the same webhook changed the run: %s/%q -> %s/%q",
			run.Status, run.Reason, again.Status, again.Reason)
	}
	r.logf("  redelivery was a no-op; run still %s", again.Status)
	return nil
}

// createWebhookProbeMailbox registers the kind=webhook mailbox. It carries no
// host, port or credentials: the API refuses them for this kind, because a row
// that looks like an IMAP account nothing ever dials is a row an operator will
// misread (ADR-0016).
func (r *runner) createWebhookProbeMailbox(ctx context.Context) error {
	var box idOnly
	if err := r.api.postJSON(ctx, "/api/v1/probe-mailboxes", map[string]any{
		"name": "e2e-probe-webhook", "kind": "webhook", "address": probeHookAddress,
		// The authserv-id the harness's synthesized Authentication-Results
		// carries. Only a header with this value is trusted (RFC 8601,
		// ADR-0012), which is exactly what this scenario exercises.
		"authserv_id": probeHookAuthServID,
		"enabled":     true,
	}, &box); err != nil {
		return fmt.Errorf("create webhook probe mailbox: %w", err)
	}
	r.probeHookMailboxID = box.ID
	r.logf("  webhook probe mailbox %s (%s)", short(box.ID), probeHookAddress)

	// The test endpoint answers for this kind without dialling anything: there
	// are no credentials, so the only thing it can report is that the
	// deployment has an inbound endpoint the provider could post to.
	var res mailboxTestResult
	if err := r.api.postJSON(ctx, "/api/v1/probe-mailboxes/"+box.ID+"/test", map[string]any{}, &res); err != nil {
		return fmt.Errorf("test webhook probe mailbox: %w", err)
	}
	if !res.OK || res.Stage != "ok" || res.Server != "webhook:sendplane" {
		r.fail("testing a webhook probe mailbox reported ok=%v stage=%q server=%q, "+
			"want ok=true stage=ok server=webhook:sendplane", res.OK, res.Stage, res.Server)
	}
	return nil
}

// triggerWebhookProbe triggers a probe on sender B and returns the run that
// belongs to the webhook mailbox. The trigger creates one run per enabled
// mailbox and reports only the first, so the run is looked up by mailbox.
func (r *runner) triggerWebhookProbe(ctx context.Context) (string, error) {
	var res probeTriggerResult
	if err := r.api.postJSON(ctx, "/api/v1/senders/"+r.senderMailID+"/probe", map[string]any{}, &res); err != nil {
		return "", fmt.Errorf("POST /senders/{id}/probe: %w", err)
	}

	runID := ""
	deadline := time.Now().Add(30 * time.Second)
	for {
		runs, err := r.probeRunsOf(ctx, r.senderMailID)
		if err != nil {
			return "", err
		}
		for _, run := range runs {
			if run.MailboxID == r.probeHookMailboxID && run.Pending {
				runID = run.ID
				break
			}
		}
		if runID != "" || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if runID == "" {
		r.fail("the probe trigger created no pending run for the webhook mailbox %s; "+
			"Trigger must treat a webhook-kind mailbox like any other one",
			short(r.probeHookMailboxID))
		return "", nil
	}
	r.probeHookRunID = runID
	r.logf("  webhook probe run %s enqueued for sender B", short(runID))
	return runID, nil
}

// hookPayload pulls the delivered probe mail out of GreenMail and rewrites it
// as the body internal/probe/inbound/sendplanehook documents.
func (r *runner) hookPayload(ctx context.Context) ([]byte, error) {
	msgs, err := r.gm.waitForMessage(ctx, probeHookAddress, 1, r.opt.mailTimeout)
	if err != nil {
		return nil, fmt.Errorf("the probe mail never reached %s: %w", probeHookAddress, err)
	}
	msg := msgs[0]

	hdr, err := textproto.NewReader(bufio.NewReader(strings.NewReader(msg.Raw))).ReadMIMEHeader()
	if err != nil && len(hdr) == 0 {
		return nil, fmt.Errorf("parsing the delivered probe mail: %w", err)
	}
	if hdr.Get("X-Sendplane-Probe") == "" {
		r.fail("the probe mail carries no X-Sendplane-Probe header; the inbound webhook has " +
			"nothing to resolve the tenant and the run from")
	}

	// Every header the message arrived with, unfiltered. That is the contract
	// the README states for whatever forwards mail here: Authentication-
	// Results, Received and X-Sendplane-Probe must survive the trip.
	headers := map[string][]string{}
	for name, values := range hdr {
		headers[name] = append([]string(nil), values...)
	}
	// What the receiving MTA would have written. Scenario 6 covers the verdict
	// when there is no trusted header; this one covers the verdict when there
	// is, which is the only way the parsing path gets exercised end to end.
	headers["Authentication-Results"] = []string{
		probeHookAuthServID + "; spf=pass smtp.mailfrom=" + probeHookAddress +
			"; dkim=pass header.d=" + mailDomain + " header.s=e2e; dmarc=pass",
	}

	body, err := json.Marshal(map[string]any{
		"from":    hdr.Get("From"),
		"to":      []string{probeHookAddress},
		"headers": headers,
		"text":    "sendplane loopback health probe.\r\n",
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// postHook signs and posts one delivery, and checks the reply.
func (r *runner) postHook(ctx context.Context, body []byte, wantOutcome string) error {
	res, err := r.api.raw(ctx, http.MethodPost, probeHookPath,
		strings.NewReader(string(body)), "application/json", map[string]string{
			sendplanehook.HeaderSignature: sendplanehook.Sign(probeHookSecret, time.Now(), body),
		})
	if err != nil {
		return fmt.Errorf("POST %s: %w", probeHookPath, err)
	}
	if res.Status != http.StatusOK {
		r.fail("POST %s answered %d, want 200; body: %s",
			probeHookPath, res.Status, strings.TrimSpace(string(res.Body)))
		return nil
	}
	if got := strings.TrimSpace(string(res.Body)); got != wantOutcome {
		r.fail("the inbound webhook reported %q, want %q", got, wantOutcome)
	}
	r.logf("  posted a delivery: %d %s", res.Status, strings.TrimSpace(string(res.Body)))
	return nil
}

// assertHookRejects covers the two ways a request fails to authenticate. Both
// have to be 401 *before* anything is parsed: this route is public, and the
// signature is the only thing standing between a stranger and a forged verdict.
func (r *runner) assertHookRejects(ctx context.Context, body []byte) error {
	cases := []struct {
		name   string
		header string
	}{
		{"wrong secret", sendplanehook.Sign("not-the-secret", time.Now(), body)},
		// A signature that was valid an hour ago is a replay. The timestamp
		// inside the signed string is what makes that detectable at all.
		{"expired timestamp", sendplanehook.Sign(probeHookSecret, time.Now().Add(-time.Hour), body)},
	}
	for _, tc := range cases {
		res, err := r.api.raw(ctx, http.MethodPost, probeHookPath,
			strings.NewReader(string(body)), "application/json",
			map[string]string{sendplanehook.HeaderSignature: tc.header})
		if err != nil {
			return fmt.Errorf("POST %s (%s): %w", probeHookPath, tc.name, err)
		}
		if res.Status != http.StatusUnauthorized {
			r.fail("a webhook with a %s answered %d, want 401", tc.name, res.Status)
		} else {
			r.logf("  %s: 401", tc.name)
		}
	}
	return nil
}

// assertWebhookRun waits for the run and checks the verdict.
func (r *runner) assertWebhookRun(ctx context.Context, runID string) (probeRun, error) {
	if runID == "" {
		return probeRun{}, nil
	}
	// The inbound handler completes the run inside the request, so this is a
	// read and not a wait — but the POST answered before the run was read
	// back, so one short retry covers a store that is momentarily behind.
	var run probeRun
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := r.probeRunByID(ctx, runID)
		if err != nil {
			return probeRun{}, err
		}
		if !got.Pending {
			run = got
			break
		}
		if time.Now().After(deadline) {
			r.fail("probe run %s is still pending after the provider posted the mail: the "+
				"inbound webhook of ADR-0016 must complete the run in the request", short(runID))
			return got, nil
		}
		select {
		case <-ctx.Done():
			return probeRun{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	r.logf("  webhook probe run: status=%s delivered=%v folder=%s reason=%q",
		run.Status, run.Delivered, run.Folder, run.Reason)

	if !run.Delivered {
		r.fail("the webhook probe run reports delivered=false although the provider posted the mail")
	}
	if run.Folder != "unknown" {
		r.fail("the webhook probe run reports folder=%q, want unknown: a webhook is told about a "+
			"delivery, not about where the message was filed", run.Folder)
	}
	if run.SPF != "pass" || run.DKIM != "pass" || run.DMARC != "pass" {
		r.fail("the webhook probe run read spf/dkim/dmarc as %q/%q/%q, want pass/pass/pass from the "+
			"Authentication-Results header the payload carried", run.SPF, run.DKIM, run.DMARC)
	}
	if run.Status != "yellow" {
		r.fail("the webhook probe verdict is %q; everything the Authentication-Results header "+
			"reports passes and an unknown folder is neutral, so the only thing left is that "+
			"GreenMail takes delivery without TLS - that is yellow", run.Status)
	}
	if run.Reason != probeHookNoTLSReason {
		r.fail("the webhook probe reason is %q, want %q", run.Reason, probeHookNoTLSReason)
	}
	if run.ReceivedAt == nil {
		r.fail("the webhook probe run has no received_at")
	}
	return run, nil
}

// assertWebhookMailboxHealthy checks that an arriving probe reports the
// mailbox healthy. A webhook mailbox has no login, so this is the only signal
// there is that the provider's forward still works (architecture 11.5).
func (r *runner) assertWebhookMailboxHealthy(ctx context.Context) error {
	var box probeMailbox
	if err := r.api.getJSON(ctx, "/api/v1/probe-mailboxes/"+r.probeHookMailboxID, &box); err != nil {
		return fmt.Errorf("GET /probe-mailboxes/{id}: %w", err)
	}
	if box.Kind != "webhook" {
		r.fail("the probe mailbox came back with kind=%q, want webhook", box.Kind)
	}
	if box.Host != "" || box.Port != 0 {
		r.fail("the webhook probe mailbox carries host=%q port=%d; the API must refuse an IMAP block "+
			"for this kind", box.Host, box.Port)
	}
	if box.Health == nil || box.Health.Status != "ok" {
		r.fail("the webhook probe mailbox health is %v, want ok after a probe arrived", box.Health)
	} else {
		r.logf("  webhook mailbox health: %s", box.Health.Status)
	}
	return nil
}

// assertWebhookSenderHealth checks that the run reached the sender summary.
//
// The sender.health_changed event is asserted only when the summary actually
// moved, which is the documented contract (architecture 11.2: the event is
// emitted "상태가 바뀌었을 때만"). Whether it moves here depends on what ran
// before: on a full run scenario 6 has already put sender B at yellow, and a
// second yellow is not a change.
func (r *runner) assertWebhookSenderHealth(
	ctx context.Context, run probeRun, before string, eventsBefore int,
) error {
	var health senderHealth
	if err := r.api.getJSON(ctx, "/api/v1/senders/"+r.senderMailID+"/health", &health); err != nil {
		return fmt.Errorf("GET /senders/{id}/health: %w", err)
	}
	found := false
	for _, m := range health.Mailboxes {
		if m.MailboxID == r.probeHookMailboxID {
			found = true
			if m.ID != run.ID {
				r.fail("sender health reports probe run %s for the webhook mailbox, want the latest %s",
					short(m.ID), short(run.ID))
			}
		}
	}
	if !found {
		r.fail("sender health lists no run for the webhook probe mailbox %s; a webhook run must "+
			"count towards the worst-of summary like an IMAP one", short(r.probeHookMailboxID))
	}
	r.logf("  sender health: %s (%s)", health.Status, health.Reason)

	if health.Status == before {
		r.logf("  sender health was already %s, so no sender.health_changed is due", before)
		return nil
	}
	evs, err := r.sink.waitFor(ctx, "sender.health_changed", 30*time.Second)
	if err != nil {
		r.fail("the webhook probe moved sender health %s -> %s but no sender.health_changed "+
			"arrived: %v", before, health.Status, err)
		return nil
	}
	if len(evs) <= eventsBefore {
		r.fail("sender health moved %s -> %s but no new sender.health_changed arrived (%d before, %d now)",
			before, health.Status, eventsBefore, len(evs))
		return nil
	}
	r.logf("  sender.health_changed delivered (%d total)", len(evs))
	return nil
}

// --- small readers ------------------------------------------------------

func (r *runner) senderHealthStatus(ctx context.Context) (string, error) {
	var health senderHealth
	if err := r.api.getJSON(ctx, "/api/v1/senders/"+r.senderMailID+"/health", &health); err != nil {
		return "", fmt.Errorf("GET /senders/{id}/health: %w", err)
	}
	return health.Status, nil
}

func (r *runner) probeRunByID(ctx context.Context, id string) (probeRun, error) {
	var out probeRun
	if err := r.api.getJSON(ctx, "/api/v1/probe-runs/"+id, &out); err != nil {
		return probeRun{}, fmt.Errorf("GET /probe-runs/{id}: %w", err)
	}
	return out, nil
}

func (r *runner) probeRunsOf(ctx context.Context, senderID string) ([]probeRun, error) {
	var out probeRunList
	if err := r.api.getJSON(ctx, "/api/v1/probe-runs?sender_id="+queryEscape(senderID)+"&limit=100", &out); err != nil {
		return nil, fmt.Errorf("GET /probe-runs: %w", err)
	}
	return out.Items, nil
}
