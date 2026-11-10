package sendplane_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

// rewrapCipher is AES-GCM with the SecretContext as associated data, plus the
// context-free open a pre-binding deployment used, which is what the rewrap
// needs as its LegacyDecrypter.
type rewrapCipher struct{ gcm cipher.AEAD }

func newRewrapCipher(t *testing.T) *rewrapCipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return &rewrapCipher{gcm: gcm}
}

func (c *rewrapCipher) seal(p, aad []byte) []byte {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return c.gcm.Seal(nonce, nonce, p, aad)
}

func (c *rewrapCipher) open(ct, aad []byte) ([]byte, error) {
	n := c.gcm.NonceSize()
	if len(ct) < n {
		return nil, errors.New("short ciphertext")
	}
	return c.gcm.Open(nil, ct[:n], ct[n:], aad)
}

func (c *rewrapCipher) Encrypt(_ context.Context, p []byte, sc store.SecretContext) ([]byte, error) {
	return c.seal(p, sc.AAD()), nil
}

func (c *rewrapCipher) Decrypt(_ context.Context, ct []byte, sc store.SecretContext) ([]byte, error) {
	return c.open(ct, sc.AAD())
}

func (c *rewrapCipher) legacyOpen(_ context.Context, ct []byte) ([]byte, error) {
	return c.open(ct, nil)
}
func (c *rewrapCipher) legacySeal(p string) []byte { return c.seal([]byte(p), nil) }

// The plaintexts the fixture stores. None of them may appear in a report.
const (
	plainTransportA = "transport-a-plaintext"
	plainDKIM       = "dkim-plaintext"
	plainBounce     = "bounce-plaintext"
	plainProbe      = "probe-plaintext"
	plainTransportB = "transport-b-plaintext"
	plainSystem     = "system-plaintext"
)

type rewrapFixture struct {
	p   *memstore.Provider
	c   *rewrapCipher
	a   store.Store
	b   store.Store
	sys store.Store

	trA, trB, trBroken, trSys, trShadow string
	domA, bounceA, probeA               string
}

func newRewrapFixture(t *testing.T) *rewrapFixture {
	t.Helper()
	ctx := context.Background()
	f := &rewrapFixture{p: memstore.New(), c: newRewrapCipher(t)}
	t.Cleanup(func() { _ = f.p.Close() })
	var err error
	for _, s := range []struct {
		dst *store.Store
		id  string
	}{{&f.a, "tenant-a"}, {&f.b, "tenant-b"}, {&f.sys, store.SystemTenantID}} {
		if *s.dst, err = f.p.ForTenant(ctx, s.id); err != nil {
			t.Fatal(err)
		}
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	// Tenant A: one legacy ciphertext in every column, plus rows with no
	// secret at all (an unauthenticated relay, a webhook probe mailbox).
	tr := &store.Transport{Name: "a", Host: "smtp.a", Port: 587, Password: f.c.legacySeal(plainTransportA)}
	must("transport a", f.a.Transports().Create(ctx, tr))
	f.trA = tr.ID
	must("transport a, no password", f.a.Transports().Create(ctx, &store.Transport{Name: "open", Host: "smtp.a", Port: 25}))
	d := &store.SendingDomain{Domain: "a.example", DKIMSelector: "s1", DKIMPrivateKey: f.c.legacySeal(plainDKIM)}
	must("domain a", f.a.Domains().Create(ctx, d))
	f.domA = d.ID
	bm := &store.BounceMailbox{Name: "bounces", Host: "imap.a", Port: 993, Password: f.c.legacySeal(plainBounce), Enabled: true}
	must("bounce mailbox a", f.a.BounceMailboxes().Create(ctx, bm))
	f.bounceA = bm.ID
	pm := &store.ProbeMailbox{Name: "probe", Address: "probe@a.example", Host: "imap.a", Port: 993, Password: f.c.legacySeal(plainProbe)}
	must("probe mailbox a", f.a.ProbeMailboxes().Create(ctx, pm))
	f.probeA = pm.ID
	must("webhook probe mailbox a", f.a.ProbeMailboxes().Create(ctx, &store.ProbeMailbox{
		Name: "hook", Kind: store.ProbeMailboxWebhook, Address: "hook@a.example",
	}))

	// Tenant B: one row already bound (written by the new version), and one
	// whose bytes no key opens.
	trB := &store.Transport{ID: store.NewID(), Name: "b", Host: "smtp.b", Port: 587}
	trB.TenantID = "tenant-b"
	trB.Password = f.c.seal([]byte(plainTransportB), store.TransportPasswordContext(trB).AAD())
	must("transport b", f.b.Transports().Create(ctx, trB))
	f.trB = trB.ID
	broken := &store.Transport{Name: "broken", Host: "smtp.b", Port: 587, Password: []byte("not a ciphertext at all, long enough")}
	must("transport b broken", f.b.Transports().Create(ctx, broken))
	f.trBroken = broken.ID

	// The system tenant: a row of its own, which is rewrapped like any
	// other, and a platform shadow row with a virtual ID, which is not. The
	// shadow carries a value here only to prove it is skipped; a real one
	// has every configuration column empty.
	sysTr := &store.Transport{Name: "own", Host: "smtp.sys", Port: 587, Password: f.c.legacySeal(plainSystem)}
	must("system transport", f.sys.Transports().Create(ctx, sysTr))
	f.trSys = sysTr.ID
	shadow := &store.Transport{ID: store.PlatformID("shared-a"), Shared: true, Password: f.c.legacySeal("shadow")}
	must("shadow transport", f.sys.Transports().Create(ctx, shadow))
	f.trShadow = shadow.ID
	return f
}

func (f *rewrapFixture) rewrap(t *testing.T, dryRun bool) sendplane.RewrapReport {
	t.Helper()
	rep, err := sendplane.RewrapSecrets(context.Background(), f.p, f.c.legacyOpen, f.c,
		sendplane.RewrapOptions{DryRun: dryRun})
	if err != nil {
		t.Fatalf("RewrapSecrets: %v", err)
	}
	// Whatever happened, the report never carries a secret.
	dump := fmt.Sprintf("%+v", rep)
	for _, k := range rep.Kinds {
		dump += fmt.Sprintf("%+v", *k)
	}
	for _, p := range []string{plainTransportA, plainDKIM, plainBounce, plainProbe, plainTransportB, plainSystem} {
		if strings.Contains(dump, p) {
			t.Fatalf("the report contains a plaintext: %s", dump)
		}
	}
	return rep
}

func wantCounts(t *testing.T, rep sendplane.RewrapReport, kind string, migrated, already, failed int) {
	t.Helper()
	c := rep.Kinds[kind]
	if c == nil {
		t.Fatalf("%s: no counts in the report", kind)
	}
	if c.Migrated != migrated || c.AlreadyMigrated != already || len(c.Failed) != failed {
		t.Errorf("%s: migrated %d, already %d, failed %d (%+v); want %d, %d, %d",
			kind, c.Migrated, c.AlreadyMigrated, len(c.Failed), c.Failed, migrated, already, failed)
	}
}

// opens checks that a stored value decrypts to want under its row's context
// and no longer under the legacy, context-free decryption.
func (f *rewrapFixture) opens(t *testing.T, what string, ct []byte, sc store.SecretContext, want string) {
	t.Helper()
	got, err := f.c.Decrypt(context.Background(), ct, sc)
	if err != nil || string(got) != want {
		t.Errorf("%s: bound decrypt = %q, %v; want %q", what, got, err, want)
	}
	if _, err := f.c.legacyOpen(context.Background(), ct); err == nil {
		t.Errorf("%s: still opens without its context", what)
	}
}

func TestRewrapSecrets(t *testing.T) {
	ctx := context.Background()
	f := newRewrapFixture(t)
	before, err := f.a.Transports().Get(ctx, f.trA)
	if err != nil {
		t.Fatal(err)
	}

	// A dry run reports what it would do and writes nothing.
	rep := f.rewrap(t, true)
	if !rep.DryRun {
		t.Error("the report does not say it was a dry run")
	}
	wantCounts(t, rep, store.PurposeTransportPassword, 2, 1, 1)
	wantCounts(t, rep, store.PurposeDomainDKIMPrivateKey, 1, 0, 0)
	wantCounts(t, rep, store.PurposeBounceMailboxPassword, 1, 0, 0)
	wantCounts(t, rep, store.PurposeProbeMailboxPassword, 1, 0, 0)
	after, err := f.a.Transports().Get(ctx, f.trA)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Password) != string(before.Password) || after.Version != before.Version {
		t.Fatal("the dry run wrote to the store")
	}

	// The real run migrates the same rows.
	rep = f.rewrap(t, false)
	wantCounts(t, rep, store.PurposeTransportPassword, 2, 1, 1)
	wantCounts(t, rep, store.PurposeDomainDKIMPrivateKey, 1, 0, 0)
	wantCounts(t, rep, store.PurposeBounceMailboxPassword, 1, 0, 0)
	wantCounts(t, rep, store.PurposeProbeMailboxPassword, 1, 0, 0)
	if fl := rep.Kinds[store.PurposeTransportPassword].Failed; len(fl) == 1 &&
		(fl[0].TenantID != "tenant-b" || fl[0].ID != f.trBroken || fl[0].Reason == "") {
		t.Errorf("failure = %+v, want tenant-b/%s with a reason", fl[0], f.trBroken)
	}
	if rep.FailedTotal() != 1 {
		t.Errorf("FailedTotal = %d, want 1", rep.FailedTotal())
	}

	tr, err := f.a.Transports().Get(ctx, f.trA)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Version != before.Version+1 {
		t.Errorf("version %d, want %d: the write did not go through Update", tr.Version, before.Version+1)
	}
	f.opens(t, "transport a", tr.Password, store.TransportPasswordContext(tr), plainTransportA)
	dom, err := f.a.Domains().Get(ctx, f.domA)
	if err != nil {
		t.Fatal(err)
	}
	f.opens(t, "dkim key", dom.DKIMPrivateKey, store.DomainDKIMKeyContext(dom), plainDKIM)
	bm, err := f.a.BounceMailboxes().Get(ctx, f.bounceA)
	if err != nil {
		t.Fatal(err)
	}
	f.opens(t, "bounce mailbox", bm.Password, store.BounceMailboxPasswordContext(bm), plainBounce)
	pm, err := f.a.ProbeMailboxes().Get(ctx, f.probeA)
	if err != nil {
		t.Fatal(err)
	}
	f.opens(t, "probe mailbox", pm.Password, store.ProbeMailboxPasswordContext(pm), plainProbe)
	sysTr, err := f.sys.Transports().Get(ctx, f.trSys)
	if err != nil {
		t.Fatal(err)
	}
	f.opens(t, "system transport", sysTr.Password, store.TransportPasswordContext(sysTr), plainSystem)

	// The platform shadow row was not touched.
	shadow, err := f.sys.Transports().Get(ctx, f.trShadow)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.c.legacyOpen(ctx, shadow.Password); err != nil || string(got) != "shadow" {
		t.Error("the sys: shadow row was rewritten")
	}
	// Nor was the row that failed.
	broken, err := f.b.Transports().Get(ctx, f.trBroken)
	if err != nil {
		t.Fatal(err)
	}
	if string(broken.Password) != "not a ciphertext at all, long enough" {
		t.Error("a row that failed to migrate was written")
	}

	// A rerun finds everything done: it is idempotent, which is what makes
	// it safe to run again after fixing a failure.
	rep = f.rewrap(t, false)
	wantCounts(t, rep, store.PurposeTransportPassword, 0, 3, 1)
	wantCounts(t, rep, store.PurposeDomainDKIMPrivateKey, 0, 1, 0)
	wantCounts(t, rep, store.PurposeBounceMailboxPassword, 0, 1, 0)
	wantCounts(t, rep, store.PurposeProbeMailboxPassword, 0, 1, 0)
	again, err := f.a.Transports().Get(ctx, f.trA)
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != tr.Version {
		t.Error("a rerun rewrote an already migrated row")
	}
}

func TestRewrapSecretsRequiresBothCiphers(t *testing.T) {
	c := newRewrapCipher(t)
	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	if _, err := sendplane.RewrapSecrets(context.Background(), p, nil, c, sendplane.RewrapOptions{}); err == nil {
		t.Error("no legacy decrypter accepted")
	}
	if _, err := sendplane.RewrapSecrets(context.Background(), p, c.legacyOpen, nil, sendplane.RewrapOptions{}); err == nil {
		t.Error("no cipher accepted")
	}
}
