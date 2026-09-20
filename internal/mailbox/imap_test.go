package mailbox

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/sendplane/sendplane/store"
)

// literal adapts a byte slice to imap.LiteralReader so that a test can append
// to the in-memory server.
type literal struct {
	*bytes.Reader
	size int64
}

func newLiteral(b []byte) *literal {
	return &literal{Reader: bytes.NewReader(b), size: int64(len(b))}
}

func (l *literal) Size() int64 { return l.size }

func mail(subject, body string) []byte {
	return []byte(strings.ReplaceAll(
		"From: a@example.com\nTo: b@example.com\nSubject: "+subject+
			"\nX-Sendplane-Probe: "+subject+"\n\n"+body+"\n", "\n", "\r\n"))
}

// startIMAP runs an in-memory IMAP server with one user and INBOX, and returns
// a Config pointing at it plus the user so a test can append more mail.
func startIMAP(t *testing.T, extraFolders ...string) (Config, *imapmemserver.User) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("bounce@example.com", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	for _, f := range extraFolders {
		if err := user.Create(f, nil); err != nil {
			t.Fatalf("create %s: %v", f, err)
		}
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {},
			imap.CapUIDPlus: {}, imap.CapMove: {}, imap.CapESearch: {},
		},
		Logger: discardLogger{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := Config{
		Protocol: ProtocolIMAP, Host: host, TLS: store.TLSNone,
		Username: "bounce@example.com", Password: []byte("secret"),
		Timeout: 5 * time.Second,
	}
	cfg.Port = atoi(port)
	return cfg, user
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func appendMail(t *testing.T, user *imapmemserver.User, folder string, raw []byte) {
	t.Helper()
	if _, err := user.Append(folder, newLiteral(raw), &imap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func TestIMAPFetchAndAck(t *testing.T) {
	ctx := context.Background()
	cfg, user := startIMAP(t, "Handled")
	appendMail(t, user, "INBOX", mail("first", "one"))
	appendMail(t, user, "INBOX", mail("second", "two"))
	appendMail(t, user, "INBOX", mail("third", "three"))

	c, err := Dial(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	msgs, err := c.Fetch(ctx, 2)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Fetch returned %d messages, want 2", len(msgs))
	}
	if !bytes.Contains(msgs[0].Raw, []byte("Subject: first")) {
		t.Fatalf("first message is %q", msgs[0].Raw)
	}
	if msgs[0].Received.IsZero() {
		t.Error("INTERNALDATE was not fetched")
	}

	// keep marks \Seen, so the next Fetch moves on.
	if err := c.Ack(ctx, []string{msgs[0].ID}, ActionKeep); err != nil {
		t.Fatalf("Ack keep: %v", err)
	}
	// delete removes the message.
	if err := c.Ack(ctx, []string{msgs[1].ID}, ActionDelete); err != nil {
		t.Fatalf("Ack delete: %v", err)
	}
	rest, err := c.Fetch(ctx, 10)
	if err != nil {
		t.Fatalf("Fetch after ack: %v", err)
	}
	if len(rest) != 1 || !bytes.Contains(rest[0].Raw, []byte("Subject: third")) {
		t.Fatalf("after ack Fetch returned %d messages", len(rest))
	}

	// move takes the last one out of INBOX.
	if err := c.Ack(ctx, []string{rest[0].ID}, Action("move:Handled")); err != nil {
		t.Fatalf("Ack move: %v", err)
	}
	left, err := c.Fetch(ctx, 10)
	if err != nil {
		t.Fatalf("Fetch after move: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("after move Fetch returned %d messages", len(left))
	}

	handled := cfg
	handled.Folder = "Handled"
	h, err := Dial(ctx, handled, nil)
	if err != nil {
		t.Fatalf("Dial Handled: %v", err)
	}
	defer func() { _ = h.Close() }()
	// Ack marked the message \Seen before moving it, so it is searched for by
	// header rather than fetched as unhandled work.
	moved, err := h.(HeaderSearcher).FetchByHeader(ctx, "X-Sendplane-Probe", "third")
	if err != nil {
		t.Fatalf("Fetch Handled: %v", err)
	}
	if len(moved) != 1 || !bytes.Contains(moved[0].Raw, []byte("Subject: third")) {
		t.Fatalf("Handled holds %d messages", len(moved))
	}
}

func TestIMAPFetchByHeader(t *testing.T) {
	ctx := context.Background()
	cfg, user := startIMAP(t)
	appendMail(t, user, "INBOX", mail("run-a", "one"))
	appendMail(t, user, "INBOX", mail("run-b", "two"))

	c, err := Dial(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	s, ok := c.(HeaderSearcher)
	if !ok {
		t.Fatal("IMAP client does not implement HeaderSearcher")
	}
	got, err := s.FetchByHeader(ctx, "X-Sendplane-Probe", "run-b")
	if err != nil {
		t.Fatalf("FetchByHeader: %v", err)
	}
	if len(got) != 1 || !bytes.Contains(got[0].Raw, []byte("Subject: run-b")) {
		t.Fatalf("FetchByHeader returned %d messages", len(got))
	}
}

func TestIMAPBadLogin(t *testing.T) {
	cfg, _ := startIMAP(t)
	cfg.Password = []byte("wrong")
	if _, err := Dial(context.Background(), cfg, nil); err == nil {
		t.Fatal("Dial with a wrong password succeeded")
	}
}

// staticCipher is a SecretCipher that reverses the bytes, which is enough to
// prove Dial decrypts the stored password.
type staticCipher struct{}

func (staticCipher) Encrypt(_ context.Context, b []byte) ([]byte, error) { return reverse(b), nil }
func (staticCipher) Decrypt(_ context.Context, b []byte) ([]byte, error) { return reverse(b), nil }

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

func TestDialDecryptsPassword(t *testing.T) {
	cfg, _ := startIMAP(t)
	cfg.Password = reverse([]byte("secret"))
	c, err := Dial(context.Background(), cfg, staticCipher{})
	if err != nil {
		t.Fatalf("Dial with cipher: %v", err)
	}
	_ = c.Close()
}

var _ io.Reader = (*literal)(nil)
