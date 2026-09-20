package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Scenario 3 of the brief: POST /messages with two recipients and a custom
// header, both reaching `sent`, and a replay under the same Idempotency-Key
// returning the same delivery ids rather than sending again.
//
// It goes through sender B (GreenMail) so the custom header can be read back
// off the wire: "the API accepted it" and "it was on the message" are two
// different claims and only the second one matters.
func (r *runner) scenarioTransactional(ctx context.Context) error {
	const (
		one = "tx-one@" + mailDomain
		two = "tx-two@" + mailDomain
	)
	const idemKey = "e2e-transactional-1"
	req := messageRequest{
		TemplateID: r.templateID,
		SenderID:   r.senderMailID,
		To: []messageRecipient{
			{Email: one, Name: "TX One", Locale: "en"},
			{Email: two, Name: "TX Two", Locale: "ko"},
		},
		// X- prefixed names pass the whitelist of architecture 16
		// (internal/sender/message.go ValidateCustomHeader).
		Headers: map[string]string{"X-E2E-Case": "transactional"},
	}

	var res messageResult
	if err := r.api.postJSONWith(ctx, "/api/v1/messages", req,
		map[string]string{"Idempotency-Key": idemKey}, &res); err != nil {
		return fmt.Errorf("POST /messages: %w", err)
	}
	if len(res.Deliveries) != 2 {
		return fmt.Errorf("POST /messages created %d deliveries, want 2", len(res.Deliveries))
	}
	if res.IdempotentReplay {
		r.fail("the first POST /messages reported idempotent_replay=true")
	}
	first := map[string]string{}
	for _, d := range res.Deliveries {
		first[strings.ToLower(d.Email)] = d.DeliveryID
		if d.Status != "queued" {
			r.fail("delivery for %s came back %q, want queued", d.Email, d.Status)
		}
	}

	for email, id := range first {
		d, err := r.waitForDelivery(ctx, id, r.opt.mailTimeout, "sent")
		if err != nil {
			r.fail("%s: %v", email, err)
			continue
		}
		r.logf("  %s -> sent (%s)", email, short(d.ID))
	}

	// The custom header, read back from the mailbox.
	msgs, err := r.gm.waitForMessage(ctx, one, 1, r.opt.mailTimeout)
	if err != nil {
		r.fail("reading %s: %v", one, err)
	} else if got := msgs[0].header("X-E2E-Case"); got != "transactional" {
		r.fail("X-E2E-Case on the delivered message is %q, want %q", got, "transactional")
	} else {
		r.logf("  custom header survived to the wire: X-E2E-Case: %s", got)
	}

	// Idempotent replay: the same key returns the stored result.
	var replay messageResult
	if err := r.api.postJSONWith(ctx, "/api/v1/messages", req,
		map[string]string{"Idempotency-Key": idemKey}, &replay); err != nil {
		return fmt.Errorf("replay POST /messages: %w", err)
	}
	if !replay.IdempotentReplay {
		r.fail("replaying POST /messages with the same Idempotency-Key was not reported as an idempotent replay")
	}
	if len(replay.Deliveries) != len(res.Deliveries) {
		r.fail("replay returned %d deliveries, want %d", len(replay.Deliveries), len(res.Deliveries))
	}
	for _, d := range replay.Deliveries {
		if want := first[strings.ToLower(d.Email)]; want != d.DeliveryID {
			r.fail("replay returned delivery %s for %s, want the original %s", d.DeliveryID, d.Email, want)
		}
	}
	// Nothing was re-sent: the mailbox still holds exactly one message.
	time.Sleep(2 * time.Second)
	again, err := r.gm.messages(ctx, one)
	if err != nil {
		r.fail("re-reading %s: %v", one, err)
	} else if len(again) != 1 {
		r.fail("%s holds %d messages after the replay, want 1", one, len(again))
	}
	r.logf("  replay returned the same %d delivery ids and sent nothing", len(replay.Deliveries))
	return nil
}
