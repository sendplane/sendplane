package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Scenario 6: the loopback sending health check of architecture 11.
//
// What the probe must end up saying is decided by what GreenMail does and does
// not do. It delivers the mail to the probe mailbox's INBOX, so `delivered`
// and `folder=inbox` are right; it adds no `Authentication-Results` header, so
// internal/probe/verdict.go takes the `!o.TrustedAR` branch and the verdict is
// **yellow** with the reason below. Anything else — green in particular —
// would mean a verdict was reached without evidence, which is what ADR-0012
// exists to prevent.
//
// The scenario runs after the bulk campaign on purpose. A probe triggered
// while other traffic is in flight can be collected by accident (see BUG-2 in
// README.md): the collect loop only ticks for tenants in
// Provider.ActiveTenants, so whether a probe is ever judged currently depends
// on whether the tenant happens to have unrelated work queued at the moment
// the loop fires. Triggering it when the tenant is idle is the honest test.
const probeNoARReason = "신뢰할 수 있는 Authentication-Results 헤더가 없습니다"

func (r *runner) scenarioProbe(ctx context.Context) error {
	if err := r.triggerProbe(ctx); err != nil {
		return err
	}
	return r.collectProbe(ctx)
}

func (r *runner) triggerProbe(ctx context.Context) error {
	var res probeTriggerResult
	if err := r.api.postJSON(ctx, "/api/v1/senders/"+r.senderMailID+"/probe", map[string]any{}, &res); err != nil {
		if statusOf(err) == 501 {
			return fmt.Errorf("POST /senders/{id}/probe answered 501: probe.enabled is off in test/e2e/config.yaml: %w", err)
		}
		return fmt.Errorf("POST /senders/{id}/probe: %w", err)
	}
	if len(res.Runs) != 1 {
		return fmt.Errorf("the probe trigger created %d runs, want 1 (one probe mailbox is registered)", len(res.Runs))
	}
	r.probeRunID = res.Runs[0].RunID
	r.probeMailboxID = res.Runs[0].MailboxID
	r.logf("  probe run %s enqueued for sender B", short(r.probeRunID))

	// The probe goes out through the normal sender path (ADR-0012), so the
	// mail itself should be in the probe mailbox within seconds. This half
	// never depends on the collect loop.
	msgs, err := r.gm.waitForMessage(ctx, probeAddress, 1, r.opt.mailTimeout)
	if err != nil {
		return fmt.Errorf("the probe mail never reached %s: %w", probeAddress, err)
	}
	if tok := msgs[0].header("X-Sendplane-Probe"); tok == "" {
		r.fail("the probe mail carries no X-Sendplane-Probe header; Collect's header search cannot find it")
	}
	if subj := msgs[0].decodedSubject(); !strings.Contains(subj, r.probeRunID) {
		r.fail("the probe mail subject is %q, want it to carry run id %s", subj, r.probeRunID)
	}
	r.logf("  probe mail arrived in %s; waiting for the collect loop", probeAddress)
	return nil
}

func (r *runner) collectProbe(ctx context.Context) error {
	deadline := time.Now().Add(r.opt.probeTimeout)
	var run probeRun
	for {
		var got probeRun
		if err := r.api.getJSON(ctx, "/api/v1/probe-runs/"+r.probeRunID, &got); err != nil {
			r.logf("  reading probe run failed (retrying): %v", err)
		} else if !got.Pending {
			run = got
			break
		}
		if time.Now().After(deadline) {
			r.knownFail("BUG-2",
				"probe run %s is still pending after %s: the collect loop never ran for this tenant, "+
					"so the loopback health check of architecture 11 never finishes for an idle tenant",
				short(r.probeRunID), r.opt.probeTimeout)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	r.probeCollected = true

	r.logf("  probe run: status=%s delivered=%v folder=%s latency=%s reason=%q",
		run.Status, run.Delivered, run.Folder, run.Latency, run.Reason)

	if !run.Delivered {
		r.fail("the probe run reports delivered=false although the mail was in %s", probeAddress)
	}
	if run.Folder != "inbox" {
		r.fail("the probe run reports folder=%q, want inbox", run.Folder)
	}
	if run.Status != "yellow" {
		r.fail("the probe verdict is %q; GreenMail adds no Authentication-Results, so internal/probe/verdict.go "+
			"must answer yellow", run.Status)
	}
	if run.Reason != probeNoARReason {
		r.fail("the probe reason is %q, want %q", run.Reason, probeNoARReason)
	}
	if run.ReceivedAt == nil {
		r.fail("the probe run has no received_at")
	}

	// The same verdict has to reach the sender's health summary, which is what
	// the console shows (architecture 11.4).
	var health senderHealth
	if err := r.api.getJSON(ctx, "/api/v1/senders/"+r.senderMailID+"/health", &health); err != nil {
		return fmt.Errorf("GET /senders/{id}/health: %w", err)
	}
	if health.Status != "yellow" {
		r.fail("sender health is %q, want yellow (worst of one yellow probe run)", health.Status)
	}
	found := false
	for _, m := range health.Mailboxes {
		if m.MailboxID == r.probeMailboxID {
			found = true
			if m.ID != run.ID {
				r.fail("sender health reports probe run %s for the probe mailbox, want the latest %s",
					short(m.ID), short(run.ID))
			}
		}
	}
	if !found {
		r.fail("sender health lists no run for probe mailbox %s", short(r.probeMailboxID))
	}
	r.logf("  sender health: %s (%s)", health.Status, health.Reason)
	return nil
}
