package sender

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

func testMessage() *host.OutboundMessage {
	return &host.OutboundMessage{
		TenantID: "t1", DeliveryID: "d1", CampaignID: "c1",
		Lane: store.LaneBulk,
		Recipient: host.RecipientContext{
			TenantID: "t1", DeliveryID: "d1",
			Email: "user@example.org", EmailNorm: "user@example.org", Name: "User",
		},
		FromName: "Example News", From: "news@example.com", ReplyTo: "reply@example.com",
		Subject: "Hello", HTML: "<html><body>hi</body></html>", Text: "hi",
		Headers: map[string]string{},
	}
}

// headersOf parses the built message's header block.
func headersOf(t *testing.T, raw []byte) textproto.MIMEHeader {
	t.Helper()
	h, err := textproto.NewReader(bufio.NewReader(strings.NewReader(string(raw)))).ReadMIMEHeader()
	if err != nil {
		t.Fatalf("parsing headers: %v\n%s", err, raw)
	}
	return h
}

func TestBuildMessageHeaders(t *testing.T) {
	raw, err := buildMessage(messageInput{
		msg:       testMessage(),
		messageID: "d1@example.com",
		attemptNo: 3,
		date:      time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := headersOf(t, raw)

	if got, want := h.Get("Message-Id"), "<d1@example.com>"; got != want {
		t.Errorf("Message-ID = %q, want %q", got, want)
	}
	if got, want := h.Get(HeaderSendplaneID), "t1/d1"; got != want {
		t.Errorf("%s = %q, want %q", HeaderSendplaneID, got, want)
	}
	if got, want := h.Get(HeaderAttempt), "3"; got != want {
		t.Errorf("%s = %q, want %q", HeaderAttempt, got, want)
	}
	if got := h.Get("From"); !strings.Contains(got, "news@example.com") {
		t.Errorf("From = %q", got)
	}
	if got := h.Get("To"); !strings.Contains(got, "user@example.org") {
		t.Errorf("To = %q", got)
	}
	if got := h.Get("Reply-To"); !strings.Contains(got, "reply@example.com") {
		t.Errorf("Reply-To = %q", got)
	}
	if got := h.Get("Subject"); got != "Hello" {
		t.Errorf("Subject = %q", got)
	}
	// Bulk mail declares itself so auto-responders stay quiet.
	if got := h.Get("Precedence"); got != "bulk" {
		t.Errorf("Precedence = %q", got)
	}
	if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/alternative") {
		t.Errorf("Content-Type = %q, want multipart/alternative", ct)
	}
	if !strings.Contains(string(raw), "text/plain") || !strings.Contains(string(raw), "text/html") {
		t.Error("both parts should be present")
	}
}

func TestBuildMessageSinglePart(t *testing.T) {
	m := testMessage()
	m.HTML = ""
	raw, err := buildMessage(messageInput{msg: m, messageID: "d1@example.com", attemptNo: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ct := headersOf(t, raw).Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}

	m = testMessage()
	m.Text = ""
	raw, err = buildMessage(messageInput{msg: m, messageID: "d1@example.com", attemptNo: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ct := headersOf(t, raw).Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}

	m = testMessage()
	m.Text, m.HTML = "", ""
	if _, err := buildMessage(messageInput{msg: m, messageID: "x@y"}); !errors.Is(err, ErrNoBody) {
		t.Errorf("empty message: err = %v", err)
	}
}

// TestUnsubscribeHeaderMatrix covers the three modes of architecture 9.2 for
// both the body variable and the List-Unsubscribe headers.
func TestUnsubscribeHeaderMatrix(t *testing.T) {
	s := &Sender{}
	key := store.SigningKey{KID: "k1", Secret: []byte("secret"), CreatedAt: time.Now()}
	const dest = "https://host.example/unsub/abc"

	cases := []struct {
		name       string
		mode       store.UnsubscribeMode
		dest       string
		oneClick   bool // TenantSettings.UnsubscribeOneClick
		hasKey     bool
		domain     string
		wantBody   string // "" = none, "token" = a signed sendplane URL, else exact
		wantHeader string
		wantPost   bool
	}{
		{"sendplane", store.UnsubscribeSendplane, dest, false, true, trackDomain, "token", "token", true},
		{"sendplane without a destination", store.UnsubscribeSendplane, "", false, true, trackDomain, "", "", false},
		{"sendplane without a tracking domain", store.UnsubscribeSendplane, dest, false, true, "", dest, dest, false},
		{"sendplane without a key", store.UnsubscribeSendplane, dest, false, false, trackDomain, dest, dest, false},
		{"host", store.UnsubscribeHost, dest, false, true, trackDomain, dest, dest, false},
		// The host endpoint only gets List-Unsubscribe-Post once the tenant
		// says it accepts one, and only over https (RFC 8058).
		{"host with one-click", store.UnsubscribeHost, dest, true, true, trackDomain, dest, dest, true},
		{"host with one-click over http", store.UnsubscribeHost, "http://host.example/u", true, true, trackDomain,
			"http://host.example/u", "http://host.example/u", false},
		{"host without a destination", store.UnsubscribeHost, "", true, true, trackDomain, "", "", false},
		{"none", store.UnsubscribeNone, dest, true, true, trackDomain, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := &store.TenantSettings{UnsubscribeMode: tc.mode, UnsubscribeOneClick: tc.oneClick}
			got := s.unsubscribeLinks(settings, "t1", "d1", tc.dest, key, tc.hasKey, tc.domain)

			checkURL := func(field, want, have string) {
				switch want {
				case "":
					if have != "" {
						t.Errorf("%s = %q, want none", field, have)
					}
				case "token":
					prefix := "https://" + trackDomain + tracking.PathUnsubscribe
					if !strings.HasPrefix(have, prefix) {
						t.Errorf("%s = %q, want a %s URL", field, have, prefix)
						return
					}
					token := strings.TrimPrefix(have, prefix)
					p, err := tracking.NewSigner().Verify([]store.SigningKey{key}, token)
					if err != nil {
						t.Errorf("%s token does not verify: %v", field, err)
						return
					}
					if p.Kind != tracking.KindUnsubscribe || p.TenantID != "t1" || p.DeliveryID != "d1" || p.Dest != dest {
						t.Errorf("%s token payload = %+v", field, p)
					}
				default:
					if have != want {
						t.Errorf("%s = %q, want %q", field, have, want)
					}
				}
			}
			checkURL("body", tc.wantBody, got.body)
			checkURL("header", tc.wantHeader, got.header)
			if got.oneClick != tc.wantPost {
				t.Errorf("oneClick = %v, want %v", got.oneClick, tc.wantPost)
			}

			// And the headers the mode produces on the wire.
			m := testMessage()
			m.UnsubscribeURL = got.body
			raw, err := buildMessage(messageInput{
				msg: m, messageID: "d1@example.com", attemptNo: 1,
				listUnsubscribe: got.header, oneClick: got.oneClick,
			})
			if err != nil {
				t.Fatal(err)
			}
			h := headersOf(t, raw)
			lu := h.Get("List-Unsubscribe")
			if got.header == "" {
				if lu != "" {
					t.Errorf("List-Unsubscribe = %q, want none", lu)
				}
			} else if !strings.Contains(lu, got.header) || !strings.HasPrefix(lu, "<") {
				t.Errorf("List-Unsubscribe = %q, want <%s>", lu, got.header)
			}
			post := h.Get("List-Unsubscribe-Post")
			if tc.wantPost && post != "List-Unsubscribe=One-Click" {
				t.Errorf("List-Unsubscribe-Post = %q", post)
			}
			if !tc.wantPost && post != "" {
				t.Errorf("List-Unsubscribe-Post = %q, want none", post)
			}
		})
	}
}

func TestHeaderInjectionChecks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*host.OutboundMessage)
		want   error
	}{
		{"subject", func(m *host.OutboundMessage) { m.Subject = "a\r\nBcc: x@y" }, ErrHeaderInjection},
		{"from name", func(m *host.OutboundMessage) { m.FromName = "a\nb" }, ErrHeaderInjection},
		{"recipient name", func(m *host.OutboundMessage) { m.Recipient.Name = "a\r\nb" }, ErrHeaderInjection},
		{"from address", func(m *host.OutboundMessage) { m.From = "a@b\r\nc@d" }, ErrHeaderInjection},
		{"reply-to", func(m *host.OutboundMessage) { m.ReplyTo = "not an address" }, ErrHeaderInjection},
		{"empty from", func(m *host.OutboundMessage) { m.From = "" }, ErrHeaderInjection},
		{"empty to", func(m *host.OutboundMessage) { m.Recipient.Email = "" }, ErrHeaderInjection},
		{"unsubscribe url", func(m *host.OutboundMessage) { m.UnsubscribeURL = "https://x\r\n" }, ErrHeaderInjection},
		{"header value", func(m *host.OutboundMessage) { m.Headers["X-A"] = "b\r\nBcc: x@y" }, ErrHeaderInjection},
		{"header name", func(m *host.OutboundMessage) { m.Headers["X-A: b\r\nBcc"] = "c" }, ErrHeaderInjection},
		{"bcc", func(m *host.OutboundMessage) { m.Headers["Bcc"] = "x@y" }, ErrHeaderNotAllowed},
		{"message-id override", func(m *host.OutboundMessage) { m.Headers["Message-ID"] = "<x@y>" }, ErrHeaderNotAllowed},
		{"unknown header", func(m *host.OutboundMessage) { m.Headers["Organization"] = "x" }, ErrHeaderNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := testMessage()
			tc.mutate(m)
			_, err := buildMessage(messageInput{msg: m, messageID: "d1@example.com", attemptNo: 1})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// The allowed shapes still build.
	for _, name := range []string{"X-Custom", "x-custom", "In-Reply-To", "List-Id", "Precedence"} {
		m := testMessage()
		m.Headers[name] = "value"
		if _, err := buildMessage(messageInput{msg: m, messageID: "d1@example.com", attemptNo: 1}); err != nil {
			t.Errorf("header %q rejected: %v", name, err)
		}
	}
}

func TestDKIMSigning(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := buildMessage(messageInput{
		msg: testMessage(), messageID: "d1@example.com", attemptNo: 1,
		listUnsubscribe: "https://t.example/t/u/abc", oneClick: true,
		dkim: &dkimKey{domain: "example.com", selector: "sp1", signer: key},
	})
	if err != nil {
		t.Fatal(err)
	}
	sig := headersOf(t, raw).Get("Dkim-Signature")
	if sig == "" {
		t.Fatal("no DKIM-Signature header")
	}
	for _, want := range []string{"v=1", "a=rsa-sha256", "d=example.com", "s=sp1", "bh=", "b="} {
		if !strings.Contains(sig, want) {
			t.Errorf("DKIM-Signature missing %q: %s", want, sig)
		}
	}
	// The signed header list covers the headers a receiver judges the mail by.
	for _, want := range []string{"from", "to", "subject", "list-unsubscribe"} {
		if !strings.Contains(strings.ToLower(sig), want) {
			t.Errorf("DKIM h= does not cover %q: %s", want, sig)
		}
	}
	// Signing must not disturb the message itself.
	if !strings.Contains(string(raw), "Message-ID: <d1@example.com>") {
		t.Error("the signed message lost its Message-ID")
	}
}

func TestParseDKIMKey(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey),
	})
	if _, err := parseDKIMKey(pkcs1); err != nil {
		t.Errorf("PKCS#1: %v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseDKIMKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		t.Errorf("PKCS#8 RSA: %v", err)
	}

	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	der, err = x509.MarshalPKCS8PrivateKey(edKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseDKIMKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		t.Errorf("PKCS#8 Ed25519: %v", err)
	}

	for _, bad := range [][]byte{
		nil,
		[]byte("not pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")}),
	} {
		if _, err := parseDKIMKey(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestVERPRoundTrip(t *testing.T) {
	keys := []store.SigningKey{{KID: "k1", Secret: testSecret}}
	addr := tracking.VERPAddress("bounce.example.com", "delivery-1", testSecret)
	if !strings.HasPrefix(addr, "bounce+delivery-1.") || !strings.HasSuffix(addr, "@bounce.example.com") {
		t.Fatalf("VERP address = %q", addr)
	}
	id, ok := tracking.ParseVERP(addr, keys)
	if !ok || id != "delivery-1" {
		t.Fatalf("ParseVERP = %q %v", id, ok)
	}
	// A forged tag is reported, not trusted.
	forged := "bounce+delivery-1.deadbeef@bounce.example.com"
	if id, ok := tracking.ParseVERP(forged, keys); ok || id != "delivery-1" {
		t.Fatalf("forged tag: %q %v", id, ok)
	}
	// Missing configuration yields no address, so the sender falls back to From.
	if got := tracking.VERPAddress("", "d", testSecret); got != "" {
		t.Errorf("no bounce domain: %q", got)
	}
	if got := tracking.VERPAddress("b.example", "d", nil); got != "" {
		t.Errorf("no key: %q", got)
	}
	for _, bad := range []string{"plain@example.com", "bounce+@example.com", "bounce+noTag@example.com"} {
		if _, ok := tracking.ParseVERP(bad, keys); ok {
			t.Errorf("%q verified", bad)
		}
	}
}

func TestDomainOf(t *testing.T) {
	cases := map[string]string{
		"a@b.com":     "b.com",
		"a@B.COM":     "b.com",
		"no-at":       "",
		"trailing@":   "",
		"a@b@c.com":   "c.com",
		"Name <a@b>":  "b>",
		"":            "",
		"x@sub.b.com": "sub.b.com",
	}
	for in, want := range cases {
		if got := domainOf(in); got != want {
			t.Errorf("domainOf(%q) = %q, want %q", in, got, want)
		}
	}
}
