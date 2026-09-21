package mailbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sendplane/sendplane/store"
)

// pop3Mailbox is a minimal POP3 client (RFC 1939 + STLS from RFC 2595).
//
// It is written here rather than pulled in because the protocol is a dozen
// commands and the available libraries stop short of what a poller needs: no
// context or deadline plumbing, no STARTTLS, and no way to pass a tls.Config
// (only "skip verification"), which is exactly the knob a mailbox with a
// private CA needs. See README.
//
// POP3 has no concept of a handled message: a session either deletes a message
// or leaves it for the next poll. With AfterProcess keep, every poll therefore
// re-delivers everything, and the consumer has to be idempotent - which the
// bounce processor is, because MarkBounced only transitions a delivery once.
type pop3Mailbox struct {
	cfg  Config
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	// nums maps the UIDL of the last Fetch to its message number, which is
	// the only handle DELE takes and which is only valid in this session.
	nums   map[string]int
	closed bool
}

var _ Client = (*pop3Mailbox)(nil)

// ErrNoFolders is returned when a folder operation is asked of POP3.
var ErrNoFolders = errors.New("mailbox: POP3 has no folders")

func dialPOP3(ctx context.Context, cfg Config, password string) (Client, error) {
	conn, err := cfg.dialConn(ctx)
	if err != nil {
		return nil, err
	}
	m := &pop3Mailbox{cfg: cfg, conn: conn, nums: map[string]int{}}
	m.reset(conn)

	_ = conn.SetDeadline(deadline(ctx, cfg.DialTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	if _, err := m.readStatus(); err != nil {
		_ = conn.Close()
		return nil, stageErr(StageDial, fmt.Errorf("mailbox: pop3 greeting %s: %w", cfg.Addr(), err))
	}
	if cfg.TLS == store.TLSSTARTTLS {
		if _, err := m.cmd("STLS"); err != nil {
			_ = conn.Close()
			return nil, stageErr(StageTLS, fmt.Errorf("mailbox: pop3 stls %s: %w", cfg.Addr(), err))
		}
		tc := tls.Client(conn, cfg.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, stageErr(StageTLS, fmt.Errorf("mailbox: pop3 tls handshake %s: %w", cfg.Addr(), err))
		}
		m.conn = tc
		m.reset(tc)
		_ = tc.SetDeadline(deadline(ctx, cfg.DialTimeout))
	}
	if _, err := m.cmd("USER " + cfg.Username); err != nil {
		_ = m.conn.Close()
		return nil, stageErr(loginStage(err), fmt.Errorf("mailbox: pop3 user %s: %w", cfg.Username, err))
	}
	if _, err := m.cmd("PASS " + password); err != nil {
		_ = m.conn.Close()
		return nil, stageErr(loginStage(err), fmt.Errorf("mailbox: pop3 login %s: %w", cfg.Username, err))
	}
	return m, nil
}

func (m *pop3Mailbox) reset(conn net.Conn) {
	m.r = bufio.NewReader(conn)
	m.w = bufio.NewWriter(conn)
}

// Fetch lists the mailbox with UIDL and retrieves the first max messages in
// arrival order.
func (m *pop3Mailbox) Fetch(ctx context.Context, max int) ([]Message, error) {
	_ = m.conn.SetDeadline(deadline(ctx, m.cfg.Timeout))
	defer func() { _ = m.conn.SetDeadline(time.Time{}) }()

	lines, err := m.multi("UIDL")
	if err != nil {
		return nil, err
	}
	type entry struct {
		num int
		uid string
	}
	entries := make([]entry, 0, len(lines))
	for _, line := range lines {
		num, uid, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		entries = append(entries, entry{num: n, uid: strings.TrimSpace(uid)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].num < entries[j].num })
	if max > 0 && len(entries) > max {
		entries = entries[:max]
	}

	out := make([]Message, 0, len(entries))
	for _, e := range entries {
		raw, err := m.retr(e.num)
		if err != nil {
			return out, err
		}
		m.nums[e.uid] = e.num
		// POP3 exposes no arrival time; Received stays zero and the consumer
		// falls back to the Date header.
		out = append(out, Message{ID: e.uid, Raw: raw})
	}
	return out, ctx.Err()
}

// Ack deletes the handled messages. The deletions only take effect when the
// session ends with QUIT, which Close sends.
func (m *pop3Mailbox) Ack(ctx context.Context, ids []string, action Action) error {
	if len(ids) == 0 || action == ActionKeep {
		return nil
	}
	if _, ok := action.MoveFolder(); ok {
		return ErrNoFolders
	}
	if action != ActionDelete {
		return fmt.Errorf("mailbox: unknown after-process action %q", action)
	}
	_ = m.conn.SetDeadline(deadline(ctx, m.cfg.Timeout))
	defer func() { _ = m.conn.SetDeadline(time.Time{}) }()

	for _, id := range ids {
		num, ok := m.nums[id]
		if !ok {
			// Another replica got there first, or the session was recycled:
			// the message number is meaningless now, so skip it.
			continue
		}
		if _, err := m.cmd("DELE " + strconv.Itoa(num)); err != nil {
			return err
		}
		delete(m.nums, id)
	}
	return nil
}

// Close sends QUIT, which is what commits the deletions, and closes the
// connection either way.
func (m *pop3Mailbox) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	_ = m.conn.SetDeadline(time.Now().Add(m.cfg.Timeout))
	_, quitErr := m.cmd("QUIT")
	closeErr := m.conn.Close()
	if quitErr != nil {
		return quitErr
	}
	return closeErr
}

// --- protocol ----------------------------------------------------------

func (m *pop3Mailbox) send(line string) error {
	if _, err := m.w.WriteString(line + "\r\n"); err != nil {
		return fmt.Errorf("mailbox: pop3 write: %w", err)
	}
	if err := m.w.Flush(); err != nil {
		return fmt.Errorf("mailbox: pop3 flush: %w", err)
	}
	return nil
}

// ServerError is a command the server itself refused: an IMAP tagged NO/BAD
// or a POP3 -ERR. It is told apart from an I/O error so that Test can report
// "the password is wrong" rather than "something went wrong"; nothing else in
// the package branches on it.
type ServerError struct {
	// Text is the server's own wording, without the status token.
	Text string
}

func (e *ServerError) Error() string { return "mailbox: server refused: " + e.Text }

// readStatus reads one +OK/-ERR status line.
func (m *pop3Mailbox) readStatus() (string, error) {
	line, err := m.r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("mailbox: pop3 read: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "+OK"):
		return strings.TrimSpace(strings.TrimPrefix(line, "+OK")), nil
	case strings.HasPrefix(line, "-ERR"):
		return "", &ServerError{Text: strings.TrimSpace(strings.TrimPrefix(line, "-ERR"))}
	default:
		return "", fmt.Errorf("mailbox: pop3 unexpected response %q", line)
	}
}

func (m *pop3Mailbox) cmd(line string) (string, error) {
	if err := m.send(line); err != nil {
		return "", err
	}
	return m.readStatus()
}

// multi runs a command whose response is a dot-terminated list of lines.
func (m *pop3Mailbox) multi(command string) ([]string, error) {
	if _, err := m.cmd(command); err != nil {
		return nil, err
	}
	var out []string
	for {
		line, err := m.r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("mailbox: pop3 read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "." {
			return out, nil
		}
		out = append(out, unstuff(line))
	}
}

// retr downloads one message. The body is kept byte for byte, CRLF and all,
// because the DKIM signature of a returned original message depends on it.
func (m *pop3Mailbox) retr(num int) ([]byte, error) {
	if _, err := m.cmd("RETR " + strconv.Itoa(num)); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for {
		line, err := m.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("mailbox: pop3 truncated message %d", num)
			}
			return nil, fmt.Errorf("mailbox: pop3 read: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return buf.Bytes(), nil
		}
		buf.WriteString(unstuff(trimmed))
		buf.WriteString("\r\n")
	}
}

// unstuff undoes the byte-stuffing of RFC 1939 section 3: a line that starts
// with "." was sent with an extra one.
func unstuff(line string) string {
	if strings.HasPrefix(line, "..") {
		return line[1:]
	}
	return line
}
