// Package mailbox is the IMAP/POP3 client the bounce poller (architecture 10)
// and the loopback probe (architecture 11.1) both read mail with.
//
// It is deliberately small and protocol-agnostic: fetch a batch of whole
// messages, acknowledge the ones that were handled, close. Everything above it
// works on [Message] values, so a consumer can be tested against [Fake]
// without a network.
//
// The package does not import the root sendplane package. The SecretCipher it
// decrypts mailbox passwords with comes from the leaf package host, like it
// does for internal/sender.
package mailbox

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// Protocol selects the implementation Dial returns.
type Protocol string

const (
	ProtocolIMAP Protocol = "imap"
	ProtocolPOP3 Protocol = "pop3"
)

// Action is what happens to a message after it was handled. It is a string
// rather than an enum because "move" carries its destination folder:
//
//	keep          leave it where it is (IMAP: mark it \Seen)
//	delete        delete it
//	move:Handled  move it to the named folder (IMAP only)
type Action string

const (
	// ActionKeep leaves the message in the mailbox. On IMAP it still marks it
	// \Seen, which is what keeps Fetch from returning it forever; POP3 has no
	// such flag, so a kept message comes back on the next poll and the
	// consumer has to be idempotent.
	ActionKeep Action = "keep"
	// ActionDelete deletes the message.
	ActionDelete Action = "delete"
	// MovePrefix starts a move action: "move:<folder>".
	MovePrefix = "move:"
)

// ParseAction validates an AfterProcess string. An empty string means keep.
func ParseAction(s string) (Action, error) {
	switch {
	case s == "":
		return ActionKeep, nil
	case s == string(ActionKeep), s == string(ActionDelete):
		return Action(s), nil
	case strings.HasPrefix(s, MovePrefix):
		if strings.TrimSpace(s[len(MovePrefix):]) == "" {
			return "", fmt.Errorf("mailbox: %q has no destination folder", s)
		}
		return Action(s), nil
	default:
		return "", fmt.Errorf("mailbox: unknown after-process action %q", s)
	}
}

// MoveFolder returns the destination of a move action.
func (a Action) MoveFolder() (string, bool) {
	if !strings.HasPrefix(string(a), MovePrefix) {
		return "", false
	}
	return strings.TrimSpace(string(a)[len(MovePrefix):]), true
}

// Message is one fetched mail. ID is the handle Ack takes: an IMAP UID or a
// POP3 UIDL, stable for the life of the connection and, on both protocols,
// stable across connections as long as the mailbox is not reset.
type Message struct {
	ID string
	// Raw is the whole message, headers and body, as the server stored it.
	Raw []byte
	// Received is the server-side arrival time (IMAP INTERNALDATE). POP3 has
	// no such thing, so it is the zero time there and the consumer falls back
	// to the Date header or its own clock.
	Received time.Time
}

// Client is one open mailbox connection. It is not safe for concurrent use:
// one poller owns one client.
type Client interface {
	// Fetch returns at most max unhandled messages, oldest first.
	Fetch(ctx context.Context, max int) ([]Message, error)
	// Ack applies action to the messages with those IDs. IDs the server no
	// longer knows are skipped, not an error: another replica may have got
	// there first.
	Ack(ctx context.Context, ids []string, action Action) error
	Close() error
}

// HeaderSearcher is implemented by clients that can search server-side by
// header value. The loopback probe uses it to find its own probe mail
// (IMAP SEARCH HEADER X-Sendplane-Probe, architecture 11.2); POP3 cannot do
// it, so probe mailboxes must be IMAP.
type HeaderSearcher interface {
	FetchByHeader(ctx context.Context, name, value string) ([]Message, error)
}

// Idler is implemented by clients that can block until the mailbox changes
// (IMAP IDLE). A poller uses it instead of sleeping its poll interval when the
// server advertises the capability. It returns when something arrived, when
// timeout elapsed, or when ctx is done - in every case the caller just fetches
// again, so a spurious wake-up is harmless.
type Idler interface {
	Idle(ctx context.Context, timeout time.Duration) error
}

// Config describes one mailbox. It is the shape a store model (a bounce
// mailbox row, a store.ProbeMailbox) is adapted into; the package has no
// opinion about where it is persisted.
type Config struct {
	Protocol Protocol
	Host     string
	Port     int
	TLS      store.TLSMode
	Username string
	// Password is the ciphertext the host's SecretCipher produced, exactly as
	// the store holds it. Dial decrypts it; nothing else in this package ever
	// sees the plaintext.
	Password []byte

	// Folder is the IMAP mailbox to read. Empty means INBOX. POP3 ignores it.
	Folder string
	// AfterProcess is the default Action for handled messages.
	AfterProcess Action

	// TLSConfig overrides the default (ServerName = Host). Tests use it.
	TLSConfig *tls.Config
	// DialTimeout bounds connection setup, Timeout one command. Zero means
	// DefaultDialTimeout / DefaultTimeout.
	DialTimeout time.Duration
	Timeout     time.Duration
	// DialContext replaces net.Dialer, so a test can serve both protocols on
	// a pipe or a loopback listener.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
}

// Defaults.
const (
	DefaultDialTimeout = 30 * time.Second
	DefaultTimeout     = 2 * time.Minute
	DefaultFolder      = "INBOX"
)

// Validate reports whether the config can be dialed.
func (c Config) Validate() error {
	switch c.Protocol {
	case ProtocolIMAP, ProtocolPOP3:
	default:
		return fmt.Errorf("mailbox: unknown protocol %q", c.Protocol)
	}
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("mailbox: no host")
	}
	switch c.TLS {
	case store.TLSNone, store.TLSSTARTTLS, store.TLSImplicit, "":
	default:
		return fmt.Errorf("mailbox: unknown TLS mode %q", c.TLS)
	}
	if c.AfterProcess != "" {
		if _, err := ParseAction(string(c.AfterProcess)); err != nil {
			return err
		}
		if folder, ok := c.AfterProcess.MoveFolder(); ok && c.Protocol == ProtocolPOP3 {
			return fmt.Errorf("mailbox: POP3 cannot move a message to %q", folder)
		}
	}
	return nil
}

func (c Config) withDefaults() Config {
	if c.TLS == "" {
		c.TLS = store.TLSImplicit
	}
	if c.Port == 0 {
		c.Port = defaultPort(c.Protocol, c.TLS)
	}
	if c.Folder == "" {
		c.Folder = DefaultFolder
	}
	if c.AfterProcess == "" {
		c.AfterProcess = ActionKeep
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	return c
}

func defaultPort(p Protocol, mode store.TLSMode) int {
	switch {
	case p == ProtocolIMAP && mode == store.TLSImplicit:
		return 993
	case p == ProtocolIMAP:
		return 143
	case mode == store.TLSImplicit:
		return 995
	default:
		return 110
	}
}

// Addr is host:port.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func (c Config) tlsConfig() *tls.Config {
	if c.TLSConfig != nil {
		return c.TLSConfig.Clone()
	}
	return &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
}

// dialConn opens the TCP (or TLS) connection, honouring ctx.
func (c Config) dialConn(ctx context.Context) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, c.DialTimeout)
	defer cancel()

	dial := c.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(dialCtx, "tcp", c.Addr())
	if err != nil {
		return nil, fmt.Errorf("mailbox: dial %s: %w", c.Addr(), err)
	}
	if c.TLS != store.TLSImplicit {
		return conn, nil
	}
	tc := tls.Client(conn, c.tlsConfig())
	if err := tc.HandshakeContext(dialCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mailbox: tls handshake %s: %w", c.Addr(), err)
	}
	return tc, nil
}

// Dial opens the mailbox and authenticates. cipher decrypts cfg.Password; a
// nil cipher means the password is already plaintext, which is what tests and
// a host that stores secrets outside sendplane use.
func Dial(ctx context.Context, cfg Config, cipher host.SecretCipher) (Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	password := cfg.Password
	if cipher != nil && len(password) > 0 {
		plain, err := cipher.Decrypt(ctx, password)
		if err != nil {
			return nil, fmt.Errorf("mailbox: decrypt password for %s: %w", cfg.Username, err)
		}
		password = plain
	}

	switch cfg.Protocol {
	case ProtocolIMAP:
		return dialIMAP(ctx, cfg, string(password))
	default:
		return dialPOP3(ctx, cfg, string(password))
	}
}

// deadline turns a context into a connection deadline, so that a command
// cannot outlive its caller on protocols whose client has no context support.
func deadline(ctx context.Context, timeout time.Duration) time.Time {
	d, ok := ctx.Deadline()
	if fallback := time.Now().Add(timeout); !ok || fallback.Before(d) {
		return fallback
	}
	return d
}
