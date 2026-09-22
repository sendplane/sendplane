package api

import (
	"context"

	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
)

// Bounce mailboxes are the IMAP/POP3 accounts the poller reads DSNs and
// feedback reports from (architecture 10). They are a tenant resource like
// probe mailboxes, and the same secret rule applies: the password is
// writeOnly, encrypted on the way in and never echoed back (architecture 16).

func (s *server) ListBounceMailboxes(ctx context.Context, req ListBounceMailboxesRequestObject) (ListBounceMailboxesResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.BounceMailboxes().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]BounceMailbox, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, bounceMailboxOut(&res.Items[i]))
	}
	return ListBounceMailboxes200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) CreateBounceMailbox(ctx context.Context, req CreateBounceMailboxRequestObject) (CreateBounceMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	m := &store.BounceMailbox{}
	if err := s.applyBounceMailbox(ctx, m, BounceMailboxUpdate{
		Name: req.Body.Name, Address: req.Body.Address, Protocol: req.Body.Protocol,
		Host: req.Body.Host, Port: req.Body.Port, Tls: req.Body.Tls,
		Username: req.Body.Username, Password: req.Body.Password,
		Folder: req.Body.Folder, AfterProcess: req.Body.AfterProcess,
		Enabled: req.Body.Enabled,
	}); err != nil {
		return nil, err
	}
	if err := t.st.BounceMailboxes().Create(ctx, m); err != nil {
		return nil, err
	}
	return CreateBounceMailbox201JSONResponse(bounceMailboxOut(m)), nil
}

func (s *server) GetBounceMailbox(ctx context.Context, req GetBounceMailboxRequestObject) (GetBounceMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	m, err := t.st.BounceMailboxes().Get(ctx, req.MailboxId)
	if err != nil {
		return nil, err
	}
	return GetBounceMailbox200JSONResponse(bounceMailboxOut(m)), nil
}

func (s *server) UpdateBounceMailbox(ctx context.Context, req UpdateBounceMailboxRequestObject) (UpdateBounceMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("bounce mailbox", req.MailboxId); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	m, err := t.st.BounceMailboxes().Get(ctx, req.MailboxId)
	if err != nil {
		return nil, err
	}
	if err := s.applyBounceMailbox(ctx, m, *req.Body); err != nil {
		return nil, err
	}
	m.Version = req.Body.Version
	if err := t.st.BounceMailboxes().Update(ctx, m); err != nil {
		return nil, err
	}
	return UpdateBounceMailbox200JSONResponse(bounceMailboxOut(m)), nil
}

func (s *server) DeleteBounceMailbox(ctx context.Context, req DeleteBounceMailboxRequestObject) (DeleteBounceMailboxResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlatformWrite("bounce mailbox", req.MailboxId); err != nil {
		return nil, err
	}
	if err := t.st.BounceMailboxes().Delete(ctx, req.MailboxId); err != nil {
		return nil, err
	}
	return DeleteBounceMailbox204Response{}, nil
}

func (s *server) applyBounceMailbox(ctx context.Context, m *store.BounceMailbox, in BounceMailboxUpdate) error {
	if err := requireNonEmpty("name", in.Name); err != nil {
		return err
	}
	if err := requireNonEmpty("host", in.Host); err != nil {
		return err
	}
	if in.Port <= 0 || in.Port > 65535 {
		return errInvalid("port must be between 1 and 65535")
	}
	protocol, err := mailboxProtocolIn(in.Protocol)
	if err != nil {
		return err
	}
	mode, err := tlsModeIn(in.Tls)
	if err != nil {
		return err
	}
	// The poller has to be able to act on the policy, so it is validated here
	// rather than failing on every pass of a mailbox nobody can fix from the
	// API (internal/mailbox.ParseAction).
	action, err := mailbox.ParseAction(deref(in.AfterProcess))
	if err != nil {
		return errInvalid("after_process: %v", err)
	}
	if _, isMove := action.MoveFolder(); isMove && protocol == string(mailbox.ProtocolPOP3) {
		return errInvalid("after_process: POP3 cannot move a message to another folder")
	}
	pw, err := s.secret(ctx, in.Password, m.Password)
	if err != nil {
		return err
	}
	m.Name, m.Host, m.Port, m.TLS = in.Name, in.Host, int(in.Port), mode
	m.Address = deref(in.Address)
	m.Protocol = protocol
	m.Username = deref(in.Username)
	m.Password = pw
	m.Folder = deref(in.Folder)
	m.AfterProcess = string(action)
	m.Enabled = true
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}
	return nil
}

func mailboxProtocolIn(p *MailboxProtocol) (string, error) {
	if p == nil || *p == "" {
		return string(mailbox.ProtocolIMAP), nil
	}
	switch mailbox.Protocol(*p) {
	case mailbox.ProtocolIMAP, mailbox.ProtocolPOP3:
		return string(*p), nil
	}
	return "", errInvalid("unknown mailbox protocol %q", *p)
}

func bounceMailboxOut(v *store.BounceMailbox) BounceMailbox {
	protocol := MailboxProtocol(v.Protocol)
	if protocol == "" {
		protocol = MailboxProtocol(mailbox.ProtocolIMAP)
	}
	return BounceMailbox{
		Id: rid(v.ID), Shared: sharedOut(v.Shared), Name: v.Name, Address: strPtr(v.Address),
		Protocol: &protocol,
		Host:     v.Host, Port: clampInt32(v.Port), Tls: tlsModeOut(v.TLS),
		Username: strPtr(v.Username),
		// The mailbox password is writeOnly (architecture 16).
		HasPassword:  ptr(len(v.Password) > 0),
		Folder:       strPtr(v.Folder),
		AfterProcess: strPtr(v.AfterProcess),
		Enabled:      ptr(v.Enabled),
		Health:       mailboxHealthOut(v.Health),
		Version:      ptr(v.Version),
		CreatedAt:    timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
}
