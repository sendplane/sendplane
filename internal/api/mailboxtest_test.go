package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
)

// fakeTester is the MailboxTester seam: it records the config it was handed
// and answers whatever the test told it to. Standing up an IMAP server here
// would test internal/mailbox, which has its own tests, instead of the
// handlers.
type fakeTester struct {
	mu   sync.Mutex
	res  mailbox.TestResult
	last mailbox.Config
	n    int
}

func (f *fakeTester) Test(_ context.Context, cfg mailbox.Config) mailbox.TestResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = cfg
	f.n++
	return f.res
}

func (f *fakeTester) seen() (mailbox.Config, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.n
}

func okResult() mailbox.TestResult {
	return mailbox.TestResult{
		OK: true, Stage: mailbox.StageOK, Latency: 42 * time.Millisecond,
		Server: "IMAP4rev2 MOVE",
		Folders: map[string]mailbox.FolderInfo{
			"INBOX": {Exists: true, Messages: 3},
		},
	}
}

func authFailure() mailbox.TestResult {
	return mailbox.TestResult{
		Stage: mailbox.StageAuth, Error: "Invalid credentials",
		Latency: 7 * time.Millisecond, Folders: map[string]mailbox.FolderInfo{},
	}
}

func newTesterEnv(t *testing.T, res mailbox.TestResult) (*env, *fakeTester) {
	t.Helper()
	ft := &fakeTester{res: res}
	return newEnv(t, func(d *Deps) { d.MailboxTester = ft }), ft
}

func (e *env) seedProbeMailbox() ProbeMailbox {
	e.t.Helper()
	return decodeInto[ProbeMailbox](e.t, e.do(http.MethodPost, "/api/v1/probe-mailboxes", ProbeMailboxInput{
		Name: "gmail", Address: "probe@example.com", Host: "imap.example.com", Port: 993,
		Username: ptr("probe"), Password: ptr("s3cret"),
		InboxFolder: ptr("INBOX"), SpamFolder: ptr("[Gmail]/Spam"),
	}), http.StatusCreated)
}

func (e *env) seedBounceMailbox() BounceMailbox {
	e.t.Helper()
	return decodeInto[BounceMailbox](e.t, e.do(http.MethodPost, "/api/v1/bounce-mailboxes", BounceMailboxInput{
		Name: "bounces", Host: "imap.example.com", Port: 993,
		Username: ptr("bounces"), Password: ptr("s3cret"),
	}), http.StatusCreated)
}

// Testing credentials that are not saved anywhere is what a "Test" button on
// a create form calls.
func TestProbeMailboxCredentialsEndpoint(t *testing.T) {
	e, ft := newTesterEnv(t, okResult())

	got := decodeInto[MailboxTestResult](t, e.do(http.MethodPost, "/api/v1/probe-mailboxes/test",
		ProbeMailboxInput{
			Name: "gmail", Address: "probe@example.com", Host: "imap.example.com", Port: 993,
			Username: ptr("probe"), Password: ptr("s3cret"), SpamFolder: ptr("[Gmail]/Spam"),
		}), http.StatusOK)

	if !got.Ok || got.Stage != "ok" || got.LatencyMs != 42 {
		t.Fatalf("result = %+v", got)
	}
	if info := got.Folders["INBOX"]; !info.Exists || info.Messages == nil || *info.Messages != 3 {
		t.Fatalf("folders = %+v", got.Folders)
	}

	cfg, n := ft.seen()
	if n != 1 {
		t.Fatalf("the tester ran %d times", n)
	}
	// The spam folder is inspected too: a probe verdict depends on telling
	// inbox from spam.
	if len(cfg.ExtraFolders) != 1 || cfg.ExtraFolders[0] != "[Gmail]/Spam" {
		t.Errorf("ExtraFolders = %v", cfg.ExtraFolders)
	}
	// The password reached the tester encrypted, not in the clear.
	if string(cfg.Password) == "s3cret" {
		t.Error("the plaintext password was handed to the tester")
	}
	if string(xorAll(cfg.Password)) != "s3cret" {
		t.Errorf("the tester got %q, which is not the encrypted password", cfg.Password)
	}

	// Nothing was persisted.
	res, err := e.st.ProbeMailboxes().List(context.Background(), store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("the test created %d mailbox rows", len(res.Items))
	}
}

// A mailbox that refuses the login is a 200 carrying the refusal, not a 4xx:
// the request succeeded, the mailbox is what failed.
func TestMailboxTestReportsAuthFailureAs200(t *testing.T) {
	e, _ := newTesterEnv(t, authFailure())

	got := decodeInto[MailboxTestResult](t, e.do(http.MethodPost, "/api/v1/bounce-mailboxes/test",
		BounceMailboxInput{
			Name: "bounces", Host: "imap.example.com", Port: 993,
			Username: ptr("bounces"), Password: ptr("wrong"),
		}), http.StatusOK)

	if got.Ok {
		t.Fatal("a refused login was reported as ok")
	}
	if got.Stage != "auth" {
		t.Fatalf("stage = %q, want auth", got.Stage)
	}
	if got.Error == nil || *got.Error != "Invalid credentials" {
		t.Fatalf("error = %v, want the server's own wording", got.Error)
	}
}

// The credentials endpoint validates its body exactly as create does, so a
// mailbox that passes the test is one that can be saved.
func TestMailboxTestValidatesBody(t *testing.T) {
	e, ft := newTesterEnv(t, okResult())

	decodeError(t, e.do(http.MethodPost, "/api/v1/probe-mailboxes/test", ProbeMailboxInput{
		Name: "gmail", Address: "probe@example.com", Host: "imap.example.com", Port: 0,
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)

	if _, n := ft.seen(); n != 0 {
		t.Fatalf("an invalid body still reached the tester (%d calls)", n)
	}
}

// Testing a stored mailbox records the outcome, so the badge agrees with the
// test the operator just ran.
func TestStoredProbeMailboxTestPersistsHealth(t *testing.T) {
	e, ft := newTesterEnv(t, okResult())
	m := e.seedProbeMailbox()

	// A fresh mailbox is unknown until something checks it.
	if m.Health == nil || m.Health.Status != "unknown" {
		t.Fatalf("health on create = %+v, want unknown", m.Health)
	}

	got := decodeInto[MailboxTestResult](t,
		e.do(http.MethodPost, "/api/v1/probe-mailboxes/"+m.Id.String()+"/test", nil),
		http.StatusOK)
	if !got.Ok {
		t.Fatalf("result = %+v", got)
	}

	after := decodeInto[ProbeMailbox](t,
		e.do(http.MethodGet, "/api/v1/probe-mailboxes/"+m.Id.String(), nil), http.StatusOK)
	if after.Health == nil || after.Health.Status != "ok" {
		t.Fatalf("health = %+v, want ok", after.Health)
	}
	if after.Health.LastOkAt == nil || !after.Health.LastOkAt.Equal(testNow) {
		t.Errorf("last_ok_at = %v, want %v", after.Health.LastOkAt, testNow)
	}
	// A health write is not an edit: the version an operator holds stays valid.
	if after.Version == nil || *after.Version != *m.Version {
		t.Errorf("version = %v, want %v", after.Version, m.Version)
	}

	// The stored ciphertext is what was dialed with.
	cfg, _ := ft.seen()
	if string(xorAll(cfg.Password)) != "s3cret" {
		t.Errorf("the stored password was not used: %q", cfg.Password)
	}
}

func TestStoredBounceMailboxTestPersistsFailure(t *testing.T) {
	e, _ := newTesterEnv(t, authFailure())
	m := e.seedBounceMailbox()

	for range 2 {
		got := decodeInto[MailboxTestResult](t,
			e.do(http.MethodPost, "/api/v1/bounce-mailboxes/"+m.Id.String()+"/test", nil),
			http.StatusOK)
		if got.Ok {
			t.Fatal("a refused login was reported as ok")
		}
	}

	after := decodeInto[BounceMailbox](t,
		e.do(http.MethodGet, "/api/v1/bounce-mailboxes/"+m.Id.String(), nil), http.StatusOK)
	if after.Health == nil || after.Health.Status != "error" {
		t.Fatalf("health = %+v, want error", after.Health)
	}
	if after.Health.Stage == nil || *after.Health.Stage != "auth" {
		t.Fatalf("stage = %v, want auth", after.Health.Stage)
	}
	if after.Health.ConsecutiveFailures == nil || *after.Health.ConsecutiveFailures != 2 {
		t.Fatalf("consecutive_failures = %v, want 2", after.Health.ConsecutiveFailures)
	}

	// Two in a row is what tells the host.
	evs, err := e.st.Outbox().List(context.Background(), "", store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, ev := range evs.Items {
		if ev.Type == "mailbox.unhealthy" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("%d mailbox.unhealthy events, want 1", found)
	}
}

// A password sent in the body is tried, not saved, and does not write the
// badge: it answers a different question from "is this mailbox working".
func TestStoredMailboxTestWithOverridePassword(t *testing.T) {
	e, ft := newTesterEnv(t, authFailure())
	m := e.seedProbeMailbox()

	decodeInto[MailboxTestResult](t,
		e.do(http.MethodPost, "/api/v1/probe-mailboxes/"+m.Id.String()+"/test",
			MailboxTestRequest{Password: ptr("a-typo")}), http.StatusOK)

	cfg, _ := ft.seen()
	if string(xorAll(cfg.Password)) != "a-typo" {
		t.Errorf("the override password was not used: %q", xorAll(cfg.Password))
	}

	after := decodeInto[ProbeMailbox](t,
		e.do(http.MethodGet, "/api/v1/probe-mailboxes/"+m.Id.String(), nil), http.StatusOK)
	if after.Health == nil || after.Health.Status != "unknown" {
		t.Fatalf("health = %+v: a typo in the form must not mark the mailbox broken", after.Health)
	}
	// The stored password is untouched, so the mailbox still works.
	stored, err := e.st.ProbeMailboxes().Get(context.Background(), m.Id.String())
	if err != nil {
		t.Fatal(err)
	}
	if string(xorAll(stored.Password)) != "s3cret" {
		t.Errorf("the stored password was overwritten: %q", xorAll(stored.Password))
	}
}

func TestStoredMailboxTestUnknownID(t *testing.T) {
	e, _ := newTesterEnv(t, okResult())
	decodeError(t, e.do(http.MethodPost, "/api/v1/probe-mailboxes/"+store.NewID()+"/test", nil),
		http.StatusNotFound, ErrorCodeNotFound)
}
