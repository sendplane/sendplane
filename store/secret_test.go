package store

import (
	"bytes"
	"testing"
)

// AAD must be injective: two different contexts must never produce the same
// bytes, or a ciphertext bound to one would open under the other. The pairs
// below are the ones a separator-joined or plainly concatenated encoding gets
// wrong.
func TestSecretContextAADIsInjective(t *testing.T) {
	contexts := []SecretContext{
		{},
		{Purpose: "a"},
		{TenantID: "a"},
		{ObjectID: "a"},
		{Bind: "a"},
		// Plain concatenation: "ab" + "c" == "a" + "bc".
		{Purpose: "p", TenantID: "ab", ObjectID: "c"},
		{Purpose: "p", TenantID: "a", ObjectID: "bc"},
		// A separator inside a field.
		{Purpose: "p", TenantID: "a/b", ObjectID: "c"},
		{Purpose: "p", TenantID: "a", ObjectID: "b/c"},
		{Purpose: "p", TenantID: "a\x00b", ObjectID: "c"},
		{Purpose: "p", TenantID: "a", ObjectID: "b\x00c"},
		// A field that looks like a length prefix.
		{Purpose: "p", TenantID: "\x00\x00\x00\x01a", ObjectID: ""},
		{Purpose: "p", TenantID: "", ObjectID: "\x00\x00\x00\x01a"},
		{Purpose: "p\x00\x00\x00\x01a", TenantID: ""},
		// Fields shifted by one position.
		{Purpose: "x", TenantID: "y", ObjectID: "", Bind: ""},
		{Purpose: "", TenantID: "x", ObjectID: "y", Bind: ""},
		{Purpose: "", TenantID: "", ObjectID: "x", Bind: "y"},
		// The version tag as field content.
		{Purpose: secretAADVersion},
		{Purpose: "transport.password", TenantID: "t1", ObjectID: "sys:shared-a"},
		{Purpose: "transport.password", TenantID: "t1sys:", ObjectID: "shared-a"},
	}
	seen := map[string]SecretContext{}
	for _, sc := range contexts {
		k := string(sc.AAD())
		if prev, ok := seen[k]; ok {
			t.Fatalf("AAD collision: %+v and %+v both encode to %q", prev, sc, k)
		}
		seen[k] = sc
	}
}

func TestSecretContextAADIsVersionedAndStable(t *testing.T) {
	sc := SecretContext{Purpose: "p", TenantID: "t", ObjectID: "o"}
	if !bytes.Equal(sc.AAD(), sc.AAD()) {
		t.Fatal("AAD is not deterministic")
	}
	// The exact encoding is part of every stored ciphertext; changing it
	// makes every secret undecryptable, so it is pinned here.
	want := []byte("\x00\x00\x00\x13sendplane/secret/v1" +
		"\x00\x00\x00\x01p" + "\x00\x00\x00\x01t" + "\x00\x00\x00\x01o" + "\x00\x00\x00\x00")
	if got := sc.AAD(); !bytes.Equal(got, want) {
		t.Fatalf("AAD = %q, want %q", got, want)
	}
}

// The constructors bind to the row's own tenant and ID, and each column to a
// purpose of its own.
func TestSecretContextConstructors(t *testing.T) {
	tr := &Transport{ID: "id-1", TenantID: "t-1"}
	d := &SendingDomain{ID: "id-1", TenantID: "t-1"}
	b := &BounceMailbox{ID: "id-1", TenantID: "t-1"}
	p := &ProbeMailbox{ID: "id-1", TenantID: "t-1"}
	all := []SecretContext{
		TransportPasswordContext(tr), DomainDKIMKeyContext(d),
		BounceMailboxPasswordContext(b), ProbeMailboxPasswordContext(p),
		MailboxTestPasswordContext("t-1", "id-1"),
	}
	purposes := map[string]bool{}
	for _, sc := range all {
		if sc.TenantID != "t-1" || sc.ObjectID != "id-1" || sc.Bind != "" {
			t.Errorf("%s: context %+v does not name the row", sc.Purpose, sc)
		}
		if purposes[sc.Purpose] {
			t.Errorf("purpose %q used twice", sc.Purpose)
		}
		purposes[sc.Purpose] = true
	}

	// A delivery secret binds the recipient too, as a hash of the exact
	// Email the sender puts in RCPT TO. EmailNorm is not part of it.
	dl := &Delivery{ID: "id-1", TenantID: "t-1", Email: "a@example.com", EmailNorm: "a@example.com"}
	sc := DeliverySecretContext(dl)
	if sc.TenantID != "t-1" || sc.ObjectID != "id-1" {
		t.Errorf("delivery context %+v does not name the row", sc)
	}
	if purposes[sc.Purpose] {
		t.Errorf("purpose %q used twice", sc.Purpose)
	}
	// sha256("a@example.com"), pinned: it is part of every stored ciphertext.
	const wantBind = "sha256:08168cd80dfd534ab0f10af10f1303fe00af2d43ab5c1432360d137f8197e17a"
	if sc.Bind != wantBind {
		t.Errorf("delivery context Bind = %q, want %q", sc.Bind, wantBind)
	}
	other := *dl
	other.Email = "b@example.com"
	if DeliverySecretContext(&other).Bind == sc.Bind {
		t.Error("two recipients share a delivery secret binding")
	}
	// The same address after normalization is still another RCPT string.
	other.Email = "A@example.com"
	if DeliverySecretContext(&other).Bind == sc.Bind {
		t.Error("the binding normalizes the address; it must hash the RCPT bytes")
	}
	// email_norm is not what is sent, so it is not bound.
	other = *dl
	other.EmailNorm = "b@example.com"
	if DeliverySecretContext(&other).Bind != sc.Bind {
		t.Error("the binding depends on EmailNorm")
	}
}
