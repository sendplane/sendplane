package sender

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/wneessen/go-mail/smtp"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// DialFunc opens the TCP connection to a transport. Tests replace it.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// PoolConfig configures Pool.
type PoolConfig struct {
	// MaxMsgsPerConn reconnects after this many messages on one connection.
	// Long-lived connections accumulate per-connection limits on the far side
	// (architecture 8.1).
	MaxMsgsPerConn int
	// IdleTimeout closes a connection that has been idle this long.
	IdleTimeout time.Duration
	// DialTimeout bounds connect + greeting + EHLO + AUTH.
	DialTimeout time.Duration
	// SendTimeout bounds one MAIL/RCPT/DATA transaction.
	SendTimeout time.Duration
	// EHLOName is the name announced in EHLO.
	EHLOName string
	// TLSConfig is the base config for STARTTLS and implicit TLS; ServerName
	// is filled in from the transport host.
	TLSConfig *tls.Config
	// Dialer defaults to a net.Dialer.
	Dialer DialFunc
	Now    func() time.Time
	// Metrics is never nil after withDefaults.
	Metrics host.Metrics
}

// ErrPoolClosed is returned by Get after Close.
var ErrPoolClosed = errors.New("sender: connection pool closed")

// Pool holds one connection group per transport.
type Pool struct {
	cfg    PoolConfig
	mu     sync.Mutex
	groups map[string]*group
	closed bool
}

// NewPool returns a pool with defaults applied.
func NewPool(cfg PoolConfig) *Pool {
	if cfg.MaxMsgsPerConn <= 0 {
		cfg.MaxMsgsPerConn = 100
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 30 * time.Second
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = 2 * time.Minute
	}
	if cfg.EHLOName == "" {
		cfg.EHLOName = "localhost"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = host.NopMetrics{}
	}
	if cfg.Dialer == nil {
		d := &net.Dialer{Timeout: cfg.DialTimeout}
		cfg.Dialer = d.DialContext
	}
	return &Pool{cfg: cfg, groups: map[string]*group{}}
}

type group struct {
	key string
	// sem bounds the number of connections checked out at once, which is also
	// the number of live connections: an idle connection has already returned
	// its token.
	sem chan struct{}

	mu sync.Mutex
	// version is the transport row version the idle connections were opened
	// for. A newer row (new password, new host) drops them.
	version int64
	idle    []*Conn
}

// Conn is a checked-out SMTP connection.
type Conn struct {
	g    *group
	c    *smtp.Client
	nc   net.Conn
	sent int
	// idleSince is set when the connection goes back into the pool.
	idleSince time.Time
	version   int64
}

// Sent reports how many messages this connection has accepted.
func (c *Conn) Sent() int { return c.sent }

func (p *Pool) groupFor(t *store.Transport) (*group, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	g, ok := p.groups[t.ID]
	if !ok {
		maxConns := t.MaxConns
		if maxConns <= 0 {
			maxConns = 4
		}
		g = &group{key: t.ID, sem: make(chan struct{}, maxConns), version: t.Version}
		p.groups[t.ID] = g
	}
	return g, nil
}

// Get checks out a connection for a transport, dialing if the pool has none
// that can be reused. password is the decrypted SMTP password.
func (p *Pool) Get(ctx context.Context, t *store.Transport, password string) (*Conn, error) {
	g, err := p.groupFor(t)
	if err != nil {
		return nil, err
	}
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if c := p.popIdle(g, t.Version); c != nil {
		return c, nil
	}
	c, err := p.dial(ctx, g, t, password)
	if err != nil {
		<-g.sem
		return nil, err
	}
	return c, nil
}

// popIdle returns a reusable idle connection, closing the ones that expired or
// belong to an older version of the transport row.
func (p *Pool) popIdle(g *group, version int64) *Conn {
	now := p.cfg.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if version != g.version {
		for _, c := range g.idle {
			p.close(c)
		}
		g.idle, g.version = nil, version
		return nil
	}
	for len(g.idle) > 0 {
		c := g.idle[len(g.idle)-1]
		g.idle = g.idle[:len(g.idle)-1]
		if now.Sub(c.idleSince) > p.cfg.IdleTimeout || c.sent >= p.cfg.MaxMsgsPerConn {
			p.close(c)
			continue
		}
		return c
	}
	return nil
}

func (p *Pool) dial(ctx context.Context, g *group, t *store.Transport, password string) (*Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, p.cfg.DialTimeout)
	defer cancel()

	port := t.Port
	if port == 0 {
		port = 25
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(port))
	nc, err := p.cfg.Dialer(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sender: dial %s: %w", addr, err)
	}
	_ = nc.SetDeadline(p.cfg.Now().Add(p.cfg.DialTimeout))

	tlsCfg := p.tlsConfig(t.Host)
	if t.TLS == store.TLSImplicit {
		tc := tls.Client(nc, tlsCfg)
		if err := tc.HandshakeContext(dialCtx); err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("sender: tls %s: %w", addr, err)
		}
		nc = tc
	}

	client, err := smtp.NewClient(nc, t.Host)
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("sender: greeting %s: %w", addr, err)
	}
	if err := client.Hello(p.cfg.EHLOName); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("sender: ehlo %s: %w", addr, err)
	}
	if t.TLS == store.TLSSTARTTLS {
		if err := client.StartTLS(tlsCfg); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("sender: starttls %s: %w", addr, err)
		}
	}
	if t.Username != "" {
		if err := client.Auth(authFor(client, t, password)); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("sender: auth %s: %w", addr, err)
		}
	}
	_ = nc.SetDeadline(time.Time{})
	p.cfg.Metrics.Count(MetricConnOpened, 1, "transport", t.ID)
	return &Conn{g: g, c: client, nc: nc, version: t.Version}, nil
}

func (p *Pool) tlsConfig(host string) *tls.Config {
	var cfg *tls.Config
	if p.cfg.TLSConfig != nil {
		cfg = p.cfg.TLSConfig.Clone()
	} else {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	return cfg
}

// authFor picks a mechanism from what the server advertised. Unencrypted AUTH
// is allowed only when the operator configured a plaintext transport on
// purpose, which is also what makes the in-process chaos server usable.
func authFor(c *smtp.Client, t *store.Transport, password string) smtp.Auth {
	allowUnencrypted := t.TLS == store.TLSNone
	_, mechs := c.Extension("AUTH")
	if containsFold(mechs, "PLAIN") || mechs == "" {
		return smtp.PlainAuth("", t.Username, password, t.Host, allowUnencrypted)
	}
	return smtp.LoginAuth(t.Username, password, t.Host, allowUnencrypted)
}

func containsFold(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		indexFold(haystack, needle) >= 0
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFoldASCII(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFoldASCII(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'a' <= ca && ca <= 'z' {
			ca -= 32
		}
		if 'a' <= cb && cb <= 'z' {
			cb -= 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// Send runs one MAIL/RCPT/DATA transaction. The returned error is whatever the
// server or the network produced; Classify turns it into an error class.
func (c *Conn) Send(from string, rcpts []string, data []byte, timeout time.Duration) error {
	if timeout > 0 {
		_ = c.nc.SetDeadline(time.Now().Add(timeout))
		defer func() { _ = c.nc.SetDeadline(time.Time{}) }()
	}
	if err := c.c.Mail("<" + from + ">"); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.c.Rcpt("<" + r + ">"); err != nil {
			return err
		}
	}
	w, err := c.c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	c.sent++
	return nil
}

// Put returns a connection to the pool. reusable is false when the connection
// is in an unknown state (network error, 421); Reusable computes it from a
// classified failure.
func (p *Pool) Put(c *Conn, reusable bool) {
	if c == nil {
		return
	}
	g := c.g
	defer func() { <-g.sem }()

	if !reusable || c.sent >= p.cfg.MaxMsgsPerConn {
		p.close(c)
		return
	}
	// A failed transaction may have left the server mid-command.
	if err := c.c.Reset(); err != nil {
		p.close(c)
		return
	}
	c.idleSince = p.cfg.Now()

	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		p.close(c)
		return
	}

	g.mu.Lock()
	if c.version != g.version {
		g.mu.Unlock()
		p.close(c)
		return
	}
	g.idle = append(g.idle, c)
	g.mu.Unlock()
}

// Reusable reports whether a connection survives a failure: a normal SMTP
// rejection leaves the session usable, a 421 or a network error does not.
func Reusable(f Failure) bool {
	if f.Class == store.ErrorClassNone {
		return true
	}
	if f.Code == 0 || f.Code == 421 {
		return false
	}
	return f.Code >= 400 && f.Code < 600
}

func (p *Pool) close(c *Conn) {
	if c == nil {
		return
	}
	_ = c.c.Quit()
	_ = c.nc.Close()
	p.cfg.Metrics.Count(MetricConnClosed, 1)
}

// CloseTransport drops every idle connection of a transport, used when it is
// marked unhealthy.
func (p *Pool) CloseTransport(id string) {
	p.mu.Lock()
	g, ok := p.groups[id]
	p.mu.Unlock()
	if !ok {
		return
	}
	g.mu.Lock()
	idle := g.idle
	g.idle = nil
	g.mu.Unlock()
	for _, c := range idle {
		p.close(c)
	}
}

// Close drops every idle connection. Connections still checked out are closed
// by their Put.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	groups := make([]*group, 0, len(p.groups))
	for _, g := range p.groups {
		groups = append(groups, g)
	}
	p.mu.Unlock()
	for _, g := range groups {
		g.mu.Lock()
		idle := g.idle
		g.idle = nil
		g.mu.Unlock()
		for _, c := range idle {
			p.close(c)
		}
	}
}
