package api

import (
	"context"
	"net/http"
	"testing"
)

// TestBounceMailboxCRUD covers the tenant resource the bounce poller reads its
// mailbox list from: create, read, replace, delete, and the two rules that are
// specific to it — the password never leaves the store, and after_process has
// to be something internal/mailbox can act on.
func TestBounceMailboxCRUD(t *testing.T) {
	e := newEnv(t)

	created := decodeInto[BounceMailbox](t, e.do(http.MethodPost, "/api/v1/bounce-mailboxes",
		BounceMailboxInput{
			Name: "bounces", Address: ptr("bounce@example.com"),
			Host: "imap.example.com", Port: 993, Tls: ptr(TLSMode("tls")),
			Username: ptr("bounces"), Password: ptr("s3cret"),
			Folder: ptr("INBOX"), AfterProcess: ptr("move:Handled"),
		}), http.StatusCreated)

	if created.Id == nil {
		t.Fatal("create returned no id")
	}
	if created.Protocol == nil || *created.Protocol != Imap {
		t.Errorf("protocol = %v, want the imap default", created.Protocol)
	}
	if created.HasPassword == nil || !*created.HasPassword {
		t.Error("has_password = false, want true")
	}

	// The response carries no password at all, and the store holds the
	// ciphertext rather than what the caller sent (architecture 16).
	raw := decodeInto[map[string]any](t, e.do(http.MethodGet,
		"/api/v1/bounce-mailboxes/"+created.Id.String(), nil), http.StatusOK)
	if _, ok := raw["password"]; ok {
		t.Error("the response body contains a password")
	}
	stored, err := e.st.BounceMailboxes().Get(context.Background(), created.Id.String())
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if string(stored.Password) == "s3cret" {
		t.Error("the password was stored in the clear")
	}
	if string(xorAll(stored.Password)) != "s3cret" {
		t.Errorf("stored password = %q, want the encrypted secret", stored.Password)
	}
	if stored.AfterProcess != "move:Handled" {
		t.Errorf("after_process = %q", stored.AfterProcess)
	}

	list := decodeInto[BounceMailboxList](t, e.do(http.MethodGet,
		"/api/v1/bounce-mailboxes", nil), http.StatusOK)
	if len(list.Items) != 1 || *list.Items[0].Id != *created.Id {
		t.Fatalf("list = %+v, want the one mailbox", list.Items)
	}

	// Omitting the password on a replace keeps the stored one, so a GET body
	// can be PUT back without wiping the secret.
	updated := decodeInto[BounceMailbox](t, e.do(http.MethodPut,
		"/api/v1/bounce-mailboxes/"+created.Id.String(), BounceMailboxUpdate{
			Name: "bounces", Host: "imap.example.com", Port: 993,
			Tls: ptr(TLSMode("tls")), Username: ptr("bounces"),
			AfterProcess: ptr("delete"), Enabled: ptr(false),
			Version: *created.Version,
		}), http.StatusOK)
	if *updated.Version != *created.Version+1 {
		t.Errorf("version = %d, want it bumped", *updated.Version)
	}
	if updated.HasPassword == nil || !*updated.HasPassword {
		t.Error("omitting password cleared the stored one")
	}
	if updated.Enabled == nil || *updated.Enabled {
		t.Error("enabled = true, want the disabled value")
	}

	// A disabled mailbox drops out of what the poller reads.
	enabled, err := e.st.BounceMailboxes().ListEnabled(context.Background())
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(enabled) != 0 {
		t.Errorf("ListEnabled = %d, want the disabled mailbox excluded", len(enabled))
	}

	// A stale version is a 409, like every other optimistic-concurrency write.
	w := e.do(http.MethodPut, "/api/v1/bounce-mailboxes/"+created.Id.String(),
		BounceMailboxUpdate{
			Name: "bounces", Host: "imap.example.com", Port: 993,
			Version: *created.Version,
		})
	decodeError(t, w, http.StatusConflict, ErrorCodeVersionConflict)

	e.do(http.MethodDelete, "/api/v1/bounce-mailboxes/"+created.Id.String(), nil)
	w = e.do(http.MethodGet, "/api/v1/bounce-mailboxes/"+created.Id.String(), nil)
	decodeError(t, w, http.StatusNotFound, ErrorCodeNotFound)
}

func TestBounceMailboxValidation(t *testing.T) {
	e := newEnv(t)

	for _, tc := range []struct {
		name string
		in   BounceMailboxInput
	}{
		{"unknown after-process", BounceMailboxInput{
			Name: "b", Host: "h", Port: 993, AfterProcess: ptr("archive"),
		}},
		{"move on pop3", BounceMailboxInput{
			Name: "b", Host: "h", Port: 995, Protocol: ptr(Pop3),
			AfterProcess: ptr("move:Handled"),
		}},
		{"move with no folder", BounceMailboxInput{
			Name: "b", Host: "h", Port: 993, AfterProcess: ptr("move:"),
		}},
		{"port out of range", BounceMailboxInput{Name: "b", Host: "h", Port: 70000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := e.do(http.MethodPost, "/api/v1/bounce-mailboxes", tc.in)
			decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
		})
	}
}
