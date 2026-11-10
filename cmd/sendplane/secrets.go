package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"github.com/sendplane/sendplane/host"
)

// aesGCMCipher implements host.SecretCipher with AES-256-GCM. The nonce is
// generated per call and stored as a prefix of the ciphertext, and the
// SecretContext's AAD is the GCM associated data, so a ciphertext only opens
// for the row it was sealed for.
type aesGCMCipher struct {
	gcm cipher.AEAD
}

var _ host.SecretCipher = (*aesGCMCipher)(nil)

// newAESGCMCipher builds a SecretCipher from a 32-byte key (AES-256). A
// shorter key would still be accepted by aes.NewCipher as AES-128/192, which
// is not what config.yaml's secrets.key documents, so the length is checked
// here rather than left to that implicit fallback.
func newAESGCMCipher(key []byte) (*aesGCMCipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return &aesGCMCipher{gcm: gcm}, nil
}

func (c *aesGCMCipher) Encrypt(_ context.Context, plaintext []byte, sc host.SecretContext) ([]byte, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: encrypt: %w", err)
	}
	return c.gcm.Seal(nonce, nonce, plaintext, sc.AAD()), nil
}

func (c *aesGCMCipher) Decrypt(_ context.Context, ciphertext []byte, sc host.SecretContext) ([]byte, error) {
	return c.open(ciphertext, sc.AAD())
}

// decryptLegacy opens a ciphertext sealed before secrets were bound to their
// row (no associated data). It exists only for `sendplane secrets rewrap`;
// nothing on the serving path may call it, because accepting an unbound
// ciphertext there is the very thing the binding is meant to stop.
func (c *aesGCMCipher) decryptLegacy(_ context.Context, ciphertext []byte) ([]byte, error) {
	return c.open(ciphertext, nil)
}

func (c *aesGCMCipher) open(ciphertext, aad []byte) ([]byte, error) {
	n := c.gcm.NonceSize()
	if len(ciphertext) < n {
		return nil, fmt.Errorf("secrets: decrypt: ciphertext shorter than nonce")
	}
	nonce, sealed := ciphertext[:n], ciphertext[n:]
	plaintext, err := c.gcm.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	return plaintext, nil
}
