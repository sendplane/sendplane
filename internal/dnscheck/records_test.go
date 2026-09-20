package dnscheck

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"testing"

	"github.com/sendplane/sendplane/store"
)

// testRSA is generated once: 2048-bit key generation is the slowest thing in
// this package by an order of magnitude.
var testRSA = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func rsaPEM(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
}

func rsaPublicB64(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func TestDKIM(t *testing.T) {
	t.Parallel()

	key := testRSA()
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, err := x509.MarshalPKIXPublicKey(&otherKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	// A 512-bit modulus, built by hand: Go refuses to generate a key this
	// weak, but a domain can certainly publish one, and that is the case the
	// check has to catch.
	weakDER, err := x509.MarshalPKIXPublicKey(&rsa.PublicKey{
		N: new(big.Int).SetBit(new(big.Int), 511, 1), E: 65537,
	})
	if err != nil {
		t.Fatal(err)
	}
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	pub := rsaPublicB64(t, key)
	z := zone{txt: map[string][]string{
		"s1._domainkey.example.com":   {"v=DKIM1; k=rsa; p=" + pub},
		"nov._domainkey.example.com":  {"k=rsa; p=" + pub},
		"test._domainkey.example.com": {"v=DKIM1; k=rsa; t=y; p=" + pub},
		"rev._domainkey.example.com":  {"v=DKIM1; k=rsa; p="},
		"nop._domainkey.example.com":  {"v=DKIM1; k=rsa"},
		"junk._domainkey.example.com": {"v=DKIM1; k=rsa; p=!!!not-base64!!!"},
		"old._domainkey.example.com":  {"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(otherDER)},
		"weak._domainkey.example.com": {"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(weakDER)},
		"ed._domainkey.example.com":   {"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(edPub)},
		"v2._domainkey.example.com":   {"v=DKIM2; p=" + pub},
	}}

	tests := []struct {
		name     string
		selector string
		key      []byte
		want     store.HealthStatus
	}{
		{name: "match", selector: "s1", key: rsaPEM(t, key), want: store.HealthGreen},
		{name: "match without v tag", selector: "nov", key: rsaPEM(t, key), want: store.HealthGreen},
		{name: "relay signs, no key stored", selector: "s1", want: store.HealthGreen},
		{name: "ed25519", selector: "ed", want: store.HealthGreen},
		{name: "expected public key as base64", selector: "s1", key: []byte(pub), want: store.HealthGreen},

		{name: "missing", selector: "nope", key: rsaPEM(t, key), want: store.HealthRed},
		{name: "no p tag", selector: "nop", key: rsaPEM(t, key), want: store.HealthRed},
		{name: "revoked", selector: "rev", key: rsaPEM(t, key), want: store.HealthRed},
		{name: "malformed base64", selector: "junk", key: rsaPEM(t, key), want: store.HealthRed},
		{name: "key mismatch", selector: "old", key: rsaPEM(t, key), want: store.HealthRed},
		{name: "wrong version", selector: "v2", key: rsaPEM(t, key), want: store.HealthRed},

		{name: "testing mode", selector: "test", key: rsaPEM(t, key), want: store.HealthYellow},
		{name: "short key", selector: "weak", want: store.HealthYellow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := New(newFake(z)).DKIM(context.Background(), "example.com", tc.selector, tc.key)
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s (%s) %v", got.Status, tc.want, got.Summary, got.Details)
			}
		})
	}
}

func TestDMARC(t *testing.T) {
	t.Parallel()

	z := zone{txt: map[string][]string{
		"_dmarc.reject.test":   {"v=DMARC1; p=reject; rua=mailto:d@reject.test; adkim=s; aspf=s"},
		"_dmarc.quar.test":     {"v=DMARC1; p=quarantine; rua=mailto:d@quar.test"},
		"_dmarc.none.test":     {"v=DMARC1; p=none; rua=mailto:d@none.test"},
		"_dmarc.norua.test":    {"v=DMARC1; p=reject"},
		"_dmarc.nopolicy.test": {"v=DMARC1; rua=mailto:d@nopolicy.test"},
		"_dmarc.bogus.test":    {"v=DMARC1; p=whatever"},
		"_dmarc.double.test":   {"v=DMARC1; p=reject", "v=DMARC1; p=none"},
		"_dmarc.other.test":    {"some-verification=1"},
	}}

	tests := []struct {
		domain string
		want   store.HealthStatus
	}{
		{"reject.test", store.HealthGreen},
		{"quar.test", store.HealthGreen},
		{"none.test", store.HealthYellow},
		{"norua.test", store.HealthYellow},
		{"nopolicy.test", store.HealthRed},
		{"bogus.test", store.HealthRed},
		{"double.test", store.HealthRed},
		{"other.test", store.HealthRed},
		{"missing.test", store.HealthRed},
	}
	for _, tc := range tests {
		t.Run(tc.domain, func(t *testing.T) {
			t.Parallel()
			got := New(newFake(z)).DMARC(context.Background(), tc.domain)
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s (%s)", got.Status, tc.want, got.Summary)
			}
		})
	}

	t.Run("alignment defaults to relaxed", func(t *testing.T) {
		t.Parallel()
		got := New(newFake(z)).DMARC(context.Background(), "quar.test")
		if got.Details["adkim"] != "r" || got.Details["aspf"] != "r" {
			t.Fatalf("alignment = %v", got.Details)
		}
	})
}

func TestMX(t *testing.T) {
	t.Parallel()

	z := zone{mx: map[string][]MX{
		"bounce.test": {{Host: "mx1.bounce.test", Pref: 10}, {Host: "mx2.bounce.test", Pref: 20}},
		"null.test":   {{Host: ".", Pref: 0}},
	}}
	tests := []struct {
		domain string
		want   store.HealthStatus
	}{
		{"bounce.test", store.HealthGreen},
		{"null.test", store.HealthRed},
		{"nothing.test", store.HealthRed},
	}
	for _, tc := range tests {
		t.Run(tc.domain, func(t *testing.T) {
			t.Parallel()
			got := New(newFake(z)).MX(context.Background(), tc.domain)
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s (%s)", got.Status, tc.want, got.Summary)
			}
		})
	}
}

func TestPTR(t *testing.T) {
	t.Parallel()

	z := zone{
		ptr: map[string][]string{
			"10.113.0.203.in-addr.arpa": {"mail.example.com"},
			"11.113.0.203.in-addr.arpa": {"other.example.com"},
			"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa": {"v6.example.com"},
		},
		a: map[string][]string{
			"mail.example.com":  {"203.0.113.10"},
			"other.example.com": {"198.51.100.1"},
		},
		aaaa: map[string][]string{
			"v6.example.com": {"2001:db8::1"},
		},
	}
	tests := []struct {
		name string
		ip   string
		want store.HealthStatus
	}{
		{"forward confirmed", "203.0.113.10", store.HealthGreen},
		{"forward confirmed ipv6", "2001:db8::1", store.HealthGreen},
		{"ptr does not point back", "203.0.113.11", store.HealthYellow},
		{"no ptr", "203.0.113.99", store.HealthYellow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := New(newFake(z)).PTR(context.Background(), net.ParseIP(tc.ip))
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s (%s) %v", got.Status, tc.want, got.Summary, got.Details)
			}
		})
	}
}

func TestRunAll(t *testing.T) {
	t.Parallel()

	key := testRSA()
	z := zone{
		txt: map[string][]string{
			"example.com":               {"v=spf1 ip4:203.0.113.0/24 -all"},
			"_dmarc.example.com":        {"v=DMARC1; p=none; rua=mailto:d@example.com"},
			"s1._domainkey.example.com": {"v=DKIM1; k=rsa; p=" + rsaPublicB64(t, key)},
		},
		mx:  map[string][]MX{"bounce.example.com": {{Host: "mx.example.com", Pref: 10}}},
		ptr: map[string][]string{"10.113.0.203.in-addr.arpa": {"mail.example.com"}},
		a:   map[string][]string{"mail.example.com": {"203.0.113.10"}},
	}
	c := New(newFake(z))

	rep := c.RunAll(context.Background(), Inputs{
		Domain:           "example.com",
		DKIMSelector:     "s1",
		DKIMKey:          rsaPEM(t, key),
		ReturnPathDomain: "bounce.example.com",
		ObservedIP:       net.ParseIP("203.0.113.10"),
	})
	if rep.SPF == nil || rep.SPF.Status != store.HealthGreen {
		t.Fatalf("spf = %+v", rep.SPF)
	}
	if rep.DKIM == nil || rep.DKIM.Status != store.HealthGreen {
		t.Fatalf("dkim = %+v", rep.DKIM)
	}
	if rep.PTR == nil || rep.PTR.Status != store.HealthGreen {
		t.Fatalf("ptr = %+v", rep.PTR)
	}
	// p=none is the only yellow, and the summary is the worst of the five.
	if rep.DMARC == nil || rep.DMARC.Status != store.HealthYellow {
		t.Fatalf("dmarc = %+v", rep.DMARC)
	}
	if rep.Status != store.HealthYellow {
		t.Fatalf("status = %s, want yellow", rep.Status)
	}

	t.Run("checks with no input are skipped, not failed", func(t *testing.T) {
		rep := c.RunAll(context.Background(), Inputs{Domain: "example.com"})
		if rep.DKIM != nil {
			t.Errorf("dkim ran without a selector: %+v", rep.DKIM)
		}
		if rep.PTR != nil {
			t.Errorf("ptr ran without an observed IP: %+v", rep.PTR)
		}
		if rep.MX == nil {
			t.Error("mx did not fall back to the From domain")
		}
	})
}
