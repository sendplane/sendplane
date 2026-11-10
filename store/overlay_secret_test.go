package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

// prefixCipher "encrypts" by prefixing the context's AAD and refuses any
// other context on the way back. It is not encryption; it is the smallest
// thing that fails the way a real binding cipher fails.
type prefixCipher struct{}

func (prefixCipher) Encrypt(_ context.Context, p []byte, sc store.SecretContext) ([]byte, error) {
	return append(sc.AAD(), p...), nil
}

func (prefixCipher) Decrypt(_ context.Context, c []byte, sc store.SecretContext) ([]byte, error) {
	aad := sc.AAD()
	if !bytes.HasPrefix(c, aad) {
		return nil, errors.New("bound to another context")
	}
	return c[len(aad):], nil
}

// The overlay encrypts each platform secret under the context of the virtual
// entity it serves it on, so that the decrypting side — which builds the
// context from the entity it was handed, exactly like it does for a row —
// opens it. That has to hold after a state write too, when the entity is
// merged over its shadow row.
func TestPlatformSecretsAreBoundToTheVirtualEntity(t *testing.T) {
	ctx := context.Background()
	inner := memstore.New()
	t.Cleanup(func() { _ = inner.Close() })
	cat := store.PlatformCatalog{
		Transports: []store.PlatformTransport{{ID: "relay", Name: "r", Host: "smtp.example.com", Port: 587, Password: "relay-pw"}},
		Domains:    []store.PlatformDomain{{ID: "dom", Domain: "mail.example.com", DKIMSelector: "s1", DKIMPrivateKey: "dkim-pem"}},
		ProbeMailboxes: []store.PlatformProbeMailbox{{
			ID: "probe", Name: "p", Address: "probe@mail.example.com", Host: "imap.example.com", Port: 993, Password: "probe-pw",
		}},
		BounceMailboxes: []store.PlatformBounceMailbox{{
			ID: "bounce", Name: "b", Host: "imap.example.com", Port: 993, Password: "bounce-pw",
		}},
	}
	p := store.WithPlatform(inner, cat, prefixCipher{}, time.Now)
	sys, err := store.PlatformView(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	open := func(what string, ct []byte, sc store.SecretContext, want string) {
		t.Helper()
		if sc.TenantID != store.SystemTenantID || !store.IsPlatformID(sc.ObjectID) {
			t.Errorf("%s: context %+v is not the virtual entity's", what, sc)
		}
		got, err := prefixCipher{}.Decrypt(ctx, ct, sc)
		if err != nil || string(got) != want {
			t.Errorf("%s: decrypt = %q, %v; want %q", what, got, err, want)
		}
	}

	tr, err := sys.Transports().Get(ctx, "sys:relay")
	if err != nil {
		t.Fatal(err)
	}
	open("transport", tr.Password, store.TransportPasswordContext(tr), "relay-pw")
	dom, err := sys.Domains().Get(ctx, "sys:dom")
	if err != nil {
		t.Fatal(err)
	}
	open("domain", dom.DKIMPrivateKey, store.DomainDKIMKeyContext(dom), "dkim-pem")
	pm, err := sys.ProbeMailboxes().Get(ctx, "sys:probe")
	if err != nil {
		t.Fatal(err)
	}
	open("probe mailbox", pm.Password, store.ProbeMailboxPasswordContext(pm), "probe-pw")
	boxes, err := sys.BounceMailboxes().ListEnabled(ctx)
	if err != nil || len(boxes) != 1 {
		t.Fatalf("ListEnabled = %d boxes, %v", len(boxes), err)
	}
	open("bounce mailbox", boxes[0].Password, store.BounceMailboxPasswordContext(&boxes[0]), "bounce-pw")

	// A circuit-state write lands on the shadow row; the merged entity still
	// carries the context the secret was sealed under.
	tr.Status, tr.StatusReason = store.TransportUnhealthy, "test"
	if err := sys.Transports().Update(ctx, tr); err != nil {
		t.Fatal(err)
	}
	merged, err := sys.Transports().Get(ctx, "sys:relay")
	if err != nil {
		t.Fatal(err)
	}
	if merged.Status != store.TransportUnhealthy {
		t.Fatalf("status = %s, the state write did not land", merged.Status)
	}
	open("merged transport", merged.Password, store.TransportPasswordContext(merged), "relay-pw")
}
