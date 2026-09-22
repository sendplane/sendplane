package bounce

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
)

// The two platform paths of ADR-0017.
//
// A *shared* bounce mailbox is one the operator runs for every tenant, so the
// mail in it belongs to whoever sent the delivery that bounced. The mailbox
// cannot say who that is, and the VERP return path deliberately does not
// either: it stays short enough to survive every MTA on the way back
// (`bounce+<deliveryID>.<mac8>@…`), which leaves room for the delivery ID and
// eight hex characters of MAC and nothing else (architecture 10). The delivery
// ID is enough, because it is unique across tenants by construction, and
// Provider.LookupDeliveryTenant is what turns it into a tenant.
//
// A bounce that came back through a shared *transport* is also the platform's
// business, not only the tenant's: a hard bounce or a complaint damages the
// sending reputation every tenant on that relay depends on, so the address
// goes on the system tenant's suppression list as well, and every later send
// through a shared transport checks both.

// HandlePlatform is Handle for a message from a shared bounce mailbox: it
// resolves the tenant first and then runs the ordinary path in that tenant.
//
// The resolution order is the correlation order of architecture 10, narrowed
// to what can identify a *tenant*:
//
//  1. X-Sendplane-ID, the header sendplane put on its own outgoing mail and
//     the only correlation that carries a tenant. A returned original brings
//     it back verbatim.
//  2. The VERP delivery ID, unverified — at this point there is no tenant
//     whose keys the MAC could be checked against, which is exactly why the
//     lookup is by ID and the MAC is verified afterwards, in Handle, with the
//     keys of the tenant the ID resolved to. A forged ID therefore resolves to
//     the real tenant and is then recorded as unverified, which is the same
//     outcome a forged VERP gets in a tenant's own mailbox.
//  3. Nothing. The message is still evidence that a mailbox is receiving
//     bounces sendplane cannot attribute, so it is recorded in the system
//     tenant, which is whose mailbox it is.
//
// The message is parsed twice: once here without keys, for the correlation
// hints alone, and once in Handle with the resolved tenant's keys. Bounce
// volume is orders of magnitude below send volume, and the alternative is a
// Handle that takes a half-parsed message and has to be trusted to finish the
// job.
func (p *Processor) HandlePlatform(ctx context.Context, provider store.Provider, msg mailbox.Message) (Outcome, error) {
	if provider == nil {
		return Outcome{}, errors.New("bounce: a shared bounce mailbox needs a store.Provider")
	}
	tenantID, err := p.resolveTenant(ctx, provider, msg)
	if err != nil {
		return Outcome{}, err
	}
	st, err := provider.ForTenant(ctx, tenantID)
	if err != nil {
		return Outcome{}, fmt.Errorf("bounce: store for tenant %s: %w", tenantID, err)
	}
	return p.Handle(ctx, st, tenantID, msg)
}

// resolveTenant implements the order above. It never fails for a message it
// cannot attribute: the system tenant is the honest fallback, because a shared
// mailbox is the system tenant's.
func (p *Processor) resolveTenant(ctx context.Context, provider store.Provider, msg mailbox.Message) (string, error) {
	parsed, err := Parse(msg.Raw)
	if err != nil {
		// Unparseable: Handle will drop it, and it does not matter which
		// tenant it is dropped in. The system tenant keeps a tenant's data
		// clean of somebody else's junk.
		return store.SystemTenantID, nil
	}
	if id := parsed.Correlation.TenantID; id != "" {
		return id, nil
	}
	deliveryID := parsed.Correlation.DeliveryID
	if deliveryID == "" {
		return store.SystemTenantID, nil
	}
	tenantID, err := provider.LookupDeliveryTenant(ctx, deliveryID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The delivery never existed or retention removed it. Recording the
		// event in the system tenant keeps the evidence without inventing an
		// owner for it.
		return store.SystemTenantID, nil
	case err != nil:
		// A lookup failure is a store failure: the caller must not
		// acknowledge the message, or a real bounce would be lost.
		return "", fmt.Errorf("bounce: lookup delivery %s: %w", deliveryID, err)
	}
	return tenantID, nil
}

// suppressPlatform adds the address to the system tenant's suppression list
// when the delivery went out through a shared transport.
//
// It does not consult the tenant's SuppressionEnabled: that setting governs
// the tenant's own list, and the platform list exists to protect a relay
// everybody shares from an address that has already bounced on it. A tenant
// that has turned its own suppression off still may not keep mailing a dead
// address through the operator's relay.
//
// The list is only visible and manageable from the system tenant, so this adds
// nothing a tenant can see.
func (p *Processor) suppressPlatform(
	ctx context.Context, st store.Store, d *store.Delivery,
	reason store.SuppressionReason, now time.Time,
) error {
	if p.opts.Provider == nil || d.SenderID == "" || d.EmailNorm == "" {
		return nil
	}
	snd, err := st.Senders().Get(ctx, d.SenderID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The sender was deleted since the send. Nothing left to say which
		// transport it used, so nothing to suppress on the platform's behalf.
		return nil
	case err != nil:
		return fmt.Errorf("bounce: sender %s: %w", d.SenderID, err)
	}
	if !store.IsPlatformID(snd.TransportID) {
		return nil
	}
	sys, err := store.PlatformView(ctx, p.opts.Provider)
	if err != nil {
		return fmt.Errorf("bounce: platform view: %w", err)
	}
	if err := sys.Suppressions().Upsert(ctx, &store.Suppression{
		EmailNorm:        d.EmailNorm,
		Reason:           reason,
		SourceDeliveryID: d.ID,
		CreatedAt:        now,
	}); err != nil {
		return fmt.Errorf("bounce: platform suppress %s: %w", d.EmailNorm, err)
	}
	p.metrics.Count(MetricPlatformSuppressed, 1, "reason", string(reason))
	return nil
}

// MetricPlatformSuppressed counts addresses added to the platform suppression
// list, which is the operator's early warning that a shared relay is being
// mailed at dead addresses.
const MetricPlatformSuppressed = "sendplane_bounce_platform_suppressed_total"
