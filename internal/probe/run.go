// Package probe is the loopback sending health check (docs/architecture.md
// 11, ADR-0012).
//
// It sends a real mail through the real sender to a real mailbox and reads
// the receiving MTA's own verdict back out of the headers. DNS records are
// checked alongside (internal/dnscheck) but only to explain a failure: a
// record can be perfect while the mail still fails SPF alignment, arrives
// unencrypted or lands in spam.
//
// The package does not send anything. Trigger enqueues a lane=probe Delivery
// and the ordinary sender pipeline carries it, which is the whole point —
// a probe that took a different path would not test the path that matters.
package probe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/dnscheck"
	"github.com/sendplane/sendplane/internal/mbhealth"
	"github.com/sendplane/sendplane/store"
)

// HeaderProbe correlates a probe mail with its ProbeRun. The sender does not
// set it yet (see README "sender에 필요한 한 줄"); until it does, the run ID in
// the subject is the search key.
const HeaderProbe = "X-Sendplane-Probe"

// EventSenderHealthChanged is the outbox event type of architecture 12.
const EventSenderHealthChanged = "sender.health_changed"

// ProbeTemplateID is the marker TemplateID of the built-in probe
// MessageVersion. It is not a real template: probes are not editable content,
// and giving them one would put a row in the operator's template list that
// they must not change. A version is looked up by this marker plus
// probeChecksum and created on first use, per tenant.
const ProbeTemplateID = "_sendplane_probe"

// probeChecksum versions the built-in templates below. Bump it and the next
// Trigger creates a new MessageVersion instead of reusing the old one; running
// probes keep the version they were queued with, like any other campaign
// (architecture 6.3).
const probeChecksum = "sendplane-probe-v1"

// Default timings (ADR-0012).
const (
	DefaultInterval                  = 6 * time.Hour
	DefaultTimeout                   = 15 * time.Minute
	DefaultConsecutiveFailuresForRed = 2
)

// maxRawHeaders caps what is kept for diagnosis on a ProbeRun.
const maxRawHeaders = 16 << 10

// Errors.
var (
	// ErrNoMailbox is returned by Trigger when the tenant has no enabled probe
	// mailbox and no DNS checker is configured: there is nothing to run.
	ErrNoMailbox = errors.New("probe: no enabled probe mailbox")
)

// RawMessage is one fetched probe mail. Raw only has to carry the header
// block: nothing in this package reads the body.
type RawMessage struct {
	// ID is whatever the mailbox uses to address the message in Delete
	// (an IMAP UID as a string, a POP3 UIDL).
	ID string
	// Folder is the mailbox folder it was found in, in the mailbox's own
	// naming ("INBOX", "[Gmail]/Spam"); folderKind normalizes it.
	Folder string
	Raw    []byte
	// ReceivedAt is the mailbox's own arrival timestamp (IMAP INTERNALDATE).
	ReceivedAt time.Time
}

// MailboxFetcher is the narrow slice of an IMAP/POP3 client this package
// needs. internal/mailbox owns the real client; the root adapts it (README).
type MailboxFetcher interface {
	FetchByHeader(ctx context.Context, name, value string) ([]RawMessage, error)
	Delete(ctx context.Context, ids []string) error
}

// MailboxOpener connects to one probe mailbox. A probe tenant normally has
// several (Gmail, Outlook, own MTA) and each is a separate account, so Collect
// asks for one connection per mailbox and closes it (when the fetcher is an
// io.Closer) as soon as that mailbox is done.
type MailboxOpener interface {
	Open(ctx context.Context, m *store.ProbeMailbox) (MailboxFetcher, error)
}

// Options configures a Runner.
type Options struct {
	// Interval is how stale a Sender's health may get before Tick re-probes
	// it. Default DefaultInterval.
	Interval time.Duration
	// Timeout is how long a probe mail may take to arrive before the run is
	// called undelivered. Default DefaultTimeout.
	Timeout time.Duration
	// ConsecutiveFailuresForRed is how many undelivered runs in a row a
	// mailbox needs before the verdict goes red rather than yellow. A
	// provider's greylisting produces one, which is why ADR-0012 asks for
	// two. Default DefaultConsecutiveFailuresForRed.
	ConsecutiveFailuresForRed int
	// HMACKey signs the run ID in the probe header, so that a mail someone
	// else put in the mailbox cannot be read as a probe result. Empty leaves
	// the token unsigned.
	HMACKey []byte
	// DNS is the diagnostic layer. Nil skips it, which is what a deployment
	// with no outbound DNS does.
	DNS *dnscheck.Checker
	// Secrets decrypts SendingDomain.DKIMPrivateKey for the DKIM key
	// comparison. Nil skips the comparison.
	Secrets host.SecretCipher
	Clock   func() time.Time
	Logger  *slog.Logger
}

// Runner owns no state beyond its configuration: every call takes the store it
// works on, so one Runner serves every tenant.
type Runner struct {
	opts Options
}

// New returns a Runner with defaults filled in.
func New(opts Options) *Runner {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.ConsecutiveFailuresForRed <= 0 {
		opts.ConsecutiveFailuresForRed = DefaultConsecutiveFailuresForRed
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Runner{opts: opts}
}

// Subject is the probe mail's subject. It doubles as the fallback search key,
// which is why the run ID is in it verbatim (architecture 11.2).
func Subject(runID string) string { return "[sendplane probe " + runID + "]" }

// Token is the X-Sendplane-Probe value: "<tenant>/<run>" plus a short HMAC
// over both. The IDs alone would be enough to find the mail; the HMAC is what
// makes a forged probe mail unable to produce a green verdict.
//
// The tenant travels in the token for the same reason it travels in a tracking
// token (internal/tracking.TokenPayload): the inbound webhook of ADR-0016 is
// one global, unauthenticated endpoint, and resolving the tenant has to be one
// Provider.ForTenant call rather than a sweep over every tenant's pending runs.
func (r *Runner) Token(tenantID, runID string) string {
	body := tenantID + "/" + runID
	if len(r.opts.HMACKey) == 0 {
		return body
	}
	mac := hmac.New(sha256.New, r.opts.HMACKey)
	mac.Write([]byte(tenantID))
	mac.Write([]byte{0})
	mac.Write([]byte(runID))
	return body + "/" + hex.EncodeToString(mac.Sum(nil))[:16]
}

// ParseToken splits a token into the tenant and run it names, *without*
// checking the MAC — like internal/tracking.TenantOf. The caller loads that
// tenant's store, reads the run and only then calls VerifyToken, because the
// key the MAC is checked against is process-wide but the run is not.
//
// The MAC is the last segment and a run ID never contains "/", so the split is
// from the right: a tenant ID with a "/" in it still parses.
func ParseToken(token string) (tenantID, runID string, ok bool) {
	parts := strings.Split(token, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	// Two segments is the unsigned form, three or more the signed one.
	if len(parts) > 2 {
		parts = parts[:len(parts)-1]
	}
	runID = parts[len(parts)-1]
	tenantID = strings.Join(parts[:len(parts)-1], "/")
	if tenantID == "" || runID == "" {
		return "", "", false
	}
	return tenantID, runID, true
}

// VerifyToken reports whether a token found on a mail belongs to this run.
func (r *Runner) VerifyToken(tenantID, runID, token string) bool {
	return hmac.Equal([]byte(token), []byte(r.Token(tenantID, runID)))
}

// Trigger starts one probe run for a sender: one lane=probe Delivery and one
// pending ProbeRun per enabled probe mailbox. It is what POST
// /senders/{id}/probe calls.
//
// The returned run ID is the first mailbox's ProbeRun. Every run of one
// trigger shares a GroupID, so a console can ask for "the result of this
// trigger" without guessing from StartedAt.
//
// With no probe mailbox configured, and a DNS checker available, it falls back
// to a DNS-only run (ADR-0012: "없으면 DNS 검사만 수행하고 상태에 loopback
// 미구성을 표시").
func (r *Runner) Trigger(ctx context.Context, st store.Store, senderID string) (string, error) {
	now := store.TruncateTime(r.opts.Clock())

	snd, err := st.Senders().Get(ctx, senderID)
	if err != nil {
		return "", err
	}
	boxes, err := enabledMailboxes(ctx, st)
	if err != nil {
		return "", err
	}
	if len(boxes) == 0 {
		if r.opts.DNS == nil {
			return "", ErrNoMailbox
		}
		return r.dnsOnlyRun(ctx, st, snd, now)
	}

	versionID, err := r.probeVersion(ctx, st)
	if err != nil {
		return "", err
	}

	groupID := store.NewID()
	first := ""
	for _, box := range boxes {
		runID := store.NewID()
		deliveryID := store.NewID()

		emailNorm, err := store.NormalizeEmail(box.Address)
		if err != nil {
			r.opts.Logger.Error("sendplane: probe mailbox address is not an email",
				"mailbox", box.ID, "address", box.Address, "err", err)
			continue
		}
		d := store.Delivery{
			ID:        deliveryID,
			VersionID: versionID,
			SenderID:  senderID,
			Lane:      store.LaneProbe,
			Status:    store.DeliveryQueued,
			Email:     box.Address,
			EmailNorm: emailNorm,
			Vars: map[string]any{
				"run_id":      runID,
				"probe_token": r.Token(snd.TenantID, runID),
				"mailbox":     box.Name,
			},
			NextAttemptAt: now,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if _, err := st.Deliveries().InsertBatch(ctx, []store.Delivery{d}); err != nil {
			return "", fmt.Errorf("probe: enqueue delivery: %w", err)
		}

		// The run row is written after the delivery, so a failure in between
		// leaves an unmatched probe mail rather than a run that can only ever
		// time out.
		run := &store.ProbeRun{
			ID:         runID,
			SenderID:   senderID,
			MailboxID:  box.ID,
			DeliveryID: deliveryID,
			GroupID:    groupID,
			Pending:    true,
			Status:     store.HealthUnknown,
			StartedAt:  now,
			CreatedAt:  now,
		}
		if err := st.ProbeRuns().Create(ctx, run); err != nil {
			return "", fmt.Errorf("probe: create run: %w", err)
		}
		if first == "" {
			first = runID
		}
	}
	if first == "" {
		return "", ErrNoMailbox
	}
	return first, nil
}

// Tick is the periodic path of architecture 11.2, driven by the control
// leader loop. It triggers every sender whose health has never been checked or
// is older than Interval, and every sender whose transport or sending domain
// changed since the last check.
func (r *Runner) Tick(ctx context.Context, st store.Store, now time.Time) error {
	senders, err := listAll(ctx, st.Senders().List)
	if err != nil {
		return err
	}
	if len(senders) == 0 {
		return nil
	}

	changed, err := r.changedAfter(ctx, st)
	if err != nil {
		return err
	}
	// One read of the pending set for the whole tick, rather than one walk of
	// each sender's history per sender.
	waiting, err := pendingRuns(ctx, st)
	if err != nil {
		return err
	}
	inFlight := map[string]bool{}
	for i := range waiting {
		if now.Sub(waiting[i].StartedAt) < r.opts.Timeout {
			inFlight[waiting[i].SenderID] = true
		}
	}

	var firstErr error
	for i := range senders {
		s := &senders[i]
		due, why := r.due(s, changed, now)
		if !due {
			continue
		}
		if inFlight[s.ID] {
			// A run is still within its timeout window. Queuing another would
			// double the mail and make "consecutive failures" meaningless.
			continue
		}
		if _, err := r.Trigger(ctx, st, s.ID); err != nil {
			if errors.Is(err, ErrNoMailbox) {
				continue
			}
			firstErr = cmpErr(firstErr, fmt.Errorf("probe: sender %s: %w", s.ID, err))
			continue
		}
		r.opts.Logger.Info("sendplane: probe triggered", "sender", s.ID, "reason", why)
	}
	return firstErr
}

// due decides whether a sender needs a probe now.
func (r *Runner) due(s *store.Sender, changed map[string]time.Time, now time.Time) (bool, string) {
	if s.HealthCheckedAt.IsZero() {
		return true, "never checked"
	}
	if at, ok := changed[s.TransportID]; ok && at.After(s.HealthCheckedAt) {
		return true, "transport changed"
	}
	if at, ok := changed[s.DomainID]; ok && at.After(s.HealthCheckedAt) {
		return true, "domain changed"
	}
	if now.Sub(s.HealthCheckedAt) >= r.opts.Interval {
		return true, "interval elapsed"
	}
	return false, ""
}

// changedAfter maps transport and domain IDs to their UpdatedAt. Neither model
// has an explicit "configuration changed" signal, so UpdatedAt is the signal:
// it moves when a transport's host or a domain's DKIM key is edited, which are
// exactly the edits that invalidate a health verdict. It also moves when the
// sender marks a transport unhealthy, which re-probes one interval early —
// harmless, and arguably right.
func (r *Runner) changedAfter(ctx context.Context, st store.Store) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	transports, err := listAll(ctx, st.Transports().List)
	if err != nil {
		return nil, err
	}
	for _, t := range transports {
		out[t.ID] = t.UpdatedAt
	}
	domains, err := listAll(ctx, st.Domains().List)
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		out[d.ID] = d.UpdatedAt
	}
	return out, nil
}

// Collect is the second half of the loop: it reads the probe mailboxes, turns
// what it finds into verdicts, and updates the senders' health. fetcher is
// used for every pending mailbox; use CollectWith when each mailbox needs its
// own connection, which is the normal deployment.
func (r *Runner) Collect(ctx context.Context, st store.Store, fetcher MailboxFetcher, now time.Time) error {
	return r.CollectWith(ctx, st, singleOpener{fetcher}, now)
}

// CollectWith is Collect with one connection per probe mailbox.
func (r *Runner) CollectWith(ctx context.Context, st store.Store, opener MailboxOpener, now time.Time) error {
	// Only the runs still waiting are read: paging every sender's whole
	// history to find them cost the length of the history, which retention
	// only bounds eventually (store.ProbeRunRepo.ListPending).
	waiting, err := pendingRuns(ctx, st)
	if err != nil {
		return err
	}
	if len(waiting) == 0 {
		return nil
	}
	boxes, err := listAll(ctx, st.ProbeMailboxes().List)
	if err != nil {
		return err
	}
	byID := make(map[string]*store.ProbeMailbox, len(boxes))
	for i := range boxes {
		byID[boxes[i].ID] = &boxes[i]
	}

	// Runs are collected per mailbox so that one connection serves every
	// sender's pending run in that mailbox.
	all := map[string][]*store.ProbeRun{}
	for i := range waiting {
		if waiting[i].MailboxID == "" {
			continue // a DNS-only run waits for nothing
		}
		all[waiting[i].MailboxID] = append(all[waiting[i].MailboxID], &waiting[i])
	}
	if len(all) == 0 {
		return nil
	}

	var firstErr error
	touched := map[string]bool{}
	for mailboxID, pending := range all {
		box := byID[mailboxID]
		if box == nil {
			// The mailbox was deleted while a run was in flight. Nothing can
			// ever arrive for it; let the timeout close the run out.
			box = &store.ProbeMailbox{ID: mailboxID}
		}
		if err := r.collectMailbox(ctx, st, opener, box, pending, now, touched); err != nil {
			firstErr = cmpErr(firstErr, err)
		}
	}

	// The summary is the worst of the newest finished run per mailbox, so it
	// needs the history of the senders that just changed - and only those.
	for senderID := range touched {
		runs, err := r.runsOf(ctx, st, senderID)
		if err != nil {
			firstErr = cmpErr(firstErr, err)
			continue
		}
		if err := r.updateSenderHealth(ctx, st, senderID, runs, now); err != nil {
			firstErr = cmpErr(firstErr, err)
		}
	}
	return firstErr
}

// collectMailbox handles every pending run of one mailbox over one connection.
func (r *Runner) collectMailbox(
	ctx context.Context, st store.Store, opener MailboxOpener,
	box *store.ProbeMailbox, pending []*store.ProbeRun, now time.Time,
	touched map[string]bool,
) error {
	if box.Kind.Normalized() == store.ProbeMailboxWebhook {
		return r.collectWebhook(ctx, st, box, pending, now, touched)
	}

	var fetcher MailboxFetcher
	var openErr error
	// A mailbox row that was deleted while a run was in flight is left alone:
	// there is nothing to connect to and nothing to record health on.
	attempted := box.Enabled || box.Host != "" || box.Address != ""
	if attempted {
		fetcher, openErr = opener.Open(ctx, box)
		if fetcher != nil {
			if c, ok := fetcher.(io.Closer); ok {
				defer func() { _ = c.Close() }()
			}
		}
	}

	var firstErr error
	if openErr != nil {
		firstErr = fmt.Errorf("probe: mailbox %s: %w", box.ID, openErr)
	}
	if attempted {
		r.recordMailboxHealth(ctx, st, box, openErr, now)
	}

	for _, run := range pending {
		var msg *RawMessage
		if fetcher != nil {
			got, err := r.find(ctx, fetcher, run.TenantID, run.ID)
			if err != nil {
				firstErr = cmpErr(firstErr, fmt.Errorf("probe: mailbox %s: %w", box.ID, err))
			}
			msg = got
		}
		switch {
		case msg != nil:
			if err := r.CompleteRun(ctx, st, run, evidenceOf(box, msg), now); err != nil {
				firstErr = cmpErr(firstErr, err)
				continue
			}
			touched[run.SenderID] = true
			if err := fetcher.Delete(ctx, []string{msg.ID}); err != nil {
				// The verdict is already saved; a probe mail left behind is
				// clutter, not a wrong answer.
				r.opts.Logger.Warn("sendplane: cannot delete probe mail",
					"mailbox", box.ID, "run", run.ID, "err", err)
			}
		case now.Sub(run.StartedAt) >= r.opts.Timeout:
			// A mailbox that could not be opened proves nothing about the
			// sender: the mail may well have arrived and simply not been
			// looked at. Closing such a run out as "not delivered" is how a
			// rotated IMAP password turns into a red sender and a wild goose
			// chase through DNS (ADR-0015).
			var err error
			if openErr != nil {
				err = r.finishUnreachable(ctx, st, run, mailboxStage(openErr), now)
			} else {
				err = r.finishUndelivered(ctx, st, run, "메일이", now)
			}
			if err != nil {
				firstErr = cmpErr(firstErr, err)
				continue
			}
			touched[run.SenderID] = true
		}
	}
	return firstErr
}

// collectWebhook is collectMailbox for a webhook-kind mailbox (ADR-0016).
// There is nothing to open and nothing to fetch: the inbound endpoint
// completes a run the moment the provider posts the mail. All this loop does
// is close out the runs whose probe never arrived, which is the only way a
// forward that was silently switched off ever becomes visible.
func (r *Runner) collectWebhook(
	ctx context.Context, st store.Store,
	box *store.ProbeMailbox, pending []*store.ProbeRun, now time.Time,
	touched map[string]bool,
) error {
	var firstErr error
	timedOut := false
	for _, run := range pending {
		if now.Sub(run.StartedAt) < r.opts.Timeout {
			continue
		}
		if err := r.finishUndelivered(ctx, st, run, "웹훅으로 프로브가", now); err != nil {
			firstErr = cmpErr(firstErr, err)
			continue
		}
		timedOut = true
		touched[run.SenderID] = true
	}
	if timedOut && box.ID != "" {
		// A webhook mailbox has no login to check, so the mailbox-check loop
		// skips it and a missed probe is the only evidence there is that the
		// provider is no longer forwarding. Recording it as a mailbox error
		// puts a broken forward next to a rotated IMAP password in the
		// console instead of only inside a probe verdict (architecture 11.5).
		r.recordWebhookHealth(ctx, st, box,
			mbhealth.Fail(store.MailboxStageWebhook, "no probe received within timeout"), now)
	}
	return firstErr
}

// recordWebhookHealth files one observation about a webhook mailbox. It is
// logged rather than returned for the same reason recordMailboxHealth is: the
// verdicts matter more than the bookkeeping around them.
func (r *Runner) recordWebhookHealth(
	ctx context.Context, st store.Store, box *store.ProbeMailbox,
	outcome mbhealth.Outcome, now time.Time,
) {
	m := mbhealth.Mailbox{ID: box.ID, Name: box.Name, Health: box.Health}
	if _, err := mbhealth.Record(ctx, st, mbhealth.KindProbe, m, outcome, now); err != nil {
		r.opts.Logger.Warn("sendplane: recording probe mailbox health failed",
			"mailbox", box.ID, "err", err)
	}
}

// stager is implemented by an Open error that knows how far it got
// (internal/mailbox.DialError). This package deliberately does not import
// internal/mailbox, so it reads the stage through this one-method interface
// instead.
type stager interface{ MailboxStage() string }

// mailboxStage is the stage an Open failure reached, defaulting to dial for an
// opener that does not classify its errors.
func mailboxStage(err error) string {
	var s stager
	if errors.As(err, &s) {
		if stage := s.MailboxStage(); stage != "" {
			return stage
		}
	}
	return store.MailboxStageDial
}

// recordMailboxHealth files the outcome of one Open as the mailbox's
// reachability. The probe collector runs every minute, so it is the fastest
// thing in the system to notice a mailbox going away - but only while a run
// is pending, which is why the mailbox-check loop exists as well.
//
// A failed health write is logged, never returned: the verdicts this collect
// is producing matter more than the bookkeeping around them.
func (r *Runner) recordMailboxHealth(
	ctx context.Context, st store.Store, box *store.ProbeMailbox, openErr error, now time.Time,
) {
	outcome := mbhealth.OK()
	if openErr != nil {
		outcome = mbhealth.Fail(mailboxStage(openErr), openErr.Error())
	}
	m := mbhealth.Mailbox{ID: box.ID, Name: box.Name, Health: box.Health}
	if _, err := mbhealth.Record(ctx, st, mbhealth.KindProbe, m, outcome, now); err != nil {
		r.opts.Logger.Warn("sendplane: recording probe mailbox health failed",
			"mailbox", box.ID, "err", err)
	}
}

// finishUnreachable closes out a run whose mailbox could not be opened. The
// verdict is unknown rather than red: nothing was observed, so there is
// nothing to blame the sender for. The reason names the stage, because
// "auth" and "dial" have different owners.
func (r *Runner) finishUnreachable(
	ctx context.Context, st store.Store, run *store.ProbeRun, stage string, now time.Time,
) error {
	run.Status = store.HealthUnknown
	run.Reason = "probe mailbox unreachable: " + stage
	run.Delivered = false
	run.ReceivedAt = time.Time{}
	run.Pending = false
	return updateRun(ctx, st, run)
}

// find looks the probe mail up by its header and falls back to the subject.
// The fallback is not a nicety today: the sender does not yet put
// X-Sendplane-Probe on the mail (README).
func (r *Runner) find(ctx context.Context, f MailboxFetcher, tenantID, runID string) (*RawMessage, error) {
	msgs, err := f.FetchByHeader(ctx, HeaderProbe, r.Token(tenantID, runID))
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		msgs, err = f.FetchByHeader(ctx, "Subject", Subject(runID))
		if err != nil {
			return nil, err
		}
	}
	for i := range msgs {
		h := ParseHeaders(msgs[i].Raw)
		if tok := h.Get(HeaderProbe); tok != "" && !r.VerifyToken(tenantID, runID, tok) {
			// Someone put a mail with our header in the mailbox. It is not a
			// probe result.
			continue
		}
		if tok := h.Get(HeaderProbe); tok == "" && !strings.Contains(h.Get("Subject"), runID) {
			continue
		}
		return &msgs[i], nil
	}
	return nil, nil
}

// Evidence is one probe mail as the channel it arrived over saw it. The IMAP
// collector fills it from the raw message it fetched, the inbound webhook from
// the provider's JSON (ADR-0016); from CompleteRun on, the two are the same
// code, which is the point of the type.
type Evidence struct {
	// Mailbox is the probe mailbox the mail was addressed to. It carries the
	// authserv-id the Authentication-Results header is trusted against and the
	// folder mapping. Nil for a mailbox that was deleted mid-flight.
	Mailbox *store.ProbeMailbox

	// Headers is the message's header block. ParseHeaders builds it from a raw
	// message, HeadersFromMap from a provider's header map.
	Headers Headers

	// Folder is the folder the mail was filed in, in the mailbox's own naming
	// ("INBOX", "[Gmail]/Spam"). Empty means the channel cannot see one — a
	// webhook is told about a delivery, not about where the recipient's client
	// later filed it — and an unknown folder never downgrades a verdict
	// (verdict.go).
	Folder string

	// ReceivedAt is the channel's own arrival timestamp (an IMAP INTERNALDATE,
	// a webhook's `date`). Zero falls back to the newest Received header.
	ReceivedAt time.Time

	// RawHeaders is what is kept on the run for diagnosis, already capped.
	RawHeaders string
}

// evidenceOf is the IMAP channel's Evidence.
func evidenceOf(box *store.ProbeMailbox, msg *RawMessage) Evidence {
	return Evidence{
		Mailbox:    box,
		Headers:    ParseHeaders(msg.Raw),
		Folder:     msg.Folder,
		ReceivedAt: msg.ReceivedAt,
		RawHeaders: rawHeaders(msg.Raw),
	}
}

// CompleteRun turns a received probe mail into a verdict and saves the run. It
// is the shared half of the two inbound channels: whatever brought the mail
// back, the verdict of architecture 11.4, the DNS diagnostic layer and the
// ProbeRun columns are decided here and nowhere else.
//
// It does *not* touch the sender's summary health. CollectWith finishes every
// mailbox first and refreshes each affected sender once, because the summary
// is the worst of all mailboxes and recomputing it per run would emit a
// sender.health_changed for every intermediate value. A caller that completes
// one run on its own calls RefreshSenderHealth after it.
func (r *Runner) CompleteRun(
	ctx context.Context, st store.Store, run *store.ProbeRun, ev Evidence, now time.Time,
) error {
	h := ev.Headers
	obs := Observe(ev, now)

	report, err := r.runDNS(ctx, st, run.SenderID, obs.ObservedIP)
	if err != nil {
		r.opts.Logger.Warn("sendplane: probe DNS layer failed", "run", run.ID, "err", err)
	}
	if report != nil && report.PTR != nil {
		obs.PTRChecked = true
		obs.PTRMatch = report.PTR.Details["fcrdns"] == "true"
		obs.PTR = report.PTR.Details["ptr"]
	}

	status, reason := Verdict(obs)
	// The DNS layer never overrides the loopback verdict (ADR-0012): it can
	// only explain it. A red record set with a green probe stays green, and
	// the record shows up in the run's DNS column.
	if report != nil && report.Status == store.HealthRed && status == store.HealthGreen {
		status = store.HealthYellow
		reason = "루프백은 통과했지만 DNS 정적 검사가 실패했습니다"
	}

	run.Status = status
	run.Reason = reason
	run.Delivered = true
	run.Folder = obs.Folder
	run.Latency = obs.Latency
	run.SPF, run.DKIM, run.DMARC = obs.SPF, obs.DKIM, obs.DMARC
	run.DKIMDomain, run.DKIMSelector = obs.DKIMDomain, obs.DKIMSelector
	run.DMARCPolicy = obs.DMARCPolicy
	run.TLS = obs.TLS
	if obs.ObservedIP != nil {
		run.ObservedIP = obs.ObservedIP.String()
	}
	run.PTR, run.PTRMatch = obs.PTR, obs.PTRMatch
	run.RawHeaders = capRawHeaders(ev.RawHeaders)
	run.ReceivedAt = store.TruncateTime(receivedAt(ev.ReceivedAt, h, now))
	run.Pending = false
	if report != nil {
		if b, err := json.Marshal(report); err == nil {
			run.DNS = b
		}
	}
	return updateRun(ctx, st, run)
}

// finishUndelivered closes out a run whose mail never arrived. ADR-0012 asks
// for a consecutive-failure threshold because a provider's greylisting
// produces exactly one of these.
// subject names what did not arrive, so that a webhook mailbox says so
// instead of blaming a mailbox nobody polls.
func (r *Runner) finishUndelivered(
	ctx context.Context, st store.Store, run *store.ProbeRun, subject string, now time.Time,
) error {
	streak, err := r.undeliveredStreak(ctx, st, run)
	if err != nil {
		return err
	}
	status := store.HealthYellow
	reason := fmt.Sprintf("%s %s 안에 도착하지 않았습니다 (%d/%d)",
		subject, r.opts.Timeout, streak, r.opts.ConsecutiveFailuresForRed)
	if streak >= r.opts.ConsecutiveFailuresForRed {
		status = store.HealthRed
		reason = fmt.Sprintf("%s 도착하지 않았습니다 (연속 %d회)", subject, streak)
	}

	report, err := r.runDNS(ctx, st, run.SenderID, nil)
	if err != nil {
		r.opts.Logger.Warn("sendplane: probe DNS layer failed", "run", run.ID, "err", err)
	}
	if report != nil {
		if b, err := json.Marshal(report); err == nil {
			run.DNS = b
		}
		if report.Status == store.HealthRed {
			reason += " · " + firstRedSummary(report)
		}
	}

	run.Status = status
	run.Reason = reason
	run.Delivered = false
	run.ReceivedAt = time.Time{}
	run.Pending = false
	return updateRun(ctx, st, run)
}

// undeliveredStreak counts how many finished runs in a row for this mailbox
// were undelivered, including the one being finished now.
func (r *Runner) undeliveredStreak(ctx context.Context, st store.Store, run *store.ProbeRun) (int, error) {
	runs, err := r.runsOf(ctx, st, run.SenderID)
	if err != nil {
		return 1, err
	}
	streak := 1
	for i := len(runs) - 1; i >= 0; i-- {
		prev := &runs[i]
		if prev.ID == run.ID || prev.MailboxID != run.MailboxID || isPending(prev) {
			continue
		}
		if prev.Status == store.HealthUnknown && !prev.Delivered {
			// A run closed out because the mailbox could not be opened saw
			// nothing at all. It neither extends the streak nor ends it.
			continue
		}
		if prev.Delivered {
			break
		}
		streak++
	}
	return streak, nil
}

// dnsOnlyRun is the ADR-0012 fallback for a tenant with no probe mailbox: the
// DNS layer still runs, but the verdict can never be green, because nothing
// checked that mail actually arrives.
func (r *Runner) dnsOnlyRun(ctx context.Context, st store.Store, snd *store.Sender, now time.Time) (string, error) {
	report, err := r.runDNS(ctx, st, snd.ID, nil)
	if err != nil {
		return "", err
	}
	status := store.HealthYellow
	reason := "loopback 미구성: 프로브 메일박스가 없어 DNS 정적 검사만 수행했습니다"
	if report != nil && report.Status == store.HealthRed {
		status = store.HealthRed
		reason = "loopback 미구성 · " + firstRedSummary(report)
	}

	// A DNS-only run has nothing to wait for, so it is written finished.
	run := &store.ProbeRun{
		ID:        store.NewID(),
		SenderID:  snd.ID,
		GroupID:   store.NewID(),
		Status:    status,
		Reason:    reason,
		StartedAt: now,
		CreatedAt: now,
	}
	if report != nil {
		if b, err := json.Marshal(report); err == nil {
			run.DNS = b
		}
	}
	if err := st.ProbeRuns().Create(ctx, run); err != nil {
		return "", err
	}
	if err := r.setSenderHealth(ctx, st, snd.ID, status, reason, now); err != nil {
		return run.ID, err
	}
	return run.ID, nil
}

// runDNS runs the diagnostic layer for a sender, with the IP the probe was
// actually seen from (architecture 11.3: no separate self-IP detection is
// needed, the Received header has it).
func (r *Runner) runDNS(ctx context.Context, st store.Store, senderID string, observedIP net.IP) (*dnscheck.Report, error) {
	if r.opts.DNS == nil {
		return nil, nil
	}
	snd, err := st.Senders().Get(ctx, senderID)
	if err != nil {
		return nil, err
	}
	in := dnscheck.Inputs{Domain: domainOf(snd.FromEmail), ObservedIP: observedIP}

	if snd.DomainID != "" {
		dom, err := st.Domains().Get(ctx, snd.DomainID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if dom != nil {
			if dom.Domain != "" {
				in.Domain = dom.Domain
			}
			in.DKIMSelector = dom.DKIMSelector
			in.ReturnPathDomain = dom.ReturnPathDomain
			if len(dom.DKIMPrivateKey) > 0 && r.opts.Secrets != nil {
				key, err := r.opts.Secrets.Decrypt(ctx, dom.DKIMPrivateKey)
				if err != nil {
					r.opts.Logger.Warn("sendplane: cannot decrypt DKIM key",
						"domain", dom.ID, "err", err)
				} else {
					in.DKIMKey = key
				}
			}
		}
	}
	if in.Domain == "" {
		return nil, nil
	}
	rep := r.opts.DNS.RunAll(ctx, in)
	return &rep, nil
}

// RefreshSenderHealth recomputes one sender's summary from its run history and
// emits sender.health_changed when it moved. A caller that completed a single
// run outside the collect loop — the inbound webhook of ADR-0016 — calls it
// right after CompleteRun; CollectWith does the same thing once per affected
// sender at the end of a tick instead.
func (r *Runner) RefreshSenderHealth(
	ctx context.Context, st store.Store, senderID string, now time.Time,
) error {
	runs, err := r.runsOf(ctx, st, senderID)
	if err != nil {
		return err
	}
	return r.updateSenderHealth(ctx, st, senderID, runs, now)
}

// updateSenderHealth recomputes a sender's summary from the newest finished
// run of each mailbox — the worst of them (architecture 11.2) — and emits
// sender.health_changed when it moved.
func (r *Runner) updateSenderHealth(
	ctx context.Context, st store.Store, senderID string, runs []store.ProbeRun, now time.Time,
) error {
	latest := map[string]*store.ProbeRun{}
	for i := range runs {
		run := &runs[i]
		if isPending(run) {
			continue
		}
		cur, ok := latest[run.MailboxID]
		if !ok || run.ID > cur.ID {
			// IDs are UUIDv7, so a larger ID is the later run.
			latest[run.MailboxID] = run
		}
	}
	if len(latest) == 0 {
		return nil
	}

	status := store.HealthUnknown
	var reasons []string
	for _, run := range latest {
		status = worst(status, run.Status)
	}
	for _, run := range latest {
		if run.Status == status && run.Reason != "" {
			reasons = append(reasons, run.Reason)
		}
	}
	reason := ""
	if len(reasons) > 0 {
		reason = reasons[0]
		if len(reasons) > 1 {
			reason = fmt.Sprintf("%s (메일박스 %d곳)", reason, len(reasons))
		}
	}
	return r.setSenderHealth(ctx, st, senderID, status, reason, now)
}

// setSenderHealth writes the verdict with the optimistic concurrency the
// Sender model uses, retrying on a lost race: the API may be editing the same
// row, and a probe verdict must not be the write that gets dropped.
func (r *Runner) setSenderHealth(
	ctx context.Context, st store.Store, senderID string,
	status store.HealthStatus, reason string, now time.Time,
) error {
	const attempts = 3
	for i := range attempts {
		snd, err := st.Senders().Get(ctx, senderID)
		if err != nil {
			return err
		}
		before := snd.Health
		snd.Health = status
		snd.HealthReason = reason
		snd.HealthCheckedAt = store.TruncateTime(now)
		if err := st.Senders().Update(ctx, snd); err != nil {
			if errors.Is(err, store.ErrConflict) && i < attempts-1 {
				continue
			}
			return err
		}
		if before == status {
			return nil
		}
		return r.enqueueHealthChanged(ctx, st, snd, before, now)
	}
	return nil
}

// healthChangedPayload is the JSON body of sender.health_changed.
type healthChangedPayload struct {
	SenderID   string             `json:"sender_id"`
	Name       string             `json:"name,omitempty"`
	From       store.HealthStatus `json:"from"`
	To         store.HealthStatus `json:"to"`
	Reason     string             `json:"reason,omitempty"`
	CheckedAt  time.Time          `json:"checked_at"`
	OccurredAt time.Time          `json:"occurred_at"`
}

func (r *Runner) enqueueHealthChanged(
	ctx context.Context, st store.Store, snd *store.Sender,
	before store.HealthStatus, now time.Time,
) error {
	payload, err := json.Marshal(healthChangedPayload{
		SenderID:   snd.ID,
		Name:       snd.Name,
		From:       before,
		To:         snd.Health,
		Reason:     snd.HealthReason,
		CheckedAt:  snd.HealthCheckedAt,
		OccurredAt: now,
	})
	if err != nil {
		return err
	}
	return st.Outbox().Enqueue(ctx, []store.OutboxEvent{{
		Type:          EventSenderHealthChanged,
		Payload:       payload,
		Status:        store.OutboxPending,
		CreatedAt:     now,
		NextAttemptAt: now,
	}})
}

// --- the built-in probe message ----------------------------------------

const (
	probeSubjectTpl = "[sendplane probe {{ vars.run_id }}]"
	probeHTMLTpl    = `<!doctype html><html><body>` +
		`<p>sendplane loopback health probe.</p>` +
		`<p>run: {{ vars.run_id }}</p>` +
		`<p>This message is generated automatically and can be deleted.</p>` +
		`</body></html>`
	probeTextTpl = "sendplane loopback health probe.\r\n" +
		"run: {{ vars.run_id }}\r\n" +
		"This message is generated automatically and can be deleted.\r\n"
)

// probeVersion finds or creates the tenant's built-in probe MessageVersion.
//
// It is a MessageVersion and not a Template because probes are not editable
// content: a version is immutable, which is exactly right for a fixed
// message, and it keeps the probe out of the operator's template list. It is
// found by the ProbeTemplateID marker rather than a fixed UUID because
// message_version.id is a global primary key, so one hard-coded ID could not
// be shared by two tenants.
func (r *Runner) probeVersion(ctx context.Context, st store.Store) (string, error) {
	page := store.Page{Limit: store.MaxPageLimit}
	for {
		res, err := st.Versions().ListByTemplate(ctx, ProbeTemplateID, page)
		if err != nil {
			return "", err
		}
		for i := range res.Items {
			if res.Items[i].Checksum == probeChecksum {
				return res.Items[i].ID, nil
			}
		}
		if res.NextCursor == "" {
			break
		}
		page.Cursor = res.NextCursor
	}

	v := &store.MessageVersion{
		ID:         store.NewID(),
		TemplateID: ProbeTemplateID,
		SubjectTpl: probeSubjectTpl,
		HTMLTpl:    probeHTMLTpl,
		TextTpl:    probeTextTpl,
		Checksum:   probeChecksum,
		CreatedAt:  store.TruncateTime(r.opts.Clock()),
	}
	if err := st.Versions().Create(ctx, v); err != nil {
		return "", fmt.Errorf("probe: create probe version: %w", err)
	}
	return v.ID, nil
}

// --- helpers -----------------------------------------------------------

// Observe reads one delivered probe mail into an Observation.
func Observe(ev Evidence, now time.Time) Observation {
	h := ev.Headers
	obs := Observation{Delivered: true}
	obs.Folder = folderKind(ev.Mailbox, ev.Folder)

	authServID := ""
	if ev.Mailbox != nil {
		authServID = ev.Mailbox.AuthServID
	}
	for _, ar := range TrustedAuthResults(h, authServID) {
		obs.TrustedAR = true
		if m, ok := ar.Method("spf"); ok && obs.SPF == "" {
			obs.SPF = m.Result
			obs.MailFrom = m.Prop("smtp.mailfrom")
		}
		if m, ok := ar.Method("dkim"); ok && obs.DKIM == "" {
			obs.DKIM = m.Result
			obs.DKIMDomain = strings.TrimPrefix(m.Prop("header.d"), "@")
			if obs.DKIMDomain == "" {
				obs.DKIMDomain = strings.TrimPrefix(m.Prop("header.i"), "@")
			}
			obs.DKIMSelector = m.Prop("header.s")
		}
		if m, ok := ar.Method("dmarc"); ok && obs.DMARC == "" {
			obs.DMARC = m.Result
			obs.DMARCPolicy = DMARCPolicy(m)
		}
	}

	if hop, ok := FirstExternalHop(h); ok {
		obs.ObservedIP = hop.IP
		obs.TLS = hop.TLS
		obs.PTR = hop.RDNS
	}
	obs.Latency, _ = Latency(h, ev.ReceivedAt)
	return obs
}

// receivedAt is the channel's own timestamp, or the newest Received, or now.
func receivedAt(channelAt time.Time, h Headers, now time.Time) time.Time {
	if !channelAt.IsZero() {
		return channelAt
	}
	chain := ReceivedChain(h)
	for i := len(chain) - 1; i >= 0; i-- {
		if !chain[i].At.IsZero() {
			return chain[i].At
		}
	}
	return now
}

// capRawHeaders bounds what a run keeps for diagnosis, whichever channel
// filled it in.
func capRawHeaders(s string) string {
	if len(s) > maxRawHeaders {
		return s[:maxRawHeaders]
	}
	return s
}

func rawHeaders(raw []byte) string {
	if i := strings.Index(string(raw), "\r\n\r\n"); i >= 0 {
		raw = raw[:i]
	} else if i := strings.Index(string(raw), "\n\n"); i >= 0 {
		raw = raw[:i]
	}
	return capRawHeaders(string(raw))
}

// isPending reports whether a run is still waiting for its mail
// (store.ProbeRun.Pending).
func isPending(r *store.ProbeRun) bool { return r.Pending }

func updateRun(ctx context.Context, st store.Store, run *store.ProbeRun) error {
	return st.ProbeRuns().Update(ctx, run)
}

// pendingRuns lists every run still waiting for its mail, across senders. It
// is what Collect walks instead of paging each sender's whole history.
func pendingRuns(ctx context.Context, st store.Store) ([]store.ProbeRun, error) {
	var out []store.ProbeRun
	page := store.Page{Limit: store.MaxPageLimit}
	for {
		res, err := st.ProbeRuns().ListPending(ctx, page)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if res.NextCursor == "" {
			return out, nil
		}
		page.Cursor = res.NextCursor
	}
}

// runsOf lists every run of one sender, oldest first.
func (r *Runner) runsOf(ctx context.Context, st store.Store, senderID string) ([]store.ProbeRun, error) {
	var out []store.ProbeRun
	page := store.Page{Limit: store.MaxPageLimit}
	for {
		res, err := st.ProbeRuns().ListBySender(ctx, senderID, page)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if res.NextCursor == "" {
			return out, nil
		}
		page.Cursor = res.NextCursor
	}
}

func enabledMailboxes(ctx context.Context, st store.Store) ([]store.ProbeMailbox, error) {
	all, err := listAll(ctx, st.ProbeMailboxes().List)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, m := range all {
		if m.Enabled {
			out = append(out, m)
		}
	}
	return out, nil
}

// listAll drains a cursor-paginated List.
func listAll[T any](ctx context.Context, list func(context.Context, store.Page) (store.Result[T], error)) ([]T, error) {
	var out []T
	page := store.Page{Limit: store.MaxPageLimit}
	for {
		res, err := list(ctx, page)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if res.NextCursor == "" {
			return out, nil
		}
		page.Cursor = res.NextCursor
	}
}

func domainOf(email string) string {
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		return email[i+1:]
	}
	return ""
}

func firstRedSummary(rep *dnscheck.Report) string {
	for _, res := range []*dnscheck.Result{rep.SPF, rep.DKIM, rep.DMARC, rep.MX, rep.PTR} {
		if res != nil && res.Status == store.HealthRed {
			return res.Summary
		}
	}
	return ""
}

func cmpErr(first, err error) error {
	if first != nil {
		return first
	}
	return err
}

// singleOpener adapts one fetcher to the per-mailbox opener. The wrapper
// hides any Close the fetcher has: the caller owns a fetcher it passed in, and
// closing it after the first mailbox would break every mailbox after it.
type singleOpener struct{ f MailboxFetcher }

type nonClosing struct{ MailboxFetcher }

func (s singleOpener) Open(context.Context, *store.ProbeMailbox) (MailboxFetcher, error) {
	return nonClosing{s.f}, nil
}
