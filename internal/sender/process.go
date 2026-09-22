package sender

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/liquid"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/render"
	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

// internalRetryAfter is how long a delivery waits when the sender itself could
// not do its job (store read failed, context cancelled). It goes back to
// queued without consuming an attempt: the recipient did nothing wrong.
const internalRetryAfter = 30 * time.Second

// process runs one delivery end to end and returns the result to commit. A
// zero DeliveryResult means "commit nothing": the lease was lost, so another
// replica owns the delivery now.
func (s *Sender) process(ctx context.Context, t *tenantState, d store.Delivery) store.DeliveryResult {
	started := s.cfg.Clock()

	settings, err := t.tenantSettings(ctx, started)
	if err != nil {
		return s.internalDefer(d, "tenant settings", err)
	}
	pol := s.policy(settings)

	var campaign *store.Campaign
	if d.CampaignID != "" {
		if campaign, err = t.campaign(ctx, d.CampaignID, started); err != nil {
			return s.configResult(d, "campaign", err)
		}
	}
	// A campaign delivery may carry no version of its own: a campaign created
	// from a template is only bound to a published version at start
	// (control.StartCampaign), and its rows were ingested while it was still a
	// draft. The campaign row is the single source of truth, which is what
	// keeps start a one-row write instead of a backfill over a million rows.
	versionID := d.VersionID
	if versionID == "" && campaign != nil {
		versionID = campaign.VersionID
	}
	version, err := t.version(ctx, versionID, started)
	if err != nil {
		return s.configResult(d, "message version", err)
	}
	snd, err := t.sender(ctx, d.SenderID, started)
	if err != nil {
		return s.configResult(d, "sender", err)
	}
	// A shared sender's transport and sending domain are the operator's, and
	// only the system tenant's view can see them (store.PlatformView): a
	// tenant must not be able to read the relay's host or the platform
	// domain's DKIM key (ADR-0017).
	transport, err := s.transportFor(ctx, t, snd.TransportID, started)
	if err != nil {
		return s.configResult(d, "transport", err)
	}
	domain, err := s.domainFor(ctx, t, snd.DomainID, started)
	if err != nil && !isNotFound(err) {
		return s.internalDefer(d, "sending domain", err)
	}

	// Suppression (ADR-0008). Probe traffic bypasses it by design (ADR-0012).
	if settings.SuppressionEnabled && d.Lane != store.LaneProbe {
		suppressed, _, err := t.st.Suppressions().IsSuppressed(ctx, d.EmailNorm, started)
		if err != nil {
			return s.internalDefer(d, "suppression lookup", err)
		}
		if !suppressed {
			// A send through a shared relay also checks the platform list: a
			// hard bounce anybody on the relay collected is a hard bounce for
			// everybody on it.
			suppressed, err = s.platformSuppressed(ctx, transport, d.EmailNorm, started)
			if err != nil {
				return s.internalDefer(d, "platform suppression lookup", err)
			}
		}
		if suppressed {
			return store.DeliveryResult{
				DeliveryID: d.ID, LeaseOwner: s.cfg.WorkerID,
				NewStatus: store.DeliverySuppressed, Error: "address is suppressed",
			}
		}
	}

	if !s.transportUsable(t, transport, started) {
		return store.DeliveryResult{
			DeliveryID: d.ID, LeaseOwner: s.cfg.WorkerID,
			NewStatus:     store.DeliveryQueued,
			NextAttemptAt: started.Add(pol.AuthRetryAfter),
			ErrorClass:    store.ErrorClassAuth,
			Error:         FailureReason(transport.Shared, "transport is unhealthy"),
		}
	}

	msg, unsub, err := s.renderMessage(ctx, t, settings, campaign, version, snd, &d)
	if err != nil {
		if ctx.Err() != nil {
			return s.internalDefer(d, "render", err)
		}
		return s.failResult(d, store.ErrorClassPermanent, 0, err.Error(), started)
	}

	if s.cfg.Hooks.BeforeSend != nil {
		if err := s.cfg.Hooks.BeforeSend(ctx, msg); err != nil {
			if errors.Is(err, host.ErrSkip) {
				return store.DeliveryResult{
					DeliveryID: d.ID, LeaseOwner: s.cfg.WorkerID,
					NewStatus: store.DeliverySuppressed, Error: "skipped by BeforeSend",
				}
			}
			// A failing hook is the host's problem, which is usually
			// temporary: retry on the transient schedule rather than throwing
			// the delivery away, but do consume attempts so it cannot loop.
			return s.decide(d, pol, Failure{
				Class: store.ErrorClassTransient, Message: "BeforeSend: " + err.Error(),
			}, started, transport.ID)
		}
	}

	attemptNo := d.AttemptCount + 1
	messageID := d.ID + "@" + domainOf(msg.From)
	raw, err := buildMessage(messageInput{
		msg:             msg,
		messageID:       messageID,
		attemptNo:       attemptNo,
		date:            started,
		listUnsubscribe: unsub.header,
		oneClick:        unsub.oneClick,
		dkim:            s.dkimFor(ctx, t, domain),
	})
	if err != nil {
		// Header injection and an unbuildable message are permanent: the same
		// inputs will produce the same failure on every retry.
		return s.failResult(d, store.ErrorClassPermanent, 0, err.Error(), started)
	}

	// Envelope sender: VERP when the sending domain has a bounce domain and
	// the tenant has a signing key, otherwise the From address
	// (architecture 10).
	returnPath := msg.From
	if key, ok := tracking.SelectKey(settings.Tracking.SigningKeys); ok && domain != nil {
		if v := tracking.VERPAddress(domain.ReturnPathDomain, d.ID, key.Secret); v != "" {
			returnPath = v
		}
	}

	rate := transport.RatePerSecond
	if rate <= 0 {
		rate = s.cfg.DefaultRatePerSecond
	}
	rcptDomain := domainOf(d.Email)
	// A shared transport's buckets are keyed under the system tenant, not this
	// one: the configured rate is the whole relay's capacity, and one bucket
	// per tenant would silently multiply it (architecture 8.2).
	transportKey := sharedTransportKey(t.id, transport)
	domainKey := transportKey + "|" + rcptDomain
	keys := []Key{
		{Name: transportKey, Rate: rate},
		{Name: domainKey, Rate: transport.DomainRatePerSecond[rcptDomain]},
	}
	// The fair share: one tenant's slice of a shared relay, so that a single
	// campaign cannot consume the capacity everybody else needs.
	if perTenant := s.perTenantRate(transport); perTenant > 0 {
		keys = append(keys, Key{Name: tenantBucket(transportKey, t.id), Rate: perTenant})
	}
	waited, err := s.limiter.Wait(ctx, keys...)
	if waited > 0 {
		s.cfg.Metrics.Observe(MetricLimiterWait, waited.Seconds(), "transport", transport.ID)
	}
	if err != nil {
		return s.internalDefer(d, "rate limiter", err)
	}

	password, err := s.password(ctx, t, transport)
	if err != nil {
		return s.internalDefer(d, "transport password", err)
	}

	sendStart := s.cfg.Clock()
	var f Failure
	conn, err := s.pool.Get(ctx, transport, password)
	if err != nil {
		if ctx.Err() != nil {
			return s.internalDefer(d, "connect", err)
		}
		f = Classify(err)
	} else {
		sendErr := conn.Send(returnPath, []string{d.Email}, raw, s.cfg.SendTimeout)
		f = Classify(sendErr)
		s.pool.Put(conn, Reusable(f))
	}
	finished := s.cfg.Clock()
	s.cfg.Metrics.Observe(MetricSendTime, finished.Sub(sendStart).Seconds(),
		"transport", transport.ID, "class", f.Class.String())

	// The circuit is scoped like the rate buckets: a shared relay has one
	// status for the whole cluster, stored in the system tenant's shadow row.
	scope := limitScope(t.id, transport)
	switch f.Class {
	case store.ErrorClassNone:
		if status, changed := s.health.success(scope, transport.ID, finished); changed {
			s.setTransportStatus(ctx, t, transport, status, "delivery succeeded", s.statusUntil(status, finished))
		}
	case store.ErrorClassRateLimited:
		s.limiter.Penalize(transportKey, domainKey)
		if status, changed := s.health.fail(scope, transport.ID, f.Class, finished); changed {
			s.setTransportStatus(ctx, t, transport, status, f.Message, s.statusUntil(status, finished))
		}
	case store.ErrorClassAuth:
		if status, changed := s.health.fail(scope, transport.ID, f.Class, finished); changed {
			s.setTransportStatus(ctx, t, transport, status, f.Message, s.statusUntil(status, finished))
		}
	}

	res := s.decide(d, pol, f, finished, transport.ID)
	res.MessageID = messageID
	if res.Attempt != nil {
		res.Attempt.StartedAt = sendStart
		res.Attempt.FinishedAt = finished
	}

	if f.Class == store.ErrorClassNone {
		// The fast path of architecture 8.1: record "sent" immediately, so a
		// crash before the batched Complete costs at most a duplicate attempt
		// record, never a second send.
		if err := t.st.Deliveries().MarkSent(ctx, d.ID, s.cfg.WorkerID, messageID, finished); err != nil {
			if errors.Is(err, store.ErrLeaseLost) {
				// Another replica owns this delivery now; its result wins.
				s.cfg.Logger.Warn("sendplane: lease lost after a successful send",
					"delivery", d.ID, "worker", s.cfg.WorkerID)
				return store.DeliveryResult{}
			}
			s.cfg.Logger.Error("sendplane: MarkSent failed", "delivery", d.ID, "err", err)
		}
	}
	return res
}

// unsubscribeLinks is what the unsubscribe mode produced.
type unsubscribeLinks struct {
	// body is what {{ unsubscribe_url }} renders to.
	body string
	// header is the List-Unsubscribe URI, empty when there is none.
	header string
	// oneClick adds List-Unsubscribe-Post (RFC 8058).
	oneClick bool
}

// renderMessage resolves the unsubscribe destination, renders the three parts
// for this recipient and applies the tracking transforms of architecture 9.2.
func (s *Sender) renderMessage(
	ctx context.Context,
	t *tenantState,
	settings *store.TenantSettings,
	campaign *store.Campaign,
	version *store.MessageVersion,
	snd *store.Sender,
	d *store.Delivery,
) (*host.OutboundMessage, unsubscribeLinks, error) {
	rc := host.RecipientContext{
		TenantID: t.id, CampaignID: d.CampaignID, DeliveryID: d.ID,
		Email: d.Email, EmailNorm: d.EmailNorm, Name: d.Name,
		Locale: d.Locale, Vars: d.Vars,
	}
	// The tenant attributes this send was requested with. They are the
	// `tenant` binding in the message templates and, for a shared sender, what
	// its From templates resolve from (ADR-0017).
	tenantVars := tenantVarsFor(d, campaign)
	if d.Lane == store.LaneProbe && len(tenantVars) == 0 {
		// A platform sender is probed once, in the system tenant, and its
		// From templates still have to resolve: the configured ProbeVars are
		// what they resolve from (architecture 11.2).
		tenantVars = s.probeVarsFor(snd.ID)
	}
	from, err := s.resolveFrom(snd, tenantVars)
	if err != nil {
		return nil, unsubscribeLinks{}, err
	}
	// A probe mail is a health check, not a message to a subscriber: it takes
	// no part in statistics, tracking or unsubscribe (architecture 11.2,
	// ADR-0012). Rewriting its links or pixelling it would file open and click
	// events against a mailbox sendplane owns, and an unsubscribe header on it
	// is meaningless.
	tracked := d.Lane != store.LaneProbe

	var unsub unsubscribeLinks
	var key store.SigningKey
	var hasKey bool
	domain := settings.Tracking.Domain
	if tracked {
		dest, err := s.unsubscribeDest(ctx, t, settings, d, campaign, rc)
		if err != nil {
			return nil, unsubscribeLinks{}, err
		}
		key, hasKey = tracking.SelectKey(settings.Tracking.SigningKeys)
		unsub = s.unsubscribeLinks(settings, t.id, d.ID, dest, key, hasKey, domain)
	}

	locales := []string{d.Locale}
	if campaign != nil {
		locales = append(locales, campaign.DefaultLocale)
	}
	prepared, err := s.cfg.Renderer.PrepareChain(version, locales...)
	if err != nil {
		return nil, unsub, err
	}

	b := render.Bindings{
		Recipient: render.Recipient{
			Email: d.Email, Name: d.Name, Locale: d.Locale, Vars: d.Vars,
		},
		TenantVars:     tenantVars,
		UnsubscribeURL: unsub.body,
	}
	if campaign != nil {
		b.Vars = campaign.Vars
		b.Campaign = render.Campaign{ID: campaign.ID, Name: campaign.Name}
	}

	renderCtx, cancel := context.WithTimeout(ctx, s.cfg.RenderTimeout)
	defer cancel()
	start := s.cfg.Clock()
	out, warnings, err := prepared.Render(renderCtx, b)
	s.cfg.Metrics.Observe(MetricRenderTime, s.cfg.Clock().Sub(start).Seconds())
	if err != nil {
		return nil, unsub, err
	}
	for _, w := range warnings {
		s.cfg.Logger.Debug("sendplane: render warning",
			"delivery", d.ID, "warning", fmt.Sprint(w))
	}

	html := out.HTML
	if tracked && hasKey && domain != "" {
		if settings.Tracking.Clicks {
			html, err = render.RewriteLinks(html, func(linkNo int, href string) string {
				token := s.signer.Sign(key.KID, key.Secret, tracking.TokenPayload{
					TenantID: t.id, DeliveryID: d.ID, Kind: tracking.KindClick, LinkNo: linkNo, Dest: href,
				})
				if u := tracking.ClickURL(domain, token, href); u != "" {
					return u
				}
				return href
			})
			if err != nil {
				return nil, unsub, err
			}
		}
		if settings.Tracking.Opens {
			token := s.signer.Sign(key.KID, key.Secret, tracking.TokenPayload{
				TenantID: t.id, DeliveryID: d.ID, Kind: tracking.KindOpen,
			})
			html = render.InsertPixel(html, tracking.OpenURL(domain, token))
		}
	}

	return &host.OutboundMessage{
		TenantID: t.id, DeliveryID: d.ID, CampaignID: d.CampaignID,
		VersionID: version.ID, SenderID: d.SenderID, Lane: d.Lane,
		Recipient: rc,
		FromName:  from.Name, From: from.Email, ReplyTo: from.ReplyTo,
		Subject: out.Subject, HTML: html, Text: out.Text,
		Headers:        outboundHeaders(d),
		UnsubscribeURL: unsub.body,
	}, unsub, nil
}

// outboundHeaders is the caller-supplied header set: what POST /messages put
// on the delivery, plus the probe token on a lane=probe mail. Both go through
// the same whitelist as a Hooks.BeforeSend header (validateOutbound), so a
// name that is not allowed fails the delivery instead of reaching the wire.
func outboundHeaders(d *store.Delivery) map[string]string {
	out := make(map[string]string, len(d.Headers)+1)
	for name, value := range d.Headers {
		out[name] = value
	}
	if tok, ok := d.Vars["probe_token"].(string); ok && tok != "" {
		out[HeaderProbe] = tok
	}
	return out
}

// unsubscribeLinks applies the mode of architecture 9.2.
func (s *Sender) unsubscribeLinks(
	settings *store.TenantSettings, tenantID, deliveryID, dest string,
	key store.SigningKey, hasKey bool, domain string,
) unsubscribeLinks {
	switch settings.UnsubscribeMode {
	case store.UnsubscribeSendplane:
		if dest == "" {
			return unsubscribeLinks{}
		}
		if hasKey && domain != "" {
			token := s.signer.Sign(key.KID, key.Secret, tracking.TokenPayload{
				TenantID: tenantID, DeliveryID: deliveryID, Kind: tracking.KindUnsubscribe, Dest: dest,
			})
			if u := tracking.UnsubscribeURL(domain, token); u != "" {
				// RFC 8058 one-click requires an https endpoint.
				return unsubscribeLinks{body: u, header: u, oneClick: strings.HasPrefix(u, "https://")}
			}
		}
		// No tracking domain or no key: ship the host URL rather than a dead
		// link, and record the misconfiguration through the missing tracking.
		return unsubscribeLinks{body: dest, header: dest}

	case store.UnsubscribeHost:
		if dest == "" {
			return unsubscribeLinks{}
		}
		// The host owns the endpoint here, so sendplane only announces
		// one-click when the tenant has declared that it accepts an RFC 8058
		// POST. Announcing it on an endpoint that answers a POST with a login
		// page makes mailbox providers record the unsubscribe as failed
		// (ADR-0011). RFC 8058 also requires https.
		return unsubscribeLinks{
			body: dest, header: dest,
			oneClick: settings.UnsubscribeOneClick && strings.HasPrefix(dest, "https://"),
		}

	default: // UnsubscribeNone
		return unsubscribeLinks{}
	}
}

// unsubscribeDest resolves the host destination in the order of
// architecture 9.2: recipient variable, tenant URL template, hook.
func (s *Sender) unsubscribeDest(
	ctx context.Context, t *tenantState, settings *store.TenantSettings,
	d *store.Delivery, campaign *store.Campaign, rc host.RecipientContext,
) (string, error) {
	if settings.UnsubscribeMode == store.UnsubscribeNone {
		return "", nil
	}
	if d.UnsubscribeURL != "" {
		return d.UnsubscribeURL, nil
	}
	if v, ok := d.Vars["unsubscribe_url"].(string); ok && v != "" {
		return v, nil
	}
	if tpl := strings.TrimSpace(settings.UnsubscribeURLTemplate); tpl != "" {
		url, err := s.renderUnsubscribeTemplate(t, settings, tpl, d, campaign)
		if err != nil {
			return "", err
		}
		if url != "" {
			return url, nil
		}
	}
	if s.cfg.Hooks.UnsubscribeURL != nil {
		return s.cfg.Hooks.UnsubscribeURL(ctx, rc)
	}
	return "", nil
}

// renderUnsubscribeTemplate evaluates the tenant's Liquid URL template. The
// parsed template is cached under the settings version so a rotation picks the
// new one up.
func (s *Sender) renderUnsubscribeTemplate(
	t *tenantState, settings *store.TenantSettings, tpl string,
	d *store.Delivery, campaign *store.Campaign,
) (string, error) {
	key := strconv.FormatInt(settings.Version, 10) + "\x00" + tpl
	parsed, err := t.unsubTpl.get(key, s.cfg.Clock(), func() (*liquid.Template, error) {
		return render.NewEngine().Parse(tpl)
	})
	if err != nil {
		return "", fmt.Errorf("sender: unsubscribe url template: %w", err)
	}
	bindings := map[string]any{
		"recipient": map[string]any{
			"email": d.Email, "name": d.Name, "locale": d.Locale, "vars": d.Vars,
		},
		"delivery_id": d.ID,
		"tenant_id":   t.id,
		"vars":        d.Vars,
	}
	if campaign != nil {
		bindings["campaign"] = map[string]any{"id": campaign.ID, "name": campaign.Name}
	} else {
		bindings["campaign"] = map[string]any{"id": "", "name": ""}
	}
	out, err := parsed.Render(bindings)
	if err != nil {
		return "", fmt.Errorf("sender: unsubscribe url template: %w", err)
	}
	url := strings.TrimSpace(string(out))
	if strings.ContainsAny(url, "\r\n") {
		return "", fmt.Errorf("%w: unsubscribe url", ErrHeaderInjection)
	}
	return url, nil
}

// policy builds the retry policy for a tenant.
func (s *Sender) policy(settings *store.TenantSettings) Policy {
	p := DefaultPolicy(settings.Retry)
	if s.cfg.RateLimitCap > 0 {
		p.RateLimitCap = s.cfg.RateLimitCap
	}
	if s.cfg.AuthRetryAfter > 0 {
		p.AuthRetryAfter = s.cfg.AuthRetryAfter
	}
	p.Rand = s.cfg.Rand
	return p
}

// decide turns a classified failure into the result to commit, including the
// DeliveryAttempt row.
func (s *Sender) decide(
	d store.Delivery, pol Policy, f Failure, now time.Time, transportID string,
) store.DeliveryResult {
	dec := pol.Decide(f.Class, d.AttemptCount, now)
	res := store.DeliveryResult{
		DeliveryID:       d.ID,
		LeaseOwner:       s.cfg.WorkerID,
		NewStatus:        dec.Status,
		NextAttemptAt:    dec.NextAttemptAt,
		ErrorClass:       f.Class,
		SMTPCode:         f.Code,
		Error:            f.Message,
		IncrementAttempt: dec.IncrementAttempt,
	}
	if !dec.RecordAttempt {
		return res
	}
	no := d.AttemptCount
	if dec.IncrementAttempt {
		no++
	}
	res.Attempt = &store.DeliveryAttempt{
		TenantID:     d.TenantID,
		DeliveryID:   d.ID,
		AttemptNo:    no,
		RetryGen:     d.RetryGen,
		TransportID:  transportID,
		StartedAt:    now,
		FinishedAt:   now,
		SMTPCode:     f.Code,
		EnhancedCode: f.Enhanced,
		ErrorClass:   f.Class,
		Error:        f.Message,
	}
	return res
}

// failResult is a terminal failure that never reached SMTP (render, MIME,
// header injection).
func (s *Sender) failResult(
	d store.Delivery, class store.ErrorClass, code int, msg string, now time.Time,
) store.DeliveryResult {
	return store.DeliveryResult{
		DeliveryID: d.ID, LeaseOwner: s.cfg.WorkerID,
		NewStatus: store.DeliveryFailed, ErrorClass: class, SMTPCode: code, Error: msg,
		Attempt: &store.DeliveryAttempt{
			TenantID: d.TenantID, DeliveryID: d.ID,
			AttemptNo: d.AttemptCount, RetryGen: d.RetryGen,
			StartedAt: now, FinishedAt: now,
			ErrorClass: class, Error: msg, SMTPCode: code,
		},
	}
}

// internalDefer puts a delivery back in the queue without consuming an
// attempt: the sender, not the delivery, is what failed.
func (s *Sender) internalDefer(d store.Delivery, what string, err error) store.DeliveryResult {
	s.cfg.Logger.Warn("sendplane: requeueing delivery", "delivery", d.ID, "stage", what, "err", err)
	return store.DeliveryResult{
		DeliveryID: d.ID, LeaseOwner: s.cfg.WorkerID,
		NewStatus:     store.DeliveryQueued,
		NextAttemptAt: s.cfg.Clock().Add(internalRetryAfter),
		Error:         what + ": " + err.Error(),
	}
}

// configResult handles a missing configuration row: a deleted campaign,
// version, sender or transport can never be sent, so the delivery fails.
func (s *Sender) configResult(d store.Delivery, what string, err error) store.DeliveryResult {
	if !isNotFound(err) {
		return s.internalDefer(d, what, err)
	}
	return s.failResult(d, store.ErrorClassPermanent, 0,
		what+" not found: "+err.Error(), s.cfg.Clock())
}

// password decrypts and caches a transport password. Without a SecretCipher
// the stored bytes are used as-is, which is what a host that keeps its own
// secrets outside sendplane wants.
func (s *Sender) password(ctx context.Context, t *tenantState, tr *store.Transport) (string, error) {
	if len(tr.Password) == 0 {
		return "", nil
	}
	key := tr.ID + "/" + strconv.FormatInt(tr.Version, 10)
	return t.passwords.get(key, s.cfg.Clock(), func() (string, error) {
		if s.cfg.Secrets == nil {
			return string(tr.Password), nil
		}
		plain, err := s.cfg.Secrets.Decrypt(ctx, tr.Password)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	})
}

// dkimFor returns the parsed signing key of a sending domain, or nil when the
// domain has none (the relay signs) or the key cannot be used.
func (s *Sender) dkimFor(ctx context.Context, t *tenantState, dom *store.SendingDomain) *dkimKey {
	if dom == nil || dom.DKIMSelector == "" || len(dom.DKIMPrivateKey) == 0 {
		return nil
	}
	key := dom.ID + "/" + strconv.FormatInt(dom.Version, 10)
	k, err := t.dkimKeys.get(key, s.cfg.Clock(), func() (*dkimKey, error) {
		raw := dom.DKIMPrivateKey
		if s.cfg.Secrets != nil {
			plain, err := s.cfg.Secrets.Decrypt(ctx, raw)
			if err != nil {
				return nil, err
			}
			raw = plain
		}
		signer, err := parseDKIMKey(raw)
		if err != nil {
			return nil, err
		}
		return &dkimKey{domain: dom.Domain, selector: dom.DKIMSelector, signer: signer}, nil
	})
	if err != nil {
		s.cfg.Logger.Error("sendplane: DKIM key unusable, sending unsigned",
			"domain", dom.Domain, "err", err)
		return nil
	}
	return k
}

// domainOf returns the domain part of an address.
func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 && i < len(addr)-1 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}
