package sendplane

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

var checkNow = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// scriptedTester answers per host, so one tick can have a working mailbox and
// a broken one.
type scriptedTester struct {
	mu      sync.Mutex
	byHost  map[string]mailbox.TestResult
	calls   []mailbox.Config
	fallbak mailbox.TestResult
}

func (s *scriptedTester) test(_ context.Context, cfg mailbox.Config, _ SecretCipher) mailbox.TestResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, cfg)
	if res, ok := s.byHost[cfg.Host]; ok {
		return res
	}
	return s.fallbak
}

func (s *scriptedTester) seen() []mailbox.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mailbox.Config(nil), s.calls...)
}

func newCheckEnv(t *testing.T, ts *scriptedTester) (store.Store, mailboxCheck) {
	t.Helper()
	p := memstore.New(memstore.WithClock(func() time.Time { return checkNow }))
	t.Cleanup(func() { _ = p.Close() })
	st, err := p.ForTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	return st, mailboxCheck{
		st:       st,
		interval: 15 * time.Minute,
		log:      slog.New(slog.DiscardHandler),
		tester:   ts.test,
	}
}

func okTest() mailbox.TestResult {
	return mailbox.TestResult{OK: true, Stage: mailbox.StageOK}
}

func authTest() mailbox.TestResult {
	return mailbox.TestResult{Stage: mailbox.StageAuth, Error: "Invalid credentials"}
}

// The loop is what catches a password changed between probes: it logs in to
// every enabled mailbox of both kinds and records what it found.
func TestMailboxCheckTick(t *testing.T) {
	ctx := context.Background()
	ts := &scriptedTester{
		byHost:  map[string]mailbox.TestResult{"imap.broken.example": authTest()},
		fallbak: okTest(),
	}
	st, loop := newCheckEnv(t, ts)

	good := &store.ProbeMailbox{
		Name: "gmail", Address: "p@example.com", Host: "imap.good.example", Port: 993,
		InboxFolder: "INBOX", SpamFolder: "[Gmail]/Spam", Enabled: true,
	}
	bad := &store.BounceMailbox{
		Name: "bounces", Host: "imap.broken.example", Port: 993, Enabled: true,
	}
	off := &store.ProbeMailbox{
		Name: "retired", Address: "old@example.com", Host: "imap.retired.example", Port: 993,
	}
	for _, m := range []*store.ProbeMailbox{good, off} {
		if err := st.ProbeMailboxes().Create(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.BounceMailboxes().Create(ctx, bad); err != nil {
		t.Fatal(err)
	}

	if err := loop.Tick(ctx, checkNow); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	calls := ts.seen()
	if len(calls) != 2 {
		t.Fatalf("%d checks, want 2 (the disabled mailbox must be skipped): %+v", len(calls), calls)
	}
	// A probe mailbox is checked with its spam folder: the verdict depends on
	// telling inbox from spam.
	for _, cfg := range calls {
		if cfg.Host == "imap.good.example" &&
			(len(cfg.ExtraFolders) != 1 || cfg.ExtraFolders[0] != "[Gmail]/Spam") {
			t.Errorf("probe check did not inspect the spam folder: %+v", cfg.ExtraFolders)
		}
	}

	gotGood, err := st.ProbeMailboxes().Get(ctx, good.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotGood.Health.Status != store.MailboxOK {
		t.Errorf("good mailbox health = %+v", gotGood.Health)
	}
	gotBad, err := st.BounceMailboxes().Get(ctx, bad.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBad.Health.Status != store.MailboxError || gotBad.Health.Stage != store.MailboxStageAuth {
		t.Errorf("broken mailbox health = %+v", gotBad.Health)
	}
	gotOff, err := st.ProbeMailboxes().Get(ctx, off.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOff.Health.Status != store.MailboxUnknown {
		t.Errorf("a disabled mailbox was checked: %+v", gotOff.Health)
	}

	// A second tick inside the interval checks nothing: the poller and the
	// probe collector also write CheckedAt, so this loop only fills the gaps.
	if err := loop.Tick(ctx, checkNow.Add(time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(ts.seen()) != 2 {
		t.Fatalf("a tick inside the interval re-checked: %d calls", len(ts.seen()))
	}

	// Past the interval the broken one is checked again, and the second
	// failure in a row is what tells the host.
	if err := loop.Tick(ctx, checkNow.Add(20*time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	evs, err := st.Outbox().List(ctx, "", store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	unhealthy := 0
	for _, ev := range evs.Items {
		if ev.Type == "mailbox.unhealthy" {
			unhealthy++
		}
	}
	if unhealthy != 1 {
		t.Fatalf("%d mailbox.unhealthy events, want exactly 1", unhealthy)
	}

	// And when it comes back, the recovery is announced once.
	ts.mu.Lock()
	ts.byHost["imap.broken.example"] = okTest()
	ts.mu.Unlock()
	if err := loop.Tick(ctx, checkNow.Add(40*time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	evs, err = st.Outbox().List(ctx, "", store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	recovered := 0
	for _, ev := range evs.Items {
		if ev.Type == "mailbox.recovered" {
			recovered++
		}
	}
	if recovered != 1 {
		t.Fatalf("%d mailbox.recovered events, want exactly 1", recovered)
	}
}

// One broken mailbox must not stop the others being checked.
func TestMailboxCheckContinuesAfterFailure(t *testing.T) {
	ctx := context.Background()
	ts := &scriptedTester{byHost: map[string]mailbox.TestResult{}, fallbak: authTest()}
	st, loop := newCheckEnv(t, ts)

	for _, host := range []string{"a.example", "b.example", "c.example"} {
		if err := st.BounceMailboxes().Create(ctx, &store.BounceMailbox{
			Name: host, Host: host, Port: 993, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := loop.Tick(ctx, checkNow); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := len(ts.seen()); got != 3 {
		t.Fatalf("%d checks, want 3", got)
	}
}

// A webhook-kind probe mailbox has no credentials and no server to reach, so
// the loop must not try: its health comes from probe mail arriving, or failing
// to (ADR-0016).
func TestMailboxCheckSkipsWebhookProbeMailboxes(t *testing.T) {
	ctx := context.Background()
	ts := &scriptedTester{fallbak: okTest()}
	st, loop := newCheckEnv(t, ts)

	hook := &store.ProbeMailbox{
		Name: "forwarder", Kind: store.ProbeMailboxWebhook,
		Address: "probe@example.net", AuthServID: "mx.example.net", Enabled: true,
	}
	if err := st.ProbeMailboxes().Create(ctx, hook); err != nil {
		t.Fatal(err)
	}
	if err := loop.Tick(ctx, checkNow); err != nil {
		t.Fatal(err)
	}
	if calls := ts.seen(); len(calls) != 0 {
		t.Fatalf("the loop dialled a webhook mailbox: %+v", calls)
	}
	got, err := st.ProbeMailboxes().Get(ctx, hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Health.Status != store.MailboxUnknown {
		t.Fatalf("health = %s (%s), want it untouched: the loop cannot observe this kind",
			got.Health.Status, got.Health.Reason)
	}
}
