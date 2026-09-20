package mailbox

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

// pop3Server is a scripted RFC 1939 server: enough of the protocol to drive
// the client, and a record of what it was asked to delete.
type pop3Server struct {
	mu       sync.Mutex
	msgs     []string // index 0 is message 1
	deleted  []int    // message numbers DELE'd, in order
	quitSeen bool
}

func (s *pop3Server) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, format+"\r\n", args...)
		_ = w.Flush()
	}
	write("+OK sendplane test pop3 ready")

	marked := map[int]bool{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(cmd, " ")
		switch strings.ToUpper(verb) {
		case "USER":
			write("+OK")
		case "PASS":
			if arg != "secret" {
				write("-ERR bad password")
				continue
			}
			write("+OK logged in")
		case "STAT":
			write("+OK %d 1024", len(s.msgs))
		case "UIDL":
			write("+OK")
			s.mu.Lock()
			for i := range s.msgs {
				if !marked[i+1] {
					_, _ = fmt.Fprintf(w, "%d uid-%d\r\n", i+1, i+1)
				}
			}
			s.mu.Unlock()
			write(".")
		case "RETR":
			n, _ := strconv.Atoi(arg)
			s.mu.Lock()
			body := ""
			if n >= 1 && n <= len(s.msgs) {
				body = s.msgs[n-1]
			}
			s.mu.Unlock()
			if body == "" {
				write("-ERR no such message")
				continue
			}
			write("+OK %d octets", len(body))
			for _, l := range strings.Split(strings.TrimSuffix(body, "\r\n"), "\r\n") {
				if strings.HasPrefix(l, ".") {
					l = "." + l // byte-stuffing, RFC 1939 section 3
				}
				_, _ = fmt.Fprintf(w, "%s\r\n", l)
			}
			write(".")
		case "DELE":
			n, _ := strconv.Atoi(arg)
			marked[n] = true
			s.mu.Lock()
			s.deleted = append(s.deleted, n)
			s.mu.Unlock()
			write("+OK marked")
		case "NOOP":
			write("+OK")
		case "QUIT":
			s.mu.Lock()
			s.quitSeen = true
			s.mu.Unlock()
			write("+OK bye")
			return
		default:
			write("-ERR unknown command")
		}
	}
}

func startPOP3(t *testing.T, msgs ...string) (Config, *pop3Server) {
	t.Helper()
	srv := &pop3Server{msgs: msgs}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	return Config{
		Protocol: ProtocolPOP3, Host: host, Port: atoi(port), TLS: store.TLSNone,
		Username: "bounce@example.com", Password: []byte("secret"),
		Timeout: 5 * time.Second,
	}, srv
}

const dotBody = "From: a@example.com\r\nSubject: dsn\r\n\r\nline one\r\n.hidden dot\r\nline three\r\n"

func TestPOP3FetchAndDelete(t *testing.T) {
	ctx := context.Background()
	cfg, srv := startPOP3(t, "From: a@example.com\r\n\r\none\r\n", dotBody, "From: c@example.com\r\n\r\nthree\r\n")

	c, err := Dial(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	msgs, err := c.Fetch(ctx, 2)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Fetch returned %d messages, want 2", len(msgs))
	}
	if msgs[0].ID != "uid-1" || msgs[1].ID != "uid-2" {
		t.Fatalf("IDs are %q, %q", msgs[0].ID, msgs[1].ID)
	}
	// The dot-stuffed line must come back exactly as it was stored.
	if string(msgs[1].Raw) != dotBody {
		t.Fatalf("un-stuffing failed:\n got %q\nwant %q", msgs[1].Raw, dotBody)
	}

	if err := c.Ack(ctx, []string{"uid-1"}, ActionDelete); err != nil {
		t.Fatalf("Ack delete: %v", err)
	}
	// keep sends nothing at all.
	if err := c.Ack(ctx, []string{"uid-2"}, ActionKeep); err != nil {
		t.Fatalf("Ack keep: %v", err)
	}
	// An ID from an earlier session is skipped, not an error.
	if err := c.Ack(ctx, []string{"uid-99"}, ActionDelete); err != nil {
		t.Fatalf("Ack unknown: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.deleted) != 1 || srv.deleted[0] != 1 {
		t.Fatalf("server saw DELE %v, want [1]", srv.deleted)
	}
	if !srv.quitSeen {
		t.Fatal("Close did not send QUIT, so the deletions were never committed")
	}
}

func TestPOP3MoveUnsupported(t *testing.T) {
	ctx := context.Background()
	cfg, _ := startPOP3(t, "From: a@example.com\r\n\r\none\r\n")
	c, err := Dial(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Fetch(ctx, 10); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := c.Ack(ctx, []string{"uid-1"}, Action("move:Done")); err == nil {
		t.Fatal("POP3 accepted a move action")
	}
}

func TestPOP3BadLogin(t *testing.T) {
	cfg, _ := startPOP3(t)
	cfg.Password = []byte("nope")
	if _, err := Dial(context.Background(), cfg, nil); err == nil {
		t.Fatal("Dial with a wrong password succeeded")
	}
}
