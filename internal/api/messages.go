package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sendplane/sendplane/store"
)

// maxTransactionalRecipients mirrors the maxItems of MessageRequest.to. One
// call fans out to one delivery per recipient, so the cap is what keeps a
// single request from turning into a campaign.
const maxTransactionalRecipients = 1000

// Transactional idempotency reuses RecipientChunkRepo, which is keyed by
// (campaign_id, key). A transactional send has no campaign and the repository
// rejects an empty campaign ID, so it files its chunks under a sentinel that
// no real campaign can have: campaign IDs are UUIDs, and this one starts with
// an underscore, the same convention store.SystemTenantID uses.
const (
	msgChunkCampaign = "_transactional"
	msgChunkPrefix   = "msg:"
)

// SendMessage creates one transactional delivery per recipient and answers
// 202: the SMTP work happens in the sender (architecture 7).
func (s *server) SendMessage(ctx context.Context, req SendMessageRequestObject) (SendMessageResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	in := *req.Body
	if len(in.To) == 0 {
		return nil, errInvalid("to must name at least one recipient")
	}
	if len(in.To) > maxTransactionalRecipients {
		return nil, errInvalid("to has %d recipients, the maximum is %d",
			len(in.To), maxTransactionalRecipients)
	}
	if in.Headers != nil && len(*in.Headers) > 0 {
		// store.Delivery has no place to keep per-message headers, so
		// accepting them would mean silently dropping them at send time.
		return nil, errInvalid(
			"headers are not supported yet: a delivery has no header storage, " +
				"put the values in vars and render them into the template instead")
	}

	version, err := s.resolveVersion(ctx, t, in.TemplateId, in.VersionId)
	if err != nil {
		return nil, err
	}
	snd, err := t.st.Senders().Get(ctx, idOf(in.SenderId))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errInvalid("sender %s does not exist", in.SenderId)
		}
		return nil, err
	}

	key := strings.TrimSpace(deref(req.Params.IdempotencyKey))
	chunkKey := ""
	if key != "" {
		chunkKey = msgChunkPrefix + key
		prev, err := t.st.RecipientChunks().Get(ctx, msgChunkCampaign, chunkKey)
		switch {
		case err == nil && prev.State == store.ChunkCompleted:
			return s.replayMessage(ctx, t, key, version.ID, in.To)
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}

	settings, err := settingsFor(ctx, t, s.now())
	if err != nil {
		return nil, err
	}
	now := s.now()

	deliveries := make([]store.Delivery, 0, len(in.To))
	items := make([]MessageResultItem, 0, len(in.To))
	seen := map[string]bool{}
	for i, r := range in.To {
		norm, err := store.NormalizeEmail(string(r.Email))
		if err != nil {
			return nil, errInvalid("to[%d].email: %v", i, err)
		}
		if err := noCRLF("to[].name", deref(r.Name)); err != nil {
			return nil, err
		}
		if seen[norm] {
			return nil, errInvalid("to[%d] repeats the address %s", i, norm)
		}
		seen[norm] = true

		status := store.DeliveryQueued
		if settings.SuppressionEnabled {
			ok, _, err := t.st.Suppressions().IsSuppressed(ctx, norm, now)
			if err != nil {
				return nil, err
			}
			if ok {
				// A suppressed address is reported, not refused: one bad
				// recipient must not fail the other 999 (ADR-0008).
				status = store.DeliverySuppressed
			}
		}

		locale := deref(r.Locale)
		if locale == "" {
			locale = deref(in.DefaultLocale)
		}
		d := store.Delivery{
			ID:        transactionalID(t.id, key, norm, i),
			TenantID:  t.id,
			VersionID: version.ID,
			SenderID:  snd.ID,
			Lane:      store.LaneTransactional,
			Priority:  int(deref(in.Priority)),
			Status:    status,
			Email:     string(r.Email), EmailNorm: norm,
			Name: deref(r.Name), Locale: locale,
			Vars:          mergeVars(varsOf(in.Vars), varsOf(r.Vars)),
			NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if status == store.DeliverySuppressed {
			d.FinishedAt = now
		}
		deliveries = append(deliveries, d)
		items = append(items, MessageResultItem{
			DeliveryId: uuidOf(d.ID), Email: r.Email, Status: deliveryStatusOut(status),
		})
	}

	if _, err := t.st.Deliveries().InsertBatch(ctx, deliveries); err != nil {
		return nil, err
	}
	if chunkKey != "" {
		if err := t.st.RecipientChunks().Put(ctx, &store.RecipientChunk{
			CampaignID: msgChunkCampaign, Key: chunkKey, State: store.ChunkCompleted,
			Accepted: len(deliveries), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return nil, err
		}
	}
	return SendMessage202JSONResponse(MessageResult{
		VersionId: uuidOf(version.ID), Deliveries: items,
	}), nil
}

// replayMessage answers a repeated Idempotency-Key with the original result.
//
// RecipientChunk stores counts, not IDs, so the delivery IDs are recomputed
// the same way they were minted: transactionalID is a UUIDv5 of the tenant,
// the key, the normalized address and the position. That is also what makes
// the insert itself idempotent, because a replay that got past the chunk check
// would write the same primary keys.
func (s *server) replayMessage(ctx context.Context, t *tenant, key, versionID string, to []MessageRecipient) (SendMessageResponseObject, error) {
	items := make([]MessageResultItem, 0, len(to))
	for i, r := range to {
		norm, err := store.NormalizeEmail(string(r.Email))
		if err != nil {
			return nil, errInvalid("to[%d].email: %v", i, err)
		}
		id := transactionalID(t.id, key, norm, i)
		status := DeliveryStatusQueued
		if d, err := t.st.Deliveries().Get(ctx, id); err == nil {
			status = deliveryStatusOut(d.Status)
			versionID = d.VersionID
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		items = append(items, MessageResultItem{
			DeliveryId: uuidOf(id), Email: r.Email, Status: status,
		})
	}
	return SendMessage202JSONResponse(MessageResult{
		VersionId: uuidOf(versionID), Deliveries: items, IdempotentReplay: ptr(true),
	}), nil
}

// transactionalID derives the delivery ID. With an Idempotency-Key it is a
// deterministic UUIDv5 so a replay names the same rows; without one it is a
// fresh UUIDv7, which keeps the ID time-ordered like every other row.
func transactionalID(tenantID, key, emailNorm string, index int) string {
	if key == "" {
		return store.NewID()
	}
	// The position is part of the name as well, so the derivation does not
	// depend on the duplicate-address check above staying in place.
	name := "urn:sendplane:message:" + tenantID + ":" + key + ":" +
		strconv.Itoa(index) + ":" + emailNorm
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String()
}

// resolveVersion implements the spec's rule: version_id pins an exact version,
// template_id uses whatever the template has published right now.
func (s *server) resolveVersion(ctx context.Context, t *tenant, templateID, versionID *UUID) (*store.MessageVersion, error) {
	if versionID != nil {
		v, err := t.st.Versions().Get(ctx, idOf(*versionID))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errInvalid("message version %s does not exist", *versionID)
			}
			return nil, err
		}
		return v, nil
	}
	if templateID == nil {
		return nil, errInvalid("one of template_id or version_id is required")
	}
	tpl, err := t.st.Templates().Get(ctx, idOf(*templateID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errInvalid("template %s does not exist", *templateID)
		}
		return nil, err
	}
	if tpl.PublishedVersionID == "" {
		return nil, newErr(422, ErrorCodeTemplateNotPublished,
			"template %s has never been published", tpl.ID)
	}
	return t.st.Versions().Get(ctx, tpl.PublishedVersionID)
}

// mergeVars layers the recipient's own variables over the request-level ones.
func mergeVars(base, own map[string]any) map[string]any {
	if len(base) == 0 && len(own) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(own))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range own {
		out[k] = v
	}
	return out
}
