package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Addresses and names the whole run shares. Everything lives under
// sendplane.test, the domain GreenMail accepts mail for; GreenMail creates the
// account on first delivery *and* on first login, so nothing has to be
// provisioned (test/e2e/docker-compose.yml).
const (
	mailDomain    = "sendplane.test"
	bounceAddress = "bounce@" + mailDomain
	probeAddress  = "probe@" + mailDomain
	bulkDomain    = "bulk.e2e.test"

	// The VERP return-path domain of architecture 10. Nothing resolves it;
	// the harness reads the address back out of the delivered message and
	// hands the DSN to the bounce mailbox itself.
	returnPathDomain = "bounce." + mailDomain

	// Fixed, public, test-only. The harness needs the secret to forge a VERP
	// tag that must NOT verify (scenario 5).
	trackingKID = "e2e"
	//nolint:gosec // G101: a fixed, public, test-only key; the stack is torn down after every run.
	trackingSecret = "sendplane-e2e-tracking-key-01234"

	// Where the unsubscribe redirect is supposed to land.
	hostUnsubscribeBase = "https://app.e2e.test/u"
	// The one tracked link in the template.
	offerURL = "https://app.e2e.test/offer"
)

// browserUA is what the tracking requests send. It matters: internal/api's
// bot heuristic drops an interaction whose user agent looks like a scanner,
// and Go's default "Go-http-client/1.1" is on that list (internal/api/
// tracking.go scannerUAs). A run that forgot this would see every open and
// click recorded as suspected_bot and excluded from the aggregate.
const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

// e2eMJML is the template of scenario 1: an i18n'd subject and body, a
// per-recipient Liquid substitution, one trackable link and the unsubscribe
// link. `{% t "key" %}` is the i18n tag of internal/render (ADR-0004).
const e2eMJML = `<mjml><mj-body><mj-section><mj-column>
<mj-text>{% t "greeting" name: recipient.name %}</mj-text>
<mj-text>{% t "body" %}</mj-text>
<mj-text><a href="` + offerURL + `">{% t "cta" %}</a></mj-text>
<mj-text><a href="{{ unsubscribe_url }}">{% t "unsubscribe" %}</a></mj-text>
</mj-column></mj-section></mj-body></mjml>`

// The two locales of scenario 2. The subject differs per locale, which is what
// "ko recipients received the ko subject" is checked against.
var e2eBundle = &i18nBundle{
	DefaultLocale: "en",
	Locales: map[string]map[string]string{
		"en": {
			"subject":     "sendplane e2e newsletter",
			"greeting":    "Hello {{ name }},",
			"body":        "This is the sendplane end-to-end test.",
			"cta":         "See the offer",
			"unsubscribe": "Unsubscribe",
		},
		"ko": {
			"subject":     "sendplane e2e 뉴스레터",
			"greeting":    "{{ name }}님, 안녕하세요.",
			"body":        "sendplane 종단 간 테스트입니다.",
			"cta":         "혜택 보기",
			"unsubscribe": "수신거부",
		},
	},
}

func (r *runner) scenarioBootstrap(ctx context.Context) error {
	if err := r.waitHealthy(ctx); err != nil {
		return err
	}
	if err := r.configureTenant(ctx); err != nil {
		return err
	}
	if err := r.configureSending(ctx); err != nil {
		return err
	}
	if err := r.configureMailboxes(ctx); err != nil {
		return err
	}
	// Checked here rather than in scenario 9: everything the operator's view
	// asserts depends on the tenant header being honoured, and finding that
	// out first turns "scenario 9 failed" into "the switch is not configured"
	// (ADR-0017).
	if err := r.whoamiCheck(ctx); err != nil {
		return err
	}
	return r.publishTemplate(ctx)
}

func (r *runner) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(3 * time.Minute)
	var lastAPI, lastGM, lastChaos error
	for {
		// cmd/sendplane registers a plain-text /healthz on the root mux that
		// shadows the JSON ServiceHealth, so any 200 counts.
		lastAPI = r.api.getJSON(ctx, "/healthz", nil)
		lastGM = r.gm.ready(ctx)
		_, lastChaos = fetchChaosStats(ctx, r.hc, r.opt.chaosStats)
		if lastAPI == nil && lastGM == nil && lastChaos == nil {
			r.logf("  control, GreenMail and chaos-smtp are up")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("stack never became healthy: control=%v greenmail=%v chaos-smtp=%v",
				lastAPI, lastGM, lastChaos)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// configureTenant switches on everything the run exercises: tracking with a
// known signing key, sendplane-mode unsubscribe (which also turns on the RFC
// 8058 one-click header), suppression and a retry backoff short enough that
// the 5.5% of deliveries chaos-smtp defers still finish inside the budget.
func (r *runner) configureTenant(ctx context.Context) error {
	var cur tenantSettings
	if err := r.api.getJSON(ctx, "/api/v1/settings", &cur); err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	yes := true
	secret := base64.StdEncoding.EncodeToString([]byte(trackingSecret))
	update := map[string]any{
		"version": cur.Version,
		"retry": retryPolicy{
			Backoff:     []string{"3s", "5s", "8s", "10s"},
			MaxAttempts: int32(r.opt.maxAttempts), //nolint:gosec // a flag, bounded by parseFlags
		},
		"retention_days":      7,
		"suppression_enabled": true,
		"unsubscribe_mode":    "sendplane",
		// Only meaningful under unsubscribe_mode=host; sendplane mode sets
		// List-Unsubscribe-Post itself because it owns the endpoint
		// (ADR-0011). Set anyway so the field is exercised and the run states
		// what it asked for.
		"unsubscribe_one_click": true,
		// The destination the /t/u/ redirect must land on, per recipient.
		"unsubscribe_url_template": hostUnsubscribeBase + "?e={{ recipient.email | url_encode }}",
		"default_locale":           "en",
		"tracking": trackingConfig{
			Domain:      r.opt.trackingDomain,
			Opens:       &yes,
			Clicks:      &yes,
			SigningKeys: []signingKeyInfo{{Kid: trackingKID, Secret: secret}},
		},
	}
	var got tenantSettings
	if err := r.api.putJSON(ctx, "/api/v1/settings", update, &got); err != nil {
		return fmt.Errorf("update settings: %w", err)
	}
	if got.Tracking == nil || got.Tracking.Domain != r.opt.trackingDomain {
		r.fail("tracking domain did not stick: %+v", got.Tracking)
	}
	if got.UnsubscribeMode != "sendplane" {
		r.fail("unsubscribe_mode is %q, want sendplane", got.UnsubscribeMode)
	}
	if got.SuppressionEnabled == nil || !*got.SuppressionEnabled {
		r.fail("suppression_enabled did not stick")
	}
	if got.UnsubscribeOneClick == nil || !*got.UnsubscribeOneClick {
		r.fail("unsubscribe_one_click did not stick")
	}
	r.trackingKeyKID, r.trackingKeySecret = trackingKID, []byte(trackingSecret)
	r.logf("  settings: tracking on (%s), unsubscribe_mode=%s, suppression on, max_attempts=%d",
		got.Tracking.Domain, got.UnsubscribeMode, r.opt.maxAttempts)
	return nil
}

// configureSending creates the two transports and two senders of scenario 1:
// A goes to chaos-smtp (deterministic failures, used by the bulk campaign) and
// B to GreenMail (real mailboxes, used by everything that has to read the
// message back). Only B gets a sending domain, because only B needs the VERP
// return path the bounce scenario correlates on.
func (r *runner) configureSending(ctx context.Context) error {
	var chaos idOnly
	if err := r.api.postJSON(ctx, "/api/v1/transports", transportInput{
		Name: "chaos", Host: "chaos-smtp", Port: 2525, TLS: "none", MaxConns: 32,
	}, &chaos); err != nil {
		return fmt.Errorf("create transport A: %w", err)
	}
	r.transportChaosID = chaos.ID

	var mailT idOnly
	if err := r.api.postJSON(ctx, "/api/v1/transports", transportInput{
		Name: "greenmail", Host: "greenmail", Port: 3025, TLS: "none", MaxConns: 8,
	}, &mailT); err != nil {
		return fmt.Errorf("create transport B: %w", err)
	}
	r.transportMailID = mailT.ID

	var dom idOnly
	if err := r.api.postJSON(ctx, "/api/v1/sending-domains", sendingDomainInput{
		Domain: mailDomain, ReturnPathDomain: returnPathDomain,
	}, &dom); err != nil {
		return fmt.Errorf("create sending domain: %w", err)
	}
	r.domainMailID = dom.ID

	// bulk.e2e.test is registered too, even though sender A does not need a
	// VERP return path: a sender's from_email has to be on a sending domain
	// the tenant owns, or the create is 422 from_domain_not_owned (ADR-0017).
	// Without that rule a tenant could put another tenant's domain in its
	// From address; with it, "the tenant owns this domain" is one row and the
	// only way to say so.
	if err := r.api.postJSON(ctx, "/api/v1/sending-domains", sendingDomainInput{
		Domain: bulkDomain,
	}, &idOnly{}); err != nil {
		return fmt.Errorf("create bulk sending domain: %w", err)
	}

	var sndA idOnly
	if err := r.api.postJSON(ctx, "/api/v1/senders", senderInput{
		Name: "bulk", FromName: "sendplane e2e", FromEmail: "bulk@" + bulkDomain,
		TransportID: chaos.ID,
	}, &sndA); err != nil {
		return fmt.Errorf("create sender A: %w", err)
	}
	r.senderChaosID = sndA.ID

	var sndB idOnly
	if err := r.api.postJSON(ctx, "/api/v1/senders", senderInput{
		Name: "mail", FromName: "sendplane e2e", FromEmail: "news@" + mailDomain,
		TransportID: mailT.ID, DomainID: dom.ID,
	}, &sndB); err != nil {
		return fmt.Errorf("create sender B: %w", err)
	}
	r.senderMailID = sndB.ID
	r.logf("  transports A=%s (chaos-smtp) B=%s (greenmail); senders A=%s B=%s",
		short(chaos.ID), short(mailT.ID), short(sndA.ID), short(sndB.ID))
	return nil
}

// configureMailboxes registers the bounce and probe mailboxes as tenant rows
// (architecture 10, 11.1). GreenMail has authentication disabled, so the
// stored password is never checked — it still goes through SecretCipher, which
// is the part worth exercising.
func (r *runner) configureMailboxes(ctx context.Context) error {
	var box idOnly
	if err := r.api.postJSON(ctx, "/api/v1/bounce-mailboxes", map[string]any{
		"name": "e2e-bounce", "address": bounceAddress, "protocol": "imap",
		"host": "greenmail", "port": 3143, "tls": "none",
		"username": bounceAddress, "password": "e2e",
		"folder": "INBOX", "after_process": "delete", "enabled": true,
	}, &box); err != nil {
		return fmt.Errorf("create bounce mailbox: %w", err)
	}

	var probe idOnly
	if err := r.api.postJSON(ctx, "/api/v1/probe-mailboxes", map[string]any{
		"name": "e2e-probe", "address": probeAddress,
		"host": "greenmail", "port": 3143, "tls": "none",
		"username": probeAddress, "password": "e2e",
		"inbox_folder": "INBOX",
		// GreenMail has no spam folder and adds no Authentication-Results, so
		// there is no authserv-id to trust. Scenario 6 asserts the yellow
		// verdict that follows from exactly that (internal/probe/verdict.go).
		"enabled": true,
	}, &probe); err != nil {
		return fmt.Errorf("create probe mailbox: %w", err)
	}
	r.logf("  mailboxes: bounce=%s probe=%s (both on GreenMail IMAP)", short(box.ID), short(probe.ID))
	return nil
}

func (r *runner) publishTemplate(ctx context.Context) error {
	var tpl idOnly
	if err := r.api.postJSON(ctx, "/api/v1/templates", templateInput{
		Name:          "e2e",
		Subject:       `{% t "subject" %}`,
		Mode:          "mjml",
		Body:          e2eMJML,
		I18n:          e2eBundle,
		DefaultLocale: "en",
	}, &tpl); err != nil {
		return fmt.Errorf("create template: %w", err)
	}
	r.templateID = tpl.ID

	var version messageVersion
	if err := r.api.postJSON(ctx, "/api/v1/templates/"+tpl.ID+"/publish", map[string]any{}, &version); err != nil {
		return fmt.Errorf("publish template: %w", err)
	}
	r.versionID, r.links = version.ID, version.Links
	if len(version.Links) == 0 {
		return errors.New("the published version has no trackable link; the tracking scenario cannot run")
	}
	if version.Links[0] != offerURL {
		r.fail("link_no 0 is %q, want %q", version.Links[0], offerURL)
	}
	r.logf("  published version %s with link(s) %v", short(version.ID), version.Links)
	return nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// waitForDelivery polls one delivery until it reaches one of want, and returns
// it. It is the building block of every "did it actually send" assertion.
func (r *runner) waitForDelivery(ctx context.Context, id string, timeout time.Duration, want ...string) (delivery, error) {
	deadline := time.Now().Add(timeout)
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w] = true
	}
	var d delivery
	for {
		if err := r.api.getJSON(ctx, "/api/v1/deliveries/"+id, &d); err != nil {
			return d, err
		}
		if wanted[d.Status] {
			return d, nil
		}
		if time.Now().After(deadline) {
			return d, fmt.Errorf("delivery %s is %s after %s, want one of %s (last error: %s)",
				short(id), d.Status, timeout, strings.Join(want, "/"), d.LastError)
		}
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
