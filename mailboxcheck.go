package sendplane

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/sendplane/sendplane/internal/control"
	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/internal/mbhealth"
	"github.com/sendplane/sendplane/store"
)

// The mailbox-check leader loop (architecture 11.5).
//
// Everything else that touches a mailbox does so as a side effect of other
// work: the bounce poller only runs where bounce polling is switched on, and
// the probe collector only looks at a probe mailbox while a run is pending -
// which, at the six-hourly default, is a few minutes out of every six hours.
// A password rotated in the provider's console in between would surface as
// "the last four probes did not arrive", six hours late and blamed on the
// sender.
//
// So one loop, on the leader, walks every enabled mailbox of every tenant and
// logs in. It is the cheapest thing in the control plane that can notice a
// credential going bad, and the only one that notices it for a deployment
// that does not poll bounces at all.

const (
	// defaultMailboxCheckInterval is both the loop's tick and its staleness
	// bound: a mailbox checked within one interval is skipped, so a mailbox
	// the poller or the collector just exercised costs nothing here.
	defaultMailboxCheckInterval = 15 * time.Minute
	// mailboxCheckPerTick bounds one tenant's tick. Each check is a real
	// login with its own timeout, so a tenant with a long list is covered
	// over several ticks rather than holding the loop for minutes. The
	// oldest check goes first, so nothing starves.
	mailboxCheckPerTick = 20
)

// mailboxCheckLoopOption registers the loop with the control leader. It is
// registered whether or not probing is enabled: bounce mailboxes need the
// same watch, and a deployment with no probe at all still has credentials
// that expire.
func (s *Sendplane) mailboxCheckLoopOption() control.Option {
	interval := s.opts.Probe.MailboxCheckInterval
	if interval <= 0 {
		interval = defaultMailboxCheckInterval
	}
	return control.WithLoop(control.Loop{
		Name: "mailbox-check", Interval: interval,
		// AllTenants: the whole point is to check a tenant that is doing
		// nothing. A tenant that is busy sending would be reached by
		// ActiveTenants, and that is not the one whose bounce mailbox has
		// been rejecting logins for a week.
		AllTenants: true,
		// IncludeSystem: the operator's shared probe and bounce mailboxes are
		// only visible in the system tenant's view, and their credentials
		// expire exactly like a tenant's (ADR-0017).
		IncludeSystem: true,
		NewTenant: func(st store.Store, _ string) control.TickLoop {
			return mailboxCheck{
				st:       st,
				secrets:  s.opts.Secrets,
				interval: interval,
				log:      s.opts.Logger,
			}
		},
	})
}

type mailboxCheck struct {
	st       store.Store
	secrets  SecretCipher
	interval time.Duration
	log      *slog.Logger
	// tester replaces mailbox.Test. Nil is the real one; only the loop's own
	// test sets it, so that asserting on the loop does not need an IMAP
	// server.
	tester func(ctx context.Context, cfg mailbox.Config, cipher SecretCipher) mailbox.TestResult
}

// Tick checks the enabled mailboxes whose last check is older than the
// interval. One mailbox's failure never stops the others: a tenant with a
// broken mailbox must still get the rest of them checked.
func (l mailboxCheck) Tick(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-l.interval)
	due := 0

	boxes, err := l.probeMailboxes(ctx)
	if err != nil {
		return err
	}
	for _, m := range boxes {
		if due >= mailboxCheckPerTick {
			break
		}
		if !l.dueAt(m.Health, cutoff) {
			continue
		}
		due++
		l.check(ctx, mbhealth.KindProbe,
			mbhealth.Mailbox{ID: m.ID, Name: m.Name, Health: m.Health},
			probeCheckConfig(&m), now)
	}

	bounces, err := l.st.BounceMailboxes().ListEnabled(ctx)
	if err != nil {
		return err
	}
	sortByCheckedAt(bounces, func(m *store.BounceMailbox) time.Time { return m.Health.CheckedAt })
	for _, m := range bounces {
		if due >= mailboxCheckPerTick {
			break
		}
		if !l.dueAt(m.Health, cutoff) {
			continue
		}
		due++
		l.check(ctx, mbhealth.KindBounce,
			mbhealth.Mailbox{ID: m.ID, Name: m.Name, Health: m.Health},
			bounceCheckConfig(&m), now)
	}
	return ctx.Err()
}

// dueAt reports whether a mailbox is worth logging in to again. A mailbox
// nothing has ever checked is always due.
func (l mailboxCheck) dueAt(h store.MailboxHealth, cutoff time.Time) bool {
	return h.CheckedAt.IsZero() || h.CheckedAt.Before(cutoff)
}

// check runs one login and records what it found. Neither the test nor the
// health write can fail the tick: a mailbox that is down is the answer, not an
// error, and a health write that failed will be retried next interval.
func (l mailboxCheck) check(
	ctx context.Context, kind mbhealth.Kind, m mbhealth.Mailbox,
	cfg mailbox.Config, now time.Time,
) {
	test := l.tester
	if test == nil {
		test = mailbox.Test
	}
	res := test(ctx, cfg, l.secrets)
	outcome := mbhealth.OK()
	if !res.OK {
		outcome = mbhealth.Fail(res.Stage, res.Error)
	}
	if _, err := mbhealth.Record(ctx, l.st, kind, m, outcome, now); err != nil {
		if ctx.Err() == nil {
			l.log.Error("sendplane: recording mailbox health failed",
				"kind", kind, "mailbox", m.ID, "err", err)
		}
		return
	}
	if !res.OK {
		l.log.Warn("sendplane: mailbox check failed",
			"kind", kind, "mailbox", m.ID, "stage", res.Stage, "err", res.Error)
	}
}

// probeMailboxes lists the tenant's enabled probe mailboxes, oldest check
// first. ProbeMailboxRepo has no ListEnabled - the probe trigger pages the
// whole list too - so this pages and filters.
func (l mailboxCheck) probeMailboxes(ctx context.Context) ([]store.ProbeMailbox, error) {
	var out []store.ProbeMailbox
	page := store.Page{Limit: store.MaxPageLimit}
	for {
		res, err := l.st.ProbeMailboxes().List(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, m := range res.Items {
			// A webhook mailbox has no credentials and no server to reach, so
			// there is nothing here to check: its health is written by probe
			// mail arriving, or failing to (ADR-0016).
			if m.Enabled && m.Kind.Normalized() != store.ProbeMailboxWebhook {
				out = append(out, m)
			}
		}
		if res.NextCursor == "" {
			break
		}
		page.Cursor = res.NextCursor
	}
	sortByCheckedAt(out, func(m *store.ProbeMailbox) time.Time { return m.Health.CheckedAt })
	return out, nil
}

// sortByCheckedAt puts the longest-unchecked mailbox first, so the per-tick
// cap cannot starve one.
func sortByCheckedAt[T any](rows []T, checkedAt func(*T) time.Time) {
	sort.SliceStable(rows, func(i, j int) bool {
		return checkedAt(&rows[i]).Before(checkedAt(&rows[j]))
	})
}

// probeCheckConfig is what the probe collector would dial this mailbox with,
// including the spam folder: a probe verdict depends on telling inbox from
// spam, so a spam folder that does not exist breaks the probe even though
// every credential is right (architecture 11.4).
func probeCheckConfig(m *store.ProbeMailbox) mailbox.Config {
	folders := probeFolders(m)
	return mailbox.Config{
		Protocol: mailbox.ProtocolIMAP,
		Host:     m.Host, Port: m.Port, TLS: m.TLS,
		Username: m.Username, Password: m.Password,
		Folder: folders[0], ExtraFolders: folders[1:],
	}
}

// bounceCheckConfig is what the poller would dial this mailbox with.
func bounceCheckConfig(m *store.BounceMailbox) mailbox.Config {
	protocol := mailbox.Protocol(m.Protocol)
	if protocol == "" {
		protocol = mailbox.ProtocolIMAP
	}
	return mailbox.Config{
		Protocol: protocol,
		Host:     m.Host, Port: m.Port, TLS: m.TLS,
		Username: m.Username, Password: m.Password,
		Folder: m.Folder,
	}
}
