package main

import (
	"fmt"
	"strings"
	"time"
)

// The bounce fixtures of scenario 5, built from the real message sendplane
// delivered rather than from a canned file: the correlation under test is
// VERP -> X-Sendplane-ID -> Message-ID (architecture 10), and all three come
// out of the message GreenMail received.
//
// They are shaped after internal/bounce/testdata/{postfix_hard,arf_complaint,
// forged_verp}.eml, which is also what internal/bounce's unit tests parse, so
// a change to the parser that breaks the fixtures breaks this too.

// bouncedOriginal is what a reporting MTA returns: enough of the original
// message for sendplane to recognize its own delivery.
type bouncedOriginal struct {
	ReturnPath  string // the VERP envelope sender, as GreenMail recorded it
	From        string
	To          string
	Subject     string
	MessageID   string // with angle brackets
	SendplaneID string // "{tenant}/{deliveryID}"
	Date        string
}

// returnedHeaders renders the text/rfc822-headers part.
func (o bouncedOriginal) returnedHeaders() string {
	var b strings.Builder
	if o.ReturnPath != "" {
		fmt.Fprintf(&b, "Return-Path: <%s>\r\n", o.ReturnPath)
	}
	fmt.Fprintf(&b, "From: %s\r\n", o.From)
	fmt.Fprintf(&b, "To: %s\r\n", o.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", o.Subject)
	fmt.Fprintf(&b, "Date: %s\r\n", o.Date)
	fmt.Fprintf(&b, "Message-ID: %s\r\n", o.MessageID)
	fmt.Fprintf(&b, "X-Sendplane-ID: %s\r\n", o.SendplaneID)
	b.WriteString("MIME-Version: 1.0\r\n")
	return b.String()
}

func rfc5322Date(t time.Time) string { return t.UTC().Format(time.RFC1123Z) }

// hardDSN is an RFC 3464 report for a 5.1.1 "user unknown", the shape Postfix
// produces. envelopeVERP is what goes in the DSN's own To header, which is
// where the correlation normally starts.
func hardDSN(o bouncedOriginal, envelopeVERP string, now time.Time) []byte {
	const boundary = "E2EDSNBOUNDARY"
	var b strings.Builder
	fmt.Fprintf(&b, "Return-Path: <>\r\n")
	fmt.Fprintf(&b, "Delivered-To: %s\r\n", envelopeVERP)
	fmt.Fprintf(&b, "From: MAILER-DAEMON@mx.sendplane.test (Mail Delivery System)\r\n")
	fmt.Fprintf(&b, "To: %s\r\n", envelopeVERP)
	fmt.Fprintf(&b, "Subject: Undelivered Mail Returned to Sender\r\n")
	fmt.Fprintf(&b, "Date: %s\r\n", rfc5322Date(now))
	fmt.Fprintf(&b, "Auto-Submitted: auto-replied\r\n")
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Message-Id: <dsn-%d@mx.sendplane.test>\r\n", now.UnixNano())
	fmt.Fprintf(&b, "Content-Type: multipart/report; report-type=delivery-status;\r\n\tboundary=%q\r\n", boundary)
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Description: Notification\r\n")
	b.WriteString("Content-Type: text/plain; charset=us-ascii\r\n\r\n")
	fmt.Fprintf(&b, "This is the mail system at host mx.sendplane.test.\r\n\r\n"+
		"<%s>: host mx.sendplane.test said: 550 5.1.1 <%s>: Recipient address\r\n"+
		"    rejected: User unknown in local recipient table (in reply to RCPT TO command)\r\n\r\n",
		o.To, o.To)

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Description: Delivery report\r\n")
	b.WriteString("Content-Type: message/delivery-status\r\n\r\n")
	b.WriteString("Reporting-MTA: dns; mx.sendplane.test\r\n")
	fmt.Fprintf(&b, "X-Postfix-Sender: rfc822; %s\r\n", envelopeVERP)
	fmt.Fprintf(&b, "Arrival-Date: %s\r\n\r\n", rfc5322Date(now))
	fmt.Fprintf(&b, "Final-Recipient: rfc822; %s\r\n", o.To)
	fmt.Fprintf(&b, "Original-Recipient: rfc822;%s\r\n", o.To)
	b.WriteString("Action: failed\r\n")
	b.WriteString("Status: 5.1.1\r\n")
	b.WriteString("Remote-MTA: dns; mx.sendplane.test\r\n")
	fmt.Fprintf(&b, "Diagnostic-Code: smtp; 550 5.1.1 <%s>: Recipient address\r\n"+
		"    rejected: User unknown in local recipient table\r\n\r\n", o.To)

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Description: Undelivered Message Headers\r\n")
	b.WriteString("Content-Type: text/rfc822-headers\r\n")
	b.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
	b.WriteString(o.returnedHeaders())
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String())
}

// arfComplaint is an RFC 5965 feedback report ("this was spam"), which has to
// end in `complained` rather than `bounced`.
func arfComplaint(o bouncedOriginal, envelopeVERP string, now time.Time) []byte {
	const boundary = "E2EARFBOUNDARY"
	var b strings.Builder
	fmt.Fprintf(&b, "Return-Path: <complaints@feedback.sendplane.test>\r\n")
	fmt.Fprintf(&b, "From: complaints@feedback.sendplane.test\r\n")
	fmt.Fprintf(&b, "To: %s\r\n", envelopeVERP)
	fmt.Fprintf(&b, "Subject: FW: %s\r\n", o.Subject)
	fmt.Fprintf(&b, "Date: %s\r\n", rfc5322Date(now))
	fmt.Fprintf(&b, "Message-ID: <arf-%d@feedback.sendplane.test>\r\n", now.UnixNano())
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/report; report-type=feedback-report; boundary=%q\r\n", boundary)
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=\"US-ASCII\"\r\n\r\n")
	b.WriteString("This is an email abuse report for an email message received from\r\n" +
		"IP 198.51.100.10.\r\n\r\n")

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: message/feedback-report\r\n\r\n")
	b.WriteString("Feedback-Type: abuse\r\n")
	b.WriteString("User-Agent: sendplane-e2e/1.0\r\n")
	b.WriteString("Version: 1\r\n")
	fmt.Fprintf(&b, "Original-Mail-From: <%s>\r\n", envelopeVERP)
	fmt.Fprintf(&b, "Original-Rcpt-To: <%s>\r\n", o.To)
	fmt.Fprintf(&b, "Arrival-Date: %s\r\n", rfc5322Date(now))
	b.WriteString("Reported-Domain: sendplane.test\r\n")
	b.WriteString("Source-IP: 198.51.100.10\r\n\r\n")

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: message/rfc822\r\n\r\n")
	b.WriteString(o.returnedHeaders())
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString("The message the recipient reported.\r\n\r\n")

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String())
}

// forgeVERP corrupts the HMAC tag of a VERP address while keeping the delivery
// ID intact: the forgery an attacker who read `X-Sendplane-ID` off their own
// copy of the mail could actually mount (architecture 16). "deadbeef" is the
// tag internal/bounce/testdata/forged_verp.eml uses.
func forgeVERP(verp string) string {
	local, domain, ok := strings.Cut(verp, "@")
	if !ok {
		return verp
	}
	i := strings.LastIndex(local, ".")
	if i < 0 {
		return verp
	}
	return local[:i] + ".deadbeef@" + domain
}
