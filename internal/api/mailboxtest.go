package api

import (
	"context"
	"strings"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/internal/mbhealth"
	"github.com/sendplane/sendplane/store"
)

// Testing a mailbox is the one operation here that talks to somebody else's
// server, so it is the one that needs a seam: MailboxTester is what a handler
// test replaces to avoid standing up an IMAP server (architecture 11.5).
//
// A remote failure is a 200 carrying ok:false, never a 4xx or 5xx. "The
// password is wrong" is the answer to the question the caller asked, and a
// form that has to read it out of an error body is a form that will show
// "request failed" instead.

// MailboxTester connects to a mailbox and reports how far it got.
type MailboxTester interface {
	// Test never returns an error: a failure is in the result. cfg.Password is
	// the ciphertext the store holds, which the tester decrypts.
	Test(ctx context.Context, cfg mailbox.Config) mailbox.TestResult
}

// cipherTester is the default MailboxTester: internal/mailbox with the host's
// SecretCipher bound to it.
type cipherTester struct{ cipher host.SecretCipher }

func (t cipherTester) Test(ctx context.Context, cfg mailbox.Config) mailbox.TestResult {
	return mailbox.Test(ctx, cfg, t.cipher)
}

// --- probe mailboxes ---------------------------------------------------

func (s *server) TestProbeMailboxCredentials(ctx context.Context, req TestProbeMailboxCredentialsRequestObject) (TestProbeMailboxCredentialsResponseObject, error) {
	if _, err := tenantFrom(ctx); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	// Validated and encrypted through the same path a create goes through, so
	// a mailbox that passes the test is one that can be saved.
	m := &store.ProbeMailbox{}
	if err := s.applyMailbox(ctx, m, ProbeMailboxUpdate{
		Name: req.Body.Name, Kind: req.Body.Kind,
		Address: req.Body.Address, Host: req.Body.Host, Port: req.Body.Port,
		Tls: req.Body.Tls, Username: req.Body.Username, Password: req.Body.Password,
		InboxFolder: req.Body.InboxFolder, SpamFolder: req.Body.SpamFolder,
		AuthservId: req.Body.AuthservId, Enabled: req.Body.Enabled,
	}); err != nil {
		return nil, err
	}
	if m.Kind.Normalized() == store.ProbeMailboxWebhook {
		return TestProbeMailboxCredentials200JSONResponse(s.webhookMailboxTestOut()), nil
	}
	res := s.deps.MailboxTester.Test(ctx, probeMailboxConfig(m))
	return TestProbeMailboxCredentials200JSONResponse(mailboxTestOut(res)), nil
}

func (s *server) TestProbeMailbox(ctx context.Context, req TestProbeMailboxRequestObject) (TestProbeMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	m, err := t.st.ProbeMailboxes().Get(ctx, req.MailboxId)
	if err != nil {
		return nil, err
	}
	if m.Kind.Normalized() == store.ProbeMailboxWebhook {
		// Nothing is dialled and nothing is recorded: the health of a webhook
		// mailbox is written by probe mail arriving (or not), and a test that
		// wrote a badge here would report on a channel it never exercised.
		return TestProbeMailbox200JSONResponse(s.webhookMailboxTestOut()), nil
	}
	cfg := probeMailboxConfig(m)
	override, err := s.overridePassword(ctx, &cfg, passwordOf(req.Body))
	if err != nil {
		return nil, err
	}

	res := s.deps.MailboxTester.Test(ctx, cfg)
	if !override {
		s.recordMailboxHealth(ctx, t.st, mbhealth.KindProbe,
			mbhealth.Mailbox{ID: m.ID, Name: m.Name, Health: m.Health}, res)
	}
	return TestProbeMailbox200JSONResponse(mailboxTestOut(res)), nil
}

// webhookMailboxTestOut is what "test this mailbox" means for a webhook-kind
// probe mailbox (ADR-0016). There are no credentials to try, so the only thing
// that can be answered is whether this deployment has an inbound endpoint the
// provider could post to at all — which is exactly the misconfiguration an
// operator hits first, and which is otherwise invisible until a probe times
// out fifteen minutes later.
func (s *server) webhookMailboxTestOut() MailboxTestResult {
	out := MailboxTestResult{Folders: map[string]MailboxTestFolder{}}
	if len(s.deps.ProbeInbound) == 0 {
		out.Ok = false
		out.Stage = MailboxStageConfig
		out.Error = ptr("webhook mailboxes are verified by the provider webhook, not by login")
		return out
	}
	names := make([]string, 0, len(s.deps.ProbeInbound))
	for _, route := range s.deps.ProbeInbound {
		names = append(names, route.Provider.Name())
	}
	out.Ok = true
	out.Stage = MailboxStageOk
	out.Server = ptr("webhook:" + strings.Join(names, ","))
	return out
}

// probeMailboxConfig is the mailbox.Config the probe collector dials this row
// with. It names the spam folder too: a probe verdict depends on telling
// "inbox" from "spam", so a spam folder that does not exist is a broken probe
// mailbox even though every credential is right (architecture 11.4).
func probeMailboxConfig(m *store.ProbeMailbox) mailbox.Config {
	cfg := mailbox.Config{
		// A probe mailbox has to be IMAP: POP3 cannot search by header and
		// has no folders (internal/mailbox).
		Protocol: mailbox.ProtocolIMAP,
		Host:     m.Host, Port: m.Port, TLS: m.TLS,
		Username: m.Username, Password: m.Password,
		Folder: m.InboxFolder,
	}
	if m.SpamFolder != "" {
		cfg.ExtraFolders = []string{m.SpamFolder}
	}
	return cfg
}

// --- bounce mailboxes --------------------------------------------------

func (s *server) TestBounceMailboxCredentials(ctx context.Context, req TestBounceMailboxCredentialsRequestObject) (TestBounceMailboxCredentialsResponseObject, error) {
	if _, err := tenantFrom(ctx); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	m := &store.BounceMailbox{}
	if err := s.applyBounceMailbox(ctx, m, BounceMailboxUpdate{
		Name: req.Body.Name, Address: req.Body.Address, Protocol: req.Body.Protocol,
		Host: req.Body.Host, Port: req.Body.Port, Tls: req.Body.Tls,
		Username: req.Body.Username, Password: req.Body.Password,
		Folder: req.Body.Folder, AfterProcess: req.Body.AfterProcess,
		Enabled: req.Body.Enabled,
	}); err != nil {
		return nil, err
	}
	res := s.deps.MailboxTester.Test(ctx, bounceMailboxConfig(m))
	return TestBounceMailboxCredentials200JSONResponse(mailboxTestOut(res)), nil
}

func (s *server) TestBounceMailbox(ctx context.Context, req TestBounceMailboxRequestObject) (TestBounceMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	m, err := t.st.BounceMailboxes().Get(ctx, req.MailboxId)
	if err != nil {
		return nil, err
	}
	cfg := bounceMailboxConfig(m)
	override, err := s.overridePassword(ctx, &cfg, passwordOf(req.Body))
	if err != nil {
		return nil, err
	}

	res := s.deps.MailboxTester.Test(ctx, cfg)
	if !override {
		s.recordMailboxHealth(ctx, t.st, mbhealth.KindBounce,
			mbhealth.Mailbox{ID: m.ID, Name: m.Name, Health: m.Health}, res)
	}
	return TestBounceMailbox200JSONResponse(mailboxTestOut(res)), nil
}

// bounceMailboxConfig is the mailbox.Config the poller dials this row with.
func bounceMailboxConfig(m *store.BounceMailbox) mailbox.Config {
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

// --- shared ------------------------------------------------------------

// passwordOf reads the optional override out of a test body.
func passwordOf(body *MailboxTestRequest) *string {
	if body == nil {
		return nil
	}
	return body.Password
}

// overridePassword swaps in a password from the request body and reports
// whether it did. The plaintext is encrypted first rather than handed to the
// tester as-is, so that only one thing in the process ever holds a mailbox
// password in the clear (architecture 16).
func (s *server) overridePassword(ctx context.Context, cfg *mailbox.Config, pw *string) (bool, error) {
	if pw == nil {
		return false, nil
	}
	enc, err := s.secret(ctx, pw, nil)
	if err != nil {
		return false, err
	}
	cfg.Password = enc
	return true, nil
}

// recordMailboxHealth files a manual test as the mailbox's health, so that the
// badge an operator is looking at agrees with the test they just ran.
//
// It is skipped for a test that used a password from the request body: that
// answers "would this password work", which is not the same question as "is
// this mailbox working", and letting it write the badge would mean a typo in
// the form marks a perfectly healthy mailbox broken.
//
// A failed write is logged, not returned: the caller asked for a test result
// and got one.
func (s *server) recordMailboxHealth(
	ctx context.Context, st store.Store, kind mbhealth.Kind,
	m mbhealth.Mailbox, res mailbox.TestResult,
) {
	outcome := mbhealth.OK()
	if !res.OK {
		outcome = mbhealth.Fail(res.Stage, res.Error)
	}
	if _, err := mbhealth.Record(ctx, st, kind, m, outcome, s.deps.Clock()); err != nil {
		s.deps.Logger.Error("sendplane: recording mailbox health failed",
			"kind", kind, "mailbox", m.ID, "err", err)
	}
}

func mailboxTestOut(res mailbox.TestResult) MailboxTestResult {
	out := MailboxTestResult{
		Ok:        res.OK,
		Stage:     MailboxStage(res.Stage),
		LatencyMs: res.Latency.Milliseconds(),
		Folders:   make(map[string]MailboxTestFolder, len(res.Folders)),
	}
	for name, info := range res.Folders {
		out.Folders[name] = MailboxTestFolder{
			Exists:   info.Exists,
			Messages: i32(info.Messages),
		}
	}
	out.Error = strPtr(res.Error)
	out.Server = strPtr(res.Server)
	return out
}

// mailboxHealthOut renders the stored reachability of a mailbox. A mailbox
// nothing has checked yet still gets an object, with status unknown, so a
// caller never has to tell "not checked" from "field missing".
func mailboxHealthOut(h store.MailboxHealth) *MailboxHealth {
	out := MailboxHealth{
		Status:              MailboxStatus(h.Status.String()),
		Reason:              strPtr(h.Reason),
		ConsecutiveFailures: i32(h.ConsecutiveFailures),
		CheckedAt:           timePtr(h.CheckedAt),
		LastOkAt:            timePtr(h.LastOKAt),
	}
	if h.Stage != "" {
		out.Stage = ptr(MailboxStage(h.Stage))
	}
	return &out
}
