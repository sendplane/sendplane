package main

import (
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/bounce"
	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

// The synthesized reports of dsn.go are the one part of the harness that can
// be wrong without the stack noticing: a DSN the parser quietly classifies as
// "not a bounce" is indistinguishable, from the outside, from a bounce path
// that does not work. These tests run internal/bounce's real parser over the
// exact bytes scenario 5 injects, so a fixture that stops correlating fails in
// `go test ./test/e2e` instead of a twelve-minute compose run.

const (
	testDeliveryID = "0191f2c3-4d5e-7a8b-9c0d-1e2f3a4b5c6d"
	testTenant     = "default"
)

func testKeys() []store.SigningKey {
	return []store.SigningKey{{KID: trackingKID, Secret: []byte(trackingSecret)}}
}

func testOriginal(verp string) bouncedOriginal {
	return bouncedOriginal{
		ReturnPath:  verp,
		From:        "sendplane e2e <news@" + mailDomain + ">",
		To:          "bounce-hard@" + mailDomain,
		Subject:     "sendplane e2e newsletter",
		MessageID:   "<" + testDeliveryID + "@" + mailDomain + ">",
		SendplaneID: testTenant + "/" + testDeliveryID,
		Date:        rfc5322Date(time.Now()),
	}
}

func TestHardDSNParsesAndCorrelates(t *testing.T) {
	verp := tracking.VERPAddress(returnPathDomain, testDeliveryID, []byte(trackingSecret))
	if verp == "" {
		t.Fatal("VERPAddress returned empty")
	}
	raw := hardDSN(testOriginal(verp), verp, time.Now())

	parsed, err := bounce.ParseWithKeys(raw, testKeys())
	if err != nil {
		t.Fatalf("ParseWithKeys: %v", err)
	}
	if parsed.AutoReply {
		t.Error("the DSN was classified as an auto-reply; the processor would drop it")
	}
	if !parsed.IsBounce() {
		t.Fatalf("IsBounce() is false; type=%q", parsed.Type)
	}
	if parsed.Type != store.BounceHard {
		t.Errorf("type = %q, want hard", parsed.Type)
	}
	if got := parsed.Correlation.DeliveryID; got != testDeliveryID {
		t.Errorf("correlated delivery = %q, want %q", got, testDeliveryID)
	}
	if !parsed.Correlation.Verified {
		t.Error("a DSN carrying a genuine VERP tag came back unverified")
	}
}

func TestARFParsesAsComplaint(t *testing.T) {
	verp := tracking.VERPAddress(returnPathDomain, testDeliveryID, []byte(trackingSecret))
	raw := arfComplaint(testOriginal(verp), verp, time.Now())

	parsed, err := bounce.ParseWithKeys(raw, testKeys())
	if err != nil {
		t.Fatalf("ParseWithKeys: %v", err)
	}
	if parsed.Type != store.BounceComplaint {
		t.Errorf("type = %q, want complaint", parsed.Type)
	}
	if got := parsed.Correlation.DeliveryID; got != testDeliveryID {
		t.Errorf("correlated delivery = %q, want %q", got, testDeliveryID)
	}
	if !parsed.Correlation.Verified {
		t.Error("a complaint carrying a genuine VERP tag came back unverified")
	}
}

func TestForgedDSNCorrelatesButDoesNotVerify(t *testing.T) {
	verp := tracking.VERPAddress(returnPathDomain, testDeliveryID, []byte(trackingSecret))
	forged := forgeVERP(verp)
	if forged == verp {
		t.Fatalf("forgeVERP did not change %q", verp)
	}
	o := testOriginal(forged)
	raw := hardDSN(o, forged, time.Now())

	parsed, err := bounce.ParseWithKeys(raw, testKeys())
	if err != nil {
		t.Fatalf("ParseWithKeys: %v", err)
	}
	if parsed.Correlation.Verified {
		t.Error("a forged VERP tag verified")
	}
	if got := parsed.Correlation.DeliveryID; got != testDeliveryID {
		t.Errorf("correlated delivery = %q, want %q (the id is still readable, only the tag is wrong)",
			got, testDeliveryID)
	}
}
