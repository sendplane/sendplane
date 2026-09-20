package bounce

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

// The fixture corpus is built around two delivery IDs and one signing key.
// The VERP addresses inside the .eml files were produced by
// tracking.VERPAddress with testSecret, which TestFixtureVERPAddresses pins.
const (
	testTenant       = "acme"
	testDelivery     = "0191f2c3-4d5e-7a8b-9c0d-1e2f3a4b5c6d"
	testDelivery2    = "0191f2c3-4d5e-7a8b-9c0d-000000000002"
	testBounceDomain = "bounce.example.com"
)

var testSecret = []byte("bounce-test-secret")

func testKeys() []store.SigningKey {
	return []store.SigningKey{{KID: "k1", Secret: testSecret, CreatedAt: time.Unix(1, 0)}}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

// TestFixtureVERPAddresses fails if the VERP addresses baked into the corpus
// stop matching what the sender would produce, which would otherwise show up
// as every VERP case quietly turning unverified.
func TestFixtureVERPAddresses(t *testing.T) {
	for _, id := range []string{testDelivery, testDelivery2} {
		addr := tracking.VERPAddress(testBounceDomain, id, testSecret)
		got, ok := tracking.ParseVERP(addr, testKeys())
		if !ok || got != id {
			t.Fatalf("VERPAddress(%s) = %q, round trip gave %q ok=%v", id, addr, got, ok)
		}
	}
}

func TestParseCorpus(t *testing.T) {
	for _, tc := range []struct {
		file       string
		typ        store.BounceType
		autoReply  bool
		confidence Confidence
		status     string
		recipient  string
		source     store.BounceSource
		deliveryID string
		tenantID   string
		verified   bool
		// diagnostic, when set, must be a substring of DiagnosticCode.
		diagnostic string
	}{
		{
			file: "postfix_hard.eml", typ: store.BounceHard, confidence: ConfidenceHigh,
			status: "5.1.1", recipient: "nosuch@example.org",
			source: store.BounceSourceVERP, deliveryID: testDelivery, verified: true,
			diagnostic: "Recipient address",
		},
		{
			// The relay rewrote the envelope sender, so only the header plant
			// is left (ADR-0008).
			file: "exim_hard.eml", typ: store.BounceHard, confidence: ConfidenceHigh,
			status: "5.1.1", recipient: "gone@example.org",
			source: store.BounceSourceHeader, deliveryID: testDelivery,
			tenantID: testTenant, verified: true,
		},
		{
			// No VERP, no X-Sendplane-ID: the returned Message-ID is the last
			// plant standing.
			file: "gmail_hard.eml", typ: store.BounceHard, confidence: ConfidenceHigh,
			status: "5.1.1", recipient: "nobody@gmail.com",
			source: store.BounceSourceMessageID, deliveryID: testDelivery, verified: true,
			diagnostic: "does not exist",
		},
		{
			// Not a DSN at all: an Outlook NDR whose text carries the code.
			file: "outlook_ndr.eml", typ: store.BounceHard, confidence: ConfidenceMedium,
			status: "5.1.1",
			source: store.BounceSourceVERP, deliveryID: testDelivery2, verified: true,
		},
		{
			file: "arf_complaint.eml", typ: store.BounceComplaint, confidence: ConfidenceHigh,
			recipient: "reader@example.org",
			source:    store.BounceSourceVERP, deliveryID: testDelivery, verified: true,
		},
		{
			file: "soft_mailbox_full.eml", typ: store.BounceSoft, confidence: ConfidenceHigh,
			status: "4.2.2", recipient: "full@example.org",
			source: store.BounceSourceVERP, deliveryID: testDelivery2, verified: true,
		},
		{
			file: "delayed.eml", typ: store.BounceSoft, confidence: ConfidenceHigh,
			status: "4.4.1", recipient: "slow@example.org",
			source: store.BounceSourceVERP, deliveryID: testDelivery, verified: true,
		},
		{
			file: "auto_reply.eml", typ: store.BounceUnknown, autoReply: true,
			confidence: ConfidenceHigh,
			source:     store.BounceSourceVERP, deliveryID: testDelivery, verified: true,
		},
		{
			file: "autoreply_korean.eml", typ: store.BounceUnknown, autoReply: true,
			confidence: ConfidenceHigh,
			source:     store.BounceSourceVERP, deliveryID: testDelivery2, verified: true,
		},
		{
			file: "unrelated.eml", typ: store.BounceUnknown, confidence: ConfidenceLow,
			source: BounceSourceNone,
		},
		{
			// A forged bounce: the delivery ID is real, the MAC is not.
			file: "forged_verp.eml", typ: store.BounceHard, confidence: ConfidenceHigh,
			status: "5.1.1", recipient: "victim@example.org",
			source: store.BounceSourceVERP, deliveryID: testDelivery, verified: false,
		},
	} {
		t.Run(tc.file, func(t *testing.T) {
			p, err := ParseWithKeys(loadFixture(t, tc.file), testKeys())
			if err != nil {
				t.Fatalf("ParseWithKeys: %v", err)
			}
			if p.Type != tc.typ {
				t.Errorf("Type = %v, want %v", p.Type, tc.typ)
			}
			if p.AutoReply != tc.autoReply {
				t.Errorf("AutoReply = %v, want %v", p.AutoReply, tc.autoReply)
			}
			if p.Confidence != tc.confidence {
				t.Errorf("Confidence = %v, want %v", p.Confidence, tc.confidence)
			}
			if tc.status != "" && p.Status != tc.status {
				t.Errorf("Status = %q, want %q", p.Status, tc.status)
			}
			if tc.recipient != "" && p.FinalRecipient != tc.recipient {
				t.Errorf("FinalRecipient = %q, want %q", p.FinalRecipient, tc.recipient)
			}
			if tc.diagnostic != "" && !strings.Contains(p.DiagnosticCode, tc.diagnostic) {
				t.Errorf("DiagnosticCode = %q, want it to contain %q", p.DiagnosticCode, tc.diagnostic)
			}
			c := p.Correlation
			if c.Source != tc.source {
				t.Errorf("Correlation.Source = %q, want %q", c.Source, tc.source)
			}
			if c.DeliveryID != tc.deliveryID {
				t.Errorf("Correlation.DeliveryID = %q, want %q", c.DeliveryID, tc.deliveryID)
			}
			if c.TenantID != tc.tenantID {
				t.Errorf("Correlation.TenantID = %q, want %q", c.TenantID, tc.tenantID)
			}
			if c.Verified != tc.verified {
				t.Errorf("Correlation.Verified = %v, want %v", c.Verified, tc.verified)
			}
		})
	}
}

// TestParseWithoutKeys covers the keyless entry point: a VERP address still
// identifies the delivery, but nothing is verified.
func TestParseWithoutKeys(t *testing.T) {
	p, err := Parse(loadFixture(t, "postfix_hard.eml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Correlation.DeliveryID != testDelivery {
		t.Fatalf("DeliveryID = %q", p.Correlation.DeliveryID)
	}
	if p.Correlation.Verified {
		t.Fatal("Parse without keys reported a verified VERP")
	}
}

func TestParseReportingMTAAndMessageID(t *testing.T) {
	p, err := ParseWithKeys(loadFixture(t, "postfix_hard.eml"), testKeys())
	if err != nil {
		t.Fatalf("ParseWithKeys: %v", err)
	}
	if p.ReportingMTA != "mail.example.net" {
		t.Errorf("ReportingMTA = %q", p.ReportingMTA)
	}
	if p.MessageID != testDelivery+"@example.com" {
		t.Errorf("MessageID = %q", p.MessageID)
	}
	if p.Date.IsZero() {
		t.Error("Date was not parsed")
	}
	if p.Action != "failed" {
		t.Errorf("Action = %q", p.Action)
	}
}

func TestParseGarbage(t *testing.T) {
	if _, err := Parse([]byte("this is not a message at all")); err == nil {
		t.Fatal("Parse accepted a non-message")
	}
}

func TestTypeFromStatus(t *testing.T) {
	for status, want := range map[string]store.BounceType{
		"5.1.1": store.BounceHard,
		"5.1.6": store.BounceHard,
		"5.2.1": store.BounceHard,
		"5.2.2": store.BounceSoft, // mailbox full
		"5.3.4": store.BounceSoft, // message too big for system
		"5.4.4": store.BounceSoft, // routing
		"5.7.1": store.BounceSoft, // policy, not a dead address
		"4.2.2": store.BounceSoft,
		"4.4.7": store.BounceSoft,
		"2.0.0": store.BounceUnknown,
		"":      store.BounceUnknown,
		"bogus": store.BounceUnknown,
	} {
		if got := typeFromStatus(status); got != want {
			t.Errorf("typeFromStatus(%q) = %v, want %v", status, got, want)
		}
	}
}
