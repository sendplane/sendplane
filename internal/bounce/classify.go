package bounce

import (
	"regexp"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// The classification tables. They are deliberately short and data-like: a
// bounce mailbox sees an open set of MTAs, so the goal is to be right about
// the common shapes and to say "low confidence" about the rest, not to grow a
// rule per provider. The structured DSN/ARF path is what carries the weight;
// these only run when the mail has no report part at all.
var (
	// softStatusPrefixes are the 5.x.x codes that are about the message, the
	// receiving system or us, not about a dead address. Suppressing an address
	// because a provider blocked one message is worse than sending it again,
	// so these stay soft.
	softStatusPrefixes = []string{
		"5.2.2", // mailbox full
		"5.2.3", // message too large for mailbox
		"5.3.",  // mail system full / too big / conversion
		"5.4.",  // routing, network, delivery time expired
		"5.5.",  // protocol failure
		"5.7.",  // policy, reputation, authentication
	}

	// bounceSubjects are the subjects MTAs put on an unstructured failure
	// report.
	bounceSubjects = []string{
		"undeliverable", "undelivered mail returned to sender", "returned mail",
		"delivery status notification (failure)", "delivery status notification (delay)",
		"mail delivery failed", "failure notice", "delivery failure",
		"message could not be delivered", "delivery has failed",
		"mail could not be delivered", "returned to sender",
	}

	// hardPhrases say the address does not exist.
	hardPhrases = []string{
		"user unknown", "unknown user", "no such user", "no such recipient",
		"recipient address rejected", "does not exist", "doesn't exist",
		"mailbox unavailable", "mailbox not found", "address not found",
		"no mailbox here", "invalid recipient", "unrouteable address",
		"account has been disabled", "account is disabled", "user is terminated",
	}

	// softPhrases say try again.
	softPhrases = []string{
		"mailbox full", "over quota", "quota exceeded", "insufficient system storage",
		"try again later", "temporarily unavailable", "temporary failure",
		"greylist", "message too large", "deferred", "connection timed out",
	}

	// complaintSubjects are what a feedback loop sends when it does not use
	// the ARF media type.
	complaintSubjects = []string{
		"abuse report", "complaint about message", "spam report",
		"email feedback report",
	}

	// autoReplySubjects are the out-of-office shapes, including the Korean
	// ones a sendplane deployment will actually see.
	autoReplySubjects = []string{
		"out of office", "auto-reply", "autoreply", "automatic reply",
		"auto: ", "vacation", "away from the office", "away from my",
		"부재중", "자동회신", "자동 회신", "자동응답", "자동 응답",
	}
)

// enhancedStatusRe finds an RFC 3463 status code in free text, smtpCodeRe a
// bare reply code at the start of a quoted server response.
var (
	enhancedStatusRe = regexp.MustCompile(`\b([45])\.(\d{1,3})\.(\d{1,3})\b`)
	smtpCodeRe       = regexp.MustCompile(`(?m)(?:^|\s)([45]\d{2})(?:[ \-]|\b)`)
)

// classify fills Type and Confidence. The structured report wins; the
// heuristics only see a message that carried no report part.
func classify(p *Parsed, c *collected) {
	switch {
	case c.feedback != nil:
		classifyFeedback(p)
	case c.hasReport:
		classifyDSN(p)
	default:
		classifyHeuristic(p, c)
	}
}

func classifyFeedback(p *Parsed) {
	p.Confidence = ConfidenceHigh
	switch p.FeedbackType {
	case "abuse", "fraud":
		p.Type = store.BounceComplaint
	case "":
		// A feedback-report part with no type: it is a complaint feed, but
		// nothing says what kind.
		p.Type = store.BounceComplaint
		p.Confidence = ConfidenceMedium
	default:
		// not-spam, virus, other: recorded, but not something that should
		// suppress an address.
		p.Type = store.BounceUnknown
	}
}

func classifyDSN(p *Parsed) {
	p.Confidence = ConfidenceHigh
	switch p.Action {
	case "delayed":
		// A delay notice is a soft bounce whatever the status says.
		p.Type = store.BounceSoft
		return
	case "delivered", "relayed", "expanded":
		// A success DSN. Recorded by nothing: the delivery is already sent.
		p.Type = store.BounceUnknown
		return
	}
	if p.Status == "" {
		p.Type = statusFromText(p.DiagnosticCode)
		p.Confidence = ConfidenceMedium
		return
	}
	p.Type = typeFromStatus(p.Status)
	if p.Type == store.BounceUnknown {
		p.Confidence = ConfidenceMedium
	}
}

// typeFromStatus maps an RFC 3463 status code onto a bounce type.
func typeFromStatus(status string) store.BounceType {
	status = strings.TrimSpace(status)
	if len(status) < 3 || status[1] != '.' {
		return store.BounceUnknown
	}
	switch status[0] {
	case '4':
		return store.BounceSoft
	case '5':
		for _, prefix := range softStatusPrefixes {
			if strings.HasPrefix(status, prefix) {
				return store.BounceSoft
			}
		}
		return store.BounceHard
	default:
		return store.BounceUnknown
	}
}

// statusFromText digs a status or reply code out of free text.
func statusFromText(text string) store.BounceType {
	if m := enhancedStatusRe.FindString(text); m != "" {
		return typeFromStatus(m)
	}
	if m := smtpCodeRe.FindStringSubmatch(text); m != nil {
		if strings.HasPrefix(m[1], "4") {
			return store.BounceSoft
		}
		return store.BounceHard
	}
	return store.BounceUnknown
}

// classifyHeuristic is the fallback of architecture 10 for mail that is not a
// DSN: subject and body patterns, marked as low confidence unless a status
// code was found in the text.
func classifyHeuristic(p *Parsed, c *collected) {
	subject := strings.ToLower(p.Subject)
	body := strings.ToLower(c.text)

	if isAutoReply(p, c, subject) {
		p.AutoReply = true
		p.Type = store.BounceUnknown
		p.Confidence = ConfidenceHigh
		return
	}

	if containsAny(subject, complaintSubjects) {
		p.Type = store.BounceComplaint
		p.Confidence = ConfidenceLow
		return
	}

	looksLikeReport := containsAny(subject, bounceSubjects) ||
		strings.Contains(strings.ToLower(c.top.Get("From")), "mailer-daemon") ||
		strings.Contains(strings.ToLower(c.top.Get("From")), "postmaster")
	if !looksLikeReport {
		p.Type = store.BounceUnknown
		p.Confidence = ConfidenceLow
		return
	}

	// It says it is a failure report; decide how bad from the text.
	if t := statusFromText(c.text); t != store.BounceUnknown {
		if p.Status == "" {
			p.Status = enhancedStatusRe.FindString(c.text)
		}
		// A quoted server response is real evidence, just not a structured
		// one.
		p.Type = t
		p.Confidence = ConfidenceMedium
		if containsAny(body, softPhrases) && t == store.BounceHard &&
			!containsAny(body, hardPhrases) {
			p.Type = store.BounceSoft
		}
		return
	}
	switch {
	case containsAny(body, hardPhrases):
		p.Type = store.BounceHard
	case containsAny(body, softPhrases):
		p.Type = store.BounceSoft
	default:
		p.Type = store.BounceUnknown
	}
	p.Confidence = ConfidenceLow
}

// isAutoReply detects vacation responders. Auto-Submitted alone is not enough:
// RFC 3834 has DSNs carry it too, which is why this only runs for mail that
// had no report part.
func isAutoReply(_ *Parsed, c *collected, subject string) bool {
	auto := strings.ToLower(strings.TrimSpace(c.top.Get("Auto-Submitted")))
	if auto != "" && auto != "no" {
		return true
	}
	// X-Auto-Response-Suppress is deliberately not in this list: it is set by
	// senders to ask for silence, not by responders.
	for _, h := range []string{"X-Autoreply", "X-Autorespond", "X-Vacation"} {
		if c.top.Get(h) != "" {
			return true
		}
	}
	if strings.Contains(strings.ToLower(c.top.Get("Precedence")), "auto_reply") {
		return true
	}
	return containsAny(subject, autoReplySubjects)
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
