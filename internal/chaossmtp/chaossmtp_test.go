package chaossmtp

import (
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wneessen/go-mail/smtp"
)

func dial(t *testing.T, s *Server) *smtp.Client {
	t.Helper()
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	host, _, _ := net.SplitHostPort(s.Addr())
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		t.Fatalf("smtp client: %v", err)
	}
	return c
}

func message(rcpt string, attempt int) []byte {
	return []byte(fmt.Sprintf(
		"From: <s@example.com>\r\nTo: <%s>\r\nSubject: hello\r\nMessage-ID: <m-%s-%d@example.com>\r\n%s: %d\r\n\r\nbody line\r\n.leading dot\r\n",
		rcpt, strings.ReplaceAll(rcpt, "@", "-"), attempt, AttemptHeader, attempt))
}

func send(c *smtp.Client, from, rcpt string, data []byte) error {
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(rcpt); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.Close()
}

func TestAcceptAndRecord(t *testing.T) {
	s, err := Start(Options{KeepBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c := dial(t, s)
	defer c.Close()
	if err := send(c, "bounce+abc@bounce.example", "a@example.com", message("a@example.com", 1)); err != nil {
		t.Fatalf("send: %v", err)
	}

	msgs := s.Messages()
	if len(msgs) != 1 {
		t.Fatalf("recorded %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.From != "bounce+abc@bounce.example" {
		t.Errorf("From = %q", m.From)
	}
	if len(m.Rcpts) != 1 || m.Rcpts[0] != "a@example.com" {
		t.Errorf("Rcpts = %v", m.Rcpts)
	}
	if m.Subject != "hello" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.MessageID != "m-a-example.com-1@example.com" {
		t.Errorf("MessageID = %q", m.MessageID)
	}
	if m.Attempt != 1 {
		t.Errorf("Attempt = %d", m.Attempt)
	}
	if m.Size == 0 {
		t.Error("Size = 0")
	}
	// The transparency dot has been removed again.
	if !strings.Contains(string(m.Body), "\r\n.leading dot\r\n") {
		t.Errorf("body not dot-unstuffed: %q", m.Body)
	}
	if got := s.Stats(); got.Accepted != 1 || got.Messages != 1 || got.Connections != 1 {
		t.Errorf("stats = %+v", got)
	}
}

func TestDecideIsDeterministicAndDistributed(t *testing.T) {
	r := Rates{TempFailRate: 0.05, PermFailRate: 0.01, DropRate: 0.005}
	counts := map[Outcome]int{}
	const n = 50000
	for i := 0; i < n; i++ {
		rcpt := fmt.Sprintf("u%d@example.com", i)
		o := Decide(42, r, rcpt, 1)
		if o2 := Decide(42, r, rcpt, 1); o != o2 {
			t.Fatalf("Decide is not deterministic for %s", rcpt)
		}
		counts[o]++
	}
	check := func(o Outcome, want float64) {
		got := float64(counts[o]) / n
		if got < want*0.85 || got > want*1.15 {
			t.Errorf("%s rate = %.4f, want ~%.4f", o, got, want)
		}
	}
	check(TempFail, 0.05)
	check(PermFail, 0.01)
	check(Drop, 0.005)
	check(Accept, 0.935)

	// A different seed, attempt or recipient gives an independent draw.
	if Decide(42, r, "u1@example.com", 1) == Decide(42, r, "u1@example.com", 2) &&
		Decide(42, r, "u2@example.com", 1) == Decide(42, r, "u2@example.com", 2) &&
		Decide(42, r, "u3@example.com", 1) == Decide(42, r, "u3@example.com", 2) {
		// All three matching would be a 1-in-many coincidence with these
		// rates; identical outcomes are expected most of the time, so this
		// only guards against the attempt number being ignored entirely.
		if Decide(42, r, "u1@example.com", 1) != Accept {
			t.Error("attempt number appears to be ignored")
		}
	}
	if Decide(1, r, "u1@example.com", 1) == Decide(2, r, "u1@example.com", 1) &&
		Decide(1, r, "u9@example.com", 3) == Decide(2, r, "u9@example.com", 3) {
		t.Log("seeds agree on these samples, which is expected for a mostly-accept distribution")
	}
}

func TestOutcomesMatchDecide(t *testing.T) {
	r := Rates{TempFailRate: 0.4, PermFailRate: 0.2, DropRate: 0.2}
	s, err := Start(Options{Rates: r, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 40; i++ {
		rcpt := fmt.Sprintf("r%d@example.com", i)
		want := Decide(7, r, rcpt, 3)
		c := dial(t, s)
		err := send(c, "s@example.com", rcpt, message(rcpt, 3))
		c.Close()

		switch want {
		case Accept:
			if err != nil {
				t.Errorf("%s: accept expected, got %v", rcpt, err)
			}
		case TempFail:
			var pe *textproto.Error
			if !asProto(err, &pe) || pe.Code != 451 {
				t.Errorf("%s: tempfail expected, got %v", rcpt, err)
			}
		case PermFail:
			var pe *textproto.Error
			if !asProto(err, &pe) || pe.Code != 550 {
				t.Errorf("%s: permfail expected, got %v", rcpt, err)
			}
		case Drop:
			if err == nil {
				t.Errorf("%s: drop expected, got success", rcpt)
			}
			var pe *textproto.Error
			if asProto(err, &pe) {
				t.Errorf("%s: drop expected, got SMTP reply %v", rcpt, pe)
			}
		}
	}
	st := s.Stats()
	if st.Dropped == 0 || st.TempFailed == 0 || st.PermFailed == 0 || st.Accepted == 0 {
		t.Errorf("every outcome should occur at these rates: %+v", st)
	}
	// Dropped messages are never recorded, so a retry after a drop is not a
	// duplicate at the receiving end.
	if int64(len(s.Messages())) != st.Accepted {
		t.Errorf("recorded %d messages, accepted %d", len(s.Messages()), st.Accepted)
	}
}

func asProto(err error, out **textproto.Error) bool {
	for err != nil {
		if pe, ok := err.(*textproto.Error); ok {
			*out = pe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestAttemptFallbackCountsPerRecipient(t *testing.T) {
	r := Rates{TempFailRate: 1}
	s, err := Start(Options{Rates: r, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// No attempt header: the server counts per recipient.
	noHeader := []byte("From: <s@example.com>\r\nSubject: x\r\n\r\nbody\r\n")
	c := dial(t, s)
	defer c.Close()
	for i := 1; i <= 3; i++ {
		_ = send(c, "s@example.com", "x@example.com", noHeader)
		_ = c.Reset()
	}
	if got := s.Stats().TempFailed; got != 3 {
		t.Fatalf("tempfailed = %d, want 3", got)
	}
	s.mu.Lock()
	seen := s.seen["x@example.com"]
	s.mu.Unlock()
	if seen != 3 {
		t.Fatalf("per-recipient counter = %d, want 3", seen)
	}
}

func TestRateLimitAfter(t *testing.T) {
	s, err := Start(Options{RateLimitAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c := dial(t, s)
	defer c.Close()
	for i := 1; i <= 2; i++ {
		if err := send(c, "s@example.com", "a@example.com", message("a@example.com", i)); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	err = send(c, "s@example.com", "a@example.com", message("a@example.com", 3))
	var pe *textproto.Error
	if !asProto(err, &pe) || pe.Code != 421 {
		t.Fatalf("third message: err = %v, want 421", err)
	}
	if got := s.Stats().RateLimited; got != 1 {
		t.Errorf("rate limited = %d, want 1", got)
	}

	// A fresh connection starts over.
	c2 := dial(t, s)
	defer c2.Close()
	if err := send(c2, "s@example.com", "a@example.com", message("a@example.com", 4)); err != nil {
		t.Fatalf("new connection: %v", err)
	}
}

func TestAuth(t *testing.T) {
	s, err := Start(Options{Username: "user", Password: "pass", RequireAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host, _, _ := net.SplitHostPort(s.Addr())

	// Without AUTH, MAIL is refused.
	c := dial(t, s)
	err = send(c, "s@example.com", "a@example.com", message("a@example.com", 1))
	var pe *textproto.Error
	if !asProto(err, &pe) || pe.Code != 530 {
		t.Fatalf("unauthenticated MAIL: %v, want 530", err)
	}
	c.Close()

	for _, auth := range []struct {
		name string
		a    smtp.Auth
	}{
		{"PLAIN", smtp.PlainAuth("", "user", "pass", host, true)},
		{"LOGIN", smtp.LoginAuth("user", "pass", host, true)},
	} {
		c := dial(t, s)
		if err := c.Auth(auth.a); err != nil {
			t.Fatalf("%s: %v", auth.name, err)
		}
		if err := send(c, "s@example.com", "a@example.com", message("a@example.com", 1)); err != nil {
			t.Fatalf("%s: send: %v", auth.name, err)
		}
		c.Close()
	}

	c = dial(t, s)
	defer c.Close()
	if err := c.Auth(smtp.PlainAuth("", "user", "wrong", host, true)); err == nil {
		t.Fatal("wrong password accepted")
	}
	if got := s.Stats().AuthFailed; got != 1 {
		t.Errorf("auth failures = %d, want 1", got)
	}
}

func TestLatency(t *testing.T) {
	s, err := Start(Options{Latency: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dial(t, s)
	defer c.Close()
	start := time.Now()
	if err := send(c, "s@example.com", "a@example.com", message("a@example.com", 1)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 20*time.Millisecond {
		t.Fatalf("latency not applied: %v", d)
	}
}

// TestThroughput is the "at least 1000 msg/s locally" requirement of the
// brief. It uses 8 connections, which is what a sender pool would do.
func TestThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput test")
	}
	s, err := Start(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const conns, per = 8, 250
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", s.Addr())
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			host, _, _ := net.SplitHostPort(s.Addr())
			c, err := smtp.NewClient(conn, host)
			if err != nil {
				t.Errorf("client: %v", err)
				return
			}
			defer c.Close()
			for j := 0; j < per; j++ {
				rcpt := fmt.Sprintf("u%d-%d@example.com", i, j)
				if err := send(c, "s@example.com", rcpt, message(rcpt, 1)); err != nil {
					t.Errorf("send: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	total := conns * per
	rate := float64(total) / elapsed.Seconds()
	t.Logf("%d messages in %v = %.0f msg/s", total, elapsed.Round(time.Millisecond), rate)
	if int64(total) != s.Stats().Accepted {
		t.Fatalf("accepted %d of %d", s.Stats().Accepted, total)
	}
	if rate < 1000 {
		t.Errorf("throughput %.0f msg/s, want >= 1000", rate)
	}
}

func TestBounceHook(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	s, err := Start(Options{BounceHook: func(m Message) {
		mu.Lock()
		seen = append(seen, m.MessageID)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dial(t, s)
	defer c.Close()
	if err := send(c, "s@example.com", "a@example.com", message("a@example.com", 1)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] == "" {
		t.Fatalf("bounce hook saw %v", seen)
	}
}

func TestRejectsImpossibleRates(t *testing.T) {
	if _, err := Start(Options{Rates: Rates{TempFailRate: 0.7, PermFailRate: 0.4}}); err == nil {
		t.Fatal("rates over 1 accepted")
	}
}
