package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// WithPlatform wraps a Provider so that the platform catalog's shared
// transports, domains, senders and mailboxes appear as virtual entities in the
// stores it hands out (ADR-0017).
//
// The wrapper is the whole implementation of "configuration is resolved in
// code, never written to the database":
//
//   - Reads of the five configuration aggregates include the catalog's
//     entries, built fresh from cfg on every call, with Shared true and their
//     passwords and DKIM keys encrypted with cipher so that the existing
//     decrypt paths (internal/sender, internal/bounce, internal/probe) work
//     unchanged.
//   - Writes that name a virtual ID are ErrReadOnly, except the state-only
//     ones — a transport's circuit status, a sender's or domain's probe
//     verdict, a mailbox's reachability — which are routed to a *shadow row*
//     in the system tenant's own table. A shadow row carries the virtual ID,
//     Shared true and the state columns; every configuration column on it is
//     written empty, so a shared relay's host and password exist in no
//     database anywhere. A read merges the configuration over the shadow.
//   - Visibility: a normal tenant sees the shared *senders* (the thing it
//     sends with) and nothing else, with every state field zeroed. The shared
//     transports, domains and mailboxes behind them, and the state of any of
//     it, are the system tenant's alone. Whatever needs the internals uses
//     PlatformView, not a relaxed rule here.
//
// The same wrapper serves the system tenant's *shared templates and layouts*
// to every other tenant, read-through (ADR-0018, overlay_content.go). Those
// are rows, not configuration, so the wrapper is applied even when the
// catalog is empty: a deployment with no shared relay can still share a
// template.
//
// now supplies the CreatedAt/UpdatedAt the virtual entities report; nil means
// time.Now.
//
// cipher may be nil, in which case the configured secrets are handed out
// as-is — the same rule the sender reads a stored password under. An
// encryption failure cannot be reported from here (the signature has no error
// for a host to act on and a partly-encrypted catalog must never be served),
// so it is remembered and returned by ForTenant, which makes every request
// fail loudly with the cause instead of silently sending unauthenticated.
func WithPlatform(p Provider, cfg PlatformCatalog, cipher SecretCipher, now func() time.Time) Provider {
	if p == nil {
		return p
	}
	if _, ok := p.(*platformProvider); ok {
		// Wrapping twice would serve every virtual entity twice.
		return p
	}
	if now == nil {
		now = time.Now
	}
	o := &platformProvider{inner: p, cat: cfg.Normalize(), clock: now}
	o.build(cipher)
	return o
}

type platformProvider struct {
	inner Provider
	cat   PlatformCatalog
	clock func() time.Time

	// err is a build failure (only encryption can fail); ForTenant returns it.
	err error

	transports  []Transport
	domains     []SendingDomain
	senders     []Sender
	probeBoxes  []ProbeMailbox
	bounceBoxes []BounceMailbox
}

var _ Provider = (*platformProvider)(nil)

// Catalog reports the platform configuration this provider was built with. It
// is how a caller that only has the Provider (the sender process, the bounce
// runner) reaches the catalog without it being threaded separately.
func (o *platformProvider) Catalog() PlatformCatalog { return o.cat }

// PlatformCatalogOf returns the platform catalog a Provider was wrapped with,
// or the zero catalog when it was not wrapped at all. It lets a caller that is
// handed only a Provider decide whether platform resources exist.
func PlatformCatalogOf(p Provider) PlatformCatalog {
	if c, ok := p.(interface{ Catalog() PlatformCatalog }); ok {
		return c.Catalog()
	}
	return PlatformCatalog{}
}

// build materializes the virtual entities once, with their secrets encrypted.
func (o *platformProvider) build(cipher SecretCipher) {
	ctx := context.Background()
	at := TruncateTime(o.clock())
	enc := func(what, s string) []byte {
		if s == "" {
			return nil
		}
		if cipher == nil {
			return []byte(s)
		}
		b, err := cipher.Encrypt(ctx, []byte(s))
		if err != nil && o.err == nil {
			o.err = fmt.Errorf("store: encrypting the platform %s secret failed: %w", what, err)
		}
		return b
	}

	for _, t := range o.cat.Transports {
		o.transports = append(o.transports, Transport{
			ID: t.ID, TenantID: SystemTenantID, Name: t.Name,
			Host: t.Host, Port: t.Port, TLS: defaultTLS(t.TLS),
			Username: t.Username, Password: enc("transport "+t.ID, t.Password),
			MaxConns: t.MaxConns, RatePerSecond: t.RatePerSecond,
			DomainRatePerSecond: maps.Clone(t.DomainRatePerSecond),
			Shared:              true,
			Status:              TransportHealthy,
			CreatedAt:           at, UpdatedAt: at,
		})
	}
	for _, d := range o.cat.Domains {
		o.domains = append(o.domains, SendingDomain{
			ID: d.ID, TenantID: SystemTenantID, Domain: d.Domain,
			DKIMSelector:     d.DKIMSelector,
			DKIMPrivateKey:   enc("domain "+d.ID, d.DKIMPrivateKey),
			ReturnPathDomain: d.ReturnPathDomain, ExpectedSPF: d.ExpectedSPF,
			OutboundIPs: append([]string(nil), d.OutboundIPs...),
			Shared:      true,
			CreatedAt:   at, UpdatedAt: at,
		})
	}
	for _, s := range o.cat.Senders {
		o.senders = append(o.senders, Sender{
			ID: s.ID, TenantID: SystemTenantID, Name: s.Name,
			// The three From fields are Liquid templates, not addresses. They
			// are carried verbatim: whatever renders them (internal/platform)
			// is the only thing that may look at them, and a caller that
			// mistakes one for an address gets an obviously templated string
			// rather than a plausible wrong one.
			FromName: s.FromName, FromEmail: s.FromEmail, ReplyTo: s.ReplyTo,
			TransportID: s.TransportID, DomainID: s.DomainID,
			Shared:    true,
			CreatedAt: at, UpdatedAt: at,
		})
	}
	for _, m := range o.cat.ProbeMailboxes {
		o.probeBoxes = append(o.probeBoxes, ProbeMailbox{
			ID: m.ID, TenantID: SystemTenantID, Name: m.Name,
			Kind: m.Kind.Normalized(), Address: m.Address,
			Host: m.Host, Port: m.Port, TLS: defaultTLS(m.TLS),
			Username: m.Username, Password: enc("probe mailbox "+m.ID, m.Password),
			InboxFolder: m.InboxFolder, SpamFolder: m.SpamFolder,
			AuthServID: m.AuthServID, Enabled: enabledOr(m.Enabled, true),
			Shared:    true,
			CreatedAt: at, UpdatedAt: at,
		})
	}
	for _, m := range o.cat.BounceMailboxes {
		o.bounceBoxes = append(o.bounceBoxes, BounceMailbox{
			ID: m.ID, TenantID: SystemTenantID, Name: m.Name,
			Address: m.Address, Protocol: m.Protocol,
			Host: m.Host, Port: m.Port, TLS: defaultTLS(m.TLS),
			Username: m.Username, Password: enc("bounce mailbox "+m.ID, m.Password),
			Folder: m.Folder, AfterProcess: m.AfterProcess,
			Enabled:   enabledOr(m.Enabled, true),
			Shared:    true,
			CreatedAt: at, UpdatedAt: at,
		})
	}
}

func defaultTLS(m TLSMode) TLSMode {
	if m == "" {
		return TLSSTARTTLS
	}
	return m
}

func enabledOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func (o *platformProvider) ForTenant(ctx context.Context, tenantID string) (Store, error) {
	if o.err != nil {
		return nil, o.err
	}
	st, err := o.inner.ForTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return &platformStore{Store: st, o: o, tenantID: tenantID, system: tenantID == SystemTenantID}, nil
}

func (o *platformProvider) ActiveTenants(ctx context.Context) ([]string, error) {
	return o.inner.ActiveTenants(ctx)
}

func (o *platformProvider) Tenants(ctx context.Context) ([]string, error) {
	return o.inner.Tenants(ctx)
}

func (o *platformProvider) LookupDeliveryTenant(ctx context.Context, deliveryID string) (string, error) {
	return o.inner.LookupDeliveryTenant(ctx, deliveryID)
}

func (o *platformProvider) Migrate(ctx context.Context) error {
	if o.err != nil {
		return o.err
	}
	return o.inner.Migrate(ctx)
}

func (o *platformProvider) Close() error { return o.inner.Close() }

// Ping forwards to the wrapped Provider when it offers one, so that
// GET /healthz keeps probing the store through the overlay.
func (o *platformProvider) Ping(ctx context.Context) error {
	if p, ok := o.inner.(interface{ Ping(context.Context) error }); ok {
		return p.Ping(ctx)
	}
	return nil
}

// shadowStore is the system tenant's *unwrapped* store, where state shadow
// rows live. It has to be the unwrapped one: the overlay itself refuses writes
// to a platform ID, which is exactly what a shadow write is.
func (o *platformProvider) shadowStore(ctx context.Context) (Store, error) {
	return o.inner.ForTenant(ctx, SystemTenantID)
}

// --- the wrapped Store -------------------------------------------------

// platformStore embeds the tenant's real Store and overrides the five
// aggregates the catalog contributes to. Everything else passes straight
// through, which is what keeps the overlay from having to track new
// repositories.
type platformStore struct {
	Store
	o        *platformProvider
	tenantID string
	system   bool
}

func (s *platformStore) Transports() TransportRepo {
	return &platformTransports{s: s, inner: s.Store.Transports()}
}

func (s *platformStore) Senders() SenderRepo {
	return &platformSenders{s: s, inner: s.Store.Senders()}
}

func (s *platformStore) Domains() DomainRepo {
	return &platformDomains{s: s, inner: s.Store.Domains()}
}

func (s *platformStore) ProbeMailboxes() ProbeMailboxRepo {
	return &platformProbeMailboxes{s: s, inner: s.Store.ProbeMailboxes()}
}

func (s *platformStore) BounceMailboxes() BounceMailboxRepo {
	return &platformBounceMailboxes{s: s, inner: s.Store.BounceMailboxes()}
}

// readOnly is the refusal every configuration write to a platform entity gets.
func readOnly(kind, id string) error {
	return fmt.Errorf("%w: %s %s is defined in the platform configuration", ErrReadOnly, kind, id)
}

func notFound(kind, id string) error {
	return fmt.Errorf("%w: %s %s", ErrNotFound, kind, id)
}

// --- list merging ------------------------------------------------------

// The virtual entries come first, on their own synthetic cursors, and the
// backing list follows on its own. Two cursor spaces rather than one keyset
// over both: the virtual rows have no created_at in the table to interleave
// with, and a caller only ever hands back a cursor this package minted.
const (
	platformCursorPrefix = "sys#"
	// platformCursorDone is the cursor that means "the virtual entries are
	// finished, start the backing list".
	platformCursorDone = platformCursorPrefix + "$"
)

// overlayList pages virt followed by whatever list returns, dropping shadow
// rows (which are state, not entities) from the backing page.
func overlayList[T any](
	ctx context.Context, virt []T, p Page, id func(*T) string,
	list func(context.Context, Page) (Result[T], error),
) (Result[T], error) {
	p = p.Normalize()
	backing := func(pg Page) (Result[T], error) {
		res, err := list(ctx, pg)
		if err != nil {
			return Result[T]{}, err
		}
		kept := make([]T, 0, len(res.Items))
		for i := range res.Items {
			if !IsPlatformID(id(&res.Items[i])) {
				kept = append(kept, res.Items[i])
			}
		}
		return Result[T]{Items: kept, NextCursor: res.NextCursor}, nil
	}

	start := 0
	switch {
	case p.Cursor == "":
	case p.Cursor == platformCursorDone:
		return backing(Page{Limit: p.Limit})
	case strings.HasPrefix(p.Cursor, platformCursorPrefix):
		n, err := strconv.Atoi(strings.TrimPrefix(p.Cursor, platformCursorPrefix))
		if err != nil || n < 0 {
			return Result[T]{}, fmt.Errorf("%w: cursor %q", ErrInvalid, p.Cursor)
		}
		start = n
	default:
		// A backing cursor: the virtual entries are already behind us.
		return backing(p)
	}

	out := make([]T, 0, p.Limit)
	for i := start; i < len(virt) && len(out) < p.Limit; i++ {
		out = append(out, virt[i])
	}
	if len(out) == p.Limit {
		next := platformCursorDone
		if start+len(out) < len(virt) {
			next = platformCursorPrefix + strconv.Itoa(start+len(out))
		}
		return Result[T]{Items: out, NextCursor: next}, nil
	}
	res, err := backing(Page{Limit: p.Limit - len(out)})
	if err != nil {
		return Result[T]{}, err
	}
	return Result[T]{Items: append(out, res.Items...), NextCursor: res.NextCursor}, nil
}

// --- transports --------------------------------------------------------

type platformTransports struct {
	s     *platformStore
	inner TransportRepo
}

// transportState is the only part of a platform transport that may be written,
// and the only part a shadow row carries.
type transportState struct {
	status    TransportStatus
	reason    string
	changedAt time.Time
	until     time.Time
	version   int64
}

func (r *platformTransports) virtual(id string) (Transport, bool) {
	for _, t := range r.s.o.transports {
		if t.ID == id {
			return t, true
		}
	}
	return Transport{}, false
}

// merged returns the virtual transport with the shadow row's state applied.
func (r *platformTransports) merged(ctx context.Context, v Transport) (*Transport, error) {
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return nil, err
	}
	shadow, err := sys.Transports().Get(ctx, v.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return &v, nil
	case err != nil:
		return nil, err
	}
	v.Status, v.StatusReason = shadow.Status, shadow.StatusReason
	v.StatusChangedAt, v.StatusUntil = shadow.StatusChangedAt, shadow.StatusUntil
	v.Version, v.UpdatedAt = shadow.Version, shadow.UpdatedAt
	return &v, nil
}

func (r *platformTransports) Create(ctx context.Context, t *Transport) error {
	if t != nil && IsPlatformID(t.ID) {
		return readOnly("transport", t.ID)
	}
	return r.inner.Create(ctx, t)
}

func (r *platformTransports) Get(ctx context.Context, id string) (*Transport, error) {
	if !IsPlatformID(id) {
		return r.inner.Get(ctx, id)
	}
	// A shared transport is the operator's infrastructure. A tenant learns of
	// it only through the sender it is allowed to send with.
	if !r.s.system {
		return nil, notFound("transport", id)
	}
	v, ok := r.virtual(id)
	if !ok {
		return nil, notFound("transport", id)
	}
	return r.merged(ctx, v)
}

func (r *platformTransports) Update(ctx context.Context, t *Transport) error {
	if t == nil || !IsPlatformID(t.ID) {
		return r.inner.Update(ctx, t)
	}
	v, ok := r.virtual(t.ID)
	if !ok || !r.s.system {
		return readOnly("transport", t.ID)
	}
	if !sameTransportConfig(&v, t) {
		return readOnly("transport", t.ID)
	}
	return r.writeShadow(ctx, transportState{
		status: t.Status, reason: t.StatusReason,
		changedAt: t.StatusChangedAt, until: t.StatusUntil, version: t.Version,
	}, t.ID)
}

// writeShadow persists a platform transport's circuit state, and nothing else,
// as a row in the system tenant's transport table.
func (r *platformTransports) writeShadow(ctx context.Context, st transportState, id string) error {
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return err
	}
	apply := func(row *Transport) {
		// Every configuration column is zeroed on the way in, on purpose: the
		// shadow row exists to hold state, and a host or a password on it
		// would be a copy of configuration in a database
		// (store/platformtest).
		*row = Transport{
			ID: id, Shared: true, Version: row.Version,
			Status: st.status, StatusReason: st.reason,
			StatusChangedAt: st.changedAt, StatusUntil: st.until,
		}
	}
	cur, err := sys.Transports().Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		row := &Transport{ID: id}
		apply(row)
		if err := sys.Transports().Create(ctx, row); !errors.Is(err, ErrConflict) {
			return err
		}
		if cur, err = sys.Transports().Get(ctx, id); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	want := st.version
	apply(cur)
	cur.Version = want
	return sys.Transports().Update(ctx, cur)
}

func (r *platformTransports) Delete(ctx context.Context, id string) error {
	if IsPlatformID(id) {
		return readOnly("transport", id)
	}
	return r.inner.Delete(ctx, id)
}

func (r *platformTransports) List(ctx context.Context, p Page) (Result[Transport], error) {
	var virt []Transport
	if r.s.system {
		virt = make([]Transport, 0, len(r.s.o.transports))
		for _, v := range r.s.o.transports {
			m, err := r.merged(ctx, v)
			if err != nil {
				return Result[Transport]{}, err
			}
			virt = append(virt, *m)
		}
	}
	return overlayList(ctx, virt, p,
		func(v *Transport) string { return v.ID }, r.inner.List)
}

// sameTransportConfig reports whether t carries exactly the configuration of
// v. A write that changed anything else is a configuration edit, which only a
// deploy can do.
func sameTransportConfig(v, t *Transport) bool {
	return v.Name == t.Name && v.Host == t.Host && v.Port == t.Port &&
		v.TLS == t.TLS && v.Username == t.Username &&
		string(v.Password) == string(t.Password) &&
		v.MaxConns == t.MaxConns && v.RatePerSecond == t.RatePerSecond &&
		maps.Equal(v.DomainRatePerSecond, t.DomainRatePerSecond) &&
		t.Shared
}

// --- senders -----------------------------------------------------------

type platformSenders struct {
	s     *platformStore
	inner SenderRepo
}

func (r *platformSenders) virtual(id string) (Sender, bool) {
	for _, s := range r.s.o.senders {
		if s.ID == id {
			return s, true
		}
	}
	return Sender{}, false
}

// view returns the copy of a virtual sender this tenant may see. A normal
// tenant gets the identity and no state: a probe verdict about the operator's
// shared reputation is not the tenant's to read (ADR-0017), and its TenantID
// is rewritten to the caller's so that nothing downstream mistakes the sender
// for a row of another tenant.
func (r *platformSenders) view(ctx context.Context, v Sender) (*Sender, error) {
	if !r.s.system {
		v.TenantID = r.s.tenantID
		v.Health, v.HealthReason, v.HealthCheckedAt = HealthUnknown, "", time.Time{}
		v.Version = 0
		return &v, nil
	}
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return nil, err
	}
	shadow, err := sys.Senders().Get(ctx, v.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return &v, nil
	case err != nil:
		return nil, err
	}
	v.Health, v.HealthReason = shadow.Health, shadow.HealthReason
	v.HealthCheckedAt = shadow.HealthCheckedAt
	v.Version, v.UpdatedAt = shadow.Version, shadow.UpdatedAt
	return &v, nil
}

func (r *platformSenders) Create(ctx context.Context, s *Sender) error {
	if s != nil && IsPlatformID(s.ID) {
		return readOnly("sender", s.ID)
	}
	return r.inner.Create(ctx, s)
}

func (r *platformSenders) Get(ctx context.Context, id string) (*Sender, error) {
	if !IsPlatformID(id) {
		return r.inner.Get(ctx, id)
	}
	v, ok := r.virtual(id)
	if !ok {
		return nil, notFound("sender", id)
	}
	return r.view(ctx, v)
}

func (r *platformSenders) Update(ctx context.Context, s *Sender) error {
	if s == nil || !IsPlatformID(s.ID) {
		return r.inner.Update(ctx, s)
	}
	v, ok := r.virtual(s.ID)
	if !ok || !r.s.system {
		return readOnly("sender", s.ID)
	}
	if !sameSenderConfig(&v, s) {
		return readOnly("sender", s.ID)
	}
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return err
	}
	apply := func(row *Sender) {
		*row = Sender{
			ID: s.ID, Shared: true, Version: row.Version,
			Health: s.Health, HealthReason: s.HealthReason,
			HealthCheckedAt: s.HealthCheckedAt,
		}
	}
	cur, err := sys.Senders().Get(ctx, s.ID)
	if errors.Is(err, ErrNotFound) {
		row := &Sender{ID: s.ID}
		apply(row)
		if err := sys.Senders().Create(ctx, row); !errors.Is(err, ErrConflict) {
			return err
		}
		if cur, err = sys.Senders().Get(ctx, s.ID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	want := s.Version
	apply(cur)
	cur.Version = want
	return sys.Senders().Update(ctx, cur)
}

func (r *platformSenders) Delete(ctx context.Context, id string) error {
	if IsPlatformID(id) {
		return readOnly("sender", id)
	}
	return r.inner.Delete(ctx, id)
}

func (r *platformSenders) List(ctx context.Context, p Page) (Result[Sender], error) {
	virt := make([]Sender, 0, len(r.s.o.senders))
	for _, v := range r.s.o.senders {
		out, err := r.view(ctx, v)
		if err != nil {
			return Result[Sender]{}, err
		}
		virt = append(virt, *out)
	}
	return overlayList(ctx, virt, p,
		func(v *Sender) string { return v.ID }, r.inner.List)
}

func sameSenderConfig(v, s *Sender) bool {
	return v.Name == s.Name && v.FromName == s.FromName &&
		v.FromEmail == s.FromEmail && v.ReplyTo == s.ReplyTo &&
		v.TransportID == s.TransportID && v.DomainID == s.DomainID && s.Shared
}

// --- sending domains ---------------------------------------------------

type platformDomains struct {
	s     *platformStore
	inner DomainRepo
}

func (r *platformDomains) virtual(id string) (SendingDomain, bool) {
	for _, d := range r.s.o.domains {
		if d.ID == id {
			return d, true
		}
	}
	return SendingDomain{}, false
}

func (r *platformDomains) merged(ctx context.Context, v SendingDomain) (*SendingDomain, error) {
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return nil, err
	}
	shadow, err := sys.Domains().Get(ctx, v.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return &v, nil
	case err != nil:
		return nil, err
	}
	v.Health, v.HealthReason = shadow.Health, shadow.HealthReason
	v.HealthCheckedAt = shadow.HealthCheckedAt
	v.Version, v.UpdatedAt = shadow.Version, shadow.UpdatedAt
	return &v, nil
}

func (r *platformDomains) Create(ctx context.Context, d *SendingDomain) error {
	if d != nil && IsPlatformID(d.ID) {
		return readOnly("sending domain", d.ID)
	}
	return r.inner.Create(ctx, d)
}

func (r *platformDomains) Get(ctx context.Context, id string) (*SendingDomain, error) {
	if !IsPlatformID(id) {
		return r.inner.Get(ctx, id)
	}
	if !r.s.system {
		return nil, notFound("sending domain", id)
	}
	v, ok := r.virtual(id)
	if !ok {
		return nil, notFound("sending domain", id)
	}
	return r.merged(ctx, v)
}

func (r *platformDomains) Update(ctx context.Context, d *SendingDomain) error {
	if d == nil || !IsPlatformID(d.ID) {
		return r.inner.Update(ctx, d)
	}
	v, ok := r.virtual(d.ID)
	if !ok || !r.s.system || !sameDomainConfig(&v, d) {
		return readOnly("sending domain", d.ID)
	}
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return err
	}
	apply := func(row *SendingDomain) {
		*row = SendingDomain{
			ID: d.ID, Shared: true, Version: row.Version,
			Health: d.Health, HealthReason: d.HealthReason,
			HealthCheckedAt: d.HealthCheckedAt,
		}
	}
	cur, err := sys.Domains().Get(ctx, d.ID)
	if errors.Is(err, ErrNotFound) {
		row := &SendingDomain{ID: d.ID}
		apply(row)
		if err := sys.Domains().Create(ctx, row); !errors.Is(err, ErrConflict) {
			return err
		}
		if cur, err = sys.Domains().Get(ctx, d.ID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	want := d.Version
	apply(cur)
	cur.Version = want
	return sys.Domains().Update(ctx, cur)
}

func (r *platformDomains) Delete(ctx context.Context, id string) error {
	if IsPlatformID(id) {
		return readOnly("sending domain", id)
	}
	return r.inner.Delete(ctx, id)
}

func (r *platformDomains) List(ctx context.Context, p Page) (Result[SendingDomain], error) {
	var virt []SendingDomain
	if r.s.system {
		virt = make([]SendingDomain, 0, len(r.s.o.domains))
		for _, v := range r.s.o.domains {
			m, err := r.merged(ctx, v)
			if err != nil {
				return Result[SendingDomain]{}, err
			}
			virt = append(virt, *m)
		}
	}
	return overlayList(ctx, virt, p,
		func(v *SendingDomain) string { return v.ID }, r.inner.List)
}

func sameDomainConfig(v, d *SendingDomain) bool {
	return v.Domain == d.Domain && v.DKIMSelector == d.DKIMSelector &&
		string(v.DKIMPrivateKey) == string(d.DKIMPrivateKey) &&
		v.ReturnPathDomain == d.ReturnPathDomain &&
		v.ExpectedSPF == d.ExpectedSPF &&
		slicesEqual(v.OutboundIPs, d.OutboundIPs) && d.Shared
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- probe mailboxes ---------------------------------------------------

type platformProbeMailboxes struct {
	s     *platformStore
	inner ProbeMailboxRepo
}

func (r *platformProbeMailboxes) virtual(id string) (ProbeMailbox, bool) {
	for _, m := range r.s.o.probeBoxes {
		if m.ID == id {
			return m, true
		}
	}
	return ProbeMailbox{}, false
}

func (r *platformProbeMailboxes) merged(ctx context.Context, v ProbeMailbox) (*ProbeMailbox, error) {
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return nil, err
	}
	shadow, err := sys.ProbeMailboxes().Get(ctx, v.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return &v, nil
	case err != nil:
		return nil, err
	}
	v.Health = shadow.Health
	v.Version, v.UpdatedAt = shadow.Version, shadow.UpdatedAt
	return &v, nil
}

func (r *platformProbeMailboxes) Create(ctx context.Context, m *ProbeMailbox) error {
	if m != nil && IsPlatformID(m.ID) {
		return readOnly("probe mailbox", m.ID)
	}
	return r.inner.Create(ctx, m)
}

func (r *platformProbeMailboxes) Get(ctx context.Context, id string) (*ProbeMailbox, error) {
	if !IsPlatformID(id) {
		return r.inner.Get(ctx, id)
	}
	if !r.s.system {
		return nil, notFound("probe mailbox", id)
	}
	v, ok := r.virtual(id)
	if !ok {
		return nil, notFound("probe mailbox", id)
	}
	return r.merged(ctx, v)
}

// Update is always a refusal: a probe mailbox has no state on the row itself
// (its health goes through UpdateHealth), so every field Update could write is
// configuration.
func (r *platformProbeMailboxes) Update(ctx context.Context, m *ProbeMailbox) error {
	if m != nil && IsPlatformID(m.ID) {
		return readOnly("probe mailbox", m.ID)
	}
	return r.inner.Update(ctx, m)
}

func (r *platformProbeMailboxes) Delete(ctx context.Context, id string) error {
	if IsPlatformID(id) {
		return readOnly("probe mailbox", id)
	}
	return r.inner.Delete(ctx, id)
}

func (r *platformProbeMailboxes) List(ctx context.Context, p Page) (Result[ProbeMailbox], error) {
	var virt []ProbeMailbox
	if r.s.system {
		virt = make([]ProbeMailbox, 0, len(r.s.o.probeBoxes))
		for _, v := range r.s.o.probeBoxes {
			m, err := r.merged(ctx, v)
			if err != nil {
				return Result[ProbeMailbox]{}, err
			}
			virt = append(virt, *m)
		}
	}
	return overlayList(ctx, virt, p,
		func(v *ProbeMailbox) string { return v.ID }, r.inner.List)
}

func (r *platformProbeMailboxes) UpdateHealth(ctx context.Context, id string, h MailboxHealth) error {
	if !IsPlatformID(id) {
		return r.inner.UpdateHealth(ctx, id, h)
	}
	if _, ok := r.virtual(id); !ok || !r.s.system {
		return readOnly("probe mailbox", id)
	}
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return err
	}
	if err := sys.ProbeMailboxes().UpdateHealth(ctx, id, h); !errors.Is(err, ErrNotFound) {
		return err
	}
	// No shadow row yet. UpdateHealth deliberately does not insert
	// (store.MailboxHealth), so the row is created here, state-only.
	err = sys.ProbeMailboxes().Create(ctx, &ProbeMailbox{ID: id, Shared: true, Health: h})
	if errors.Is(err, ErrConflict) {
		return sys.ProbeMailboxes().UpdateHealth(ctx, id, h)
	}
	return err
}

// --- bounce mailboxes --------------------------------------------------

type platformBounceMailboxes struct {
	s     *platformStore
	inner BounceMailboxRepo
}

func (r *platformBounceMailboxes) virtual(id string) (BounceMailbox, bool) {
	for _, m := range r.s.o.bounceBoxes {
		if m.ID == id {
			return m, true
		}
	}
	return BounceMailbox{}, false
}

func (r *platformBounceMailboxes) merged(ctx context.Context, v BounceMailbox) (*BounceMailbox, error) {
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return nil, err
	}
	shadow, err := sys.BounceMailboxes().Get(ctx, v.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return &v, nil
	case err != nil:
		return nil, err
	}
	v.Health = shadow.Health
	v.Version, v.UpdatedAt = shadow.Version, shadow.UpdatedAt
	return &v, nil
}

func (r *platformBounceMailboxes) Create(ctx context.Context, m *BounceMailbox) error {
	if m != nil && IsPlatformID(m.ID) {
		return readOnly("bounce mailbox", m.ID)
	}
	return r.inner.Create(ctx, m)
}

func (r *platformBounceMailboxes) Get(ctx context.Context, id string) (*BounceMailbox, error) {
	if !IsPlatformID(id) {
		return r.inner.Get(ctx, id)
	}
	if !r.s.system {
		return nil, notFound("bounce mailbox", id)
	}
	v, ok := r.virtual(id)
	if !ok {
		return nil, notFound("bounce mailbox", id)
	}
	return r.merged(ctx, v)
}

func (r *platformBounceMailboxes) Update(ctx context.Context, m *BounceMailbox) error {
	if m != nil && IsPlatformID(m.ID) {
		return readOnly("bounce mailbox", m.ID)
	}
	return r.inner.Update(ctx, m)
}

func (r *platformBounceMailboxes) Delete(ctx context.Context, id string) error {
	if IsPlatformID(id) {
		return readOnly("bounce mailbox", id)
	}
	return r.inner.Delete(ctx, id)
}

func (r *platformBounceMailboxes) List(ctx context.Context, p Page) (Result[BounceMailbox], error) {
	var virt []BounceMailbox
	if r.s.system {
		virt = make([]BounceMailbox, 0, len(r.s.o.bounceBoxes))
		for _, v := range r.s.o.bounceBoxes {
			m, err := r.merged(ctx, v)
			if err != nil {
				return Result[BounceMailbox]{}, err
			}
			virt = append(virt, *m)
		}
	}
	return overlayList(ctx, virt, p,
		func(v *BounceMailbox) string { return v.ID }, r.inner.List)
}

func (r *platformBounceMailboxes) ListEnabled(ctx context.Context) ([]BounceMailbox, error) {
	rows, err := r.inner.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BounceMailbox, 0, len(rows)+len(r.s.o.bounceBoxes))
	if r.s.system {
		for _, v := range r.s.o.bounceBoxes {
			if !v.Enabled {
				continue
			}
			m, err := r.merged(ctx, v)
			if err != nil {
				return nil, err
			}
			out = append(out, *m)
		}
	}
	for i := range rows {
		// A shadow row is state, not a mailbox to poll. It also has no host to
		// dial, so leaving it in would give the poller a mailbox that can only
		// fail.
		if !IsPlatformID(rows[i].ID) {
			out = append(out, rows[i])
		}
	}
	return out, nil
}

func (r *platformBounceMailboxes) UpdateHealth(ctx context.Context, id string, h MailboxHealth) error {
	if !IsPlatformID(id) {
		return r.inner.UpdateHealth(ctx, id, h)
	}
	if _, ok := r.virtual(id); !ok || !r.s.system {
		return readOnly("bounce mailbox", id)
	}
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return err
	}
	if err := sys.BounceMailboxes().UpdateHealth(ctx, id, h); !errors.Is(err, ErrNotFound) {
		return err
	}
	err = sys.BounceMailboxes().Create(ctx, &BounceMailbox{ID: id, Shared: true, Health: h})
	if errors.Is(err, ErrConflict) {
		return sys.BounceMailboxes().UpdateHealth(ctx, id, h)
	}
	return err
}

// --- tenant settings: the platform fallbacks ---------------------------

// The platform catalog carries two settings an operator can supply on a
// tenant's behalf: the tracking domain and the unsubscribe URL template
// (PlatformCatalog.TrackingDomain, PlatformCatalog.UnsubscribeURLTemplate).
// A tenant that has configured neither should still get working opens, clicks
// and unsubscribe links, off the operator's own domain.
//
// The fallback is applied here, on read, rather than in each of the three
// consumers (the sender, the control plane, the settings endpoint): one place
// to get it right, and no chance of the API showing a tracking domain the
// sender does not use. The read marks which fields it filled in
// (TenantSettings.PlatformDefaults), and the write strips them again, so that
// a client reading the effective value and PUTting it back does not silently
// adopt the operator's default as the tenant's own copy — a later change to
// the platform default would then not reach that tenant.

func (s *platformStore) TenantSettings() TenantSettingsRepo {
	if s.o.cat.TrackingDomain == "" && s.o.cat.UnsubscribeURLTemplate == "" {
		return s.Store.TenantSettings()
	}
	return &platformSettings{s: s, inner: s.Store.TenantSettings()}
}

type platformSettings struct {
	s     *platformStore
	inner TenantSettingsRepo
}

func (r *platformSettings) Get(ctx context.Context) (*TenantSettings, error) {
	v, err := r.inner.Get(ctx)
	if err != nil {
		return nil, err
	}
	r.applyDefaults(v)
	return v, nil
}

func (r *platformSettings) Create(ctx context.Context, v *TenantSettings) error {
	// A brand new row is stored exactly as the caller built it. The defaults
	// are a read-time overlay, so writing them would be storing configuration.
	r.strip(v)
	if err := r.inner.Create(ctx, v); err != nil {
		return err
	}
	r.applyDefaults(v)
	return nil
}

func (r *platformSettings) Update(ctx context.Context, v *TenantSettings) error {
	r.strip(v)
	if err := r.inner.Update(ctx, v); err != nil {
		return err
	}
	r.applyDefaults(v)
	return nil
}

// applyDefaults fills the empty fields from the catalog and records that it
// did.
func (r *platformSettings) applyDefaults(v *TenantSettings) {
	if v == nil {
		return
	}
	v.PlatformDefaults = nil
	if v.Tracking.Domain == "" && r.s.o.cat.TrackingDomain != "" {
		v.Tracking.Domain = r.s.o.cat.TrackingDomain
		v.PlatformDefaults = append(v.PlatformDefaults, SettingTrackingDomain)
	}
	if v.UnsubscribeURLTemplate == "" && r.s.o.cat.UnsubscribeURLTemplate != "" {
		v.UnsubscribeURLTemplate = r.s.o.cat.UnsubscribeURLTemplate
		v.PlatformDefaults = append(v.PlatformDefaults, SettingUnsubscribeURLTemplate)
	}
}

// strip removes a filled-in default again, so the row stores only what the
// tenant itself set. A value the tenant deliberately set to the same string as
// the platform default is indistinguishable from the default and is treated as
// the default, which changes nothing about what is sent.
func (r *platformSettings) strip(v *TenantSettings) {
	if v == nil {
		return
	}
	if v.FromPlatform(SettingTrackingDomain) && v.Tracking.Domain == r.s.o.cat.TrackingDomain {
		v.Tracking.Domain = ""
	}
	if v.FromPlatform(SettingUnsubscribeURLTemplate) &&
		v.UnsubscribeURLTemplate == r.s.o.cat.UnsubscribeURLTemplate {
		v.UnsubscribeURLTemplate = ""
	}
	v.PlatformDefaults = nil
}
