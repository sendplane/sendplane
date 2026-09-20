package dnscheck

import (
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// DKIM checks the published key at {selector}._domainkey.{domain}: that the
// record exists and parses (v=DKIM1, k=, p=), and — when key is given — that
// the published public key is the one sendplane signs with.
//
// key is either the PEM private key from SendingDomain.DKIMPrivateKey
// (already decrypted by the caller) or the expected public key in the same
// base64 form the record uses. An empty key checks the record alone, which is
// the relay-signs case.
func (c *Checker) DKIM(ctx context.Context, domain, selector string, key []byte) Result {
	name := selector + "._domainkey." + domain
	details := map[string]string{"domain": domain, "selector": selector, "name": name}

	txts, err := c.r.TXT(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNoRecord) {
			return result("dkim", store.HealthRed,
				fmt.Sprintf("%s에 DKIM 레코드가 없습니다", name), details)
		}
		details["error"] = err.Error()
		return result("dkim", store.HealthYellow, "DKIM 레코드를 조회하지 못했습니다", details)
	}

	record := pickDKIMRecord(txts)
	if record == "" {
		return result("dkim", store.HealthRed,
			fmt.Sprintf("%s의 TXT가 DKIM 레코드가 아닙니다", name), details)
	}
	details["record"] = record

	tags := parseTagValue(record)
	if v, ok := tags["v"]; ok && !strings.EqualFold(v, "DKIM1") {
		details["v"] = v
		return result("dkim", store.HealthRed, "DKIM 레코드의 v가 DKIM1이 아닙니다", details)
	}
	alg := strings.ToLower(tags["k"])
	if alg == "" {
		alg = "rsa"
	}
	details["k"] = alg
	if f := tags["t"]; f != "" {
		details["t"] = f
	}

	p, ok := tags["p"]
	if !ok {
		return result("dkim", store.HealthRed, "DKIM 레코드에 p= 태그가 없습니다", details)
	}
	if strings.TrimSpace(p) == "" {
		return result("dkim", store.HealthRed, "DKIM 공개키가 비어 있습니다(키 폐기 상태)", details)
	}
	pub, err := base64.StdEncoding.DecodeString(stripWhitespace(p))
	if err != nil {
		details["error"] = err.Error()
		return result("dkim", store.HealthRed, "DKIM 공개키를 base64로 해석할 수 없습니다", details)
	}
	bits, err := dkimKeyBits(alg, pub)
	if err != nil {
		details["error"] = err.Error()
		return result("dkim", store.HealthRed, "DKIM 공개키를 해석할 수 없습니다", details)
	}
	if bits > 0 {
		details["bits"] = fmt.Sprint(bits)
	}

	if len(key) > 0 {
		want, err := expectedPublicKey(key)
		if err != nil {
			details["error"] = err.Error()
			return result("dkim", store.HealthYellow,
				"저장된 DKIM 키를 해석할 수 없어 공개키를 비교하지 못했습니다", details)
		}
		if want != stripWhitespace(p) {
			details["published_p"] = shorten(stripWhitespace(p))
			details["expected_p"] = shorten(want)
			return result("dkim", store.HealthRed,
				fmt.Sprintf("%s의 공개키가 sendplane 서명키와 다릅니다(선택자/키 불일치)", name), details)
		}
		details["key_match"] = "true"
	}

	if strings.Contains(strings.ToLower(tags["t"]), "y") {
		return result("dkim", store.HealthYellow,
			"DKIM 레코드가 테스트 모드(t=y)입니다", details)
	}
	if alg == "rsa" && bits > 0 && bits < 1024 {
		return result("dkim", store.HealthYellow,
			fmt.Sprintf("DKIM RSA 키가 %d비트로 너무 짧습니다", bits), details)
	}
	if len(key) == 0 {
		return result("dkim", store.HealthGreen,
			fmt.Sprintf("%s에 DKIM 공개키가 게시되어 있습니다(개인키 미보관, 릴레이 서명)", name), details)
	}
	return result("dkim", store.HealthGreen,
		fmt.Sprintf("%s의 공개키가 서명키와 일치합니다", name), details)
}

// pickDKIMRecord takes the first TXT that looks like a key record. A name can
// hold several TXTs (SPF's, a verification token); only one of them is ours.
func pickDKIMRecord(txts []string) string {
	for _, t := range txts {
		tags := parseTagValue(t)
		if strings.EqualFold(tags["v"], "DKIM1") {
			return t
		}
	}
	for _, t := range txts {
		if _, ok := parseTagValue(t)["p"]; ok {
			return t
		}
	}
	return ""
}

// dkimKeyBits parses the published key and returns its RSA modulus size, or 0
// for Ed25519.
func dkimKeyBits(alg string, pub []byte) (int, error) {
	switch alg {
	case "ed25519":
		if len(pub) != ed25519.PublicKeySize {
			return 0, fmt.Errorf("ed25519 키 길이가 %d바이트입니다", len(pub))
		}
		return 0, nil
	case "rsa":
		key, err := x509.ParsePKIXPublicKey(pub)
		if err != nil {
			// Some generators publish a bare PKCS#1 RSAPublicKey.
			k, err1 := x509.ParsePKCS1PublicKey(pub)
			if err1 != nil {
				return 0, err
			}
			return k.N.BitLen(), nil
		}
		k, ok := key.(*rsa.PublicKey)
		if !ok {
			return 0, fmt.Errorf("k=rsa인데 %T가 게시되어 있습니다", key)
		}
		return k.N.BitLen(), nil
	default:
		return 0, fmt.Errorf("지원하지 않는 k=%s", alg)
	}
}

// expectedPublicKey derives the base64 the record should carry. It accepts the
// PEM private key sendplane signs with (RSA or Ed25519, PKCS#1 or PKCS#8) and,
// for the case where only the public half is configured, a base64 public key.
func expectedPublicKey(key []byte) (string, error) {
	if block, _ := pem.Decode(key); block != nil {
		switch block.Type {
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return "", err
			}
			return marshalPublic(&k.PublicKey)
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return "", err
			}
			switch v := k.(type) {
			case *rsa.PrivateKey:
				return marshalPublic(&v.PublicKey)
			case ed25519.PrivateKey:
				return base64.StdEncoding.EncodeToString(v.Public().(ed25519.PublicKey)), nil
			default:
				return "", fmt.Errorf("지원하지 않는 키 타입 %T", k)
			}
		case "PUBLIC KEY", "RSA PUBLIC KEY":
			return base64.StdEncoding.EncodeToString(block.Bytes), nil
		default:
			return "", fmt.Errorf("PEM 블록 %q는 키가 아닙니다", block.Type)
		}
	}
	s := stripWhitespace(string(key))
	if _, err := base64.StdEncoding.DecodeString(s); err != nil {
		return "", errors.New("PEM도 base64 공개키도 아닙니다")
	}
	return s, nil
}

// marshalPublic renders an RSA public key the way a DKIM record does:
// base64 of the DER SubjectPublicKeyInfo (RFC 6376 3.6.1).
func marshalPublic(pub *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// parseTagValue reads the "tag=value; tag=value" form shared by DKIM, DMARC
// and the DKIM key record. Later duplicates lose, matching what a verifier
// does with the first occurrence.
func parseTagValue(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, '=')
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(part[:i])
		if k == "" {
			continue
		}
		if _, ok := out[k]; ok {
			continue
		}
		out[k] = strings.TrimSpace(part[i+1:])
	}
	return out
}

func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

func shorten(s string) string {
	if len(s) <= 24 {
		return s
	}
	return s[:12] + "…" + s[len(s)-8:]
}
