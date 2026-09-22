package api

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/sendplane/sendplane/internal/sender"
	"github.com/sendplane/sendplane/store"
)

// --- transports --------------------------------------------------------

func (s *server) ListTransports(ctx context.Context, req ListTransportsRequestObject) (ListTransportsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.Transports().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]Transport, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, transportOut(&res.Items[i]))
	}
	return ListTransports200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) CreateTransport(ctx context.Context, req CreateTransportRequestObject) (CreateTransportResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	tr := &store.Transport{}
	if err := s.applyTransport(ctx, tr, TransportUpdate{
		Name: req.Body.Name, Host: req.Body.Host, Port: req.Body.Port, Tls: req.Body.Tls,
		Username: req.Body.Username, Password: req.Body.Password, MaxConns: req.Body.MaxConns,
		RatePerSecond: req.Body.RatePerSecond, DomainRatePerSecond: req.Body.DomainRatePerSecond,
	}); err != nil {
		return nil, err
	}
	tr.Status = store.TransportHealthy
	if err := t.st.Transports().Create(ctx, tr); err != nil {
		return nil, err
	}
	return CreateTransport201JSONResponse(transportOut(tr)), nil
}

func (s *server) GetTransport(ctx context.Context, req GetTransportRequestObject) (GetTransportResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := t.st.Transports().Get(ctx, req.TransportId)
	if err != nil {
		return nil, err
	}
	return GetTransport200JSONResponse(transportOut(tr)), nil
}

func (s *server) UpdateTransport(ctx context.Context, req UpdateTransportRequestObject) (UpdateTransportResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("transport", req.TransportId); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	tr, err := t.st.Transports().Get(ctx, req.TransportId)
	if err != nil {
		return nil, err
	}
	if err := s.applyTransport(ctx, tr, *req.Body); err != nil {
		return nil, err
	}
	tr.Version = req.Body.Version
	if err := t.st.Transports().Update(ctx, tr); err != nil {
		return nil, err
	}
	return UpdateTransport200JSONResponse(transportOut(tr)), nil
}

func (s *server) DeleteTransport(ctx context.Context, req DeleteTransportRequestObject) (DeleteTransportResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("transport", req.TransportId); err != nil {
		return nil, err
	}
	if err := t.st.Transports().Delete(ctx, req.TransportId); err != nil {
		return nil, err
	}
	return DeleteTransport204Response{}, nil
}

func (s *server) GetTransportHealth(ctx context.Context, req GetTransportHealthRequestObject) (GetTransportHealthResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := t.st.Transports().Get(ctx, req.TransportId)
	if err != nil {
		return nil, err
	}
	return GetTransportHealth200JSONResponse(TransportHealth{
		TransportId: rid(tr.ID),
		Status:      transportStatusOut(tr.Status),
		Reason:      strPtr(tr.StatusReason),
		ChangedAt:   timePtr(tr.StatusChangedAt),
	}), nil
}

func (s *server) applyTransport(ctx context.Context, tr *store.Transport, in TransportUpdate) error {
	if err := requireNonEmpty("name", in.Name); err != nil {
		return err
	}
	if err := requireNonEmpty("host", in.Host); err != nil {
		return err
	}
	if in.Port <= 0 || in.Port > 65535 {
		return errInvalid("port must be between 1 and 65535")
	}
	mode, err := tlsModeIn(in.Tls)
	if err != nil {
		return err
	}
	pw, err := s.secret(ctx, in.Password, tr.Password)
	if err != nil {
		return err
	}
	tr.Name, tr.Host, tr.Port, tr.TLS = in.Name, in.Host, int(in.Port), mode
	tr.Username = deref(in.Username)
	tr.Password = pw
	tr.MaxConns = int(deref(in.MaxConns))
	tr.RatePerSecond = deref(in.RatePerSecond)
	tr.DomainRatePerSecond = nil
	if in.DomainRatePerSecond != nil {
		tr.DomainRatePerSecond = *in.DomainRatePerSecond
	}
	return nil
}

func transportOut(v *store.Transport) Transport {
	out := Transport{
		Id: rid(v.ID), Shared: sharedOut(v.Shared),
		Name: v.Name, Host: v.Host, Port: clampInt32(v.Port),
		Tls: tlsModeOut(v.TLS), Username: strPtr(v.Username),
		// The password is writeOnly; only its presence is reported.
		HasPassword:     ptr(len(v.Password) > 0),
		MaxConns:        i32(v.MaxConns),
		Status:          ptr(transportStatusOut(v.Status)),
		StatusReason:    strPtr(v.StatusReason),
		StatusChangedAt: timePtr(v.StatusChangedAt),
		Version:         ptr(v.Version), CreatedAt: timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
	if v.RatePerSecond != 0 {
		out.RatePerSecond = ptr(v.RatePerSecond)
	}
	if len(v.DomainRatePerSecond) > 0 {
		m := v.DomainRatePerSecond
		out.DomainRatePerSecond = &m
	}
	return out
}

// --- senders -----------------------------------------------------------

func (s *server) ListSenders(ctx context.Context, req ListSendersRequestObject) (ListSendersResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.Senders().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]Sender, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, s.senderOut(t, &res.Items[i]))
	}
	return ListSenders200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) CreateSender(ctx context.Context, req CreateSenderRequestObject) (CreateSenderResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	snd := &store.Sender{}
	if err := s.applySender(ctx, t, snd, SenderUpdate{
		Name: req.Body.Name, FromName: req.Body.FromName, FromEmail: req.Body.FromEmail,
		ReplyTo: req.Body.ReplyTo, TransportId: req.Body.TransportId, DomainId: req.Body.DomainId,
	}); err != nil {
		return nil, err
	}
	if err := t.st.Senders().Create(ctx, snd); err != nil {
		return nil, err
	}
	return CreateSender201JSONResponse(s.senderOut(t, snd)), nil
}

func (s *server) GetSender(ctx context.Context, req GetSenderRequestObject) (GetSenderResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	snd, err := t.st.Senders().Get(ctx, req.SenderId)
	if err != nil {
		return nil, err
	}
	return GetSender200JSONResponse(s.senderOut(t, snd)), nil
}

func (s *server) UpdateSender(ctx context.Context, req UpdateSenderRequestObject) (UpdateSenderResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("sender", req.SenderId); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	snd, err := t.st.Senders().Get(ctx, req.SenderId)
	if err != nil {
		return nil, err
	}
	if err := s.applySender(ctx, t, snd, *req.Body); err != nil {
		return nil, err
	}
	snd.Version = req.Body.Version
	if err := t.st.Senders().Update(ctx, snd); err != nil {
		return nil, err
	}
	return UpdateSender200JSONResponse(s.senderOut(t, snd)), nil
}

func (s *server) DeleteSender(ctx context.Context, req DeleteSenderRequestObject) (DeleteSenderResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("sender", req.SenderId); err != nil {
		return nil, err
	}
	if err := t.st.Senders().Delete(ctx, req.SenderId); err != nil {
		return nil, err
	}
	return DeleteSender204Response{}, nil
}

func (s *server) applySender(ctx context.Context, t *tenant, snd *store.Sender, in SenderUpdate) error {
	if err := requireNonEmpty("name", in.Name); err != nil {
		return err
	}
	if err := noCRLF("from_name", deref(in.FromName)); err != nil {
		return err
	}
	if err := noCRLF("reply_to", deref(in.ReplyTo)); err != nil {
		return err
	}
	from, err := store.NormalizeEmail(string(in.FromEmail))
	if err != nil {
		return errInvalid("from_email: %v", err)
	}
	// The platform rules of ADR-0017, before the existence checks: a shared
	// transport or domain is not assignable at all, and from_email has to be
	// on a domain this tenant owns.
	if err := s.checkSenderAssignable(ctx, t, from, string(in.TransportId), ridVal(in.DomainId)); err != nil {
		return err
	}
	// A dangling transport or domain reference would only surface when the
	// first delivery is claimed, so it is rejected here instead.
	if _, err := t.st.Transports().Get(ctx, string(in.TransportId)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errInvalid("transport %s does not exist", in.TransportId)
		}
		return err
	}
	if domID := ridVal(in.DomainId); domID != "" {
		if _, err := t.st.Domains().Get(ctx, domID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errInvalid("sending domain %s does not exist", domID)
			}
			return err
		}
	}
	snd.Name = in.Name
	snd.FromName = deref(in.FromName)
	snd.FromEmail = from
	snd.ReplyTo = deref(in.ReplyTo)
	snd.TransportID = string(in.TransportId)
	snd.DomainID = ridVal(in.DomainId)
	return nil
}

// senderOut renders a sender for one tenant's eyes.
//
// A shared sender is shown to every tenant — it is what they send with — but
// stripped of the operator's internals: the relay and the domain behind it,
// and the probe verdict of a reputation they all share. What is left is the
// identity and the `uses` that say what it is allowed for (ADR-0017).
//
// The system tenant is the operator's own view and sees all of it, which is
// what makes a platform page in a console possible without a second API.
func (s *server) senderOut(t *tenant, v *store.Sender) Sender {
	out := Sender{
		Id: rid(v.ID), Shared: sharedOut(v.Shared), Name: v.Name,
		FromName: strPtr(v.FromName), FromEmail: v.FromEmail,
		ReplyTo:     strPtr(v.ReplyTo),
		TransportId: ridPtr(v.TransportID), DomainId: ridPtr(v.DomainID),
		Uses:            s.senderUses(v),
		Health:          ptr(healthOut(v.Health)),
		HealthReason:    strPtr(v.HealthReason),
		HealthCheckedAt: timePtr(v.HealthCheckedAt),
		Version:         ptr(v.Version), CreatedAt: timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
	if v.Shared && t.id != store.SystemTenantID {
		// Omitted, not blanked: `transport_id` is not in the schema's required
		// set precisely so that a tenant's view of a shared sender can leave
		// it out. What relay the operator routes it through is the operator's.
		out.TransportId = nil
		out.DomainId = nil
		out.Health = nil
		out.HealthReason = nil
		out.HealthCheckedAt = nil
	}
	return out
}

// GetSenderHealth is the worst-of summary of architecture 11.4: the latest
// probe run per mailbox, plus the transport circuit and the domain verdict.
func (s *server) GetSenderHealth(ctx context.Context, req GetSenderHealthRequestObject) (GetSenderHealthResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	snd, err := t.st.Senders().Get(ctx, req.SenderId)
	if err != nil {
		return nil, err
	}
	out := SenderHealth{
		SenderId:  rid(snd.ID),
		Status:    healthOut(snd.Health),
		Reason:    strPtr(snd.HealthReason),
		CheckedAt: timePtr(snd.HealthCheckedAt),
	}
	if snd.Shared && t.id != store.SystemTenantID {
		// A tenant learns whether the shared identity works, and nothing
		// else: no verdict, no timestamp, no per-mailbox detail
		// (internal/api/platform.go).
		status, err := s.sharedSenderStatus(ctx, snd)
		if err != nil {
			return nil, err
		}
		out.Status = healthOut(status)
		out.Reason = nil
		if status == store.HealthRed {
			out.Reason = strPtr(sender.SharedSenderReason())
		}
		return GetSenderHealth200JSONResponse(out), nil
	}
	if tr, err := t.st.Transports().Get(ctx, snd.TransportID); err == nil {
		out.TransportStatus = ptr(transportStatusOut(tr.Status))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if snd.DomainID != "" {
		if dom, err := t.st.Domains().Get(ctx, snd.DomainID); err == nil {
			out.DomainStatus = ptr(healthOut(dom.Health))
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}

	latest, err := latestRunPerMailbox(ctx, t, snd.ID)
	if err != nil {
		return nil, err
	}
	out.Mailboxes = &latest
	return GetSenderHealth200JSONResponse(out), nil
}

// latestRunPerMailbox walks the sender's probe history and keeps the newest
// run per mailbox. ProbeRunRepo has no "latest per mailbox" query, so the walk
// is bounded by maxProbeHistoryPages rather than by the whole history.
func latestRunPerMailbox(ctx context.Context, t *tenant, senderID string) ([]ProbeRun, error) {
	const maxProbeHistoryPages = 20
	best := map[string]store.ProbeRun{}
	page := store.Page{Limit: store.MaxPageLimit}
	for i := 0; i < maxProbeHistoryPages; i++ {
		res, err := t.st.ProbeRuns().ListBySender(ctx, senderID, page)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Items {
			cur, ok := best[r.MailboxID]
			if !ok || r.CreatedAt.After(cur.CreatedAt) {
				best[r.MailboxID] = r
			}
		}
		if res.NextCursor == "" {
			break
		}
		page.Cursor = res.NextCursor
	}
	ids := make([]string, 0, len(best))
	for id := range best {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ProbeRun, 0, len(ids))
	for _, id := range ids {
		r := best[id]
		out = append(out, probeRunOut(&r))
	}
	return out, nil
}

// TriggerProbeRun hands the request to the configured ProbeTrigger. The probe
// implementation lives outside this package, so a deployment without one gets
// an honest 501 instead of a 202 for work that will never happen.
func (s *server) TriggerProbeRun(ctx context.Context, req TriggerProbeRunRequestObject) (TriggerProbeRunResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	snd, err := t.st.Senders().Get(ctx, req.SenderId)
	if err != nil {
		return nil, err
	}
	if s.deps.Probe == nil {
		return nil, newErr(http.StatusNotImplemented, ErrorCodeInternal,
			"loopback probing is not configured in this deployment")
	}
	// A probe is a use like any other: an operator that restricted its shared
	// sender to transactional mail has not offered its tenants the shared
	// probe mailbox either. The system tenant is exempt, because the platform
	// probe is the operator's own (host.DefaultSenderPolicy).
	if err := s.checkSenderUse(ctx, t, snd.ID, store.UseProbe, s.probeTenantVars(snd)); err != nil {
		return nil, err
	}
	runID, err := s.deps.Probe.Trigger(ctx, t.st, snd.ID)
	if err != nil {
		return nil, err
	}
	run, err := t.st.ProbeRuns().Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := ProbeTriggerResult{}
	out.Runs = append(out.Runs, struct {
		DeliveryId *UUID      `json:"delivery_id,omitempty"`
		MailboxId  ResourceId `json:"mailbox_id"`
		RunId      UUID       `json:"run_id"`
	}{
		DeliveryId: uuidPtrOf(run.DeliveryID),
		MailboxId:  rid(run.MailboxID),
		RunId:      uuidOf(run.ID),
	})
	return TriggerProbeRun202JSONResponse(out), nil
}

// --- sending domains ---------------------------------------------------

func (s *server) ListSendingDomains(ctx context.Context, req ListSendingDomainsRequestObject) (ListSendingDomainsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.Domains().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]SendingDomain, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, domainOut(&res.Items[i]))
	}
	return ListSendingDomains200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) CreateSendingDomain(ctx context.Context, req CreateSendingDomainRequestObject) (CreateSendingDomainResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	d := &store.SendingDomain{}
	if err := s.applyDomain(ctx, d, SendingDomainUpdate{
		Domain: req.Body.Domain, DkimSelector: req.Body.DkimSelector,
		DkimPrivateKey: req.Body.DkimPrivateKey, ReturnPathDomain: req.Body.ReturnPathDomain,
		ExpectedSpf: req.Body.ExpectedSpf, OutboundIps: req.Body.OutboundIps,
	}); err != nil {
		return nil, err
	}
	if err := t.st.Domains().Create(ctx, d); err != nil {
		return nil, err
	}
	return CreateSendingDomain201JSONResponse(domainOut(d)), nil
}

func (s *server) GetSendingDomain(ctx context.Context, req GetSendingDomainRequestObject) (GetSendingDomainResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := t.st.Domains().Get(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	return GetSendingDomain200JSONResponse(domainOut(d)), nil
}

func (s *server) UpdateSendingDomain(ctx context.Context, req UpdateSendingDomainRequestObject) (UpdateSendingDomainResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("sending domain", req.DomainId); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	d, err := t.st.Domains().Get(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := s.applyDomain(ctx, d, *req.Body); err != nil {
		return nil, err
	}
	d.Version = req.Body.Version
	if err := t.st.Domains().Update(ctx, d); err != nil {
		return nil, err
	}
	return UpdateSendingDomain200JSONResponse(domainOut(d)), nil
}

func (s *server) DeleteSendingDomain(ctx context.Context, req DeleteSendingDomainRequestObject) (DeleteSendingDomainResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("sending domain", req.DomainId); err != nil {
		return nil, err
	}
	if err := t.st.Domains().Delete(ctx, req.DomainId); err != nil {
		return nil, err
	}
	return DeleteSendingDomain204Response{}, nil
}

func (s *server) applyDomain(ctx context.Context, d *store.SendingDomain, in SendingDomainUpdate) error {
	if err := requireNonEmpty("domain", in.Domain); err != nil {
		return err
	}
	if err := noCRLF("domain", in.Domain); err != nil {
		return err
	}
	key, err := s.secret(ctx, in.DkimPrivateKey, d.DKIMPrivateKey)
	if err != nil {
		return err
	}
	d.Domain = in.Domain
	d.DKIMSelector = deref(in.DkimSelector)
	d.DKIMPrivateKey = key
	d.ReturnPathDomain = deref(in.ReturnPathDomain)
	d.ExpectedSPF = deref(in.ExpectedSpf)
	d.OutboundIPs = nil
	if in.OutboundIps != nil {
		d.OutboundIPs = *in.OutboundIps
	}
	return nil
}

func domainOut(v *store.SendingDomain) SendingDomain {
	out := SendingDomain{
		Id: rid(v.ID), Shared: sharedOut(v.Shared), Domain: v.Domain,
		DkimSelector: strPtr(v.DKIMSelector),
		// The DKIM private key is writeOnly (architecture 16).
		HasDkimPrivateKey: ptr(len(v.DKIMPrivateKey) > 0),
		ReturnPathDomain:  strPtr(v.ReturnPathDomain),
		ExpectedSpf:       strPtr(v.ExpectedSPF),
		Health:            ptr(healthOut(v.Health)),
		HealthReason:      strPtr(v.HealthReason),
		HealthCheckedAt:   timePtr(v.HealthCheckedAt),
		Version:           ptr(v.Version), CreatedAt: timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
	if len(v.OutboundIPs) > 0 {
		ips := v.OutboundIPs
		out.OutboundIps = &ips
	}
	return out
}

// --- probe mailboxes ---------------------------------------------------

func (s *server) ListProbeMailboxes(ctx context.Context, req ListProbeMailboxesRequestObject) (ListProbeMailboxesResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.ProbeMailboxes().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]ProbeMailbox, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, mailboxOut(&res.Items[i]))
	}
	return ListProbeMailboxes200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) CreateProbeMailbox(ctx context.Context, req CreateProbeMailboxRequestObject) (CreateProbeMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	m := &store.ProbeMailbox{}
	if err := s.applyMailbox(ctx, m, ProbeMailboxUpdate{
		Name: req.Body.Name, Kind: req.Body.Kind,
		Address: req.Body.Address, Host: req.Body.Host, Port: req.Body.Port,
		Tls: req.Body.Tls, Username: req.Body.Username, Password: req.Body.Password,
		InboxFolder: req.Body.InboxFolder, SpamFolder: req.Body.SpamFolder,
		AuthservId: req.Body.AuthservId, Enabled: req.Body.Enabled,
	}); err != nil {
		return nil, err
	}
	if err := t.st.ProbeMailboxes().Create(ctx, m); err != nil {
		return nil, err
	}
	return CreateProbeMailbox201JSONResponse(mailboxOut(m)), nil
}

func (s *server) GetProbeMailbox(ctx context.Context, req GetProbeMailboxRequestObject) (GetProbeMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	m, err := t.st.ProbeMailboxes().Get(ctx, req.MailboxId)
	if err != nil {
		return nil, err
	}
	return GetProbeMailbox200JSONResponse(mailboxOut(m)), nil
}

func (s *server) UpdateProbeMailbox(ctx context.Context, req UpdateProbeMailboxRequestObject) (UpdateProbeMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("probe mailbox", req.MailboxId); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	m, err := t.st.ProbeMailboxes().Get(ctx, req.MailboxId)
	if err != nil {
		return nil, err
	}
	if err := s.applyMailbox(ctx, m, *req.Body); err != nil {
		return nil, err
	}
	m.Version = req.Body.Version
	if err := t.st.ProbeMailboxes().Update(ctx, m); err != nil {
		return nil, err
	}
	return UpdateProbeMailbox200JSONResponse(mailboxOut(m)), nil
}

func (s *server) DeleteProbeMailbox(ctx context.Context, req DeleteProbeMailboxRequestObject) (DeleteProbeMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("probe mailbox", req.MailboxId); err != nil {
		return nil, err
	}
	if err := t.st.ProbeMailboxes().Delete(ctx, req.MailboxId); err != nil {
		return nil, err
	}
	return DeleteProbeMailbox204Response{}, nil
}

func (s *server) applyMailbox(ctx context.Context, m *store.ProbeMailbox, in ProbeMailboxUpdate) error {
	if err := requireNonEmpty("name", in.Name); err != nil {
		return err
	}
	kind, err := probeMailboxKindIn(in.Kind)
	if err != nil {
		return err
	}
	addr, err := store.NormalizeEmail(string(in.Address))
	if err != nil {
		return errInvalid("address: %v", err)
	}
	mode, err := tlsModeIn(in.Tls)
	if err != nil {
		return err
	}

	m.Name, m.Kind, m.Address, m.AuthServID = in.Name, kind, addr, deref(in.AuthservId)
	m.Enabled = true
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}

	if kind == store.ProbeMailboxWebhook {
		// A webhook mailbox is an address and an authserv-id. Refusing the
		// IMAP block rather than ignoring it is the point: a row that carries
		// a host and a password nothing will ever dial is a row an operator
		// will later read as "the probe logs in here" (ADR-0016).
		if field := webhookExtraField(in); field != "" {
			return errInvalid(
				"%s is not allowed for a webhook probe mailbox: its mail arrives over the "+
					"provider's inbound webhook, so there is nothing to log in to", field)
		}
		m.Host, m.Port, m.TLS = "", 0, ""
		m.Username, m.Password = "", nil
		m.InboxFolder, m.SpamFolder = "", ""
		return nil
	}

	if in.Host == nil || *in.Host == "" {
		return errInvalid("host is required")
	}
	if in.Port == nil || *in.Port <= 0 || *in.Port > 65535 {
		return errInvalid("port must be between 1 and 65535")
	}
	pw, err := s.secret(ctx, in.Password, m.Password)
	if err != nil {
		return err
	}
	m.Host, m.Port, m.TLS = *in.Host, int(*in.Port), mode
	m.Username = deref(in.Username)
	m.Password = pw
	m.InboxFolder = deref(in.InboxFolder)
	m.SpamFolder = deref(in.SpamFolder)
	return nil
}

// probeMailboxKindIn validates the kind, defaulting to imap. The spec
// documents the default in prose rather than with `default:`, so an omitted
// kind arrives here as nil and not as a value the generator invented.
func probeMailboxKindIn(k *ProbeMailboxKind) (store.ProbeMailboxKind, error) {
	if k == nil || *k == "" {
		return store.ProbeMailboxIMAP, nil
	}
	out := store.ProbeMailboxKind(*k)
	if !out.Valid() {
		return "", errInvalid("kind %q is not imap or webhook", string(*k))
	}
	return out.Normalized(), nil
}

// webhookExtraField names the first IMAP field a webhook mailbox must not
// carry, or "".
func webhookExtraField(in ProbeMailboxUpdate) string {
	switch {
	case in.Host != nil && *in.Host != "":
		return "host"
	case in.Port != nil && *in.Port != 0:
		return "port"
	case in.Tls != nil && *in.Tls != "":
		return "tls"
	case in.Username != nil && *in.Username != "":
		return "username"
	case in.Password != nil && *in.Password != "":
		return "password"
	case in.InboxFolder != nil && *in.InboxFolder != "":
		return "inbox_folder"
	case in.SpamFolder != nil && *in.SpamFolder != "":
		return "spam_folder"
	}
	return ""
}

func mailboxOut(v *store.ProbeMailbox) ProbeMailbox {
	return ProbeMailbox{
		Id: rid(v.ID), Shared: sharedOut(v.Shared), Name: v.Name,
		Kind:    ptr(ProbeMailboxKind(v.Kind.Normalized())),
		Address: openapiEmail(v.Address),
		Host:    v.Host, Port: clampInt32(v.Port), Tls: tlsModeOut(v.TLS),
		Username: strPtr(v.Username),
		// The IMAP password is writeOnly (architecture 16).
		HasPassword: ptr(len(v.Password) > 0),
		InboxFolder: strPtr(v.InboxFolder), SpamFolder: strPtr(v.SpamFolder),
		AuthservId: strPtr(v.AuthServID), Enabled: ptr(v.Enabled),
		Health:  mailboxHealthOut(v.Health),
		Version: ptr(v.Version), CreatedAt: timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
}

// --- probe runs --------------------------------------------------------

func (s *server) ListProbeRuns(ctx context.Context, req ListProbeRunsRequestObject) (ListProbeRunsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := t.st.Senders().Get(ctx, req.Params.SenderId); err != nil {
		return nil, err
	}
	res, err := t.st.ProbeRuns().ListBySender(ctx, req.Params.SenderId,
		pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]ProbeRun, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, probeRunOut(&res.Items[i]))
	}
	return ListProbeRuns200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) GetProbeRun(ctx context.Context, req GetProbeRunRequestObject) (GetProbeRunResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := t.st.ProbeRuns().Get(ctx, req.RunId.String())
	if err != nil {
		return nil, err
	}
	return GetProbeRun200JSONResponse(probeRunOut(r)), nil
}

// probeFolderOut renders the folder a probe mail was filed in. An empty
// folder is omitted rather than sent as "", which is not in the enum.
func probeFolderOut(folder string) *ProbeRunFolder {
	if folder == "" {
		return nil
	}
	return ptr(ProbeRunFolder(folder))
}

func probeRunOut(v *store.ProbeRun) ProbeRun {
	out := ProbeRun{
		Id: uuidOf(v.ID), SenderId: rid(v.SenderID), MailboxId: rid(v.MailboxID),
		DeliveryId: uuidPtrOf(v.DeliveryID), GroupId: uuidPtrOf(v.GroupID),
		Pending: ptr(v.Pending),
		Status:  healthOut(v.Status), Reason: strPtr(v.Reason),
		Delivered: ptr(v.Delivered), Folder: probeFolderOut(v.Folder),
		Spf: strPtr(v.SPF), Dkim: strPtr(v.DKIM), Dmarc: strPtr(v.DMARC),
		DkimDomain: strPtr(v.DKIMDomain), DkimSelector: strPtr(v.DKIMSelector),
		DmarcPolicy: strPtr(v.DMARCPolicy),
		Tls:         ptr(v.TLS), ObservedIp: strPtr(v.ObservedIP),
		Ptr: strPtr(v.PTR), PtrMatch: ptr(v.PTRMatch),
		RawHeaders: strPtr(v.RawHeaders),
		StartedAt:  timePtr(v.StartedAt), ReceivedAt: timePtr(v.ReceivedAt), CreatedAt: timePtr(v.CreatedAt),
	}
	if v.Latency > 0 {
		out.Latency = ptr(durationStr(v.Latency))
	}
	if len(v.DNS) > 0 {
		if m, err := jsonMap(v.DNS); err == nil {
			out.Dns = &m
		}
	}
	return out
}
