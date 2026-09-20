package tracking

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// VERP is the bounce correlation of architecture 10:
//
//	Return-Path: bounce+{deliveryID}.{hmac8}@{bounce_domain}
//
// The MAC is what stops a forged bounce: anyone can guess a delivery ID from a
// mail they received, but only the tenant's key produces a matching tag, so a
// DSN whose envelope recipient does not verify is recorded as unverified
// instead of transitioning the delivery.
const (
	// VERPPrefix starts the local part.
	VERPPrefix = "bounce+"
	// verpMACHex is how many hex characters of the MAC are carried. 32 bits is
	// enough to make forgery pointless while keeping the address short; the
	// bounce path also checks the delivery exists and is in status sent.
	verpMACHex = 8
	// verpContext keeps VERP MACs distinct from tracking token MACs even when
	// a tenant reuses one secret.
	verpContext = "sendplane/verp/v1\x00"
)

// VERPAddress builds the envelope sender for a delivery. It returns an empty
// string when the tenant has no bounce domain or no signing key, which is the
// sender's signal to fall back to the From address.
func VERPAddress(bounceDomain, deliveryID string, secret []byte) string {
	if bounceDomain == "" || deliveryID == "" || len(secret) == 0 {
		return ""
	}
	return VERPPrefix + deliveryID + "." + verpMAC(secret, deliveryID) + "@" + bounceDomain
}

// ParseVERP extracts the delivery ID from a VERP address and reports whether
// any of the tenant's keys verifies it. A malformed address returns ok=false
// with an empty ID.
func ParseVERP(addr string, keys []store.SigningKey) (deliveryID string, verified bool) {
	local, _, ok := strings.Cut(addr, "@")
	if !ok {
		local = addr
	}
	if !strings.HasPrefix(local, VERPPrefix) {
		return "", false
	}
	rest := local[len(VERPPrefix):]
	i := strings.LastIndex(rest, ".")
	if i <= 0 || i == len(rest)-1 {
		return "", false
	}
	id, mac := rest[:i], rest[i+1:]
	for _, k := range keys {
		if len(k.Secret) == 0 {
			continue
		}
		if hmac.Equal([]byte(mac), []byte(verpMAC(k.Secret, id))) {
			return id, true
		}
	}
	return id, false
}

func verpMAC(secret []byte, deliveryID string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(verpContext))
	m.Write([]byte(deliveryID))
	return hex.EncodeToString(m.Sum(nil))[:verpMACHex]
}
