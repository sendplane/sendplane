package render

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/sendplane/sendplane/store"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

const fixtureSubject = `{% t "welcome.title" %} - {{ recipient.name }}`

func readFixture(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func loadBundle(t testing.TB, name string) store.I18nBundle {
	t.Helper()
	b, warnings, err := ImportYAML([]byte(readFixture(t, name)))
	if err != nil {
		t.Fatalf("import %s: %v", name, err)
	}
	for _, w := range warnings {
		t.Logf("%s: %s", name, w)
	}
	return b
}

// fixture returns the MJML template and layout used by the golden tests.
func fixture(t testing.TB) (*store.Template, *store.Layout) {
	t.Helper()
	layout := &store.Layout{
		ID:       "layout-1",
		TenantID: "tenant-1",
		Name:     "default",
		Mode:     store.ContentMJML,
		Body:     readFixture(t, "layout.mjml"),
		I18n:     loadBundle(t, "layout.i18n.yaml"),
	}
	tpl := &store.Template{
		ID:            "template-1",
		TenantID:      "tenant-1",
		Name:          "welcome",
		LayoutID:      layout.ID,
		Subject:       fixtureSubject,
		Mode:          store.ContentMJML,
		Body:          readFixture(t, "template.mjml"),
		I18n:          loadBundle(t, "template.i18n.yaml"),
		DefaultLocale: "en",
	}
	return tpl, layout
}

// fixtureBindings is the recipient used by the golden tests.
func fixtureBindings() Bindings {
	return Bindings{
		Recipient: Recipient{
			Email:  "ada@example.com",
			Name:   "Ada",
			Locale: "ko-KR",
			Vars:   map[string]any{"nickname": "에이다"},
		},
		Vars: map[string]any{
			"order":  map[string]any{"id": "A-1001"},
			"orders": 3,
			"vip":    true,
		},
		Campaign:       Campaign{ID: "campaign-1", Name: "Welcome"},
		UnsubscribeURL: "https://t.example.com/t/u/abc123",
	}
}

// golden compares got against testdata/<name>, rewriting it under -update.
func golden(t testing.TB, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run go test -update)", name, err)
	}
	if got != string(want) {
		t.Errorf("golden %s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}
