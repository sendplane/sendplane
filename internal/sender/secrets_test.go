package sender

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/chaossmtp"
	"github.com/sendplane/sendplane/store"
)

// aeadCipher is an AES-GCM SecretCipher binding the SecretContext as
// associated data, the way the reference binary's cipher does.
type aeadCipher struct{ gcm cipher.AEAD }

func newAEADCipher(t *testing.T) *aeadCipher {
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
	return &aeadCipher{gcm: gcm}
}

func (c *aeadCipher) Encrypt(_ context.Context, p []byte, sc store.SecretContext) ([]byte, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.gcm.Seal(nonce, nonce, p, sc.AAD()), nil
}

func (c *aeadCipher) Decrypt(_ context.Context, ct []byte, sc store.SecretContext) ([]byte, error) {
	n := c.gcm.NonceSize()
	if len(ct) < n {
		return nil, errors.New("short ciphertext")
	}
	return c.gcm.Open(nil, ct[:n], ct[n:], sc.AAD())
}

func (c *aeadCipher) seal(t *testing.T, plain string, sc store.SecretContext) []byte {
	t.Helper()
	b, err := c.Encrypt(context.Background(), []byte(plain), sc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A transport password ciphertext copied out of another tenant's transport
// row is useless to whoever copied it: the sender refuses to decrypt it and
// never logs in, even though the key, the plaintext and the relay are all
// right. The same password sealed for the row itself then sends.
func TestTransportPasswordCopiedFromAnotherTenantIsRefused(t *testing.T) {
	const password = "right"
	f := newFixture(t, fixtureOptions{
		chaos: chaossmtp.Options{Username: "user", Password: password, RequireAuth: true},
	})
	ctx := context.Background()
	c := newAEADCipher(t)

	// Tenant B owns a transport with the very same password, correctly
	// encrypted for B's row.
	other, err := f.p.ForTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	trB := &store.Transport{ID: store.NewID(), TenantID: "tenant-b", Name: "b", Host: "127.0.0.1", Port: 25, Username: "user"}
	trB.Password = c.seal(t, password, store.TransportPasswordContext(trB))
	if err := other.Transports().Create(ctx, trB); err != nil {
		t.Fatal(err)
	}

	// Somebody with write access to the database copies it into tenant A's
	// transport.
	trA, err := f.st.Transports().Get(ctx, f.transportID)
	if err != nil {
		t.Fatal(err)
	}
	trA.Password = append([]byte(nil), trB.Password...)
	if err := f.st.Transports().Update(ctx, trA); err != nil {
		t.Fatal(err)
	}

	withCipher := func(cfg *Config) { cfg.Secrets = c }
	id := f.insertOne(nil)
	stop := f.runSenders(1, withCipher)
	deadline := time.Now().Add(20 * time.Second)
	var d *store.Delivery
	for time.Now().Before(deadline) {
		d = f.get(id)
		if strings.Contains(d.LastError, "transport password") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if !strings.Contains(d.LastError, "transport password") {
		t.Fatalf("delivery status %s, last error %q: the copied ciphertext was not refused", d.Status, d.LastError)
	}
	if d.Status == store.DeliverySent {
		t.Fatal("the delivery was sent with another tenant's password")
	}
	if st := f.chaos.Stats(); st.AuthFailed != 0 || len(f.chaos.Messages()) != 0 {
		t.Fatalf("the relay was reached (auth failures %d, messages %d)", st.AuthFailed, len(f.chaos.Messages()))
	}

	// Positive control: the same plaintext sealed for A's own row works, so
	// the refusal above was the binding and nothing else.
	trA, err = f.st.Transports().Get(ctx, f.transportID)
	if err != nil {
		t.Fatal(err)
	}
	trA.Password = c.seal(t, password, store.TransportPasswordContext(trA))
	if err := f.st.Transports().Update(ctx, trA); err != nil {
		t.Fatal(err)
	}
	id2 := f.insertOne(nil)
	stop = f.runSenders(1, withCipher)
	ok := f.waitTerminal([]string{id2}, 20*time.Second)
	stop()
	if !ok {
		t.Fatal("delivery with the row's own ciphertext did not finish")
	}
	if got := f.get(id2); got.Status != store.DeliverySent {
		t.Fatalf("status = %s (%s), want sent", got.Status, got.LastError)
	}
}

// The binding is to the row, not only to the tenant: a ciphertext moved
// between two transports of the same tenant, or presented under another
// purpose, does not decrypt either.
func TestTransportPasswordBoundToRow(t *testing.T) {
	ctx := context.Background()
	c := newAEADCipher(t)
	s := &Sender{cfg: Config{Secrets: c}.withDefaults()}
	ts := newTenantState("tenant-a", nil, time.Minute)

	a := &store.Transport{ID: "tr-a", TenantID: "tenant-a", Version: 1}
	a.Password = c.seal(t, "pw", store.TransportPasswordContext(a))
	if got, err := s.password(ctx, ts, a); err != nil || got != "pw" {
		t.Fatalf("own row: %q, %v", got, err)
	}

	moved := &store.Transport{ID: "tr-b", TenantID: "tenant-a", Version: 1, Password: a.Password}
	if _, err := s.password(ctx, ts, moved); err == nil {
		t.Error("a ciphertext moved to another transport of the same tenant decrypted")
	}

	// A DKIM key sealed for a domain row with the same ID is not a transport
	// password.
	dom := &store.SendingDomain{ID: "tr-c", TenantID: "tenant-a"}
	foreign := &store.Transport{ID: "tr-c", TenantID: "tenant-a", Version: 1,
		Password: c.seal(t, "pw", store.DomainDKIMKeyContext(dom))}
	if _, err := s.password(ctx, ts, foreign); err == nil {
		t.Error("a ciphertext sealed for another purpose decrypted as a transport password")
	}
}
