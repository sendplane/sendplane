package main

import (
	"github.com/sendplane/sendplane/internal/chaossmtp"
)

// expectation is what the run must produce, derived rather than recorded.
//
// chaossmtp.Decide is a pure function of (seed, rates, recipient, attempt
// number) and the sender puts the attempt number in the X-Sendplane-Attempt
// header the server keys on, so replaying the sender's retry policy over the
// same recipients gives the exact final counts. Nothing is snapshotted:
// changing --recipients, the rates or max_attempts moves the expectation with
// it (internal/chaossmtp/README.md).
//
// The retry policy replayed here is internal/sender/policy.go:
//
//	attemptNo  = AttemptCount + 1, so the first send carries attempt 1
//	tempfail   451 4.3.0 "... try again later" -> rate_limited (classify.go
//	           rule ratelimit.4xx.text)   -> consumes an attempt
//	drop       connection closed mid-DATA -> transient (rule
//	           transient.connection)      -> consumes an attempt
//	permfail   550 5.2.0 "Mailbox unavailable" -> permanent (rule
//	           permanent.5xx)             -> fails immediately, consumes nothing
//	a consumed attempt reaching max_attempts fails the delivery
type expectation struct {
	Recipients  int   `json:"recipients"`
	MaxAttempts int   `json:"max_attempts"`
	Sent        int64 `json:"sent"`
	Failed      int64 `json:"failed"`
	// FailedPermanent and FailedExhausted split Failed by cause; their sum is
	// Failed and both are asserted through the failed-delivery sample.
	FailedPermanent int64 `json:"failed_permanent"`
	FailedExhausted int64 `json:"failed_exhausted"`
	// Attempts is every SMTP attempt, i.e. every DeliveryAttempt row a clean
	// run writes.
	Attempts int64 `json:"attempts"`
	// ChaosMessages is the attempts that reach end of DATA: a dropped
	// connection never gets there and chaossmtp counts it separately.
	ChaosMessages int64 `json:"chaos_messages"`
	TempFails     int64 `json:"chaos_tempfailed"`
	PermFails     int64 `json:"chaos_permfailed"`
	Drops         int64 `json:"chaos_dropped"`
}

// computeExpectation replays the decision function over every recipient.
func computeExpectation(
	seed uint64, rates chaossmtp.Rates, email func(i int) string, n, maxAttempts int,
) expectation {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	exp := expectation{Recipients: n, MaxAttempts: maxAttempts}
	for i := range n {
		rcpt := email(i)
		for attempt := 1; ; attempt++ {
			exp.Attempts++
			outcome := chaossmtp.Decide(seed, rates, rcpt, attempt)
			if outcome == chaossmtp.Drop {
				exp.Drops++
			} else {
				exp.ChaosMessages++
			}
			switch outcome {
			case chaossmtp.Accept:
				exp.Sent++
			case chaossmtp.PermFail:
				exp.PermFails++
				exp.Failed++
				exp.FailedPermanent++
			case chaossmtp.TempFail, chaossmtp.Drop:
				if outcome == chaossmtp.TempFail {
					exp.TempFails++
				}
				// policy.go: consumed == attempt, and consumed >= max fails
				// the delivery instead of deferring it again.
				if attempt < maxAttempts {
					continue
				}
				exp.Failed++
				exp.FailedExhausted++
			}
			break
		}
	}
	return exp
}
