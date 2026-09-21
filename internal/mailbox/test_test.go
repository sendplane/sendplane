package mailbox

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func TestTestIMAPOK(t *testing.T) {
	cfg, user := startIMAP(t, "Junk")
	appendMail(t, user, "INBOX", mail("one", "hello"))
	appendMail(t, user, "INBOX", mail("two", "hello"))
	cfg.ExtraFolders = []string{"Junk"}

	res := Test(context.Background(), cfg, nil)
	if !res.OK || res.Stage != StageOK {
		t.Fatalf("Test = %+v, want ok", res)
	}
	if res.Latency <= 0 {
		t.Error("Latency was not measured")
	}
	inbox, ok := res.Folders["INBOX"]
	if !ok || !inbox.Exists || inbox.Messages != 2 {
		t.Errorf("INBOX = %+v, want 2 messages", inbox)
	}
	junk, ok := res.Folders["Junk"]
	if !ok || !junk.Exists || junk.Messages != 0 {
		t.Errorf("Junk = %+v, want an empty existing folder", junk)
	}
	if !strings.Contains(res.Server, "IMAP4rev2") {
		t.Errorf("Server = %q, want the capabilities", res.Server)
	}
	if res.Error != "" {
		t.Errorf("Error = %q on a successful test", res.Error)
	}
}

// A rejected LOGIN is an auth failure, not a network failure: that difference
// is the whole point of the stage (store.MailboxHealth).
func TestTestIMAPBadPassword(t *testing.T) {
	cfg, _ := startIMAP(t)
	cfg.Password = []byte("wrong")

	res := Test(context.Background(), cfg, nil)
	if res.OK {
		t.Fatal("a wrong password passed the test")
	}
	if res.Stage != StageAuth {
		t.Fatalf("Stage = %q, want %q (result %+v)", res.Stage, StageAuth, res)
	}
	if res.Error == "" {
		t.Error("an auth failure carried no error text")
	}
}

func TestTestIMAPMissingFolder(t *testing.T) {
	cfg, _ := startIMAP(t)
	cfg.ExtraFolders = []string{"[Gmail]/Spam"}

	res := Test(context.Background(), cfg, nil)
	if res.OK || res.Stage != StageFolder {
		t.Fatalf("Test = %+v, want a folder failure", res)
	}
	if info := res.Folders["[Gmail]/Spam"]; info.Exists {
		t.Errorf("the missing folder is reported as existing: %+v", info)
	}
	if info := res.Folders["INBOX"]; !info.Exists {
		t.Errorf("INBOX should still have been inspected: %+v", info)
	}
}

// Nothing listening is a dial failure, whatever the protocol.
func TestTestDialFailure(t *testing.T) {
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close() // the port is now free, so a dial is refused at once

	for _, proto := range []Protocol{ProtocolIMAP, ProtocolPOP3} {
		cfg := Config{
			Protocol: proto, Host: host, Port: atoi(port), TLS: store.TLSNone,
			Username: "u", Password: []byte("p"), Timeout: 2 * time.Second,
			DialTimeout: 2 * time.Second,
		}
		res := Test(ctx, cfg, nil)
		if res.OK || res.Stage != StageDial {
			t.Errorf("%s: Test = %+v, want a dial failure", proto, res)
		}
	}
}

// An implicit-TLS config pointed at a plaintext server fails the handshake,
// which is a different fix from a wrong password.
func TestTestTLSFailure(t *testing.T) {
	cfg, _ := startIMAP(t)
	cfg.TLS = store.TLSImplicit

	res := Test(context.Background(), cfg, nil)
	if res.OK || res.Stage != StageTLS {
		t.Fatalf("Test = %+v, want a tls failure", res)
	}
}

func TestTestPOP3OK(t *testing.T) {
	cfg, _ := startPOP3(t, "From: a@example.com\r\n\r\none\r\n", "From: b@example.com\r\n\r\ntwo\r\n")

	res := Test(context.Background(), cfg, nil)
	if !res.OK || res.Stage != StageOK {
		t.Fatalf("Test = %+v, want ok", res)
	}
	// POP3 has no folders; STAT is reported under the configured name.
	if info := res.Folders[DefaultFolder]; !info.Exists || info.Messages != 2 {
		t.Errorf("STAT = %+v, want 2 messages", info)
	}
	if !strings.Contains(res.Server, "pop3") {
		t.Errorf("Server = %q, want the greeting", res.Server)
	}
}

func TestTestPOP3BadPassword(t *testing.T) {
	cfg, _ := startPOP3(t)
	cfg.Password = []byte("nope")

	res := Test(context.Background(), cfg, nil)
	if res.OK || res.Stage != StageAuth {
		t.Fatalf("Test = %+v, want an auth failure", res)
	}
	if !strings.Contains(res.Error, "bad password") {
		t.Errorf("Error = %q, want the server's own wording", res.Error)
	}
}

// The stored password is ciphertext; Test decrypts it exactly as Dial does.
func TestTestDecryptsPassword(t *testing.T) {
	cfg, _ := startIMAP(t)
	cfg.Password = reverse([]byte("secret"))

	res := Test(context.Background(), cfg, staticCipher{})
	if !res.OK {
		t.Fatalf("Test with a cipher = %+v", res)
	}
}

// A config that cannot be dialed at all is answered, not returned as an error:
// a handler has one shape to render either way.
func TestTestBadConfig(t *testing.T) {
	res := Test(context.Background(), Config{Protocol: "smtp", Host: "h"}, nil)
	if res.OK || res.Stage != StageConfig {
		t.Fatalf("Test = %+v, want a config failure", res)
	}
}

// Dial tags its failures with the same stages Test reports, so the pollers -
// which only ever see an error - record the same health a manual test would.
func TestDialErrorCarriesStage(t *testing.T) {
	ctx := context.Background()

	cfg, _ := startIMAP(t)
	cfg.Password = []byte("wrong")
	if _, err := Dial(ctx, cfg, nil); StageOf(err) != StageAuth {
		t.Errorf("imap bad password: stage %q, want %q (%v)", StageOf(err), StageAuth, err)
	}

	cfg, _ = startIMAP(t)
	cfg.Folder = "NoSuchFolder"
	if _, err := Dial(ctx, cfg, nil); StageOf(err) != StageFolder {
		t.Errorf("imap missing folder: stage %q, want %q (%v)", StageOf(err), StageFolder, err)
	}

	cfg, _ = startIMAP(t)
	cfg.TLS = store.TLSImplicit
	if _, err := Dial(ctx, cfg, nil); StageOf(err) != StageTLS {
		t.Errorf("imap tls: stage %q, want %q (%v)", StageOf(err), StageTLS, err)
	}

	pcfg, _ := startPOP3(t)
	pcfg.Password = []byte("nope")
	if _, err := Dial(ctx, pcfg, nil); StageOf(err) != StageAuth {
		t.Errorf("pop3 bad password: stage %q, want %q (%v)", StageOf(err), StageAuth, err)
	}

	if _, err := Dial(ctx, Config{Protocol: "smtp", Host: "h"}, nil); StageOf(err) != StageConfig {
		t.Errorf("bad config: stage %q, want %q (%v)", StageOf(err), StageConfig, err)
	}
}
