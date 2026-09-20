package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/sendplane/sendplane/internal/ingest"
	"github.com/sendplane/sendplane/store"
)

func (s *server) ListCampaigns(ctx context.Context, req ListCampaignsRequestObject) (ListCampaignsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	page := pageOf(req.Params.Limit, req.Params.Cursor)

	var res store.Result[store.Campaign]
	if req.Params.Status != nil && len(*req.Params.Status) > 0 {
		statuses := make([]store.CampaignStatus, 0, len(*req.Params.Status))
		for _, v := range *req.Params.Status {
			st, err := campaignStatusIn(v)
			if err != nil {
				return nil, err
			}
			statuses = append(statuses, st)
		}
		res, err = t.st.Campaigns().ListByStatus(ctx, statuses, page)
	} else {
		res, err = t.st.Campaigns().List(ctx, page)
	}
	if err != nil {
		return nil, err
	}
	items := make([]Campaign, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, campaignOut(&res.Items[i]))
	}
	return ListCampaigns200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) CreateCampaign(ctx context.Context, req CreateCampaignRequestObject) (CreateCampaignResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	c := &store.Campaign{Status: store.CampaignDraft}
	if err := s.applyCampaign(ctx, t, c, CampaignUpdate{
		Name: req.Body.Name, VersionId: req.Body.VersionId, TemplateId: req.Body.TemplateId,
		SenderId: req.Body.SenderId, DefaultLocale: req.Body.DefaultLocale,
		Vars: req.Body.Vars, ScheduleAt: req.Body.ScheduleAt,
	}); err != nil {
		return nil, err
	}
	if err := t.st.Campaigns().Create(ctx, c); err != nil {
		return nil, err
	}
	return CreateCampaign201JSONResponse(campaignOut(c)), nil
}

func (s *server) GetCampaign(ctx context.Context, req GetCampaignRequestObject) (GetCampaignResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	return GetCampaign200JSONResponse(campaignOut(c)), nil
}

func (s *server) UpdateCampaign(ctx context.Context, req UpdateCampaignRequestObject) (UpdateCampaignResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	// Editing a campaign that is already sending would change the version or
	// the sender under the deliveries the sender is claiming right now.
	switch c.Status {
	case store.CampaignDraft, store.CampaignScheduled:
	default:
		return nil, errConflict(ErrorCodeInvalidState, "campaign %s is %s and cannot be edited", c.ID, c.Status)
	}
	if err := s.applyCampaign(ctx, t, c, *req.Body); err != nil {
		return nil, err
	}
	c.Version = req.Body.Version
	if err := t.st.Campaigns().Update(ctx, c); err != nil {
		return nil, err
	}
	return UpdateCampaign200JSONResponse(campaignOut(c)), nil
}

func (s *server) DeleteCampaign(ctx context.Context, req DeleteCampaignRequestObject) (DeleteCampaignResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	if c.Status == store.CampaignRunning || c.Status == store.CampaignScheduled {
		return nil, errConflict(ErrorCodeInvalidState,
			"campaign %s is %s: cancel it before deleting", c.ID, c.Status)
	}
	if err := t.st.Campaigns().Delete(ctx, c.ID); err != nil {
		return nil, err
	}
	return DeleteCampaign204Response{}, nil
}

// applyCampaign validates and copies the mutable half of a campaign.
//
// `template_id` is kept on the campaign and resolved to the template's
// published version at start (control.StartCampaign), which is what the spec
// says: a template edited and republished between creating and starting a
// campaign is the one that goes out. Only the existence of the template is
// checked here — it does not have to be published yet. Passing `version_id`
// pins an exact version and start leaves it alone.
func (s *server) applyCampaign(ctx context.Context, t *tenant, c *store.Campaign, in CampaignUpdate) error {
	if err := requireNonEmpty("name", in.Name); err != nil {
		return err
	}
	versionID := idPtrOf(in.VersionId)
	templateID := ""
	switch {
	case versionID != "":
		if _, err := t.st.Versions().Get(ctx, versionID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errInvalid("message version %s does not exist", versionID)
			}
			return err
		}
	case in.TemplateId != nil:
		tpl, err := t.st.Templates().Get(ctx, idOf(*in.TemplateId))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errInvalid("template %s does not exist", *in.TemplateId)
			}
			return err
		}
		templateID = tpl.ID
	default:
		return errInvalid("one of template_id or version_id is required")
	}
	if _, err := t.st.Senders().Get(ctx, idOf(in.SenderId)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errInvalid("sender %s does not exist", in.SenderId)
		}
		return err
	}

	c.Name = in.Name
	c.TemplateID = templateID
	c.VersionID = versionID
	c.SenderID = idOf(in.SenderId)
	c.DefaultLocale = deref(in.DefaultLocale)
	c.Vars = varsOf(in.Vars)
	c.ScheduleAt = timeVal(in.ScheduleAt)
	return nil
}

// statsTotals derives the two denominators the spec states explicitly from the
// per-status counts the control loop caches, so that a client reading a rate
// does not have to know which statuses to add up (and cannot pick a different
// set than the next client).
//
// sent counts everything the receiving MTA accepted, which includes the rows
// that have since moved on to bounced or complained: those transitions happen
// minutes to days after the send, and letting them shrink the denominator
// would make an open rate climb on its own.
func statsTotals(by map[store.DeliveryStatus]int64) (total, sent int64) {
	for st, n := range by {
		total += n
		switch st {
		case store.DeliverySent, store.DeliveryBounced, store.DeliveryComplained:
			sent += n
		}
	}
	return total, sent
}

func campaignOut(v *store.Campaign) Campaign {
	byStatus := map[string]int64{}
	for st, n := range v.Stats.ByStatus {
		byStatus[st.String()] = n
	}
	total, sent := statsTotals(v.Stats.ByStatus)
	return Campaign{
		Id: uuidPtrOf(v.ID), Name: v.Name,
		TemplateId: uuidPtrOf(v.TemplateID),
		VersionId:  uuidPtrOf(v.VersionID), SenderId: uuidOf(v.SenderID),
		DefaultLocale: strPtr(v.DefaultLocale), Vars: varsOut(v.Vars),
		Status:     campaignStatusOut(v.Status),
		ScheduleAt: timePtr(v.ScheduleAt), StartedAt: timePtr(v.StartedAt),
		CompletedAt: timePtr(v.CompletedAt),
		Stats: &CampaignStats{
			ByStatus:           &byStatus,
			Total:              ptr(total),
			Sent:               ptr(sent),
			UniqueOpens:        ptr(v.Stats.UniqueOpens),
			UniqueClicks:       ptr(v.Stats.UniqueClicks),
			Unsubscribed:       ptr(v.Stats.Unsubscribed),
			UnsubscribeClicked: ptr(v.Stats.UnsubscribeClicked),
			ComputedAt:         timePtr(v.Stats.ComputedAt),
		},
		Version: ptr(v.Version), CreatedAt: timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
}

// --- recipients --------------------------------------------------------

// IngestCampaignRecipients streams the NDJSON body straight into the ingester.
// The body is never buffered: r.Body goes in as an io.Reader, so a 1M-line
// chunk costs the batch size in memory, not the request size (ADR-0005).
func (s *server) IngestCampaignRecipients(ctx context.Context, req IngestCampaignRecipientsRequestObject) (IngestCampaignRecipientsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	campaignID := req.CampaignId.String()

	// The chunk key is the caller's Idempotency-Key. A body hash would be the
	// documented fallback, but hashing means buffering the body, which is the
	// one thing this endpoint must not do; without a key the row-level
	// (campaign_id, email_norm) unique index still makes a resend safe.
	chunkKey := deref(req.Params.IdempotencyKey)

	replay := false
	if chunkKey != "" {
		prev, err := t.st.RecipientChunks().Get(ctx, campaignID, chunkKey)
		if err == nil && prev.State == store.ChunkCompleted {
			replay = true
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}

	ing := ingest.New(t.st, s.deps.Limits, s.deps.Clock)
	res, err := ing.Ingest(ctx, campaignID, chunkKey, req.Body)
	if err != nil {
		return nil, err
	}
	out := RecipientIngestResult{
		Accepted: int64(res.Accepted), Duplicates: int64(res.Duplicates),
		Invalid: int64(res.Invalid), Total: int64(res.Total),
	}
	if replay {
		out.IdempotentReplay = ptr(true)
	}
	return IngestCampaignRecipients200JSONResponse(out), nil
}

// --- lifecycle ---------------------------------------------------------

func (s *server) StartCampaign(ctx context.Context, req StartCampaignRequestObject) (StartCampaignResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	var at time.Time
	if req.Body != nil {
		at = timeVal(req.Body.ScheduleAt)
	}
	if err := s.deps.Control.StartCampaign(ctx, t.st, req.CampaignId.String(), at); err != nil {
		return nil, err
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	return StartCampaign200JSONResponse(campaignOut(c)), nil
}

func (s *server) PauseCampaign(ctx context.Context, req PauseCampaignRequestObject) (PauseCampaignResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Control.PauseCampaign(ctx, t.st, req.CampaignId.String()); err != nil {
		return nil, err
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	return PauseCampaign200JSONResponse(campaignOut(c)), nil
}

func (s *server) ResumeCampaign(ctx context.Context, req ResumeCampaignRequestObject) (ResumeCampaignResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Control.ResumeCampaign(ctx, t.st, req.CampaignId.String()); err != nil {
		return nil, err
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	return ResumeCampaign200JSONResponse(campaignOut(c)), nil
}

func (s *server) CancelCampaign(ctx context.Context, req CancelCampaignRequestObject) (CancelCampaignResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Control.CancelCampaign(ctx, t.st, req.CampaignId.String()); err != nil {
		return nil, err
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	return CancelCampaign202JSONResponse(campaignOut(c)), nil
}

func (s *server) RetryCampaignDeliveries(ctx context.Context, req RetryCampaignDeliveriesRequestObject) (RetryCampaignDeliveriesResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	filter, err := retryFilterOf(*req.Body)
	if err != nil {
		return nil, err
	}
	filter.Now = s.now()
	n, err := s.deps.Control.RetryCampaign(ctx, t.st, req.CampaignId.String(), filter)
	if err != nil {
		return nil, err
	}
	return RetryCampaignDeliveries200JSONResponse(RetryResult{Requeued: int64(n)}), nil
}

func retryFilterOf(in RetryRequest) (store.RetryFilter, error) {
	var f store.RetryFilter
	if in.Status != nil {
		for _, v := range *in.Status {
			st, err := deliveryStatusIn(v)
			if err != nil {
				return f, err
			}
			f.Statuses = append(f.Statuses, st)
		}
	}
	if in.ErrorClass != nil {
		for _, v := range *in.ErrorClass {
			cl, err := errorClassIn(v)
			if err != nil {
				return f, err
			}
			f.ErrorClasses = append(f.ErrorClasses, cl)
		}
	}
	if in.DeliveryIds != nil {
		// A non-nil but empty list matches nothing on purpose
		// (store.RetryFilter), so the distinction is preserved here.
		f.DeliveryIDs = make([]string, 0, len(*in.DeliveryIds))
		for _, id := range *in.DeliveryIds {
			f.DeliveryIDs = append(f.DeliveryIDs, id.String())
		}
	}
	return f, nil
}

// --- reporting ---------------------------------------------------------

func (s *server) ListCampaignDeliveries(ctx context.Context, req ListCampaignDeliveriesRequestObject) (ListCampaignDeliveriesResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := t.st.Campaigns().Get(ctx, req.CampaignId.String()); err != nil {
		return nil, err
	}
	var f store.DeliveryFilter
	if f.Statuses, err = deliveryStatusesIn(req.Params.Status); err != nil {
		return nil, err
	}
	if f.ErrorClasses, err = errorClassesIn(req.Params.ErrorClass); err != nil {
		return nil, err
	}
	if f.EmailNorm, err = emailNormIn(req.Params.Email); err != nil {
		return nil, err
	}
	res, err := t.st.Deliveries().ListByCampaign(ctx, req.CampaignId.String(), f,
		pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]Delivery, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, deliveryOut(&res.Items[i]))
	}
	return ListCampaignDeliveries200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

// ListCampaignLinks joins the version's link list with the click counts, so a
// link that was never clicked still shows up with a zero and the index lines
// up with the link_no in the click tokens (architecture 9.2).
func (s *server) ListCampaignLinks(ctx context.Context, req ListCampaignLinksRequestObject) (ListCampaignLinksResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	counts, err := t.st.Tracking().LinkClicks(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	byNo := make(map[int]store.LinkClick, len(counts))
	extra := make([]store.LinkClick, 0)
	for _, lc := range counts {
		if lc.LinkNo < 0 {
			extra = append(extra, lc)
			continue
		}
		byNo[lc.LinkNo] = lc
	}

	items := make([]LinkClick, 0, len(counts))
	if c.VersionID != "" {
		v, err := t.st.Versions().Get(ctx, c.VersionID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if v != nil {
			for i, url := range v.Links {
				lc := byNo[i]
				items = append(items, LinkClick{
					LinkNo: clampInt32(i), Url: url,
					Clicks: lc.Clicks, UniqueClicks: lc.UniqueClicks,
				})
				delete(byNo, i)
			}
		}
	}
	// Anything recorded against a link the published version does not list
	// (a dynamic href, or a version replaced since) is reported as it was
	// stored rather than dropped.
	for _, no := range sortedInts(byNo) {
		lc := byNo[no]
		items = append(items, LinkClick{LinkNo: clampInt32(lc.LinkNo), Url: lc.URL,
			Clicks: lc.Clicks, UniqueClicks: lc.UniqueClicks})
	}
	for _, lc := range extra {
		items = append(items, LinkClick{LinkNo: clampInt32(lc.LinkNo), Url: lc.URL,
			Clicks: lc.Clicks, UniqueClicks: lc.UniqueClicks})
	}
	return ListCampaignLinks200JSONResponse(LinkClickList{Items: items}), nil
}

func sortedInts(m map[int]store.LinkClick) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// --- unsubscribe notifications -----------------------------------------

// NotifyCampaignUnsubscribe is the unsubscribe_mode=host path: sendplane never
// saw the click, so the host reports it here and the campaign ratio picks it
// up (architecture 9.2).
func (s *server) NotifyCampaignUnsubscribe(ctx context.Context, req NotifyCampaignUnsubscribeRequestObject) (NotifyCampaignUnsubscribeResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	c, err := t.st.Campaigns().Get(ctx, req.CampaignId.String())
	if err != nil {
		return nil, err
	}
	d, err := s.findRecipient(ctx, t, c.ID, req.Body.DeliveryId, req.Body.Email)
	if err != nil {
		return nil, err
	}
	at := timeVal(req.Body.OccurredAt)
	res, err := s.recordUnsubscribe(ctx, t, d, sourceOf(req.Body.Source), at)
	if err != nil {
		return nil, err
	}
	return NotifyCampaignUnsubscribe200JSONResponse(res), nil
}

func sourceOf(v *UnsubscribeNoticeSource) string {
	if v == nil || *v == "" {
		return "host"
	}
	return string(*v)
}

// findRecipient resolves the delivery a notification is about, by ID or by
// address within the campaign.
func (s *server) findRecipient(ctx context.Context, t *tenant, campaignID string, id *UUID, email *Email) (*store.Delivery, error) {
	switch {
	case id != nil:
		d, err := t.st.Deliveries().Get(ctx, id.String())
		if err != nil {
			return nil, err
		}
		if d.CampaignID != campaignID {
			return nil, errNotFound("delivery", id.String())
		}
		return d, nil
	case email != nil:
		norm, err := store.NormalizeEmail(string(*email))
		if err != nil {
			return nil, errInvalid("email: %v", err)
		}
		res, err := t.st.Deliveries().ListByCampaign(ctx, campaignID,
			store.DeliveryFilter{EmailNorm: norm}, store.Page{Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(res.Items) == 0 {
			return nil, errNotFound("recipient", norm)
		}
		return &res.Items[0], nil
	}
	return nil, errInvalid("one of delivery_id or email is required")
}

// recordUnsubscribe is the shared tail of every confirmed unsubscribe: the
// tracking buffer records the event and derives unsubscribed_at from it
// (control.TrackingBuffer), the suppression list is updated if the tenant
// opted in, and the host is told through the outbox.
func (s *server) recordUnsubscribe(ctx context.Context, t *tenant, d *store.Delivery, source string, at time.Time) (UnsubscribeResult, error) {
	if at.IsZero() {
		at = s.now()
	}
	// Only the first write sets unsubscribed_at, so a repeated notification is
	// a no-op that still answers 200 with recorded=false.
	already := !d.UnsubscribedAt.IsZero()

	s.deps.Control.Tracking().Record(store.TrackingEvent{
		TenantID: t.id, DeliveryID: d.ID, CampaignID: d.CampaignID,
		Kind: store.TrackingUnsubscribed, CreatedAt: at,
	})

	out := UnsubscribeResult{DeliveryId: uuidPtrOf(d.ID), Recorded: !already}

	settings, err := settingsFor(ctx, t, s.now())
	if err != nil {
		return out, err
	}
	if settings.SuppressionEnabled && d.EmailNorm != "" {
		if err := t.st.Suppressions().Upsert(ctx, &store.Suppression{
			EmailNorm: d.EmailNorm, Reason: store.SuppressionManual,
			SourceDeliveryID: d.ID, CreatedAt: at,
		}); err != nil {
			return out, err
		}
		out.Suppressed = ptr(true)
	}

	if err := s.emitUnsubscribed(ctx, t, d, source, at); err != nil {
		return out, err
	}
	return out, nil
}

// unsubscribePayload is the body of a recipient.unsubscribed outbox event
// (architecture 12). It is the host's main path for updating its own database.
type unsubscribePayload struct {
	TenantID   string    `json:"tenant_id"`
	CampaignID string    `json:"campaign_id,omitempty"`
	DeliveryID string    `json:"delivery_id"`
	Email      string    `json:"email"`
	EmailNorm  string    `json:"email_norm"`
	Source     string    `json:"source"`
	OccurredAt time.Time `json:"occurred_at"`
}

const eventRecipientUnsubscribed = "recipient.unsubscribed"

func (s *server) emitUnsubscribed(ctx context.Context, t *tenant, d *store.Delivery, source string, at time.Time) error {
	payload, err := json.Marshal(unsubscribePayload{
		TenantID: t.id, CampaignID: d.CampaignID, DeliveryID: d.ID,
		Email: d.Email, EmailNorm: d.EmailNorm, Source: source, OccurredAt: at,
	})
	if err != nil {
		return err
	}
	return t.st.Outbox().Enqueue(ctx, []store.OutboxEvent{{
		Type: eventRecipientUnsubscribed, Payload: payload,
		Status: store.OutboxPending, CreatedAt: at, NextAttemptAt: at,
	}})
}
