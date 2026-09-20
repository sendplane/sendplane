package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

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
	ciphertext, err := c.Encrypt(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}

	got, err := c.Decrypt(context.Background(), ciphertext)
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
	a, err := c.Encrypt(context.Background(), []byte("same plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt(context.Background(), []byte("same plaintext"))
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
	ciphertext, err := c.Encrypt(context.Background(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := c.Decrypt(context.Background(), tampered); err == nil {
		t.Fatal("expected an error decrypting tampered ciphertext")
	}
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
