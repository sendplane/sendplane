package platformtest

import (
	"context"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

// The shared-content half of the overlay (ADR-0018). The cases create rows in
// the system tenant of whatever database the suite runs against, so each uses
// keys no real deployment would, and removes its templates and layouts again
// when it ends: a shared template left behind would show up in every tenant of
// a developer's database. Message versions have no Delete (they are immutable
// and removed only by retention); the few these cases write are unreachable
// once their template is gone, because versions are never listed across
// tenants.

// uniqueKey is a content key no other run and no real deployment uses.
func uniqueKey(prefix string) string {
	id := store.NewID()
	return prefix + "-" + id[len(id)-12:]
}

func createSharedTemplate(t *testing.T, sys store.Store, key string, shared bool) *store.Template {
	t.Helper()
	ctx := context.Background()
	tpl := &store.Template{
		Name: "shared " + key, Key: key, Shared: shared,
		Uses:    []store.UseKind{store.UseTransactional},
		Subject: "Welcome", Mode: store.ContentHTML, Body: "<p>hi</p>",
	}
	must(t, "Create system template", sys.Templates().Create(ctx, tpl))
	t.Cleanup(func() { _ = sys.Templates().Delete(context.Background(), tpl.ID) })
	return tpl
}

func testSharedTemplates(t *testing.T, inner, p store.Provider) {
	ctx := context.Background()
	sys := systemStore(t, p)
	key := uniqueKey("welcome")
	shared := createSharedTemplate(t, sys, key, true)
	private := createSharedTemplate(t, sys, uniqueKey("private"), false)

	ten, tenantID := tenantStore(t, p)
	id := func(v *store.Template) string { return v.ID }

	// Read-through: listed, readable by ID and by key, reporting where it
	// lives. The unshared system template is invisible.
	list, err := ten.Templates().List(ctx, store.Page{Limit: store.MaxPageLimit})
	must(t, "tenant List", err)
	if !hasID(list.Items, id, shared.ID) {
		t.Fatal("a tenant's template list does not contain the shared template")
	}
	if hasID(list.Items, id, private.ID) {
		t.Fatal("a tenant's template list contains an unshared system template")
	}
	got, err := ten.Templates().Get(ctx, shared.ID)
	must(t, "tenant Get shared", err)
	eq(t, "shared tenant id", got.TenantID, store.SystemTenantID)
	eq(t, "shared flag", got.Shared, true)
	eq(t, "shared uses", len(got.Uses), 1)
	mustBe(t, "tenant Get unshared", errGet(ten.Templates().Get(ctx, private.ID)), store.ErrNotFound)
	byKey, err := ten.Templates().GetByKey(ctx, key)
	must(t, "tenant GetByKey shared", err)
	eq(t, "GetByKey finds the shared one", byKey.ID, shared.ID)
	mustBe(t, "tenant GetByKey unshared", errGet(ten.Templates().GetByKey(ctx, private.Key)), store.ErrNotFound)

	// Read-only in the tenant.
	got.Subject = "hijacked"
	mustBe(t, "tenant Update shared", ten.Templates().Update(ctx, got), store.ErrReadOnly)
	got.Shared = false
	mustBe(t, "tenant Update shared, flag cleared", ten.Templates().Update(ctx, got), store.ErrReadOnly)
	mustBe(t, "tenant Delete shared", ten.Templates().Delete(ctx, shared.ID), store.ErrReadOnly)
	mustBe(t, "tenant Create shared", ten.Templates().Create(ctx, &store.Template{
		Name: "x", Shared: true, Subject: "s", Mode: store.ContentHTML,
	}), store.ErrInvalid)

	// Nothing was copied into the tenant by any of those reads.
	raw, err := inner.ForTenant(ctx, tenantID)
	must(t, "inner ForTenant", err)
	own, err := raw.Templates().List(ctx, store.Page{Limit: store.MaxPageLimit})
	must(t, "inner List", err)
	eq(t, "rows in the tenant after reading shared ones", len(own.Items), 0)

	// An override: the tenant's own template with the shared key shadows it.
	shared.PublishedVersionID = store.NewID()
	must(t, "publish marker on the shared template", sys.Templates().Update(ctx, shared))
	override := &store.Template{
		Name: "mine", Key: key, Subject: "Mine", Mode: store.ContentHTML, Body: "<p>mine</p>",
		OverriddenFromVersion: "an-older-version",
	}
	must(t, "Create override", ten.Templates().Create(ctx, override))
	byKey, err = ten.Templates().GetByKey(ctx, key)
	must(t, "GetByKey with an override", err)
	eq(t, "GetByKey is own first", byKey.ID, override.ID)
	eq(t, "override is marked", byKey.Overridden, true)
	eq(t, "override sees the shared version", byKey.SharedPublishedVersionID, shared.PublishedVersionID)
	viaGet, err := ten.Templates().Get(ctx, override.ID)
	must(t, "Get override", err)
	eq(t, "Get marks the override", viaGet.Overridden, true)

	list, err = ten.Templates().List(ctx, store.Page{Limit: store.MaxPageLimit})
	must(t, "List with an override", err)
	if hasID(list.Items, id, shared.ID) {
		t.Fatal("an overridden shared template is still listed")
	}
	found := false
	for i := range list.Items {
		if list.Items[i].ID == override.ID {
			found = true
			eq(t, "listed override is marked", list.Items[i].Overridden, true)
			eq(t, "listed override shared version", list.Items[i].SharedPublishedVersionID,
				shared.PublishedVersionID)
		}
	}
	if !found {
		t.Fatal("the override is not listed")
	}
	// The computed fields are not persisted by a write-back.
	viaGet.Name = "renamed"
	must(t, "Update override", ten.Templates().Update(ctx, viaGet))
	rawBack, err := raw.Templates().Get(ctx, override.ID)
	must(t, "inner Get override", err)
	eq(t, "Overridden is not stored", rawBack.Overridden, false)
	eq(t, "shared version is not stored", rawBack.SharedPublishedVersionID, "")

	// The shared template is still the system tenant's, untouched.
	sysBack, err := sys.Templates().Get(ctx, shared.ID)
	must(t, "system Get", err)
	eq(t, "system subject", sysBack.Subject, "Welcome")
	eq(t, "system view is not annotated", sysBack.Overridden, false)

	// Deleting the override returns the tenant to the shared template.
	must(t, "Delete override", ten.Templates().Delete(ctx, override.ID))
	byKey, err = ten.Templates().GetByKey(ctx, key)
	must(t, "GetByKey after deleting the override", err)
	eq(t, "back to the shared template", byKey.ID, shared.ID)

	// Unsharing takes it away from every tenant at once.
	sysBack.Shared = false
	must(t, "unshare", sys.Templates().Update(ctx, sysBack))
	mustBe(t, "Get after unsharing", errGet(ten.Templates().Get(ctx, shared.ID)), store.ErrNotFound)
	mustBe(t, "GetByKey after unsharing", errGet(ten.Templates().GetByKey(ctx, key)), store.ErrNotFound)
}

func testSharedLayouts(t *testing.T, inner, p store.Provider) {
	ctx := context.Background()
	sys := systemStore(t, p)
	key := uniqueKey("base")
	l := &store.Layout{Name: "base", Key: key, Shared: true, Mode: store.ContentHTML,
		Body: "<div>{{ content }}</div>"}
	must(t, "Create shared layout", sys.Layouts().Create(ctx, l))
	t.Cleanup(func() { _ = sys.Layouts().Delete(context.Background(), l.ID) })
	hidden := &store.Layout{Name: "hidden", Key: uniqueKey("hidden"), Mode: store.ContentHTML,
		Body: "{{ content }}"}
	must(t, "Create unshared layout", sys.Layouts().Create(ctx, hidden))
	t.Cleanup(func() { _ = sys.Layouts().Delete(context.Background(), hidden.ID) })

	ten, tenantID := tenantStore(t, p)
	id := func(v *store.Layout) string { return v.ID }
	list, err := ten.Layouts().List(ctx, store.Page{Limit: store.MaxPageLimit})
	must(t, "tenant List layouts", err)
	if !hasID(list.Items, id, l.ID) || hasID(list.Items, id, hidden.ID) {
		t.Fatal("a tenant's layout list must hold the shared layout and not the unshared one")
	}
	got, err := ten.Layouts().Get(ctx, l.ID)
	must(t, "tenant Get shared layout", err)
	eq(t, "layout tenant", got.TenantID, store.SystemTenantID)
	mustBe(t, "tenant Get unshared layout", errGet(ten.Layouts().Get(ctx, hidden.ID)), store.ErrNotFound)
	got.Body = "{{ content }} hijacked"
	mustBe(t, "tenant Update shared layout", ten.Layouts().Update(ctx, got), store.ErrReadOnly)
	mustBe(t, "tenant Delete shared layout", ten.Layouts().Delete(ctx, l.ID), store.ErrReadOnly)
	mustBe(t, "tenant Create shared layout", ten.Layouts().Create(ctx, &store.Layout{
		Name: "x", Shared: true, Mode: store.ContentHTML, Body: "{{ content }}",
	}), store.ErrInvalid)

	own := &store.Layout{Name: "mine", Key: key, Mode: store.ContentHTML, Body: "<main>{{ content }}</main>"}
	must(t, "Create layout override", ten.Layouts().Create(ctx, own))
	byKey, err := ten.Layouts().GetByKey(ctx, key)
	must(t, "layout GetByKey", err)
	eq(t, "layout GetByKey is own first", byKey.ID, own.ID)
	eq(t, "layout override is marked", byKey.Overridden, true)
	list, err = ten.Layouts().List(ctx, store.Page{Limit: store.MaxPageLimit})
	must(t, "tenant List layouts with an override", err)
	if hasID(list.Items, id, l.ID) {
		t.Fatal("an overridden shared layout is still listed")
	}

	raw, err := inner.ForTenant(ctx, tenantID)
	must(t, "inner ForTenant", err)
	rows, err := raw.Layouts().List(ctx, store.Page{Limit: store.MaxPageLimit})
	must(t, "inner List layouts", err)
	eq(t, "only the explicit override is in the tenant", len(rows.Items), 1)

	must(t, "Delete layout override", ten.Layouts().Delete(ctx, own.ID))
	byKey, err = ten.Layouts().GetByKey(ctx, key)
	must(t, "layout GetByKey after delete", err)
	eq(t, "back to the shared layout", byKey.ID, l.ID)
}

func testSharedVersions(t *testing.T, p store.Provider) {
	ctx := context.Background()
	sys := systemStore(t, p)
	shared := createSharedTemplate(t, sys, uniqueKey("v"), true)
	private := createSharedTemplate(t, sys, uniqueKey("pv"), false)

	sv := &store.MessageVersion{TemplateID: shared.ID, SubjectTpl: "shared", HTMLTpl: "<p>s</p>",
		CreatedAt: time.Now()}
	must(t, "system Create version", sys.Versions().Create(ctx, sv))
	pv := &store.MessageVersion{TemplateID: private.ID, SubjectTpl: "private", HTMLTpl: "<p>p</p>"}
	must(t, "system Create private version", sys.Versions().Create(ctx, pv))

	ten, _ := tenantStore(t, p)
	got, err := ten.Versions().Get(ctx, sv.ID)
	must(t, "tenant Get shared version", err)
	eq(t, "version subject", got.SubjectTpl, "shared")
	eq(t, "version tenant", got.TenantID, store.SystemTenantID)
	// Any system version is readable by ID (the sender must keep reading what
	// an in-flight delivery was queued with), but never listed.
	_, err = ten.Versions().Get(ctx, pv.ID)
	must(t, "tenant Get a system version by ID", err)
	res, err := ten.Versions().ListByTemplate(ctx, shared.ID, store.Page{Limit: 10})
	must(t, "tenant ListByTemplate shared", err)
	eq(t, "shared versions listed", len(res.Items), 1)
	res, err = ten.Versions().ListByTemplate(ctx, private.ID, store.Page{Limit: 10})
	must(t, "tenant ListByTemplate unshared", err)
	eq(t, "unshared versions are not listed", len(res.Items), 0)

	mustBe(t, "tenant Create a version of a shared template", ten.Versions().Create(ctx,
		&store.MessageVersion{TemplateID: shared.ID, SubjectTpl: "x", HTMLTpl: "x"}), store.ErrReadOnly)
	own := &store.MessageVersion{TemplateID: store.NewID(), SubjectTpl: "own", HTMLTpl: "o"}
	must(t, "tenant Create own version", ten.Versions().Create(ctx, own))
	back, err := ten.Versions().Get(ctx, own.ID)
	must(t, "tenant Get own version", err)
	eq(t, "own version subject", back.SubjectTpl, "own")
	mustBe(t, "unknown version", errGet(ten.Versions().Get(ctx, store.NewID())), store.ErrNotFound)
}

// testSharedContentWithoutCatalog: shared templates are rows, not
// configuration, so the overlay must be in place even when the operator
// configured no platform resources at all.
func testSharedContentWithoutCatalog(t *testing.T, inner store.Provider) {
	ctx := context.Background()
	p := store.WithPlatform(inner, store.PlatformCatalog{}, nil, time.Now)
	sys := systemStore(t, p)
	shared := createSharedTemplate(t, sys, uniqueKey("nocat"), true)
	ten, _ := tenantStore(t, p)
	got, err := ten.Templates().GetByKey(ctx, shared.Key)
	must(t, "GetByKey without a catalog", err)
	eq(t, "found the shared template", got.ID, shared.ID)
	if store.WithPlatform(p, store.PlatformCatalog{}, nil, time.Now) != p {
		t.Fatal("wrapping an overlay again must return it unchanged")
	}
}
