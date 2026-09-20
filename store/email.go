package store

import (
	"fmt"
	"net/mail"
	"strings"

	"golang.org/x/net/idna"
)

// NormalizeEmail produces the canonical form used for deduplication,
// suppression lookups and the (campaign_id, email_norm) unique key.
//
// It trims surrounding space, rejects CR/LF (header injection, architecture
// 16), validates the address with net/mail (so "Name <a@b>" is accepted and
// reduced to the address), lowercases it and converts an IDN domain to
// punycode. The local part is lowercased too: sendplane treats addresses as
// case-insensitive, which is what every mainstream provider does even though
// RFC 5321 allows case-sensitive local parts.
func NormalizeEmail(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("%w: email contains CR or LF", ErrInvalid)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: empty email", ErrInvalid)
	}
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return "", fmt.Errorf("%w: email %q: %v", ErrInvalid, s, err)
	}
	at := strings.LastIndex(addr.Address, "@")
	if at <= 0 || at == len(addr.Address)-1 {
		return "", fmt.Errorf("%w: email %q has no domain", ErrInvalid, s)
	}
	local := strings.ToLower(addr.Address[:at])
	domain := strings.ToLower(addr.Address[at+1:])
	ascii, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return "", fmt.Errorf("%w: email %q domain: %v", ErrInvalid, s, err)
	}
	return local + "@" + ascii, nil
}
