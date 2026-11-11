package sendplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sendplane/sendplane/store"
)

// LegacyDecrypter opens a ciphertext written before secrets were bound to
// their row: the old, context-free Decrypt of the host's cipher. For an AEAD
// that is Open with no associated data.
//
// It is only ever handed to RewrapSecrets. Nothing on the serving path takes
// one, and nothing should: a decrypt that ignores the row is exactly what lets
// a copied ciphertext be used somewhere else.
type LegacyDecrypter func(ctx context.Context, ciphertext []byte) ([]byte, error)

// RewrapOptions configures RewrapSecrets.
type RewrapOptions struct {
	// DryRun reads and decrypts everything and writes nothing. The report
	// counts what would have been migrated as Migrated.
	DryRun bool
}

// RewrapReport is what RewrapSecrets did, per kind of secret. It names rows
// by tenant and ID and never carries a secret, encrypted or not.
type RewrapReport struct {
	DryRun bool
	// Kinds is keyed by the store.Purpose* constant of the column.
	Kinds map[string]*RewrapCounts
}

// RewrapCounts is the outcome for one kind of secret.
type RewrapCounts struct {
	// Migrated is the number of ciphertexts re-encrypted under their row's
	// context (or that would have been, on a dry run).
	Migrated int
	// AlreadyMigrated is the number that already decrypt under their row's
	// context. A rerun counts everything it migrated before here, which is
	// what makes the operation safe to repeat.
	AlreadyMigrated int
	// Failed lists the rows that could not be migrated.
	Failed []RewrapFailure
}

// RewrapFailure is one row RewrapSecrets could not migrate.
type RewrapFailure struct {
	TenantID string
	ID       string
	// Reason says which step failed. It never contains the secret.
	Reason string
}

// FailedTotal is the number of failed rows over every kind.
func (r RewrapReport) FailedTotal() int {
	n := 0
	for _, c := range r.Kinds {
		n += len(c.Failed)
	}
	return n
}

// RewrapSecrets re-encrypts every stored secret under the SecretContext of the
// row it belongs to, for a deployment upgrading from a version whose
// SecretCipher took no context (architecture 16).
//
// There is no runtime fallback: once the new version runs, a sender, poller or
// probe that meets a not-yet-rewrapped ciphertext fails to decrypt it. Run this
// once after deploying the new version and before anything needs the secrets,
// or with the sender and bounce roles stopped.
//
// It walks every tenant provider.Tenants reports, plus SystemTenantID, and in
// each the transport passwords, sending-domain DKIM keys, bounce-mailbox
// passwords and probe-mailbox passwords. For each non-empty ciphertext:
//
//   - if it already decrypts with cipher under its row's context it is left
//     alone and counted as AlreadyMigrated;
//   - otherwise legacy decrypts it, cipher re-encrypts it under the row's
//     context, the result is checked to decrypt back, and it is written with
//     the repository's normal Update (optimistic concurrency included: a row
//     edited meanwhile is a Failed conflict, and a rerun picks it up);
//   - a ciphertext neither opens is Failed, and nothing is written for it.
//
// provider must be the raw store, not one wrapped by store.WithPlatform:
// platform secrets live in configuration and are encrypted at boot, never in
// the database. Rows with a virtual "sys:" ID (the system tenant's state
// shadow rows, whose configuration columns are empty) are skipped either way.
//
// The returned error is for a failure that stopped the walk (listing tenants,
// opening a tenant's store, reading a page); the report then holds what was
// done up to that point. Per-row failures are only in the report.
func RewrapSecrets(ctx context.Context, provider store.Provider, legacy LegacyDecrypter, cipher SecretCipher, opts RewrapOptions) (RewrapReport, error) {
	rep := RewrapReport{DryRun: opts.DryRun, Kinds: map[string]*RewrapCounts{}}
	if provider == nil || legacy == nil || cipher == nil {
		return rep, errors.New("sendplane: rewrap needs a provider, a legacy decrypter and a cipher")
	}
	for _, k := range []string{
		store.PurposeTransportPassword, store.PurposeDomainDKIMPrivateKey,
		store.PurposeBounceMailboxPassword, store.PurposeProbeMailboxPassword,
	} {
		rep.Kinds[k] = &RewrapCounts{}
	}

	tenants, err := provider.Tenants(ctx)
	if err != nil {
		return rep, fmt.Errorf("sendplane: rewrap: listing tenants: %w", err)
	}
	// The system tenant is never in Tenants, and may still own rows of its
	// own besides the platform shadows.
	if !slices.Contains(tenants, store.SystemTenantID) {
		tenants = append(tenants, store.SystemTenantID)
	}

	w := rewrapper{legacy: legacy, cipher: cipher, dryRun: opts.DryRun, rep: &rep}
	for _, tenantID := range tenants {
		st, err := provider.ForTenant(ctx, tenantID)
		if err != nil {
			return rep, fmt.Errorf("sendplane: rewrap: tenant %s: %w", tenantID, err)
		}
		if err := rewrapColumn(ctx, w, tenantID, secretColumn[store.Transport]{
			kind: store.PurposeTransportPassword,
			list: st.Transports().List, update: st.Transports().Update,
			id:     func(v *store.Transport) string { return v.ID },
			secret: func(v *store.Transport) *[]byte { return &v.Password },
			sc:     store.TransportPasswordContext,
		}); err != nil {
			return rep, err
		}
		if err := rewrapColumn(ctx, w, tenantID, secretColumn[store.SendingDomain]{
			kind: store.PurposeDomainDKIMPrivateKey,
			list: st.Domains().List, update: st.Domains().Update,
			id:     func(v *store.SendingDomain) string { return v.ID },
			secret: func(v *store.SendingDomain) *[]byte { return &v.DKIMPrivateKey },
			sc:     store.DomainDKIMKeyContext,
		}); err != nil {
			return rep, err
		}
		if err := rewrapColumn(ctx, w, tenantID, secretColumn[store.BounceMailbox]{
			kind: store.PurposeBounceMailboxPassword,
			list: st.BounceMailboxes().List, update: st.BounceMailboxes().Update,
			id:     func(v *store.BounceMailbox) string { return v.ID },
			secret: func(v *store.BounceMailbox) *[]byte { return &v.Password },
			sc:     store.BounceMailboxPasswordContext,
		}); err != nil {
			return rep, err
		}
		if err := rewrapColumn(ctx, w, tenantID, secretColumn[store.ProbeMailbox]{
			kind: store.PurposeProbeMailboxPassword,
			list: st.ProbeMailboxes().List, update: st.ProbeMailboxes().Update,
			id:     func(v *store.ProbeMailbox) string { return v.ID },
			secret: func(v *store.ProbeMailbox) *[]byte { return &v.Password },
			sc:     store.ProbeMailboxPasswordContext,
		}); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

type rewrapper struct {
	legacy LegacyDecrypter
	cipher SecretCipher
	dryRun bool
	rep    *RewrapReport
}

// secretColumn is one encrypted column of one aggregate.
type secretColumn[T any] struct {
	kind   string
	list   func(context.Context, store.Page) (store.Result[T], error)
	update func(context.Context, *T) error
	id     func(*T) string
	secret func(*T) *[]byte
	sc     func(*T) store.SecretContext
}

// rewrapPageSize is the page the walk reads with. Configuration tables are
// small; the size only bounds how much is held at once.
const rewrapPageSize = 200

func rewrapColumn[T any](ctx context.Context, w rewrapper, tenantID string, col secretColumn[T]) error {
	counts := w.rep.Kinds[col.kind]
	page := store.Page{Limit: rewrapPageSize}
	for {
		res, err := col.list(ctx, page)
		if err != nil {
			return fmt.Errorf("sendplane: rewrap: tenant %s: listing %s: %w", tenantID, col.kind, err)
		}
		for i := range res.Items {
			row := &res.Items[i]
			id := col.id(row)
			field := col.secret(row)
			if store.IsPlatformID(id) || len(*field) == 0 {
				continue
			}
			sc := col.sc(row)
			fail := func(reason string) {
				counts.Failed = append(counts.Failed, RewrapFailure{TenantID: tenantID, ID: id, Reason: reason})
			}

			if plain, err := w.cipher.Decrypt(ctx, *field, sc); err == nil {
				clear(plain)
				counts.AlreadyMigrated++
				continue
			}
			// The reasons below are fixed strings on purpose: a decrypter's
			// error is the host's code, and nothing here may risk carrying a
			// secret into a report or a log.
			plain, err := w.legacy(ctx, *field)
			if err != nil {
				fail("neither the bound nor the legacy decryption opens the stored ciphertext")
				continue
			}
			enc, err := w.cipher.Encrypt(ctx, plain, sc)
			if err != nil {
				clear(plain)
				fail("re-encrypting under the row's context failed")
				continue
			}
			back, err := w.cipher.Decrypt(ctx, enc, sc)
			ok := err == nil && bytes.Equal(back, plain)
			clear(back)
			clear(plain)
			if !ok {
				fail("the re-encrypted value does not decrypt back under the row's context")
				continue
			}
			if w.dryRun {
				counts.Migrated++
				continue
			}
			*field = enc
			if err := col.update(ctx, row); err != nil {
				// A store error names the row and the version, never a value.
				fail("writing the re-encrypted value failed: " + err.Error())
				continue
			}
			counts.Migrated++
		}
		if res.NextCursor == "" {
			return nil
		}
		page.Cursor = res.NextCursor
	}
}
