package api

import (
	"context"

	"github.com/sendplane/sendplane/store"
)

// --- suppressions ------------------------------------------------------

func (s *server) ListSuppressions(ctx context.Context, req ListSuppressionsRequestObject) (ListSuppressionsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.Suppressions().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]Suppression, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, suppressionOut(&res.Items[i]))
	}
	return ListSuppressions200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

// GetSuppression answers 404 for an entry whose expires_at has passed, which
// is what IsSuppressed already decides, so the API and the send path can never
// disagree about whether an address is suppressed.
func (s *server) GetSuppression(ctx context.Context, req GetSuppressionRequestObject) (GetSuppressionResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	norm, err := store.NormalizeEmail(string(req.Email))
	if err != nil {
		return nil, errInvalid("email: %v", err)
	}
	ok, sup, err := t.st.Suppressions().IsSuppressed(ctx, norm, s.now())
	if err != nil {
		return nil, err
	}
	if !ok || sup == nil {
		return nil, errNotFound("suppression", norm)
	}
	return GetSuppression200JSONResponse(suppressionOut(sup)), nil
}

func (s *server) UpsertSuppression(ctx context.Context, req UpsertSuppressionRequestObject) (UpsertSuppressionResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	norm, err := store.NormalizeEmail(string(req.Email))
	if err != nil {
		return nil, errInvalid("email: %v", err)
	}
	reason, err := suppressionReasonIn(req.Body.Reason)
	if err != nil {
		return nil, err
	}
	sup := &store.Suppression{
		EmailNorm: norm, Reason: reason,
		SourceDeliveryID: idPtrOf(req.Body.SourceDeliveryId),
		ExpiresAt:        timeVal(req.Body.ExpiresAt),
		CreatedAt:        s.now(),
	}
	if err := t.st.Suppressions().Upsert(ctx, sup); err != nil {
		return nil, err
	}
	return UpsertSuppression200JSONResponse(suppressionOut(sup)), nil
}

func (s *server) DeleteSuppression(ctx context.Context, req DeleteSuppressionRequestObject) (DeleteSuppressionResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	norm, err := store.NormalizeEmail(string(req.Email))
	if err != nil {
		return nil, errInvalid("email: %v", err)
	}
	if err := t.st.Suppressions().Delete(ctx, norm); err != nil {
		return nil, err
	}
	return DeleteSuppression204Response{}, nil
}

func suppressionOut(v *store.Suppression) Suppression {
	return Suppression{
		EmailNorm:        openapiEmail(v.EmailNorm),
		Reason:           SuppressionReason(v.Reason),
		SourceDeliveryId: uuidPtrOf(v.SourceDeliveryID),
		CreatedAt:        v.CreatedAt.UTC(),
		ExpiresAt:        timePtr(v.ExpiresAt),
	}
}

// --- bounces -----------------------------------------------------------

func (s *server) ListBounces(ctx context.Context, req ListBouncesRequestObject) (ListBouncesResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	var want map[store.BounceType]bool
	if req.Params.Type != nil && len(*req.Params.Type) > 0 {
		want = map[store.BounceType]bool{}
		for _, v := range *req.Params.Type {
			bt, err := bounceTypeIn(v)
			if err != nil {
				return nil, err
			}
			want[bt] = true
		}
	}
	res, err := t.st.Bounces().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	// BounceRepo.List has no type filter, so the page is filtered here. The
	// cursor still advances by a whole store page, which is why a filtered
	// page may hold fewer items than `limit` while next_cursor is set: a
	// client follows next_cursor until it is absent, as the spec says.
	items := make([]BounceEvent, 0, len(res.Items))
	for i := range res.Items {
		if want != nil && !want[res.Items[i].Type] {
			continue
		}
		items = append(items, bounceOut(&res.Items[i]))
	}
	return ListBounces200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) GetBounce(ctx context.Context, req GetBounceRequestObject) (GetBounceResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	b, err := t.st.Bounces().Get(ctx, req.BounceId.String())
	if err != nil {
		return nil, err
	}
	return GetBounce200JSONResponse(bounceOut(b)), nil
}

func bounceOut(v *store.BounceEvent) BounceEvent {
	out := BounceEvent{
		Id: uuidOf(v.ID), DeliveryId: uuidPtrOf(v.DeliveryID),
		Type: bounceTypeOut(v.Type), Source: BounceSource(v.Source), Verified: v.Verified,
		Recipient: strPtr(v.Recipient), EmailNorm: strPtr(v.EmailNorm),
		SmtpStatus: strPtr(v.SMTPStatus), DiagnosticCode: strPtr(v.DiagnosticCode),
		MessageId:  strPtr(v.MessageID),
		ReceivedAt: timePtr(v.ReceivedAt), CreatedAt: timePtr(v.CreatedAt),
	}
	if len(v.Raw) > 0 {
		out.Raw = strPtr(string(v.Raw))
	}
	return out
}

// --- events ------------------------------------------------------------

func (s *server) ListEvents(ctx context.Context, req ListEventsRequestObject) (ListEventsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	status := store.OutboxStatus("")
	if req.Params.Status != nil {
		st, err := outboxStatusIn(*req.Params.Status)
		if err != nil {
			return nil, err
		}
		status = st
	}
	res, err := t.st.Outbox().List(ctx, status, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	var want map[string]bool
	if req.Params.Type != nil && len(*req.Params.Type) > 0 {
		want = map[string]bool{}
		for _, v := range *req.Params.Type {
			want[v] = true
		}
	}
	items := make([]OutboxEvent, 0, len(res.Items))
	for i := range res.Items {
		if want != nil && !want[res.Items[i].Type] {
			continue
		}
		items = append(items, outboxOut(&res.Items[i]))
	}
	return ListEvents200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) ListDeadLetterEvents(ctx context.Context, req ListDeadLetterEventsRequestObject) (ListDeadLetterEventsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.Outbox().List(ctx, store.OutboxFailed, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]OutboxEvent, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, outboxOut(&res.Items[i]))
	}
	return ListDeadLetterEvents200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) GetEvent(ctx context.Context, req GetEventRequestObject) (GetEventResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	ev, err := t.st.Outbox().Get(ctx, req.EventId.String())
	if err != nil {
		return nil, err
	}
	return GetEvent200JSONResponse(outboxOut(ev)), nil
}

// ReplayEvent returns a dead letter to pending so the dispatcher picks it up
// again (architecture 12).
func (s *server) ReplayEvent(ctx context.Context, req ReplayEventRequestObject) (ReplayEventResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	ev, err := t.st.Outbox().Get(ctx, req.EventId.String())
	if err != nil {
		return nil, err
	}
	if ev.Status == store.OutboxDelivered {
		return nil, errConflict(ErrorCodeInvalidState, "event %s was already delivered", ev.ID)
	}
	// Reset, not MarkFailed: a replay is what an operator does after fixing
	// the cause, so the event gets a full attempt budget again rather than
	// one fewer than a fresh one.
	if err := t.st.Outbox().Reset(ctx, ev.ID, s.now()); err != nil {
		return nil, err
	}
	ev, err = t.st.Outbox().Get(ctx, ev.ID)
	if err != nil {
		return nil, err
	}
	return ReplayEvent202JSONResponse(outboxOut(ev)), nil
}

func outboxStatusIn(v OutboxStatus) (store.OutboxStatus, error) {
	switch store.OutboxStatus(v) {
	case store.OutboxPending, store.OutboxDelivered, store.OutboxFailed:
		return store.OutboxStatus(v), nil
	}
	return "", errInvalid("unknown outbox status %q", v)
}

func outboxOut(v *store.OutboxEvent) OutboxEvent {
	out := OutboxEvent{
		Id: uuidOf(v.ID), Type: v.Type, Status: OutboxStatus(v.Status),
		Attempts: i32(v.Attempts), LastError: strPtr(v.LastError),
		NextAttemptAt: timePtr(v.NextAttemptAt),
		CreatedAt:     timePtr(v.CreatedAt), DeliveredAt: timePtr(v.DeliveredAt),
	}
	if len(v.Payload) > 0 {
		if m, err := jsonMap(v.Payload); err == nil {
			out.Payload = &m
		}
	}
	return out
}
