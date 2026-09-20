package sender

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	gomail "github.com/wneessen/go-mail"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// Message errors.
var (
	// ErrHeaderInjection is returned when an address, subject or header value
	// carries CR or LF (architecture 16).
	ErrHeaderInjection = errors.New("sender: header injection")
	// ErrHeaderNotAllowed is returned for a custom header outside the
	// allowlist.
	ErrHeaderNotAllowed = errors.New("sender: header not allowed")
	// ErrNoBody is returned when neither an HTML nor a text part rendered.
	ErrNoBody = errors.New("sender: message has no body")
)

// sendplane headers (architecture 10).
const (
	// HeaderSendplaneID correlates a DSN with a delivery even when the relay
	// rewrote the envelope sender.
	HeaderSendplaneID = "X-Sendplane-ID"
	// HeaderAttempt carries the attempt number. It makes a retry visible in a
	// relay's logs, and it is what chaossmtp keys its deterministic failures
	// on.
	HeaderAttempt = "X-Sendplane-Attempt"
)

// allowedCustomHeaders is the whitelist of architecture 16. Anything starting
// with "x-" is allowed as well; everything else is rejected, because a hook
// that can set arbitrary headers can set Bcc.
var allowedCustomHeaders = map[string]bool{
	"in-reply-to":    true,
	"references":     true,
	"auto-submitted": true,
	"precedence":     true,
	"list-id":        true,
	"list-help":      true,
	"list-post":      true,
	"list-owner":     true,
	"list-archive":   true,
	"importance":     true,
	"priority":       true,
}

// headersSetBySender may not be overridden by a hook: they carry the
// correlation identity of architecture 10 or are built from the mode.
var headersSetBySender = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "subject": true,
	"date": true, "message-id": true, "mime-version": true,
	"content-type": true, "content-transfer-encoding": true,
	"return-path": true, "reply-to": true,
	"list-unsubscribe": true, "list-unsubscribe-post": true,
	strings.ToLower(HeaderSendplaneID): true,
	strings.ToLower(HeaderAttempt):     true,
}

// dkimKey is a parsed sending-domain signing key.
type dkimKey struct {
	domain   string
	selector string
	signer   crypto.Signer
}

// messageInput is everything buildMessage needs. Everything in it has already
// been rendered and hooked.
type messageInput struct {
	msg       *host.OutboundMessage
	messageID string
	attemptNo int
	date      time.Time
	// listUnsubscribe is the URI for the List-Unsubscribe header, empty for
	// UnsubscribeNone.
	listUnsubscribe string
	// oneClick adds List-Unsubscribe-Post (RFC 8058).
	oneClick bool
	dkim     *dkimKey
}

// buildMessage assembles the MIME message and signs it when a DKIM key is
// configured. The returned bytes are CRLF-terminated and ready for DATA.
func buildMessage(in messageInput) ([]byte, error) {
	m := in.msg
	if strings.TrimSpace(m.HTML) == "" && strings.TrimSpace(m.Text) == "" {
		return nil, ErrNoBody
	}
	if err := validateOutbound(m); err != nil {
		return nil, err
	}

	msg := gomail.NewMsg(gomail.WithNoDefaultUserAgent())
	if in.date.IsZero() {
		msg.SetDate()
	} else {
		msg.SetDateWithValue(in.date)
	}
	if m.FromName != "" {
		if err := msg.FromFormat(m.FromName, m.From); err != nil {
			return nil, fmt.Errorf("sender: from: %w", err)
		}
	} else if err := msg.From(m.From); err != nil {
		return nil, fmt.Errorf("sender: from: %w", err)
	}
	if m.Recipient.Name != "" {
		if err := msg.AddToFormat(m.Recipient.Name, m.Recipient.Email); err != nil {
			return nil, fmt.Errorf("sender: to: %w", err)
		}
	} else if err := msg.To(m.Recipient.Email); err != nil {
		return nil, fmt.Errorf("sender: to: %w", err)
	}
	if m.ReplyTo != "" {
		if err := msg.ReplyTo(m.ReplyTo); err != nil {
			return nil, fmt.Errorf("sender: reply-to: %w", err)
		}
	}
	msg.Subject(m.Subject)
	msg.SetMessageIDWithValue(in.messageID)
	msg.SetGenHeader(HeaderSendplaneID, m.TenantID+"/"+m.DeliveryID)
	msg.SetGenHeader(HeaderAttempt, strconv.Itoa(in.attemptNo))
	setPrecedence(msg, m)

	if in.listUnsubscribe != "" {
		msg.SetListUnsubscribe(in.listUnsubscribe)
		if in.oneClick {
			msg.SetListUnsubscribePost()
		}
	}
	for name, value := range m.Headers {
		msg.SetGenHeader(gomail.Header(name), value)
	}

	// text/plain first, text/html as the alternative: that is the order
	// multipart/alternative requires (least capable part first).
	switch {
	case strings.TrimSpace(m.Text) != "" && strings.TrimSpace(m.HTML) != "":
		msg.SetBodyString(gomail.TypeTextPlain, m.Text)
		msg.AddAlternativeString(gomail.TypeTextHTML, m.HTML)
	case strings.TrimSpace(m.HTML) != "":
		msg.SetBodyString(gomail.TypeTextHTML, m.HTML)
	default:
		msg.SetBodyString(gomail.TypeTextPlain, m.Text)
	}

	var buf bytes.Buffer
	if _, err := msg.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("sender: write message: %w", err)
	}
	if in.dkim == nil {
		return buf.Bytes(), nil
	}
	return signDKIM(buf.Bytes(), in.dkim)
}

// setPrecedence marks bulk mail so that auto-responders stay quiet.
func setPrecedence(msg *gomail.Msg, m *host.OutboundMessage) {
	if m.Lane == store.LaneBulk {
		msg.SetBulk()
	}
}

// dkimHeaders is the signed header set of RFC 6376 section 5.4.1 plus the
// headers a receiver uses to decide what the mail is.
var dkimHeaders = []string{
	"From", "To", "Subject", "Date", "Message-ID", "MIME-Version",
	"Content-Type", "Content-Transfer-Encoding",
	"List-Unsubscribe", "List-Unsubscribe-Post", HeaderSendplaneID,
}

func signDKIM(raw []byte, k *dkimKey) ([]byte, error) {
	var out bytes.Buffer
	opts := &dkim.SignOptions{
		Domain:                 k.domain,
		Selector:               k.selector,
		Signer:                 k.signer,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys:             dkimHeaders,
	}
	if err := dkim.Sign(&out, bytes.NewReader(raw), opts); err != nil {
		return nil, fmt.Errorf("sender: dkim sign: %w", err)
	}
	return out.Bytes(), nil
}

// parseDKIMKey accepts a PEM private key, PKCS#1 or PKCS#8, RSA or Ed25519.
func parseDKIMKey(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("sender: dkim key is not PEM")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("sender: dkim key: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("sender: dkim key: %w", err)
		}
		switch k := key.(type) {
		case *rsa.PrivateKey:
			return k, nil
		case ed25519.PrivateKey:
			return k, nil
		default:
			return nil, fmt.Errorf("sender: dkim key type %T is not supported", key)
		}
	default:
		return nil, fmt.Errorf("sender: dkim key block %q is not a private key", block.Type)
	}
}

// validateOutbound is the header injection check of architecture 16. It runs
// after BeforeSend, so a hook cannot smuggle a header through either.
func validateOutbound(m *host.OutboundMessage) error {
	for _, f := range []struct{ name, value string }{
		{"subject", m.Subject},
		{"from name", m.FromName},
		{"recipient name", m.Recipient.Name},
		{"unsubscribe url", m.UnsubscribeURL},
	} {
		if strings.ContainsAny(f.value, "\r\n") {
			return fmt.Errorf("%w: %s contains CR or LF", ErrHeaderInjection, f.name)
		}
	}
	for _, f := range []struct{ name, value string }{
		{"from", m.From},
		{"reply-to", m.ReplyTo},
		{"to", m.Recipient.Email},
	} {
		if f.value == "" {
			if f.name == "from" || f.name == "to" {
				return fmt.Errorf("%w: %s is empty", ErrHeaderInjection, f.name)
			}
			continue
		}
		if strings.ContainsAny(f.value, "\r\n") {
			return fmt.Errorf("%w: %s contains CR or LF", ErrHeaderInjection, f.name)
		}
		if _, err := mail.ParseAddress(f.value); err != nil {
			return fmt.Errorf("%w: %s %q: %v", ErrHeaderInjection, f.name, f.value, err)
		}
	}
	for name, value := range m.Headers {
		if err := validateCustomHeader(name, value); err != nil {
			return err
		}
	}
	return nil
}

func validateCustomHeader(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%w: header %q value contains CR or LF", ErrHeaderInjection, name)
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" || lower != name && strings.ContainsAny(name, "\r\n: ") {
		return fmt.Errorf("%w: header name %q", ErrHeaderInjection, name)
	}
	if strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(name, " \t") {
		return fmt.Errorf("%w: header name %q", ErrHeaderInjection, name)
	}
	if headersSetBySender[lower] {
		return fmt.Errorf("%w: %q is set by sendplane", ErrHeaderNotAllowed, name)
	}
	if strings.HasPrefix(lower, "x-") || allowedCustomHeaders[lower] {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrHeaderNotAllowed, name)
}
