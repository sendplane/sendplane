package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sendplane/sendplane/host"
)

// webhookSink implements host.EventSink by POSTing the batch as JSON to a
// single URL, signed with HMAC-SHA256 over the body (architecture 12, 16).
//
// It is the reference binary's Go-level default (Hooks.Events); a tenant can
// still configure its own webhook URL through settings, which is handled
// inside internal/control's outbox and does not go through this type.
type webhookSink struct {
	url    string
	secret []byte
	client *http.Client
	logger *slog.Logger
}

var _ host.EventSink = (*webhookSink)(nil)

// newWebhookSink builds a sink that POSTs to cfg.URL. When
// allowPrivateNetworks is false, every dial is checked against RFC 1918 /
// loopback / link-local ranges right before the connection is made (not
// against a DNS lookup done earlier), which is what closes the DNS-rebinding
// TOCTOU window: net.Dialer.Control receives the address actually being
// dialed, after resolution.
func newWebhookSink(cfg WebhookConfig, logger *slog.Logger) *webhookSink {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !cfg.AllowPrivateNetworks {
				if err := guardPublicAddr(network, addr); err != nil {
					return nil, err
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	timeout := time.Duration(cfg.Timeout)
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &webhookSink{
		url:    cfg.URL,
		secret: []byte(cfg.Secret),
		client: &http.Client{Transport: transport, Timeout: timeout},
		logger: logger,
	}
}

// guardPublicAddr rejects addr (host:port, host already resolved to an IP by
// the time the dialer's Control hook sees it) when it names a loopback,
// private, link-local, unspecified or multicast address.
func guardPublicAddr(network, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("webhook: %s did not resolve to an IP literal before dial", addr)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("webhook: %s (%s) is a private/loopback address; set events.webhook.allow_private_networks to allow it", addr, network)
	}
	return nil
}

func (s *webhookSink) Emit(ctx context.Context, events []host.Event) error {
	if s.url == "" {
		return nil
	}
	body, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("webhook: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sendplane-Signature", signBody(s.secret, body))

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: %s returned %d", s.url, resp.StatusCode)
	}
	if s.logger != nil {
		s.logger.Debug("webhook: delivered", "count", len(events), "status", resp.StatusCode)
	}
	return nil
}

// signBody returns the hex-encoded HMAC-SHA256 of body under secret, the
// value sent in X-Sendplane-Signature.
func signBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
