package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// greenmail talks to the GreenMail standalone container two ways:
//
//   - its REST API (port 8080), to read what was delivered. sendplane itself
//     reads the same mailboxes over real IMAP — that is the path under test —
//     so the harness deliberately uses a different door and cannot mask an
//     IMAP bug by consuming the mail first.
//   - plain SMTP (port 3025), to inject the synthesized DSN and ARF reports of
//     scenario 5 into the bounce mailbox.
type greenmail struct {
	apiBase  string // http://127.0.0.1:18581
	smtpAddr string // 127.0.0.1:13325
	hc       *http.Client
}

func newGreenmail(apiBase, smtpAddr string) *greenmail {
	return &greenmail{
		apiBase:  strings.TrimRight(apiBase, "/"),
		smtpAddr: smtpAddr,
		hc:       &http.Client{Timeout: 15 * time.Second},
	}
}

// gmMessage is one entry of GET /api/user/{address}/messages. mimeMessage is
// the whole raw message, Return-Path included: GreenMail writes the envelope
// sender into it, which is how the harness gets at the real VERP address the
// sender used (architecture 10) rather than reconstructing one.
type gmMessage struct {
	UID         string `json:"uid"`
	MessageID   string `json:"Message-ID"`
	Subject     string `json:"subject"`
	ContentType string `json:"contentType"`
	Raw         string `json:"mimeMessage"`
}

// header reads one header of the raw message. Folded continuation lines are
// unfolded, because the sender wraps long List-Unsubscribe values.
func (m gmMessage) header(name string) string {
	r := textproto.NewReader(bufio.NewReader(strings.NewReader(m.Raw)))
	hdr, err := r.ReadMIMEHeader()
	if err != nil && len(hdr) == 0 {
		return ""
	}
	return hdr.Get(name)
}

// decodedSubject returns the Subject with RFC 2047 encoded words decoded; a ko
// subject is base64 `=?UTF-8?B?...?=` on the wire.
func (m gmMessage) decodedSubject() string {
	s := m.header("Subject")
	dec := &mime.WordDecoder{}
	if out, err := dec.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

// htmlPart returns the decoded text/html body. The sender emits a
// multipart/alternative whose parts are quoted-printable or base64, so the
// tracking URLs are only intact after the transfer encoding is undone — a
// regex over the raw bytes would see `=\r\n` soft line breaks in the middle of
// a token.
func (m gmMessage) htmlPart() (string, error) {
	msg, err := mail.ReadMessage(strings.NewReader(m.Raw))
	if err != nil {
		return "", fmt.Errorf("parse message: %w", err)
	}
	body, err := findPart(msg.Header.Get("Content-Type"),
		msg.Header.Get("Content-Transfer-Encoding"), msg.Body, "text/html", 0)
	if err != nil {
		return "", err
	}
	return body, nil
}

// findPart walks a MIME tree depth-first and returns the first part whose type
// matches want, decoded.
func findPart(contentType, encoding string, body io.Reader, want string, depth int) (string, error) {
	if depth > 6 {
		return "", errors.New("MIME nesting too deep")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("no %s part", want)
			}
			if err != nil {
				return "", err
			}
			out, err := findPart(p.Header.Get("Content-Type"),
				p.Header.Get("Content-Transfer-Encoding"), p, want, depth+1)
			if err == nil {
				return out, nil
			}
		}
	}
	if mediaType != want {
		return "", fmt.Errorf("no %s part", want)
	}
	r := io.Reader(body)
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, &newlineStripper{r: body})
	}
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// newlineStripper drops CR and LF so base64.NewDecoder sees a continuous
// stream; MIME wraps base64 at 76 columns.
type newlineStripper struct{ r io.Reader }

func (s *newlineStripper) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	out := p[:0]
	for _, b := range p[:n] {
		if b != '\r' && b != '\n' {
			out = append(out, b)
		}
	}
	return len(out), err
}

func (g *greenmail) ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+"/api/service/readiness", nil)
	if err != nil {
		return err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("greenmail readiness: HTTP %d", resp.StatusCode)
	}
	return nil
}

// messages returns everything currently in an address's INBOX. An address that
// has never received mail and never been logged into is not an error: GreenMail
// answers 404 and the caller is waiting for the first message.
func (g *greenmail) messages(ctx context.Context, address string) ([]gmMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		g.apiBase+"/api/user/"+queryEscape(address)+"/messages", nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("greenmail messages(%s): HTTP %d", address, resp.StatusCode)
	}
	var out []gmMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("greenmail messages(%s): %w", address, err)
	}
	return out, nil
}

// waitForMessage polls until address holds at least want messages.
func (g *greenmail) waitForMessage(ctx context.Context, address string, want int, timeout time.Duration) ([]gmMessage, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		msgs, err := g.messages(ctx, address)
		switch {
		case err != nil:
			last = err
		case len(msgs) >= want:
			return msgs, nil
		default:
			last = fmt.Errorf("%s holds %d message(s), want %d", address, len(msgs), want)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("waiting for mail in %s: %w", address, last)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// deliver injects a raw message over SMTP with an explicit envelope: the
// envelope recipient has nothing to do with the message's own To header, which
// is exactly what a real bounce looks like.
//
// envelopeFrom may not be empty. RFC 3464 wants a DSN carried with
// `MAIL FROM:<>`, but GreenMail 2.1.14 answers 250 to a null reverse path and
// then does not record it, so the next RCPT fails with "503 MAIL must come
// before RCPT". The DSN's own `Return-Path: <>` header — which is what
// internal/bounce actually reads, since the poller sees the message over IMAP
// and never sees this envelope — is unaffected.
func (g *greenmail) deliver(envelopeFrom, envelopeTo string, raw []byte) error {
	if envelopeFrom == "" {
		return errors.New("greenmail: a null reverse path is accepted and then ignored by GreenMail; " +
			"pass the reporting MTA's address instead")
	}
	c, err := smtp.Dial(g.smtpAddr)
	if err != nil {
		return fmt.Errorf("greenmail smtp dial: %w", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Hello("e2e.harness.test"); err != nil {
		return fmt.Errorf("greenmail EHLO: %w", err)
	}
	if err := c.Mail(envelopeFrom); err != nil {
		return fmt.Errorf("greenmail MAIL FROM<%s>: %w", envelopeFrom, err)
	}
	if err := c.Rcpt(envelopeTo); err != nil {
		return fmt.Errorf("greenmail RCPT TO<%s>: %w", envelopeTo, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("greenmail DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("greenmail DATA write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("greenmail end of DATA: %w", err)
	}
	return c.Quit()
}
