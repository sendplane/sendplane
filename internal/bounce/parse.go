// Package bounce turns the mail in a bounce mailbox into delivery state:
// parse, correlate with a Delivery, record a BounceEvent, transition the
// delivery, suppress the address and emit the host event (architecture 10,
// ADR-0008).
//
// The three layers are separate on purpose. Parse is a pure function of the
// raw message, so the fixture corpus in testdata is the whole test surface for
// classification. Processor.Handle turns a Parsed into store writes for one
// tenant. Runner polls mailboxes and feeds the processor, holding a store lock
// per mailbox so that replicas never process the same mail twice.
package bounce

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

// Confidence is how much the classification can be trusted: high for a
// structured DSN or feedback report, medium for a report without a status code
// or a heuristic that found an SMTP code, low for a subject/body match alone.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// BounceSourceNone is the correlation source of a message that matched
// nothing. store.BounceSource has no such value, so the package defines one
// rather than pretending the event came from a heuristic.
const BounceSourceNone store.BounceSource = ""

// Correlation is which of the three plants of architecture 10 identified the
// delivery, and whether the evidence survived verification.
type Correlation struct {
	DeliveryID string
	// TenantID is only filled by the X-Sendplane-ID header, which is the only
	// correlation that carries one.
	TenantID string
	Source   store.BounceSource
	// Verified is false only when a VERP address was present and its HMAC did
	// not match any of the tenant's keys. Such a bounce is recorded and never
	// transitions a delivery (ADR-0008). Correlation through the returned
	// original headers is Verified: it is not a MAC, but nothing contradicted
	// it, and the delivery still has to exist in this tenant and be sent.
	Verified bool
}

// Parsed is everything one bounce mail says.
type Parsed struct {
	Type store.BounceType
	// AutoReply marks an out-of-office or vacation responder. store.BounceType
	// has no value for it (the store never sees one), so it is a flag: the
	// processor drops the message without recording anything.
	AutoReply bool

	// Action and Status are the RFC 3464 per-recipient fields
	// ("failed"/"delayed"..., "5.1.1").
	Action         string
	Status         string
	DiagnosticCode string
	FinalRecipient string
	ReportingMTA   string
	// FeedbackType is the RFC 5965 Feedback-Type ("abuse", "fraud", ...).
	FeedbackType string

	// MessageID is the original message's Message-ID, when the report carried
	// it back.
	MessageID string
	Subject   string
	// Date is the report's own Date header, zero when it had none.
	Date time.Time

	Confidence  Confidence
	Correlation Correlation
}

// Recipient is the address the report is about: the DSN's final recipient when
// there is one.
func (p Parsed) Recipient() string { return p.FinalRecipient }

// IsBounce reports whether the message says something happened to a delivery.
func (p Parsed) IsBounce() bool {
	return !p.AutoReply && p.Type != store.BounceUnknown
}

// maxDepth bounds the MIME walk. A bounce mailbox is fed by strangers, so a
// deeply nested message must cost a bounded amount of work (architecture 16).
const maxDepth = 8

// maxPartSize is how much of one part is read. A returned original can be
// megabytes; everything this package looks at is in the first few KiB of
// headers, and the whole raw message is kept separately when the tenant asked
// for it.
const maxPartSize = 1 << 20

// Parse classifies one raw message without verifying any VERP MAC: the
// correlation it returns is Verified only when it came from a header. Callers
// that have the tenant's signing keys use ParseWithKeys.
func Parse(raw []byte) (Parsed, error) { return ParseWithKeys(raw, nil) }

// ParseWithKeys parses and verifies VERP addresses against the tenant's
// tracking signing keys (the same keys the sender signed the Return-Path
// with, internal/tracking).
func ParseWithKeys(raw []byte, keys []store.SigningKey) (Parsed, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Parsed{}, fmt.Errorf("bounce: parse message: %w", err)
	}
	top := textproto.MIMEHeader(msg.Header)

	c := &collected{top: top}
	c.walk(top, msg.Body, 0)

	p := Parsed{
		Subject:      decodeHeader(top.Get("Subject")),
		ReportingMTA: fieldValue(c.dsnPerMessage.Get("Reporting-MTA")),
	}
	if d, err := mail.ParseDate(top.Get("Date")); err == nil {
		p.Date = d.UTC()
	}
	applyReport(&p, c)
	classify(&p, c)
	p.Correlation = correlate(c, keys)
	return p, nil
}

// applyReport copies the structured report fields onto the result.
func applyReport(p *Parsed, c *collected) {
	if rcpt := c.recipientBlock(); rcpt != nil {
		p.Action = strings.ToLower(fieldValue(rcpt.Get("Action")))
		p.Status = strings.TrimSpace(fieldValue(rcpt.Get("Status")))
		p.DiagnosticCode = strings.TrimSpace(rcpt.Get("Diagnostic-Code"))
		p.FinalRecipient = fieldValue(rcpt.Get("Final-Recipient"))
		if p.FinalRecipient == "" {
			p.FinalRecipient = fieldValue(rcpt.Get("Original-Recipient"))
		}
	}
	if c.feedback != nil {
		p.FeedbackType = strings.ToLower(strings.TrimSpace(c.feedback.Get("Feedback-Type")))
		if p.FinalRecipient == "" {
			p.FinalRecipient = addressOnly(c.feedback.Get("Original-Rcpt-To"))
		}
	}
	if c.original != nil {
		// Stored without the angle brackets, like Delivery.MessageID.
		p.MessageID = strings.Trim(strings.TrimSpace(c.original.Get("Message-Id")), "<>")
	}
	p.FinalRecipient = addressOnly(p.FinalRecipient)
}

// --- MIME walk ---------------------------------------------------------

// collected is what the walk found. Everything the classifier and the
// correlator need is in here, so neither of them touches MIME again.
type collected struct {
	top textproto.MIMEHeader

	// dsnPerMessage is the first block of a message/delivery-status part,
	// dsnRecipients the ones after it.
	dsnPerMessage textproto.MIMEHeader
	dsnRecipients []textproto.MIMEHeader
	// feedback is a message/feedback-report part (RFC 5965).
	feedback textproto.MIMEHeader
	// original holds the headers of the returned message, from a
	// message/rfc822 or text/rfc822-headers part.
	original textproto.MIMEHeader
	// text is the human-readable part, for the heuristics.
	text string
	// hasReport is true when a structured report part was present.
	hasReport bool
}

func (c *collected) walk(h textproto.MIMEHeader, body io.Reader, depth int) {
	if depth > maxDepth {
		return
	}
	mediaType, params := contentType(h)
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			c.walk(part.Header, part, depth+1)
			_ = part.Close()
		}

	case mediaType == "message/delivery-status":
		c.hasReport = true
		blocks := readBlocks(decodeBody(h, body))
		if len(blocks) > 0 && c.dsnPerMessage == nil {
			c.dsnPerMessage = blocks[0]
			c.dsnRecipients = append(c.dsnRecipients, blocks[1:]...)
		}

	case mediaType == "message/feedback-report":
		c.hasReport = true
		if blocks := readBlocks(decodeBody(h, body)); len(blocks) > 0 && c.feedback == nil {
			c.feedback = blocks[0]
		}

	case mediaType == "message/rfc822", mediaType == "text/rfc822-headers",
		mediaType == "message/global", mediaType == "message/global-headers":
		if c.original != nil {
			return
		}
		if hdr := readHeaderOnly(decodeBody(h, body)); hdr != nil {
			c.original = hdr
		}

	case strings.HasPrefix(mediaType, "text/"):
		if len(c.text) < maxPartSize {
			b, _ := io.ReadAll(io.LimitReader(decodeBody(h, body), maxPartSize))
			c.text += "\n" + string(b)
		}
	}
}

// recipientBlock picks the per-recipient block the report is about: the first
// failure, otherwise the first block carrying a status at all.
func (c *collected) recipientBlock() textproto.MIMEHeader {
	var withStatus textproto.MIMEHeader
	for _, b := range c.dsnRecipients {
		action := strings.ToLower(fieldValue(b.Get("Action")))
		if action == "failed" {
			return b
		}
		if withStatus == nil && (b.Get("Status") != "" || action != "") {
			withStatus = b
		}
	}
	if withStatus != nil {
		return withStatus
	}
	if len(c.dsnRecipients) > 0 {
		return c.dsnRecipients[0]
	}
	// Some MTAs emit a single block that mixes per-message and per-recipient
	// fields.
	if c.dsnPerMessage != nil && c.dsnPerMessage.Get("Final-Recipient") != "" {
		return c.dsnPerMessage
	}
	return nil
}

func contentType(h textproto.MIMEHeader) (string, map[string]string) {
	v := h.Get("Content-Type")
	if v == "" {
		return "text/plain", nil
	}
	mediaType, params, err := mime.ParseMediaType(v)
	if err != nil {
		// A broken Content-Type still tells us the top-level type.
		mediaType, _, _ = strings.Cut(v, ";")
		return strings.ToLower(strings.TrimSpace(mediaType)), nil
	}
	return strings.ToLower(mediaType), params
}

// decodeBody undoes the transfer encoding of a part. An unknown encoding is
// passed through: garbled text costs a low-confidence classification, an error
// would cost the whole message.
func decodeBody(h textproto.MIMEHeader, body io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding"))) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	default:
		return body
	}
}

// readBlocks reads the blank-line separated header groups a
// message/delivery-status or message/feedback-report body is made of.
func readBlocks(r io.Reader) []textproto.MIMEHeader {
	var out []textproto.MIMEHeader
	tp := textproto.NewReader(bufio.NewReader(io.LimitReader(r, maxPartSize)))
	for {
		h, err := tp.ReadMIMEHeader()
		if len(h) > 0 {
			out = append(out, h)
		}
		if err != nil {
			return out
		}
	}
}

// readHeaderOnly reads the header of a returned original message and drops its
// body.
func readHeaderOnly(r io.Reader) textproto.MIMEHeader {
	tp := textproto.NewReader(bufio.NewReader(io.LimitReader(r, maxPartSize)))
	h, err := tp.ReadMIMEHeader()
	if len(h) == 0 && err != nil {
		return nil
	}
	return h
}

// fieldValue strips the "rfc822;" / "dns;" type prefix RFC 3464 puts in front
// of addresses and MTA names.
func fieldValue(v string) string {
	v = strings.TrimSpace(v)
	if before, after, ok := strings.Cut(v, ";"); ok && !strings.Contains(before, "@") {
		return strings.TrimSpace(after)
	}
	return v
}

// addressOnly reduces "Name <a@b>" or "<a@b>" to "a@b".
func addressOnly(v string) string {
	v = strings.TrimSpace(fieldValue(v))
	if v == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(v); err == nil {
		return addr.Address
	}
	v = strings.Trim(v, "<>")
	if i := strings.LastIndex(v, "<"); i >= 0 {
		v = strings.Trim(v[i:], "<>")
	}
	return strings.TrimSpace(v)
}

func decodeHeader(v string) string {
	out, err := (&mime.WordDecoder{}).DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}

// --- correlation -------------------------------------------------------

// verpHeaders are the headers a VERP envelope recipient survives in, in the
// order they are trusted (architecture 10).
var verpHeaders = []string{"Return-Path", "To", "Delivered-To", "X-Original-To", "Envelope-To"}

// correlate runs the three plants of architecture 10 in order: the VERP
// envelope address first, because it is the only one that carries a MAC, then
// the headers of the returned original.
func correlate(c *collected, keys []store.SigningKey) Correlation {
	for _, addr := range verpCandidates(c) {
		id, verified := tracking.ParseVERP(addr, keys)
		if id == "" {
			continue
		}
		// A VERP address that does not verify is recorded as unverified and
		// deliberately not re-correlated through the headers: a forged bounce
		// must not be laundered by an X-Sendplane-ID copied out of a mail the
		// forger received (ADR-0008).
		return Correlation{DeliveryID: id, Source: store.BounceSourceVERP, Verified: verified}
	}

	for _, h := range []textproto.MIMEHeader{c.original, c.top} {
		if h == nil {
			continue
		}
		if tenant, id, ok := parseSendplaneID(h.Get(HeaderSendplaneID)); ok {
			return Correlation{
				DeliveryID: id, TenantID: tenant,
				Source: store.BounceSourceHeader, Verified: true,
			}
		}
	}

	for _, v := range messageIDCandidates(c) {
		if id := deliveryIDFromMessageID(v); id != "" {
			return Correlation{
				DeliveryID: id,
				Source:     store.BounceSourceMessageID, Verified: true,
			}
		}
	}
	return Correlation{Source: BounceSourceNone}
}

// verpCandidates lists every address a VERP return path can show up in: the
// report's own envelope headers and the recipient fields of the report.
func verpCandidates(c *collected) []string {
	var out []string
	add := func(v string) {
		for _, part := range strings.Split(v, ",") {
			if a := addressOnly(part); a != "" {
				out = append(out, a)
			}
		}
	}
	for _, name := range verpHeaders {
		if v := c.top.Get(name); v != "" {
			add(v)
		}
	}
	if c.dsnPerMessage != nil {
		add(c.dsnPerMessage.Get("Original-Envelope-Id"))
		add(c.dsnPerMessage.Get("X-Postfix-Sender"))
	}
	// A double bounce reports our own VERP address as the recipient that
	// failed.
	if rcpt := c.recipientBlock(); rcpt != nil {
		add(rcpt.Get("Final-Recipient"))
		add(rcpt.Get("Original-Recipient"))
	}
	if c.feedback != nil {
		add(c.feedback.Get("Original-Mail-From"))
	}
	// The returned original's own Return-Path is the VERP address the sender
	// used, which survives when the DSN's envelope was rewritten.
	if c.original != nil {
		for _, name := range []string{"Return-Path", "X-Original-To", "Delivered-To"} {
			if v := c.original.Get(name); v != "" {
				add(v)
			}
		}
	}
	return out
}

// HeaderSendplaneID is the correlation header internal/sender stamps on every
// message ("tenant/deliveryID"). It is duplicated here rather than imported so
// that bounce does not depend on sender; message_test.go in this package
// pins the two together.
const HeaderSendplaneID = "X-Sendplane-ID"

func parseSendplaneID(v string) (tenantID, deliveryID string, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", false
	}
	tenant, delivery, found := strings.Cut(v, "/")
	if !found || strings.TrimSpace(delivery) == "" {
		return "", "", false
	}
	return strings.TrimSpace(tenant), strings.TrimSpace(delivery), true
}

// messageIDCandidates lists the message identifiers a report can carry the
// original's Message-ID in.
func messageIDCandidates(c *collected) []string {
	var out []string
	if c.original != nil {
		out = append(out, c.original.Get("Message-Id"))
	}
	for _, name := range []string{"In-Reply-To", "References"} {
		if v := c.top.Get(name); v != "" {
			out = append(out, strings.Fields(v)...)
		}
	}
	return out
}

// deliveryIDFromMessageID extracts the delivery ID from the Message-ID the
// sender built: "<{deliveryID}@{domain}>" (internal/sender/process.go).
func deliveryIDFromMessageID(v string) string {
	v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), "<>"))
	local, _, ok := strings.Cut(v, "@")
	if !ok || !isUUID(local) {
		return ""
	}
	return local
}

// isUUID keeps a Message-ID from some other system out of the correlation: a
// delivery ID is always a UUIDv7 string (store.NewID).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
