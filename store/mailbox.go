package store

import "time"

// MailboxHealth is the last known reachability of a mailbox account, shared by
// ProbeMailbox and BounceMailbox (architecture 11.5).
//
// It is deliberately separate from the probe verdict (HealthStatus) and from
// the bounce events a poll produced: those answer "is mail getting through",
// this one answers "can sendplane still log in to this account". A password
// somebody rotated in the provider's console makes every probe look like a
// delivery failure unless the two are told apart (ADR-0015).
//
// It is written by the bounce poller, the probe collector, the mailbox-check
// leader loop and the manual test endpoints, all of them through
// UpdateHealth, which takes no part in optimistic concurrency: the writers are
// background loops, and a health observation must not lose a race with an
// operator editing the row.
type MailboxHealth struct {
	Status MailboxStatus
	// Stage is how far the last check got: dial, tls, auth, folder or ok. It
	// is what tells "the server is down" from "the password is wrong".
	Stage string
	// Reason is the error text of a failed check, empty when Status is ok.
	Reason string

	// CheckedAt is when the last check ran, LastOKAt when one last succeeded.
	// The mailbox-check loop reads CheckedAt to decide what is due.
	CheckedAt time.Time
	LastOKAt  time.Time

	// ConsecutiveFailures counts failed checks since the last success. It is
	// what lets a notification ride out a single blip.
	ConsecutiveFailures int
}

// Stages a mailbox check can fail at. They are strings, not an enum, because
// they are diagnostics shown to an operator rather than a state machine.
const (
	MailboxStageDial   = "dial"
	MailboxStageTLS    = "tls"
	MailboxStageAuth   = "auth"
	MailboxStageFolder = "folder"
	MailboxStageOK     = "ok"
	// MailboxStageWebhook is the stage of a webhook-kind probe mailbox
	// (ProbeMailboxWebhook): there is no login to fail, so the only thing that
	// can be observed is whether the provider is still forwarding probe mail
	// (ADR-0016).
	MailboxStageWebhook = "webhook"
)
