// Package chaossmtp is an in-process ESMTP server that fails on purpose.
//
// It exists so the sender's retry, classification and rate-limit behaviour can
// be tested against something that behaves like a hostile relay, without a
// container or a network. Every failure decision is a pure function of
// (seed, recipient, attempt number), so a test can compute the expected
// outcome of a whole campaign with the same function the server used:
//
//	Decide(seed, rates, rcpt, attemptNo)
//
// The attempt number comes from the X-Sendplane-Attempt header the sender puts
// on outgoing mail; when it is absent the server counts messages per recipient
// instead, which keeps the sequence deterministic for clients that do not set
// it.
//
// The server speaks the subset of RFC 5321 that the sender uses: EHLO/HELO,
// AUTH PLAIN and LOGIN, MAIL, RCPT, DATA, RSET, NOOP, QUIT. It is plaintext
// only; see README for why STARTTLS is not implemented yet.
package chaossmtp

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Outcome is what the server does with one message.
type Outcome int

const (
	// Accept replies 250 and records the message.
	Accept Outcome = iota
	// TempFail replies 451 4.3.0 at end of DATA.
	TempFail
	// PermFail replies 550 5.2.0 at end of DATA.
	PermFail
	// Drop closes the connection in the middle of DATA, without a reply. The
	// message is never recorded, so a retry after a drop is not a duplicate.
	Drop
)

var outcomeNames = [...]string{"accept", "tempfail", "permfail", "drop"}

func (o Outcome) String() string {
	if int(o) < len(outcomeNames) {
		return outcomeNames[o]
	}
	return "outcome(" + strconv.Itoa(int(o)) + ")"
}

// Rates are the failure probabilities. They are cumulative in the order
// tempfail, permfail, drop; the rest of the [0,1) space is accepted, so
// 0.05/0.01/0.005 means 5% tempfail, 1% permfail, 0.5% drop, 93.5% accept.
// The sum must not exceed 1.
type Rates struct {
	TempFailRate float64
	PermFailRate float64
	DropRate     float64
}

// AttemptHeader is the header the sender adds so that failures are
// reproducible across retries.
const AttemptHeader = "X-Sendplane-Attempt"

// Options configures Start.
type Options struct {
	// Addr is the listen address; the default is 127.0.0.1:0 (a free port).
	Addr string
	// Hostname is announced in the greeting and the EHLO response.
	Hostname string

	Rates
	// RateLimitAfter makes the server answer 421 and hang up once this many
	// messages have been accepted on one connection. Zero disables it.
	RateLimitAfter int
	// Latency is slept before the end-of-DATA reply of every message.
	Latency time.Duration
	// Seed selects the failure sequence. Two servers with the same seed and
	// rates make the same decisions.
	Seed uint64

	// Username and Password enable AUTH. When Username is empty the server
	// neither advertises nor accepts AUTH.
	Username string
	Password string
	// RequireAuth rejects MAIL before a successful AUTH with 530 5.7.0.
	RequireAuth bool

	// MaxMessageBytes rejects larger messages with 552; the default is 10 MiB.
	MaxMessageBytes int
	// KeepBodies stores the full message body on each recorded Message. Off by
	// default: a 5,000 message test does not need the bytes, only the headers.
	KeepBodies bool

	// KeepMessages bounds how many accepted messages Messages remembers:
	//
	//	 <0  keep every message (the default, and what a test asserts on)
	//	  0  keep none, only the Stats counters
	//	 >0  keep the most recent n, as a ring
	//
	// A million-message load run is what this exists for: the default slice
	// grows without bound and a Message with its headers is not small, so a
	// long run that nobody reads Messages() on would be an out-of-memory bug
	// dressed up as a test double. Zero means "keep every message" only
	// because a zero Options must stay the behaviour every existing test
	// relies on; use KeepNone for "count only".
	KeepMessages int

	// BounceHook is called (synchronously, on the connection goroutine) for
	// every accepted message, so a test can synthesize a DSN for it.
	BounceHook func(Message)
}

// KeepNone is the Options.KeepMessages value that records no message at all,
// leaving only the Stats counters. It is not 0, because a zero-valued Options
// has to keep meaning "remember everything".
const KeepNone = -0x7fffffff

// Message is one accepted message.
type Message struct {
	// From is the envelope sender (the VERP address, when the sender uses one).
	From string
	// Rcpts are the envelope recipients, in RCPT order.
	Rcpts []string
	// Headers are the message headers, canonicalized by textproto.
	Headers textproto.MIMEHeader
	// MessageID, Subject and Attempt are lifted out of Headers for assertions.
	MessageID string
	Subject   string
	Attempt   int
	// Size is the size of the message data in bytes, after dot-unstuffing.
	Size int
	// Body is set only when Options.KeepBodies is true.
	Body       []byte
	ReceivedAt time.Time
}

// Stats counts what the server did. It is a snapshot.
type Stats struct {
	Connections int64
	// Messages is every message that reached end of DATA, including failed
	// ones, but not dropped ones.
	Messages    int64
	Accepted    int64
	TempFailed  int64
	PermFailed  int64
	Dropped     int64
	RateLimited int64
	AuthFailed  int64
}

// Server is a running chaos SMTP server.
type Server struct {
	opts     Options
	ln       net.Listener
	wg       sync.WaitGroup
	closing  atomic.Bool
	stats    stats
	mu       sync.Mutex
	messages []Message
	// seen counts messages per recipient, the fallback attempt number for
	// clients that do not send AttemptHeader.
	seen map[string]int
	// conns are the open client connections. Close hangs them up, so a test
	// that forgets to close a pooled client still terminates.
	conns map[net.Conn]struct{}
}

type stats struct {
	connections, messages, accepted, tempFailed, permFailed atomic.Int64
	dropped, rateLimited, authFailed                        atomic.Int64
}

// Start listens and serves until Close.
func Start(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.Hostname == "" {
		opts.Hostname = "chaos.invalid"
	}
	if opts.MaxMessageBytes == 0 {
		opts.MaxMessageBytes = 10 << 20
	}
	if total := opts.TempFailRate + opts.PermFailRate + opts.DropRate; total > 1 {
		return nil, fmt.Errorf("chaossmtp: failure rates sum to %v, over 1", total)
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("chaossmtp: listen: %w", err)
	}
	s := &Server{opts: opts, ln: ln, seen: map[string]int{}, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Addr is the address the server listens on, with the port resolved.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Port is the resolved TCP port.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Close stops the listener and waits for the open connections to finish.
func (s *Server) Close() error {
	if s.closing.Swap(true) {
		return nil
	}
	err := s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // closed, or unrecoverable
		}
		s.stats.connections.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn)
		}()
	}
}

// Messages returns a copy of the accepted messages it was asked to remember,
// in acceptance order (Options.KeepMessages).
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.messages))
	copy(out, s.messages)
	return out
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{
		Connections: s.stats.connections.Load(),
		Messages:    s.stats.messages.Load(),
		Accepted:    s.stats.accepted.Load(),
		TempFailed:  s.stats.tempFailed.Load(),
		PermFailed:  s.stats.permFailed.Load(),
		Dropped:     s.stats.dropped.Load(),
		RateLimited: s.stats.rateLimited.Load(),
		AuthFailed:  s.stats.authFailed.Load(),
	}
}

// Decide is the failure decision, a pure function of the seed, the rates, the
// recipient and the attempt number. A test computes the expected outcome of a
// campaign by calling it with the same arguments the server will see.
//
// attemptNo starts at 1.
func Decide(seed uint64, r Rates, rcpt string, attemptNo int) Outcome {
	u := uniform(seed, rcpt, attemptNo)
	switch {
	case u < r.TempFailRate:
		return TempFail
	case u < r.TempFailRate+r.PermFailRate:
		return PermFail
	case u < r.TempFailRate+r.PermFailRate+r.DropRate:
		return Drop
	default:
		return Accept
	}
}

// Decide is Decide with the server's own seed and rates.
func (s *Server) Decide(rcpt string, attemptNo int) Outcome {
	return Decide(s.opts.Seed, s.opts.Rates, rcpt, attemptNo)
}

// uniform hashes the decision inputs into [0,1). SHA-256 rather than a
// cheaper hash because the distribution has to be even enough that a 0.5%
// slice is really 0.5%; at a few microseconds per message it is far below the
// per-message cost of the SMTP conversation itself.
func uniform(seed uint64, rcpt string, attemptNo int) float64 {
	// The attempt number only has to make the draw differ between attempts of
	// the same recipient; a negative one would wrap into an enormous uint64
	// and still be deterministic, but 0 is the honest value for "no attempt".
	if attemptNo < 0 {
		attemptNo = 0
	}
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], seed)
	binary.BigEndian.PutUint64(buf[8:16], uint64(attemptNo))
	h := sha256.New()
	h.Write(buf[0:8])
	h.Write([]byte(strings.ToLower(rcpt)))
	h.Write([]byte{0})
	h.Write(buf[8:16])
	sum := h.Sum(nil)
	// 53 bits is the mantissa of a float64: dividing a 53-bit integer by 2^53
	// is exact and uniform on [0,1).
	n := binary.BigEndian.Uint64(sum[:8]) >> 11
	return float64(n) / float64(uint64(1)<<53)
}

// nextAttempt returns the fallback attempt number for a recipient.
func (s *Server) nextAttempt(rcpt string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(rcpt)
	s.seen[k]++
	return s.seen[k]
}

func (s *Server) record(m Message) {
	s.mu.Lock()
	switch n := s.opts.KeepMessages; {
	case n == KeepNone:
		// Counters only; s.messages stays empty.
	case n > 0:
		s.messages = append(s.messages, m)
		if len(s.messages) > n {
			// Drop from the front so Messages() stays "the most recent n in
			// acceptance order". The copy keeps the backing array bounded.
			s.messages = append(s.messages[:0], s.messages[len(s.messages)-n:]...)
		}
	default:
		s.messages = append(s.messages, m)
	}
	s.mu.Unlock()
	if s.opts.BounceHook != nil {
		s.opts.BounceHook(m)
	}
}

// session is the per-connection state.
type session struct {
	s    *Server
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	authed   bool
	from     string
	rcpts    []string
	inTx     bool
	accepted int // messages accepted on this connection
}

func (s *Server) serve(conn net.Conn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()
	ss := &session{
		s:    s,
		conn: conn,
		r:    bufio.NewReaderSize(conn, 4096),
		w:    bufio.NewWriterSize(conn, 4096),
	}
	ss.reply("220 %s chaossmtp ready", s.opts.Hostname)
	for {
		line, err := ss.readLine()
		if err != nil {
			return
		}
		if keepGoing := ss.command(line); !keepGoing {
			return
		}
	}
}

func (ss *session) readLine() (string, error) {
	line, err := ss.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (ss *session) reply(format string, args ...any) {
	_, _ = fmt.Fprintf(ss.w, format+"\r\n", args...)
	_ = ss.w.Flush()
}

// command handles one command line and reports whether the connection stays
// open.
func (ss *session) command(line string) bool {
	verb, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch strings.ToUpper(verb) {
	case "EHLO":
		ss.ehlo()
	case "HELO":
		ss.reset()
		ss.reply("250 %s", ss.s.opts.Hostname)
	case "AUTH":
		return ss.auth(arg)
	case "MAIL":
		ss.mail(arg)
	case "RCPT":
		ss.rcpt(arg)
	case "DATA":
		return ss.data()
	case "RSET":
		ss.reset()
		ss.reply("250 2.0.0 Ok")
	case "NOOP":
		ss.reply("250 2.0.0 Ok")
	case "VRFY", "EXPN", "HELP":
		ss.reply("502 5.5.1 Command not implemented")
	case "STARTTLS":
		// Not advertised; answering 454 keeps a client that tries anyway from
		// hanging (see README).
		ss.reply("454 4.7.0 TLS not available")
	case "QUIT":
		ss.reply("221 2.0.0 Bye")
		return false
	default:
		ss.reply("500 5.5.2 Unrecognized command")
	}
	return true
}

func (ss *session) ehlo() {
	ss.reset()
	lines := []string{
		"250-" + ss.s.opts.Hostname,
		"250-PIPELINING",
		"250-8BITMIME",
		"250-SIZE " + strconv.Itoa(ss.s.opts.MaxMessageBytes),
	}
	if ss.s.opts.Username != "" {
		lines = append(lines, "250-AUTH PLAIN LOGIN")
	}
	lines = append(lines, "250 ENHANCEDSTATUSCODES")
	ss.reply("%s", strings.Join(lines, "\r\n"))
}

func (ss *session) reset() {
	ss.from, ss.rcpts, ss.inTx = "", nil, false
}

func (ss *session) auth(arg string) bool {
	if ss.s.opts.Username == "" {
		ss.reply("503 5.5.1 AUTH not enabled")
		return true
	}
	mech, initial, _ := strings.Cut(arg, " ")
	var user, pass string
	switch strings.ToUpper(mech) {
	case "PLAIN":
		if initial == "" {
			ss.reply("334 ")
			line, err := ss.readLine()
			if err != nil {
				return false
			}
			initial = line
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(initial))
		if err != nil {
			ss.reply("501 5.5.2 Cannot decode response")
			return true
		}
		parts := strings.Split(string(raw), "\x00")
		if len(parts) != 3 {
			ss.reply("501 5.5.2 Malformed PLAIN response")
			return true
		}
		user, pass = parts[1], parts[2]
	case "LOGIN":
		ss.reply("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:")))
		u, err := ss.readLine()
		if err != nil {
			return false
		}
		ss.reply("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:")))
		p, err := ss.readLine()
		if err != nil {
			return false
		}
		ub, err1 := base64.StdEncoding.DecodeString(strings.TrimSpace(u))
		pb, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
		if err1 != nil || err2 != nil {
			ss.reply("501 5.5.2 Cannot decode response")
			return true
		}
		user, pass = string(ub), string(pb)
	default:
		ss.reply("504 5.5.4 Unrecognized authentication type")
		return true
	}
	if user != ss.s.opts.Username || pass != ss.s.opts.Password {
		ss.s.stats.authFailed.Add(1)
		ss.reply("535 5.7.8 Authentication credentials invalid")
		return true
	}
	ss.authed = true
	ss.reply("235 2.7.0 Authentication successful")
	return true
}

func (ss *session) mail(arg string) {
	if ss.s.opts.RequireAuth && !ss.authed {
		ss.reply("530 5.7.0 Authentication required")
		return
	}
	if n := ss.s.opts.RateLimitAfter; n > 0 && ss.accepted >= n {
		ss.s.stats.rateLimited.Add(1)
		ss.reply("421 4.7.0 Too many messages on this connection, try again later")
		return
	}
	addr, ok := bracketAddr(arg, "FROM:")
	if !ok {
		ss.reply("501 5.5.4 Syntax: MAIL FROM:<address>")
		return
	}
	ss.reset()
	ss.from, ss.inTx = addr, true
	ss.reply("250 2.1.0 Ok")
}

func (ss *session) rcpt(arg string) {
	if !ss.inTx {
		ss.reply("503 5.5.1 Need MAIL before RCPT")
		return
	}
	addr, ok := bracketAddr(arg, "TO:")
	if !ok {
		ss.reply("501 5.5.4 Syntax: RCPT TO:<address>")
		return
	}
	ss.rcpts = append(ss.rcpts, addr)
	ss.reply("250 2.1.5 Ok")
}

// bracketAddr parses "FROM:<a@b> PARAM=x" into "a@b". The angle brackets are
// optional: go-mail's smtp client sends a bare address, and real relays accept
// that, so rejecting it would only make the chaos server harder to talk to
// than the thing it stands in for.
func bracketAddr(arg, prefix string) (string, bool) {
	up := strings.ToUpper(arg)
	if !strings.HasPrefix(up, prefix) {
		return "", false
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	if strings.HasPrefix(rest, "<") {
		closeIdx := strings.Index(rest, ">")
		if closeIdx < 0 {
			return "", false
		}
		return rest[1:closeIdx], true
	}
	addr, _, _ := strings.Cut(rest, " ")
	if addr == "" {
		return "", false
	}
	return addr, true
}

// data runs the DATA phase. It reports whether the connection stays open: a
// dropped message closes it without a reply.
func (ss *session) data() bool {
	if !ss.inTx || len(ss.rcpts) == 0 {
		ss.reply("503 5.5.1 Need RCPT before DATA")
		return true
	}
	ss.reply("354 End data with <CR><LF>.<CR><LF>")

	rcpt := ss.rcpts[0]
	var (
		body       []byte
		size       int
		headerText strings.Builder
		inHeaders  = true
		decided    = false
		outcome    Outcome
		attemptNo  int
		tooLarge   bool
	)
	for {
		line, err := ss.readLine()
		if err != nil {
			return false
		}
		if line == "." {
			break
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		size += len(line) + 2
		if size > ss.s.opts.MaxMessageBytes {
			tooLarge = true
		}
		if !tooLarge && ss.s.opts.KeepBodies {
			body = append(append(body, line...), '\r', '\n')
		}
		if inHeaders {
			if line == "" {
				inHeaders = false
			} else if headerText.Len() < 64<<10 {
				headerText.WriteString(line)
				headerText.WriteString("\r\n")
			}
		}
		// The decision is taken as soon as the headers are complete, so that a
		// Drop really closes the connection in the middle of DATA and the
		// message is never recorded.
		if !decided && !inHeaders {
			decided = true
			attemptNo = attemptFrom(headerText.String())
			if attemptNo <= 0 {
				attemptNo = ss.s.nextAttempt(rcpt)
			}
			outcome = ss.s.Decide(rcpt, attemptNo)
			if outcome == Drop {
				ss.s.stats.dropped.Add(1)
				return false
			}
		}
	}
	if !decided {
		// A message with no body separator at all: decide now.
		attemptNo = attemptFrom(headerText.String())
		if attemptNo <= 0 {
			attemptNo = ss.s.nextAttempt(rcpt)
		}
		outcome = ss.s.Decide(rcpt, attemptNo)
		if outcome == Drop {
			ss.s.stats.dropped.Add(1)
			return false
		}
	}

	if ss.s.opts.Latency > 0 {
		time.Sleep(ss.s.opts.Latency)
	}
	ss.s.stats.messages.Add(1)
	rcpts := ss.rcpts
	from := ss.from
	ss.reset()

	if tooLarge {
		ss.s.stats.permFailed.Add(1)
		ss.reply("552 5.3.4 Message too big")
		return true
	}
	switch outcome {
	case TempFail:
		ss.s.stats.tempFailed.Add(1)
		ss.reply("451 4.3.0 Temporary local problem, try again later")
		return true
	case PermFail:
		ss.s.stats.permFailed.Add(1)
		ss.reply("550 5.2.0 Mailbox unavailable")
		return true
	}

	hdr, _ := textproto.NewReader(bufio.NewReader(strings.NewReader(headerText.String() + "\r\n"))).ReadMIMEHeader()
	if hdr == nil {
		hdr = textproto.MIMEHeader{}
	}
	ss.s.record(Message{
		From:       from,
		Rcpts:      rcpts,
		Headers:    hdr,
		MessageID:  strings.Trim(hdr.Get("Message-Id"), "<>"),
		Subject:    hdr.Get("Subject"),
		Attempt:    attemptNo,
		Size:       size,
		Body:       body,
		ReceivedAt: time.Now(),
	})
	ss.accepted++
	ss.s.stats.accepted.Add(1)
	ss.reply("250 2.0.0 Ok: queued")
	return true
}

// attemptFrom reads the attempt number out of the raw header block. It does
// not parse the whole header set, because the decision has to be cheap and the
// header is generated by the sender.
func attemptFrom(headers string) int {
	for _, line := range strings.Split(headers, "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), AttemptHeader) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n <= 0 {
			return 0
		}
		return n
	}
	return 0
}

var _ io.Closer = (*Server)(nil)
