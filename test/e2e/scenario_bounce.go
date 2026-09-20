package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Scenario 5: a hard DSN, an ARF complaint and a forged DSN, all injected into
// the tenant's bounce mailbox and all correlated back to a real delivery.
//
// The DSNs are built from the message GreenMail actually received, not from a
// canned fixture: `Return-Path` carries the VERP envelope sender the sender
// chose (architecture 10) and GreenMail writes it into the delivered message,
// so the correlation under test runs on real data. The forged one reuses that
// same address with its HMAC tag replaced, which is the forgery an attacker
// who read `X-Sendplane-ID` off their own copy could mount.
func (r *runner) scenarioBounce(ctx context.Context) error {
	const (
		reportingMTA = "MAILER-DAEMON@mx." + mailDomain

		hardTo     = "bounce-hard@" + mailDomain
		complainTo = "bounce-arf@" + mailDomain
		forgedTo   = "bounce-forged@" + mailDomain
	)

	sent, err := r.sendAndCollect(ctx, []string{hardTo, complainTo, forgedTo})
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	hard := sent[hardTo]
	arf := sent[complainTo]
	forged := sent[forgedTo]

	for _, s := range []sentMessage{hard, arf, forged} {
		if s.verp == "" {
			r.fail("%s: the delivered message has no VERP Return-Path; "+
				"the sending domain's return_path_domain or the tenant signing key did not reach the sender", s.original.To)
		}
	}

	// All three go in at once: the poller handles a batch per pass and each
	// report names a different delivery. The envelope sender is the reporting
	// MTA rather than the `<>` a real DSN carries, because GreenMail cannot
	// accept a null reverse path (see greenmail.deliver); the message's own
	// `Return-Path: <>` header is the real one and is what gets parsed.
	if err := r.gm.deliver(reportingMTA, bounceAddress, hardDSN(hard.original, hard.verp, now)); err != nil {
		return err
	}
	if err := r.gm.deliver("complaints@feedback.sendplane.test", bounceAddress,
		arfComplaint(arf.original, arf.verp, now)); err != nil {
		return err
	}
	if err := r.gm.deliver(reportingMTA, bounceAddress,
		hardDSN(forged.original, forgeVERP(forged.verp), now)); err != nil {
		return err
	}
	r.logf("  injected a hard DSN, an ARF complaint and a forged DSN into %s", bounceAddress)

	// The bounce role polls every 5s (test/e2e/config.yaml).
	r.assertBounced(ctx, hard, "bounced", "hard")
	r.assertBounced(ctx, arf, "complained", "complaint")
	r.assertForged(ctx, forged)
	r.assertSuppression(ctx, hard)
	return nil
}

// sentMessage ties a delivery to what came out the other end.
type sentMessage struct {
	deliveryID string
	verp       string
	original   bouncedOriginal
}

// sendAndCollect sends one transactional message per address through sender B
// and reads each one back out of GreenMail.
func (r *runner) sendAndCollect(ctx context.Context, addrs []string) (map[string]sentMessage, error) {
	to := make([]messageRecipient, 0, len(addrs))
	for i, a := range addrs {
		to = append(to, messageRecipient{Email: a, Name: fmt.Sprintf("Bounce %d", i+1), Locale: "en"})
	}
	var res messageResult
	if err := r.api.postJSON(ctx, "/api/v1/messages", messageRequest{
		TemplateID: r.templateID, SenderID: r.senderMailID, To: to,
	}, &res); err != nil {
		return nil, fmt.Errorf("POST /messages for the bounce scenario: %w", err)
	}

	out := map[string]sentMessage{}
	for _, item := range res.Deliveries {
		d, err := r.waitForDelivery(ctx, item.DeliveryID, r.opt.mailTimeout, "sent")
		if err != nil {
			return nil, err
		}
		msgs, err := r.gm.waitForMessage(ctx, item.Email, 1, r.opt.mailTimeout)
		if err != nil {
			return nil, err
		}
		m := msgs[0]
		out[item.Email] = sentMessage{
			deliveryID: d.ID,
			verp:       strings.Trim(m.header("Return-Path"), "<>"),
			original: bouncedOriginal{
				ReturnPath:  strings.Trim(m.header("Return-Path"), "<>"),
				From:        m.header("From"),
				To:          item.Email,
				Subject:     m.decodedSubject(),
				MessageID:   m.header("Message-ID"),
				SendplaneID: m.header("X-Sendplane-ID"),
				Date:        m.header("Date"),
			},
		}
		r.logf("  %s -> delivery %s, Return-Path <%s>", item.Email, short(d.ID), out[item.Email].verp)
	}
	return out, nil
}

func (r *runner) assertBounced(ctx context.Context, s sentMessage, wantStatus, wantType string) {
	d, err := r.waitForDelivery(ctx, s.deliveryID, 90*time.Second, wantStatus)
	if err != nil {
		r.fail("%s", err)
		return
	}
	r.logf("  %s -> %s", s.original.To, d.Status)

	var list bounceEventList
	if err := r.api.getJSON(ctx, "/api/v1/deliveries/"+s.deliveryID+"/bounces", &list); err != nil {
		r.fail("GET /deliveries/%s/bounces: %v", short(s.deliveryID), err)
		return
	}
	if len(list.Items) != 1 {
		r.fail("delivery %s has %d bounce events, want 1", short(s.deliveryID), len(list.Items))
		return
	}
	ev := list.Items[0]
	if ev.Type != wantType {
		r.fail("the bounce event for %s is type %q, want %q", s.original.To, ev.Type, wantType)
	}
	if !ev.Verified {
		r.fail("the bounce event for %s is not verified, although the VERP HMAC was genuine", s.original.To)
	}
	if wantType == "hard" && ev.SMTPStatus != "5.1.1" {
		r.fail("the bounce event for %s carries smtp_status %q, want 5.1.1", s.original.To, ev.SMTPStatus)
	}
}

func (r *runner) assertForged(ctx context.Context, s sentMessage) {
	// Give the poller the same window the other two got, then check that
	// nothing moved.
	deadline := time.Now().Add(60 * time.Second)
	var list bounceEventList
	for {
		if err := r.api.getJSON(ctx, "/api/v1/deliveries/"+s.deliveryID+"/bounces", &list); err != nil {
			r.fail("GET /deliveries/%s/bounces: %v", short(s.deliveryID), err)
			return
		}
		if len(list.Items) > 0 || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	var d delivery
	if err := r.api.getJSON(ctx, "/api/v1/deliveries/"+s.deliveryID, &d); err != nil {
		r.fail("re-reading the forged-DSN delivery: %v", err)
		return
	}
	if d.Status != "sent" {
		r.fail("the delivery hit by a forged DSN is %q, want it left at sent", d.Status)
	}
	if len(list.Items) != 1 {
		r.fail("the forged DSN produced %d bounce events, want exactly 1 recorded-but-unverified", len(list.Items))
		return
	}
	if list.Items[0].Verified {
		r.fail("the forged DSN was recorded as verified")
	} else {
		r.logf("  forged DSN recorded as unverified; delivery still %s", d.Status)
	}
}

func (r *runner) assertSuppression(ctx context.Context, s sentMessage) {
	email := s.original.To
	deadline := time.Now().Add(30 * time.Second)
	var sup suppression
	for {
		err := r.api.getJSON(ctx, "/api/v1/suppressions/"+queryEscape(email), &sup)
		if err == nil {
			break
		}
		if statusOf(err) != 404 {
			r.fail("GET /suppressions/%s: %v", email, err)
			return
		}
		if time.Now().After(deadline) {
			r.fail("%s was not suppressed after its hard bounce", email)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if sup.Reason == "" {
		r.fail("the suppression for %s has no reason", email)
	}
	r.logf("  %s suppressed (reason=%s)", email, sup.Reason)

	// And the suppression actually blocks the next send.
	var res messageResult
	if err := r.api.postJSON(ctx, "/api/v1/messages", messageRequest{
		TemplateID: r.templateID, SenderID: r.senderMailID,
		To: []messageRecipient{{Email: email, Name: "Bounce Again", Locale: "en"}},
	}, &res); err != nil {
		r.fail("POST /messages to the suppressed address: %v", err)
		return
	}
	if len(res.Deliveries) != 1 {
		r.fail("POST /messages to the suppressed address returned %d deliveries, want 1", len(res.Deliveries))
		return
	}
	if res.Deliveries[0].Status != "suppressed" {
		r.fail("POST /messages to the suppressed %s returned status %q, want suppressed",
			email, res.Deliveries[0].Status)
	} else {
		r.logf("  a further send to %s came back suppressed", email)
	}
}
