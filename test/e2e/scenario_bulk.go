package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"
)

// Scenario 2: 10,000 recipients with mixed en/ko locales, ingested as NDJSON
// in two chunks and sent through chaos-smtp, asserted against counts derived
// from chaossmtp.Decide rather than from a recorded snapshot (expect.go).
//
// The locale half of the brief's scenario 2 — "the ko recipients received the
// ko subject" — is asserted in scenario 4 instead, on a campaign whose mail
// can actually be read back; see the comment on trackingRecipients. The
// locales are still mixed here so the render path runs both.
func (r *runner) bulkEmail(i int) string {
	return fmt.Sprintf("u%06d@%s", i, bulkDomain)
}

func bulkLocale(i int) string {
	if i%2 == 0 {
		return "en"
	}
	return "ko"
}

func (r *runner) scenarioBulkCampaign(ctx context.Context) error {
	r.exp = computeExpectation(r.opt.seed, r.rates(), r.bulkEmail, r.opt.recipients, r.opt.maxAttempts)
	r.logf("  expected: sent=%d failed=%d (permanent=%d exhausted=%d) attempts=%d",
		r.exp.Sent, r.exp.Failed, r.exp.FailedPermanent, r.exp.FailedExhausted, r.exp.Attempts)

	before, err := fetchChaosStats(ctx, r.hc, r.opt.chaosStats)
	if err != nil {
		return fmt.Errorf("reading chaos-smtp /stats: %w", err)
	}

	var camp campaign
	if err := r.api.postJSON(ctx, "/api/v1/campaigns", campaignInput{
		Name: fmt.Sprintf("e2e-bulk-%d", r.opt.recipients), VersionID: r.versionID, SenderID: r.senderChaosID,
	}, &camp); err != nil {
		return fmt.Errorf("create the bulk campaign: %w", err)
	}
	r.bulkCampaignID = camp.ID

	if err := r.ingest(ctx, camp.ID); err != nil {
		return err
	}
	if err := r.api.postJSON(ctx, "/api/v1/campaigns/"+camp.ID+"/start", map[string]any{}, nil); err != nil {
		return fmt.Errorf("start the bulk campaign: %w", err)
	}

	final, err := r.pollBulk(ctx, camp.ID)
	if err != nil {
		return err
	}

	sent, failed, suppressed := final.count("sent"), final.count("failed"), final.count("suppressed")
	if final.Status != "completed" {
		r.fail("the bulk campaign is %s, want completed", final.Status)
	}
	if total := sent + failed + suppressed; total != int64(r.opt.recipients) {
		r.fail("sent+failed+suppressed = %d, want %d", total, r.opt.recipients)
	}
	for _, s := range []string{"pending", "queued", "leased", "deferred"} {
		if n := final.count(s); n != 0 {
			r.fail("%d deliveries are still %s", n, s)
		}
	}
	// Exact, not approximate: the decision function is deterministic in
	// (seed, recipient, attempt), so even the duplicate sends a SIGKILL causes
	// replay the same decision and cannot move these two numbers.
	if sent != r.exp.Sent {
		r.fail("sent = %d, want %d (derived from chaossmtp.Decide)", sent, r.exp.Sent)
	}
	if failed != r.exp.Failed {
		r.fail("failed = %d, want %d (derived from chaossmtp.Decide)", failed, r.exp.Failed)
	}

	after, err := fetchChaosStats(ctx, r.hc, r.opt.chaosStats)
	if err != nil {
		r.fail("reading chaos-smtp /stats: %v", err)
		return nil
	}
	accepted := after.Accepted - before.Accepted
	dup := accepted - sent
	switch {
	case dup < 0:
		r.fail("chaos-smtp accepted %d messages but the campaign counts %d sent", accepted, sent)
	case dup == 0:
		r.logf("  chaos-smtp accepted exactly %d messages: no double send", accepted)
	case !r.opt.killSender:
		r.fail("chaos-smtp accepted %d messages for %d sent deliveries (%d duplicates) without a sender kill",
			accepted, sent, dup)
	default:
		// A replica SIGKILLed between "the relay took it" and "the row says
		// sent" re-sends that message once its lease expires. At-least-once by
		// design, but only just (architecture 15.1).
		limit := float64(r.opt.recipients) * 0.001
		if float64(dup) > limit {
			r.fail("%d duplicate sends recovered after the SIGKILL, over the %.0f allowed (0.1%% of %d)",
				dup, limit, r.opt.recipients)
		} else {
			r.logf("  duplicates_from_recovery = %d (within the 0.1%% allowance)", dup)
		}
	}

	// Every failure must be explained by the policy: either it ran out of
	// attempts or the relay refused it permanently (architecture 15.1).
	r.assertFailedSample(ctx, camp.ID)
	return nil
}

func (r *runner) ingest(ctx context.Context, campaignID string) error {
	path := "/api/v1/campaigns/" + campaignID + "/recipients"
	chunks := (r.opt.recipients + r.opt.chunk - 1) / r.opt.chunk
	start := time.Now()
	var total int64
	for c := range chunks {
		from := c * r.opt.chunk
		to := min(from+r.opt.chunk, r.opt.recipients)
		var res ingestResult
		if err := r.api.postNDJSON(ctx, path, fmt.Sprintf("bulk-%04d", c),
			func(w io.Writer) error { return r.writeChunk(w, from, to) }, &res); err != nil {
			return fmt.Errorf("chunk %d: %w", c, err)
		}
		if got, want := res.Accepted, int64(to-from); got != want {
			r.fail("chunk %d accepted %d recipients, want %d (duplicates=%d invalid=%d)",
				c, got, want, res.Duplicates, res.Invalid)
		}
		total = res.Total
	}
	if total != int64(r.opt.recipients) {
		r.fail("the campaign holds %d recipients after ingest, want %d", total, r.opt.recipients)
	}
	r.logf("  ingested %d recipients in %d chunk(s) in %s",
		r.opt.recipients, chunks, time.Since(start).Round(time.Millisecond))
	return nil
}

func (r *runner) writeChunk(w io.Writer, from, to int) error {
	bw := bufio.NewWriterSize(w, 256<<10)
	for i := from; i < to; i++ {
		if _, err := fmt.Fprintf(bw, "{\"email\":%q,\"name\":\"E2E %d\",\"locale\":%q}\n",
			r.bulkEmail(i), i, bulkLocale(i)); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// pollBulk polls the campaign to completion, killing one sender replica on the
// way when --kill-sender is set (scenario 8).
func (r *runner) pollBulk(ctx context.Context, id string) (campaign, error) {
	start := time.Now()
	killed := false
	var last campaign
	for {
		var c campaign
		if err := r.api.getJSON(ctx, "/api/v1/campaigns/"+id, &c); err != nil {
			r.logf("  poll failed (retrying): %v", err)
		} else {
			last = c
			done := c.count("sent") + c.count("failed") + c.count("suppressed")
			r.logf("  status=%s sent=%d failed=%d in-flight=%d (%.1f%%)",
				c.Status, c.count("sent"), c.count("failed"), c.inFlight(),
				100*float64(done)/float64(r.opt.recipients))

			if !killed && r.opt.killSender && float64(done) >= r.opt.killAt*float64(r.opt.recipients) {
				killed = true
				if err := r.killAndRestartSender(ctx); err != nil {
					// A failed kill is worth an assertion, not an aborted run.
					r.fail("the sender kill/restart step failed: %v", err)
				}
			}
			if c.Status == "completed" && c.inFlight() == 0 {
				r.logf("  bulk campaign completed in %s", time.Since(start).Round(time.Millisecond))
				return c, nil
			}
			if c.Status == "cancelled" || c.Status == "paused" {
				return c, fmt.Errorf("the bulk campaign ended up %s", c.Status)
			}
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("budget exhausted with the bulk campaign still %s (sent=%d failed=%d in-flight=%d)",
				last.Status, last.count("sent"), last.count("failed"), last.inFlight())
		case <-time.After(r.opt.poll):
		}
	}
}

func (r *runner) assertFailedSample(ctx context.Context, campaignID string) {
	var list deliveryList
	if err := r.api.getJSON(ctx,
		"/api/v1/campaigns/"+campaignID+"/deliveries?status=failed&limit=100", &list); err != nil {
		r.fail("listing failed deliveries: %v", err)
		return
	}
	bad := 0
	for _, d := range list.Items {
		permanent := d.LastErrorClass == "permanent" || d.LastErrorClass == "policy"
		if !permanent && int(d.AttemptCount) != r.opt.maxAttempts {
			bad++
			if bad <= 3 {
				r.fail("failed delivery %s has attempt_count=%d and error class %q; "+
					"want max_attempts=%d or a permanent class",
					short(d.ID), d.AttemptCount, d.LastErrorClass, r.opt.maxAttempts)
			}
		}
	}
	r.logf("  checked %d failed deliveries, %d unexplained", len(list.Items), bad)
}
