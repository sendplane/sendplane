package mailbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/sendplane/sendplane/store"
)

// imapMailbox is an IMAP connection with one folder selected.
//
// go-imap's client takes no context, so every operation puts the context's
// deadline (or the configured timeout, whichever is sooner) on the connection
// and clears it afterwards. A command that outlives its context fails the
// connection, which is what the poller's backoff and redial are for.
type imapMailbox struct {
	cfg  Config
	conn net.Conn
	c    *imapclient.Client

	// changed is signalled by the unilateral data handler, so Idle can return
	// as soon as the server announces new mail instead of idling out.
	changed chan struct{}

	mu     sync.Mutex
	closed bool
}

var _ Client = (*imapMailbox)(nil)
var _ HeaderSearcher = (*imapMailbox)(nil)
var _ Idler = (*imapMailbox)(nil)

func dialIMAP(ctx context.Context, cfg Config, password string) (Client, error) {
	conn, err := cfg.dialConn(ctx)
	if err != nil {
		return nil, err
	}
	m := &imapMailbox{cfg: cfg, conn: conn, changed: make(chan struct{}, 1)}
	opts := &imapclient.Options{
		TLSConfig: cfg.tlsConfig(),
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(*imapclient.UnilateralDataMailbox) { m.signal() },
		},
	}

	_ = conn.SetDeadline(deadline(ctx, cfg.DialTimeout))
	if cfg.TLS == store.TLSSTARTTLS {
		m.c, err = imapclient.NewStartTLS(conn, opts)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("mailbox: imap starttls %s: %w", cfg.Addr(), err)
		}
	} else {
		m.c = imapclient.New(conn, opts)
		if err := m.c.WaitGreeting(); err != nil {
			m.c.Close()
			return nil, fmt.Errorf("mailbox: imap greeting %s: %w", cfg.Addr(), err)
		}
	}
	if err := m.c.Login(cfg.Username, password).Wait(); err != nil {
		m.c.Close()
		return nil, fmt.Errorf("mailbox: imap login %s: %w", cfg.Username, err)
	}
	if _, err := m.c.Select(cfg.Folder, nil).Wait(); err != nil {
		m.c.Close()
		return nil, fmt.Errorf("mailbox: imap select %q: %w", cfg.Folder, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return m, nil
}

func (m *imapMailbox) signal() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

// begin arms the connection deadline for one operation and returns the
// function that clears it.
func (m *imapMailbox) begin(ctx context.Context) func() {
	_ = m.conn.SetDeadline(deadline(ctx, m.cfg.Timeout))
	return func() { _ = m.conn.SetDeadline(time.Time{}) }
}

// Fetch returns the messages that have not been handled yet: everything not
// \Seen and not \Deleted, oldest UID first. Ack is what sets \Seen, so a
// message whose processing failed comes back on the next poll.
func (m *imapMailbox) Fetch(ctx context.Context, max int) ([]Message, error) {
	defer m.begin(ctx)()
	criteria := &imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen, imap.FlagDeleted},
	}
	return m.searchAndFetch(ctx, criteria, max)
}

// FetchByHeader is the IMAP SEARCH HEADER of architecture 11.2. It ignores
// \Seen: the probe looks for one specific mail, not for unhandled work.
func (m *imapMailbox) FetchByHeader(ctx context.Context, name, value string) ([]Message, error) {
	defer m.begin(ctx)()
	criteria := &imap.SearchCriteria{
		Header:  []imap.SearchCriteriaHeaderField{{Key: name, Value: value}},
		NotFlag: []imap.Flag{imap.FlagDeleted},
	}
	return m.searchAndFetch(ctx, criteria, 0)
}

func (m *imapMailbox) searchAndFetch(ctx context.Context, criteria *imap.SearchCriteria, max int) ([]Message, error) {
	data, err := m.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("mailbox: imap search: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	if max > 0 && len(uids) > max {
		uids = uids[:max]
	}

	section := &imap.FetchItemBodySection{Peek: true}
	opts := &imap.FetchOptions{
		UID:          true,
		InternalDate: true,
		BodySection:  []*imap.FetchItemBodySection{section},
	}
	bufs, err := m.c.Fetch(imap.UIDSetNum(uids...), opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("mailbox: imap fetch: %w", err)
	}
	out := make([]Message, 0, len(bufs))
	for _, b := range bufs {
		raw := b.FindBodySection(section)
		if raw == nil {
			continue
		}
		out = append(out, Message{
			ID:       strconv.FormatUint(uint64(b.UID), 10),
			Raw:      raw,
			Received: b.InternalDate,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, ctx.Err()
}

// Ack marks the handled messages \Seen and then applies the action. \Seen is
// always set, including for delete and move: if the expunge fails halfway the
// messages that survived are not re-processed.
func (m *imapMailbox) Ack(ctx context.Context, ids []string, action Action) error {
	if len(ids) == 0 {
		return nil
	}
	uids, err := parseUIDs(ids)
	if err != nil {
		return err
	}
	defer m.begin(ctx)()

	set := imap.UIDSetNum(uids...)
	if err := m.store(set, imap.StoreFlagsAdd, imap.FlagSeen); err != nil {
		return err
	}
	switch {
	case action == ActionKeep:
		return nil
	case action == ActionDelete:
		return m.expunge(set)
	default:
		folder, ok := action.MoveFolder()
		if !ok {
			return fmt.Errorf("mailbox: unknown after-process action %q", action)
		}
		if m.c.Caps().Has(imap.CapMove) {
			if _, err := m.c.Move(set, folder).Wait(); err != nil {
				return fmt.Errorf("mailbox: imap move to %q: %w", folder, err)
			}
			return nil
		}
		// Pre-RFC 6851 servers: copy, flag, expunge.
		if _, err := m.c.Copy(set, folder).Wait(); err != nil {
			return fmt.Errorf("mailbox: imap copy to %q: %w", folder, err)
		}
		return m.expunge(set)
	}
}

func (m *imapMailbox) store(set imap.UIDSet, op imap.StoreFlagsOp, flags ...imap.Flag) error {
	cmd := m.c.Store(set, &imap.StoreFlags{Op: op, Silent: true, Flags: flags}, nil)
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("mailbox: imap store flags: %w", err)
	}
	return nil
}

// expunge removes the set. UIDEXPUNGE (RFC 4315) only removes those UIDs;
// without it EXPUNGE also removes anything else already flagged \Deleted,
// which is the server's business, not ours.
func (m *imapMailbox) expunge(set imap.UIDSet) error {
	if err := m.store(set, imap.StoreFlagsAdd, imap.FlagDeleted); err != nil {
		return err
	}
	var cmd *imapclient.ExpungeCommand
	if m.c.Caps().Has(imap.CapUIDPlus) {
		cmd = m.c.UIDExpunge(set)
	} else {
		cmd = m.c.Expunge()
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("mailbox: imap expunge: %w", err)
	}
	return nil
}

// Idle blocks until the server announces a change, timeout elapses or ctx is
// done. A wake-up is only a hint: the caller fetches either way.
func (m *imapMailbox) Idle(ctx context.Context, timeout time.Duration) error {
	if !m.c.Caps().Has(imap.CapIdle) {
		return ErrNoIdle
	}
	// The connection must stay readable for the whole idle window.
	_ = m.conn.SetDeadline(time.Now().Add(timeout + m.cfg.Timeout))
	defer m.conn.SetDeadline(time.Time{})

	cmd, err := m.c.Idle()
	if err != nil {
		return fmt.Errorf("mailbox: imap idle: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	case <-m.changed:
	case <-m.c.Closed():
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("mailbox: imap idle close: %w", err)
	}
	return nil
}

// ErrNoIdle is returned by Idle when the server does not advertise IDLE. The
// poller falls back to sleeping its poll interval.
var ErrNoIdle = errors.New("mailbox: server does not support IDLE")

func (m *imapMailbox) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	_ = m.conn.SetDeadline(time.Now().Add(m.cfg.Timeout))
	// A failed logout is not worth reporting: the connection is going away.
	_ = m.c.Logout().Wait()
	return m.c.Close()
}

func parseUIDs(ids []string) ([]imap.UID, error) {
	out := make([]imap.UID, 0, len(ids))
	for _, id := range ids {
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("mailbox: %q is not an IMAP UID", id)
		}
		out = append(out, imap.UID(n))
	}
	return out, nil
}
