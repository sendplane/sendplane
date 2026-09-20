package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
)

func TestWebhookSinkSignsBody(t *testing.T) {
	const secret = "whsec_test"
	var gotBody []byte
	var gotSig string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		gotSig = r.Header.Get("X-Sendplane-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newWebhookSink(WebhookConfig{
		URL:                  srv.URL,
		Secret:               secret,
		Timeout:              Duration(5 * time.Second),
		AllowPrivateNetworks: true, // httptest servers bind to 127.0.0.1
	}, nil)

	events := []host.Event{{ID: "evt_1", TenantID: "acme", Type: "delivery.sent", OccurredAt: time.Now(), Payload: json.RawMessage(`{"a":1}`)}}
	if err := sink.Emit(context.Background(), events); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	want := hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("X-Sendplane-Signature = %q, want %q", gotSig, want)
	}

	var decoded []host.Event
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body did not decode as []host.Event: %v", err)
	}
	if len(decoded) != 1 || decoded[0].ID != "evt_1" {
		t.Errorf("decoded events = %+v", decoded)
	}
}

func TestWebhookSinkRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink := newWebhookSink(WebhookConfig{URL: srv.URL, Secret: "s", AllowPrivateNetworks: true}, nil)
	if err := sink.Emit(context.Background(), []host.Event{{ID: "e"}}); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

func TestWebhookSinkSSRFRejectsLoopbackByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// AllowPrivateNetworks defaults to false: httptest.NewServer listens on
	// 127.0.0.1, so the dial must be refused before any request reaches it.
	sink := newWebhookSink(WebhookConfig{URL: srv.URL, Secret: "s"}, nil)
	err := sink.Emit(context.Background(), []host.Event{{ID: "e"}})
	if err == nil {
		t.Fatal("expected the SSRF guard to reject a loopback destination")
	}
}

func TestWebhookSinkSSRFAllowedWhenConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newWebhookSink(WebhookConfig{URL: srv.URL, Secret: "s", AllowPrivateNetworks: true}, nil)
	if err := sink.Emit(context.Background(), []host.Event{{ID: "e"}}); err != nil {
		t.Fatalf("Emit with allow_private_networks=true: %v", err)
	}
}

func TestGuardPublicAddrRejectsPrivateRanges(t *testing.T) {
	rejected := []string{
		"127.0.0.1:443",
		"10.0.0.5:443",
		"172.16.0.5:443",
		"192.168.1.5:443",
		"169.254.1.1:443",
		"0.0.0.0:443",
	}
	for _, addr := range rejected {
		if err := guardPublicAddr("tcp", addr); err == nil {
			t.Errorf("guardPublicAddr(%q) = nil, want an error", addr)
		}
	}
}

func TestGuardPublicAddrAllowsPublicIP(t *testing.T) {
	if err := guardPublicAddr("tcp", "93.184.216.34:443"); err != nil {
		t.Errorf("guardPublicAddr(public IP) = %v, want nil", err)
	}
}

func TestEmitIsNoOpWithoutURL(t *testing.T) {
	sink := newWebhookSink(WebhookConfig{}, nil)
	if err := sink.Emit(context.Background(), []host.Event{{ID: "e"}}); err != nil {
		t.Errorf("Emit with no URL configured = %v, want nil (no-op)", err)
	}
}
