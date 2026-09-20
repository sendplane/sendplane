package sendplane

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sendplane/sendplane/internal/dnscheck"
	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/internal/probe"
	"github.com/sendplane/sendplane/store"
)

// The two leader loops of architecture 11.2. Triggering is cheap and rare (a
// sender is due every Options.Probe.Interval, six hours by default), while
// collecting has to be frequent enough that a probe mail's latency measurement
// is not dominated by how long the loop slept.
const (
	probeTriggerInterval = 5 * time.Minute
	probeCollectInterval = time.Minute
	// probeDNSTimeout bounds one DNS query in the diagnostic layer.
	probeDNSTimeout = 5 * time.Second
)

// probeRunner returns the process-wide probe runner, or nil when the host did
// not enable probing. It is built once: the HTTP handler's manual trigger and
// the control leader's loops must be the same runner, because they share the
// HMAC key that makes a probe token verifiable.
func (s *Sendplane) probeRunner() *probe.Runner {
	s.probeOnce.Do(func() {
		if !s.opts.Probe.Enabled {
			return
		}
		s.probe = probe.New(probe.Options{
			Interval: s.opts.Probe.Interval,
			Timeout:  s.opts.Probe.Timeout,
			HMACKey:  s.opts.Probe.HMACKey,
			DNS:      s.dnsChecker(),
			Secrets:  s.opts.Secrets,
			Clock:    s.opts.Clock,
			Logger:   s.opts.Logger,
		})
	})
	return s.probe
}

// dnsChecker builds the diagnostic layer (architecture 11.3). A resolver that
// cannot be built is not fatal: the loopback probe is the primary evidence and
// DNS only explains it, so the runner keeps working without one.
func (s *Sendplane) dnsChecker() *dnscheck.Checker {
	var (
		r   dnscheck.Resolver
		err error
	)
	if len(s.opts.Probe.Nameservers) > 0 {
		r, err = dnscheck.NewResolver(s.opts.Probe.Nameservers, probeDNSTimeout)
	} else {
		r, err = dnscheck.SystemResolver(probeDNSTimeout)
	}
	if err != nil {
		s.opts.Logger.Warn("sendplane: probe DNS diagnostics disabled", "err", err)
		return nil
	}
	return dnscheck.New(r)
}

// probeTrigger and probeCollect are the two leader loops of the probe README:
// thin adapters that bind the runner to one tenant's Store, because
// control.TickLoop is per tenant and the runner is not.
type probeTrigger struct {
	r  *probe.Runner
	st store.Store
}

func (l probeTrigger) Tick(ctx context.Context, now time.Time) error {
	return l.r.Tick(ctx, l.st, now)
}

type probeCollect struct {
	r      *probe.Runner
	st     store.Store
	opener probe.MailboxOpener
}

func (l probeCollect) Tick(ctx context.Context, now time.Time) error {
	return l.r.CollectWith(ctx, l.st, l.opener, now)
}

// --- mailbox adapter ---------------------------------------------------

// probeOpener adapts internal/mailbox to probe.MailboxOpener. internal/probe
// deliberately does not import internal/mailbox: it needs two methods, and the
// root is the place that knows both packages.
type probeOpener struct{ secrets SecretCipher }

// Open connects to one probe mailbox. The verdict needs to know whether the
// mail landed in the inbox or in the spam folder (architecture 11.4), and an
// IMAP connection has exactly one selected mailbox, so one connection is
// opened per folder and the folder is carried on every message ID.
func (o probeOpener) Open(ctx context.Context, m *store.ProbeMailbox) (probe.MailboxFetcher, error) {
	f := &probeFetcher{}
	for _, folder := range probeFolders(m) {
		c, err := mailbox.Dial(ctx, mailbox.Config{
			// A probe mailbox has to be IMAP: POP3 cannot search by header
			// and has no folders (internal/mailbox).
			Protocol: mailbox.ProtocolIMAP,
			Host:     m.Host, Port: m.Port, TLS: m.TLS,
			Username: m.Username, Password: m.Password,
			Folder: folder,
		}, o.secrets)
		if err != nil {
			_ = f.close()
			return nil, fmt.Errorf("probe: mailbox %s folder %q: %w", m.ID, folder, err)
		}
		searcher, ok := c.(mailbox.HeaderSearcher)
		if !ok {
			_ = c.Close()
			_ = f.close()
			return nil, fmt.Errorf("probe: mailbox %s cannot search by header", m.ID)
		}
		f.folders = append(f.folders, probeFolder{name: folder, client: c, search: searcher})
	}
	if len(f.folders) == 0 {
		return nil, fmt.Errorf("probe: mailbox %s names no folder", m.ID)
	}
	return f, nil
}

// probeFolders is the folder list to search: the inbox, plus the spam folder
// when the mailbox names a different one. A probe that only ever looked in the
// inbox would report "not delivered" for a mail that was filed as spam, which
// is precisely the case the yellow verdict exists for.
func probeFolders(m *store.ProbeMailbox) []string {
	inbox := m.InboxFolder
	if inbox == "" {
		inbox = "INBOX"
	}
	out := []string{inbox}
	if m.SpamFolder != "" && !strings.EqualFold(m.SpamFolder, inbox) {
		out = append(out, m.SpamFolder)
	}
	return out
}

type probeFolder struct {
	name   string
	client mailbox.Client
	search mailbox.HeaderSearcher
}

// probeFetcher searches every folder of one mailbox over its own connection.
type probeFetcher struct{ folders []probeFolder }

var _ probe.MailboxFetcher = (*probeFetcher)(nil)
var _ io.Closer = (*probeFetcher)(nil)

// messageIDSep joins the folder to the IMAP UID. UIDs are only unique within
// one folder, so Delete has to know which connection a message came from.
const messageIDSep = "\x00"

func (f *probeFetcher) FetchByHeader(ctx context.Context, name, value string) ([]probe.RawMessage, error) {
	var out []probe.RawMessage
	var firstErr error
	for _, fo := range f.folders {
		msgs, err := fo.search.FetchByHeader(ctx, name, value)
		if err != nil {
			// One unreadable folder must not hide a hit in another one: a
			// missing spam folder is a misconfiguration, not a verdict.
			if firstErr == nil {
				firstErr = fmt.Errorf("probe: folder %q: %w", fo.name, err)
			}
			continue
		}
		for _, m := range msgs {
			out = append(out, probe.RawMessage{
				ID:         fo.name + messageIDSep + m.ID,
				Folder:     fo.name,
				Raw:        m.Raw,
				ReceivedAt: m.Received,
			})
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	return nil, firstErr
}

// Delete removes the recovered probe mail. It is Ack with the delete action:
// a probe mailbox exists for probes, and leaving them would grow without bound.
func (f *probeFetcher) Delete(ctx context.Context, ids []string) error {
	byFolder := map[string][]string{}
	for _, id := range ids {
		folder, uid, ok := strings.Cut(id, messageIDSep)
		if !ok {
			continue
		}
		byFolder[folder] = append(byFolder[folder], uid)
	}
	var firstErr error
	for _, fo := range f.folders {
		uids := byFolder[fo.name]
		if len(uids) == 0 {
			continue
		}
		if err := fo.client.Ack(ctx, uids, mailbox.ActionDelete); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("probe: folder %q: %w", fo.name, err)
		}
	}
	return firstErr
}

func (f *probeFetcher) Close() error { return f.close() }

func (f *probeFetcher) close() error {
	var firstErr error
	for _, fo := range f.folders {
		if err := fo.client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	f.folders = nil
	return firstErr
}
