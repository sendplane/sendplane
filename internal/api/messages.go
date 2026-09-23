package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sendplane/sendplane/internal/sender"
	"github.com/sendplane/sendplane/store"
)

// customHeaders validates the caller's `headers` against the very whitelist
// the sender applies on the wire (architecture 16), so a name it would refuse
// is a 422 here instead of a delivery that fails at send time. Returning nil
// for an empty map keeps the stored delivery free of an empty object.
func customHeaders(in *map[string]string) (map[string]string, error) {
	if in == nil || len(*in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(*in))
	for name, value := range *in {
		if err := sender.ValidateCustomHeader(name, value); err != nil {
			return nil, errInvalid("headers: %v", err)
		}
		out[name] = value
	}
	return out, nil
}

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
	if err := refuseSystemTenantSend(t, "send a message"); err != nil {
		return nil, err
	}
	in := *req.Body
	if len(in.To) == 0 {
		return nil, errInvalid("to must name at least one recipient")
	}
	if len(in.To) > maxTransactionalRecipients {
		return nil, errInvalid("to has %d recipients, the maximum is %d",
			len(in.To), maxTransactionalRecipients)
	}
	headers, err := customHeaders(in.Headers)
	if err != nil {
		return nil, err
	}

	version, tpl, err := s.resolveVersion(ctx, t, in.TemplateId, in.TemplateKey, in.VersionId)
	if err != nil {
		return nil, err
	}
	snd, err := t.st.Senders().Get(ctx, string(in.SenderId))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errInvalid("sender %s does not exist", in.SenderId)
		}
		return nil, err
	}

	// The tenant attributes this send carries, through the host's hook, then
	// the two platform checks: may this sender be used for transactional mail,
	// and do the variables satisfy a shared sender's From templates
	// (ADR-0017). Both run before a single delivery row is written.
	tenantVars, err := s.tenantVars(ctx, t, in.TenantVars)
	if err != nil {
		return nil, err
	}
	if err := s.checkSenderUse(ctx, t, snd.ID, store.UseTransactional, tenantVars); err != nil {
		return nil, err
	}
	if err := s.checkTemplateUse(ctx, t, tpl, store.UseTransactional, tenantVars); err != nil {
		return nil, err
	}
	if err := s.checkSharedFrom(snd.ID, snd.Shared, tenantVars); err != nil {
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
			Vars: mergeVars(varsOf(in.Vars), varsOf(r.Vars)),
			// A transactional delivery has no campaign to inherit the tenant
			// attributes from, so it carries them itself (ADR-0017).
			TenantVars:    tenantVars,
			Headers:       headers,
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
// template_id or template_key uses whatever that template has published right
// now. A key resolves to the tenant's own template first (its override), then
// to the shared one, at request time (ADR-0018).
//
// The template comes back too, for the template-use policy; it is nil for a
// pinned version whose template no longer exists.
func (s *server) resolveVersion(ctx context.Context, t *tenant, templateID *UUID, templateKey *ContentKey,
	versionID *UUID) (*store.MessageVersion, *store.Template, error) {
	if versionID != nil {
		if templateKey != nil {
			return nil, nil, errInvalid("version_id pins a version; do not also name a template_key")
		}
		v, err := s.visibleVersion(ctx, t, idOf(*versionID))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || isNotFoundAPI(err) {
				return nil, nil, errInvalid("message version %s does not exist", *versionID)
			}
			return nil, nil, err
		}
		tpl, err := s.templateOfVersion(ctx, t, v)
		if err != nil {
			return nil, nil, err
		}
		return v, tpl, nil
	}
	tpl, err := s.templateByRef(ctx, t, templateID, templateKey)
	if err != nil {
		return nil, nil, err
	}
	if tpl == nil {
		return nil, nil, errInvalid("one of template_id, template_key or version_id is required")
	}
	if tpl.PublishedVersionID == "" {
		return nil, nil, newErr(422, ErrorCodeTemplateNotPublished,
			"template %s has never been published", tpl.ID)
	}
	v, err := t.st.Versions().Get(ctx, tpl.PublishedVersionID)
	if err != nil {
		return nil, nil, err
	}
	return v, tpl, nil
}

// isNotFoundAPI reports whether err is an apiError that already says 404.
func isNotFoundAPI(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.code == ErrorCodeNotFound
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
