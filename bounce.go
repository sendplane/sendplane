package sendplane

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/sendplane/sendplane/internal/bounce"
	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
)

// BounceConfig configures one bounce poller process (RunBounce). Every field
// has a default; see docs/architecture.md 10.
type BounceConfig struct {
	// WorkerID identifies this replica as the holder of the per-mailbox store
	// lock, so it must be unique in the cluster and stable for the life of the
	// process. Empty generates one from the hostname and PID, which is right
	// for a single process and wrong for two on the same host.
	WorkerID string

	// PollInterval is how long a mailbox waits between passes. Zero uses the
	// bounce package's default (1 minute).
	PollInterval time.Duration
	// RefreshInterval is how often the mailbox list is re-read, so a mailbox
	// added through the API is picked up without a restart. Zero uses the
	// default (5 minutes).
	RefreshInterval time.Duration
	// UseIdle lets a pass wait in IMAP IDLE instead of sleeping, when the
	// server supports it.
	UseIdle bool
}

// RunBounce runs the IMAP/POP3 bounce mailbox poller: one goroutine per
// enabled store.BounceMailbox of every tenant, each holding that mailbox's
// store lock so a mail is processed once cluster-wide (architecture 10).
//
// It blocks until ctx is done and returns nil on a clean shutdown. A mailbox
// that cannot be reached backs off and keeps retrying; a broken IMAP account
// never takes the process down.
func (s *Sendplane) RunBounce(ctx context.Context, c BounceConfig) error {
	owner := c.WorkerID
	if owner == "" {
		owner = defaultWorkerID("bounce")
	}
	r, err := bounce.NewRunner(bounce.RunnerConfig{
		Owner:    owner,
		Provider: s.opts.Store,
		Source:   bounceMailboxes{provider: s.opts.Store, log: s.opts.Logger},
		Secrets:  s.opts.Secrets,
		Processor: bounce.NewProcessor(bounce.Options{
			Clock:   s.opts.Clock,
			Logger:  s.opts.Logger,
			Metrics: s.opts.Metrics,
		}),
		PollInterval:    c.PollInterval,
		RefreshInterval: c.RefreshInterval,
		UseIdle:         c.UseIdle,
		Logger:          s.opts.Logger,
		Metrics:         s.opts.Metrics,
		Clock:           s.opts.Clock,
	})
	if err != nil {
		return err
	}
	return r.Run(ctx)
}

// defaultWorkerID names this replica when the host did not. It is good enough
// for one process per host and not good enough for two, which is why
// BounceConfig.WorkerID exists.
func defaultWorkerID(role string) string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "unknown"
	}
	return fmt.Sprintf("%s-%s-%d-%s", role, h, os.Getpid(), store.NewID()[:8])
}

// bounceMailboxes is the bounce.MailboxSource: every enabled bounce mailbox of
// every tenant this provider knows of.
//
// It iterates Provider.Tenants, not ActiveTenants. A DSN arrives hours or days
// after the campaign that caused it finished, by which time the tenant has no
// claimable delivery and no unfinished campaign left, so an active-only listing
// would stop polling exactly the mailbox the bounce is sitting in.
type bounceMailboxes struct {
	provider store.Provider
	log      *slog.Logger
}

func (s bounceMailboxes) ListMailboxes(ctx context.Context) ([]bounce.TenantMailbox, error) {
	tenants, err := s.provider.Tenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("sendplane: list tenants for bounce polling: %w", err)
	}
	var out []bounce.TenantMailbox
	for _, tenantID := range tenants {
		st, err := s.provider.ForTenant(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("sendplane: store for tenant %s: %w", tenantID, err)
		}
		boxes, err := st.BounceMailboxes().ListEnabled(ctx)
		if err != nil {
			return nil, fmt.Errorf("sendplane: bounce mailboxes of %s: %w", tenantID, err)
		}
		for i := range boxes {
			m := &boxes[i]
			// The API validates AfterProcess on write; a row that predates
			// that, or one an operator wrote directly, is skipped rather than
			// failing the whole refresh and stopping every other mailbox.
			action, err := mailbox.ParseAction(m.AfterProcess)
			if err != nil {
				s.log.Error("sendplane: bounce mailbox has an unusable after-process policy",
					"tenant", tenantID, "mailbox", m.ID, "after_process", m.AfterProcess, "err", err)
				continue
			}
			protocol := mailbox.Protocol(m.Protocol)
			if protocol == "" {
				protocol = mailbox.ProtocolIMAP
			}
			out = append(out, bounce.TenantMailbox{
				TenantID: tenantID,
				// Store IDs are unique across tenants, which is what the lock
				// name needs (bounce.LockName).
				MailboxID: m.ID,
				Config: mailbox.Config{
					Protocol: protocol,
					Host:     m.Host, Port: m.Port, TLS: m.TLS,
					Username: m.Username,
					// The ciphertext exactly as the store holds it; the dialer
					// decrypts it with Options.Secrets.
					Password:     m.Password,
					Folder:       m.Folder,
					AfterProcess: action,
				},
			})
		}
	}
	return out, nil
}
