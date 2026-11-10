package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// testContext is the context the round-trip tests seal under.
var testContext = host.SecretContext{
	Purpose: store.PurposeTransportPassword, TenantID: "tenant-a", ObjectID: "row-1",
}

func TestAESGCMCipherRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := newAESGCMCipher(key)
	if err != nil {
		t.Fatalf("newAESGCMCipher: %v", err)
	}

	plaintext := []byte("smtp-password-hunter2")
	ciphertext, err := c.Encrypt(context.Background(), plaintext, testContext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}

	got, err := c.Decrypt(context.Background(), ciphertext, testContext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Decrypt = %q, want %q", got, plaintext)
	}
}

func TestAESGCMCipherDistinctNoncePerCall(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := newAESGCMCipher(key)
	if err != nil {
		t.Fatalf("newAESGCMCipher: %v", err)
	}
	a, err := c.Encrypt(context.Background(), []byte("same plaintext"), testContext)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt(context.Background(), []byte("same plaintext"), testContext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext (nonce reuse)")
	}
}

func TestAESGCMCipherRejectsTamperedCiphertext(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := newAESGCMCipher(key)
	if err != nil {
		t.Fatalf("newAESGCMCipher: %v", err)
	}
	ciphertext, err := c.Encrypt(context.Background(), []byte("secret"), testContext)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := c.Decrypt(context.Background(), tampered, testContext); err == nil {
		t.Fatal("expected an error decrypting tampered ciphertext")
	}
}

// A ciphertext opens only under the exact context it was sealed with: a
// change to any one field — the row copied into another tenant, into another
// row of the same tenant, into another column — is a decryption failure.
func TestAESGCMCipherBindsEveryContextField(t *testing.T) {
	c := newTestCipher(t)
	ciphertext, err := c.Encrypt(context.Background(), []byte("secret"), testContext)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*host.SecretContext){
		"purpose":   func(sc *host.SecretContext) { sc.Purpose = store.PurposeBounceMailboxPassword },
		"tenant":    func(sc *host.SecretContext) { sc.TenantID = "tenant-b" },
		"object":    func(sc *host.SecretContext) { sc.ObjectID = "row-2" },
		"bind":      func(sc *host.SecretContext) { sc.Bind = "x" },
		"empty":     func(sc *host.SecretContext) { *sc = host.SecretContext{} },
		"shuffled":  func(sc *host.SecretContext) { sc.TenantID, sc.ObjectID = sc.ObjectID, sc.TenantID },
		"run-on":    func(sc *host.SecretContext) { sc.TenantID, sc.ObjectID = "tenant-ar", "ow-1" },
		"case-only": func(sc *host.SecretContext) { sc.TenantID = "Tenant-a" },
	} {
		sc := testContext
		mutate(&sc)
		if _, err := c.Decrypt(context.Background(), ciphertext, sc); err == nil {
			t.Errorf("%s: decrypted under a different context %+v", name, sc)
		}
	}
	// Nor does an unbound (legacy) open accept it: there is no path that
	// ignores the context.
	if _, err := c.decryptLegacy(context.Background(), ciphertext); err == nil {
		t.Error("decryptLegacy opened a bound ciphertext")
	}
}

// decryptLegacy is the rewrap tool's view of a ciphertext written before the
// binding existed (sealed with no associated data), and Decrypt never accepts
// one.
func TestAESGCMCipherLegacyCiphertext(t *testing.T) {
	c := newTestCipher(t)
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	legacy := c.gcm.Seal(nonce, nonce, []byte("old secret"), nil)

	got, err := c.decryptLegacy(context.Background(), legacy)
	if err != nil || string(got) != "old secret" {
		t.Fatalf("decryptLegacy = %q, %v", got, err)
	}
	if _, err := c.Decrypt(context.Background(), legacy, testContext); err == nil {
		t.Fatal("Decrypt accepted an unbound ciphertext: the serving path must have no fallback")
	}
}

func newTestCipher(t *testing.T) *aesGCMCipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := newAESGCMCipher(key)
	if err != nil {
		t.Fatalf("newAESGCMCipher: %v", err)
	}
	return c
}

func TestNewAESGCMCipherRejectsBadKeyLength(t *testing.T) {
	if _, err := newAESGCMCipher(make([]byte, 16)); err == nil {
		t.Fatal("expected an error for a 16-byte (AES-128) key")
	}
}

func TestDecodeSecretKey(t *testing.T) {
	if _, err := decodeSecretKey("not base64!!"); err == nil {
		t.Fatal("expected an error for invalid base64")
	}
	if _, err := decodeSecretKey("dG9vc2hvcnQ="); err == nil {
		t.Fatal("expected an error for a key that is not 32 bytes")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	got, err := decodeSecretKey(encoded)
	if err != nil {
		t.Fatalf("decodeSecretKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("decodeSecretKey did not round-trip the key")
	}
}
