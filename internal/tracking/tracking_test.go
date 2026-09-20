package tracking

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func keys() []store.SigningKey {
	return []store.SigningKey{
		{KID: "k1", Secret: []byte("secret-one"), CreatedAt: time.Unix(1000, 0)},
		{KID: "k2", Secret: []byte("secret-two"), CreatedAt: time.Unix(2000, 0)},
	}
}

func TestRoundTrip(t *testing.T) {
	s := NewSigner()
	ks := keys()
	cases := []TokenPayload{
		{DeliveryID: "d-1", Kind: KindOpen},
		{DeliveryID: "d-2", Kind: KindClick, LinkNo: 7, Dest: "https://example.com/a?b=c&d=e"},
		{DeliveryID: "d-3", Kind: KindClick, LinkNo: -1, Dest: "https://example.com/dyn"},
		{DeliveryID: "d-4", Kind: KindUnsubscribe, Dest: "https://host.example/unsub?u=42"},
	}
	for _, p := range cases {
		tok, err := s.SignErr("k2", ks[1].Secret, p)
		if err != nil {
			t.Fatalf("sign %v: %v", p, err)
		}
		if !strings.HasPrefix(tok, "k2.") {
			t.Fatalf("token %q does not start with its kid", tok)
		}
		var got TokenPayload
		if p.Kind == KindClick {
			got, err = s.VerifyDest(ks, tok, p.Dest)
		} else {
			got, err = s.Verify(ks, tok)
		}
		if err != nil {
			t.Fatalf("verify %v: %v", p, err)
		}
		if got.DeliveryID != p.DeliveryID || got.Kind != p.Kind || got.LinkNo != p.LinkNo || got.Dest != p.Dest {
			t.Fatalf("round trip: got %+v want %+v", got, p)
		}
	}
}

func TestUnsubscribeTokenEmbedsDestClickDoesNot(t *testing.T) {
	s := NewSigner()
	ks := keys()
	dest := "https://host.example/unsub/abc"

	unsub, _ := s.SignErr("k1", ks[0].Secret, TokenPayload{DeliveryID: "d", Kind: KindUnsubscribe, Dest: dest})
	got, err := s.Verify(ks, unsub) // no destination supplied: it is in the token
	if err != nil {
		t.Fatalf("verify unsubscribe: %v", err)
	}
	if got.Dest != dest {
		t.Fatalf("embedded dest = %q, want %q", got.Dest, dest)
	}

	click, _ := s.SignErr("k1", ks[0].Secret, TokenPayload{DeliveryID: "d", Kind: KindClick, LinkNo: 0, Dest: dest})
	if _, err := s.Verify(ks, click); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("click token verified without its destination: %v", err)
	}
	if _, err := s.VerifyDest(ks, click, dest); err != nil {
		t.Fatalf("click token with destination: %v", err)
	}
}

func TestDestMismatchIsRejected(t *testing.T) {
	s := NewSigner()
	ks := keys()
	tok, _ := s.SignErr("k1", ks[0].Secret, TokenPayload{
		DeliveryID: "d", Kind: KindClick, LinkNo: 3, Dest: "https://example.com/good",
	})
	if _, err := s.VerifyDest(ks, tok, "https://evil.example/"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("open redirect accepted: %v", err)
	}

	unsub, _ := s.SignErr("k1", ks[0].Secret, TokenPayload{
		DeliveryID: "d", Kind: KindUnsubscribe, Dest: "https://example.com/good",
	})
	if _, err := s.VerifyDest(ks, unsub, "https://evil.example/"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unsubscribe destination override accepted: %v", err)
	}
}

func TestTamper(t *testing.T) {
	s := NewSigner()
	ks := keys()
	tok, _ := s.SignErr("k1", ks[0].Secret, TokenPayload{DeliveryID: "delivery-1", Kind: KindOpen})

	kid, body, _ := strings.Cut(tok, ".")
	for i := range body {
		mutated := body[:i] + string(flip(body[i])) + body[i+1:]
		if _, err := s.Verify(ks, kid+"."+mutated); err == nil {
			t.Fatalf("byte %d of the payload could be changed unnoticed", i)
		}
	}
	for _, bad := range []string{"", ".", "k1", "k1.", ".abc", "k1.!!!!", "k1.AAAA"} {
		if _, err := s.Verify(ks, bad); err == nil {
			t.Fatalf("token %q accepted", bad)
		}
	}
}

// flip returns a different character from the base64url alphabet.
func flip(c byte) byte {
	if c == 'A' {
		return 'B'
	}
	return 'A'
}

func TestWrongKID(t *testing.T) {
	s := NewSigner()
	ks := keys()
	tok, _ := s.SignErr("k1", ks[0].Secret, TokenPayload{DeliveryID: "d", Kind: KindOpen})

	// The key is gone (rotated out).
	if _, err := s.Verify(ks[1:], tok); !errors.Is(err, ErrUnknownKID) {
		t.Fatalf("unknown kid: %v", err)
	}
	// The kid is relabelled to another existing key: the MAC no longer matches.
	if _, err := s.Verify(ks, "k2."+strings.SplitN(tok, ".", 2)[1]); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("kid substitution: %v", err)
	}
	// Both keys still verify their own tokens after rotation.
	tok2, _ := s.SignErr("k2", ks[1].Secret, TokenPayload{DeliveryID: "d", Kind: KindOpen})
	if _, err := s.Verify(ks, tok2); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
}

func TestSignRejectsBadInput(t *testing.T) {
	s := NewSigner()
	for _, tc := range []struct {
		name   string
		kid    string
		secret []byte
		p      TokenPayload
	}{
		{"no kid", "", []byte("x"), TokenPayload{DeliveryID: "d", Kind: KindOpen}},
		{"kid with separator", "a.b", []byte("x"), TokenPayload{DeliveryID: "d", Kind: KindOpen}},
		{"no secret", "k", nil, TokenPayload{DeliveryID: "d", Kind: KindOpen}},
		{"no delivery", "k", []byte("x"), TokenPayload{Kind: KindOpen}},
		{"bad kind", "k", []byte("x"), TokenPayload{DeliveryID: "d", Kind: Kind(9)}},
	} {
		if _, err := s.SignErr(tc.kid, tc.secret, tc.p); !errors.Is(err, ErrNoKey) {
			t.Errorf("%s: err = %v, want ErrNoKey", tc.name, err)
		}
		if got := s.Sign(tc.kid, tc.secret, tc.p); got != "" {
			t.Errorf("%s: Sign = %q, want empty", tc.name, got)
		}
	}
}

func TestSelectKey(t *testing.T) {
	if _, ok := SelectKey(nil); ok {
		t.Fatal("empty key set selected a key")
	}
	if _, ok := SelectKey([]store.SigningKey{{KID: "k", Secret: nil}}); ok {
		t.Fatal("key without a secret selected")
	}
	k, ok := SelectKey(keys())
	if !ok || k.KID != "k2" {
		t.Fatalf("SelectKey = %v %v, want newest key k2", k.KID, ok)
	}
	same := []store.SigningKey{
		{KID: "a", Secret: []byte("x"), CreatedAt: time.Unix(1, 0)},
		{KID: "b", Secret: []byte("x"), CreatedAt: time.Unix(1, 0)},
	}
	if k, _ := SelectKey(same); k.KID != "b" {
		t.Fatalf("tie break = %q, want b", k.KID)
	}
}

func TestURLBuilders(t *testing.T) {
	tok := "k1.abcDEF-_"
	if got, want := OpenURL("t.example.com", tok), "https://t.example.com/t/o/k1.abcDEF-_"; got != want {
		t.Errorf("OpenURL = %q want %q", got, want)
	}
	if got, want := UnsubscribeURL("https://t.example.com/", tok), "https://t.example.com/t/u/k1.abcDEF-_"; got != want {
		t.Errorf("UnsubscribeURL = %q want %q", got, want)
	}
	if got, want := OpenURL("http://127.0.0.1:8080", tok), "http://127.0.0.1:8080/t/o/k1.abcDEF-_"; got != want {
		t.Errorf("OpenURL scheme kept = %q want %q", got, want)
	}
	if got := OpenURL("", tok); got != "" {
		t.Errorf("OpenURL with no domain = %q, want empty", got)
	}

	dest := "https://example.com/p?a=1&b=2 3"
	click := ClickURL("t.example.com", tok, dest)
	u, err := url.Parse(click)
	if err != nil {
		t.Fatalf("ClickURL is not a URL: %v", err)
	}
	if got := u.Query().Get(DestParam); got != dest {
		t.Errorf("u = %q, want %q", got, dest)
	}
	if u.Path != "/t/c/"+tok {
		t.Errorf("path = %q", u.Path)
	}
}

func BenchmarkSign(b *testing.B) {
	s := NewSigner()
	secret := []byte("0123456789abcdef")
	p := TokenPayload{DeliveryID: "0199a0e5-0000-7000-8000-000000000001", Kind: KindClick, LinkNo: 3, Dest: "https://example.com/landing"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = s.Sign("k1", secret, p)
	}
}
