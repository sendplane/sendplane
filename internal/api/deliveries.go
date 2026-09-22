package api

import (
	"context"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/sendplane/sendplane/store"
)

// The delivery listing filters are shared by the tenant-wide route and the
// campaign-scoped one, which take them from differently named generated param
// structs. Each converter is small enough to compose at the call site, which
// keeps the two handlers readable instead of hiding them behind one helper
// with five pointer arguments.

func deliveryStatusesIn(v *[]DeliveryStatus) ([]store.DeliveryStatus, error) {
	if v == nil {
		return nil, nil
	}
	out := make([]store.DeliveryStatus, 0, len(*v))
	for _, x := range *v {
		st, err := deliveryStatusIn(x)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func errorClassesIn(v *[]ErrorClass) ([]store.ErrorClass, error) {
	if v == nil {
		return nil, nil
	}
	out := make([]store.ErrorClass, 0, len(*v))
	for _, x := range *v {
		cl, err := errorClassIn(x)
		if err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, nil
}

func lanesIn(v *[]Lane) ([]store.Lane, error) {
	if v == nil {
		return nil, nil
	}
	out := make([]store.Lane, 0, len(*v))
	for _, x := range *v {
		l, err := laneIn(x)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// emailNormIn normalizes a filter address the same way ingest normalizes a
// recipient's, so that "A+x@Example.COM" finds the row stored under
// "a+x@example.com".
func emailNormIn(v *openapi_types.Email) (string, error) {
	if v == nil {
		return "", nil
	}
	norm, err := store.NormalizeEmail(string(*v))
	if err != nil {
		return "", errInvalid("email: %v", err)
	}
	return norm, nil
}

// ListDeliveries is the tenant-wide listing: every delivery of the tenant,
// campaign or not. It exists because an operator looking up one address does
// not know which campaign carried it, and because transactional and probe
// deliveries have no campaign to scope a listing to at all.
//
// Unlike the campaign-scoped route it has no existence check to make, so it
// never answers 404: an unknown campaign_id is simply an empty page.
func (s *server) ListDeliveries(ctx context.Context, req ListDeliveriesRequestObject) (ListDeliveriesResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	var f store.DeliveryFilter
	if f.Statuses, err = deliveryStatusesIn(req.Params.Status); err != nil {
		return nil, err
	}
	if f.ErrorClasses, err = errorClassesIn(req.Params.ErrorClass); err != nil {
		return nil, err
	}
	if f.Lanes, err = lanesIn(req.Params.Lane); err != nil {
		return nil, err
	}
	if f.EmailNorm, err = emailNormIn(req.Params.Email); err != nil {
		return nil, err
	}
	if req.Params.CampaignId != nil {
		// Not idOf: the nil UUID is a campaign that does not exist, not the
		// "no campaign" filter, which this route reaches through lane instead.
		id := req.Params.CampaignId.String()
		f.CampaignID = &id
	}
	f.Since, f.Until = timeVal(req.Params.Since), timeVal(req.Params.Until)
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		return nil, errInvalid("until must be after since")
	}
	res, err := t.st.Deliveries().List(ctx, f, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]Delivery, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, deliveryOut(&res.Items[i]))
	}
	return ListDeliveries200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) GetDelivery(ctx context.Context, req GetDeliveryRequestObject) (GetDeliveryResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := t.st.Deliveries().Get(ctx, req.DeliveryId.String())
	if err != nil {
		return nil, err
	}
	return GetDelivery200JSONResponse(deliveryOut(d)), nil
}

func (s *server) ListDeliveryAttempts(ctx context.Context, req ListDeliveryAttemptsRequestObject) (ListDeliveryAttemptsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := t.st.Deliveries().Get(ctx, req.DeliveryId.String()); err != nil {
		return nil, err
	}
	res, err := t.st.Attempts().ListByDelivery(ctx, req.DeliveryId.String(),
		pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]DeliveryAttempt, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, attemptOut(&res.Items[i]))
	}
	return ListDeliveryAttempts200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) ListDeliveryBounces(ctx context.Context, req ListDeliveryBouncesRequestObject) (ListDeliveryBouncesResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := t.st.Deliveries().Get(ctx, req.DeliveryId.String()); err != nil {
		return nil, err
	}
	res, err := t.st.Bounces().ListByDelivery(ctx, req.DeliveryId.String(),
		pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]BounceEvent, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, bounceOut(&res.Items[i]))
	}
	return ListDeliveryBounces200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

// RetryDelivery requeues one delivery. It goes through DeliveryRepo.Requeue
// rather than a status write so that retry_gen is bumped and the earlier
// attempts stay in the history (ADR-0003).
func (s *server) RetryDelivery(ctx context.Context, req RetryDeliveryRequestObject) (RetryDeliveryResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := t.st.Deliveries().Get(ctx, req.DeliveryId.String())
	if err != nil {
		return nil, err
	}
	if !d.Status.Terminal() {
		return nil, errConflict(ErrorCodeInvalidState,
			"delivery %s is %s: only a finished delivery can be retried", d.ID, d.Status)
	}
	n, err := t.st.Deliveries().Requeue(ctx, store.RetryFilter{
		CampaignID: d.CampaignID, DeliveryIDs: []string{d.ID}, Now: s.now(),
	}, 1)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errConflict(ErrorCodeInvalidState, "delivery %s could not be requeued", d.ID)
	}
	d, err = t.st.Deliveries().Get(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	return RetryDelivery200JSONResponse(deliveryOut(d)), nil
}

func (s *server) NotifyDeliveryUnsubscribe(ctx context.Context, req NotifyDeliveryUnsubscribeRequestObject) (NotifyDeliveryUnsubscribeResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := t.st.Deliveries().Get(ctx, req.DeliveryId.String())
	if err != nil {
		return nil, err
	}
	source := "host"
	var at = s.now()
	if req.Body != nil {
		if req.Body.Source != nil && *req.Body.Source != "" {
			source = string(*req.Body.Source)
		}
		if v := timeVal(req.Body.OccurredAt); !v.IsZero() {
			at = v
		}
	}
	res, err := s.recordUnsubscribe(ctx, t, d, source, at)
	if err != nil {
		return nil, err
	}
	return NotifyDeliveryUnsubscribe200JSONResponse(res), nil
}

func deliveryOut(v *store.Delivery) Delivery {
	out := Delivery{
		Id: uuidOf(v.ID), CampaignId: uuidPtrOf(v.CampaignID),
		VersionId: uuidOf(v.VersionID), SenderId: rid(v.SenderID),
		Lane: laneOut(v.Lane), Priority: i32(v.Priority),
		Status:    deliveryStatusOut(v.Status),
		Email:     openapiEmail(v.Email),
		EmailNorm: strPtr(v.EmailNorm), Name: strPtr(v.Name), Locale: strPtr(v.Locale),
		Vars: varsOut(v.Vars), TenantVars: varsOut(v.TenantVars),
		UnsubscribeUrl: strPtr(v.UnsubscribeURL),
		AttemptCount:   i32(v.AttemptCount), RetryGen: i32(v.RetryGen),
		NextAttemptAt: timePtr(v.NextAttemptAt),
		LeaseOwner:    strPtr(v.LeaseOwner), LeaseUntil: timePtr(v.LeaseUntil),
		LastError: strPtr(v.LastError),
		MessageId: strPtr(v.MessageID),
		SentAt:    timePtr(v.SentAt), FinishedAt: timePtr(v.FinishedAt),
		FirstOpenedAt: timePtr(v.FirstOpenedAt), FirstClickedAt: timePtr(v.FirstClickedAt),
		UnsubscribedAt: timePtr(v.UnsubscribedAt),
		CreatedAt:      timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
	if v.LastErrorClass != store.ErrorClassNone {
		out.LastErrorClass = ptr(errorClassOut(v.LastErrorClass))
	}
	if v.LastSMTPCode != 0 {
		out.LastSmtpCode = i32(v.LastSMTPCode)
	}
	return out
}

func attemptOut(v *store.DeliveryAttempt) DeliveryAttempt {
	out := DeliveryAttempt{
		Id: uuidOf(v.ID), DeliveryId: uuidOf(v.DeliveryID),
		AttemptNo: clampInt32(v.AttemptNo), RetryGen: i32(v.RetryGen),
		TransportId: ridPtr(v.TransportID),
		StartedAt:   timePtr(v.StartedAt), FinishedAt: timePtr(v.FinishedAt),
		EnhancedCode: strPtr(v.EnhancedCode), Error: strPtr(v.Error),
		CreatedAt: timePtr(v.CreatedAt),
	}
	if v.SMTPCode != 0 {
		out.SmtpCode = i32(v.SMTPCode)
	}
	if v.ErrorClass != store.ErrorClassNone {
		out.ErrorClass = ptr(errorClassOut(v.ErrorClass))
	}
	return out
}
