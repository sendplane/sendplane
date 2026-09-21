package main

import (
	"net/textproto"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/chaossmtp"
)

func msg(rcpt, subject, body string) chaossmtp.Message {
	return chaossmtp.Message{
		From:       "bounce@example.com",
		Rcpts:      []string{rcpt},
		Subject:    subject,
		Size:       len(body),
		Body:       []byte(body),
		ReceivedAt: time.Unix(0, 0).UTC(),
		Headers: textproto.MIMEHeader{
			"Subject":        {subject},
			"X-Sendplane-Id": {"d1"},
		},
	}
}

// GET /messages is what makes an assertion on the rendered mail possible
// against the chaos relay (GAP-1 in test/e2e/README.md), so the filter and the
// body opt-in are the contract.
func TestSelectMessages(t *testing.T) {
	msgs := []chaossmtp.Message{
		msg("a@example.org", "first", "body-a"),
		msg("B@Example.ORG", "second", "body-b"),
		msg("a@example.org", "third", "body-c"),
	}

	all := selectMessages(msgs, "", 0, false)
	if len(all) != 3 {
		t.Fatalf("no filter returned %d messages, want 3", len(all))
	}
	if all[0].Body != "" {
		t.Errorf("body returned without body=1: %q", all[0].Body)
	}
	if all[0].Headers["X-Sendplane-Id"] != "d1" || all[0].Subject != "first" {
		t.Errorf("headers lost: %+v", all[0])
	}

	// The recipient filter is case-insensitive: an envelope recipient is
	// whatever the client sent in RCPT TO.
	mine := selectMessages(msgs, "b@example.org", 0, true)
	if len(mine) != 1 || mine[0].Body != "body-b" {
		t.Fatalf("rcpt filter = %+v, want the one message with its body", mine)
	}

	// A limit keeps the most recent matches.
	last := selectMessages(msgs, "a@example.org", 1, false)
	if len(last) != 1 || last[0].Subject != "third" {
		t.Fatalf("limit = %+v, want the newest match", last)
	}
	if got := selectMessages(msgs, "nobody@example.org", 0, false); len(got) != 0 {
		t.Fatalf("unknown recipient matched %d messages", len(got))
	}
}

func TestIntParam(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"10", 10, false},
		{"-1", 0, true},
		{"x", 0, true},
	} {
		got, err := intParam(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("intParam(%q) error = %v, want error %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("intParam(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
