package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// builtTree simulates a real `make console-sync` output: an index.html shell
// plus a content-hashed asset under assets/, the way Vite lays out a build.
func builtTree() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(`<!doctype html><html><body><div id="app"></div></body></html>`)},
		"assets/index-abc123.js": {Data: []byte(`console.log("sendplane console")`)},
		"favicon.ico":            {Data: []byte("\x00\x00\x01\x00")},
	}
}

func TestHandlerAssetGetsImmutableLongCache(t *testing.T) {
	h := newHandler("/console", builtTree())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/assets/index-abc123.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `console.log("sendplane console")` {
		t.Errorf("body = %q, want the asset content unchanged", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a javascript type", ct)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("Cache-Control = %q, want immutable long-lived caching for a hashed asset", cc)
	}
}

func TestHandlerIndexIsNoStore(t *testing.T) {
	h := newHandler("/console", builtTree())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store for index.html", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestHandlerSPAFallbackServesIndexFor200(t *testing.T) {
	h := newHandler("/console", builtTree())

	for _, p := range []string{"/console/campaigns", "/console/templates/abc-123/edit", "/console"} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (SPA fallback)", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), `<div id="app">`) {
				t.Errorf("body = %q, want the index.html shell", rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store on SPA fallback", cc)
			}
		})
	}
}

func TestHandlerOutsideBasePathNotFound(t *testing.T) {
	h := newHandler("/console", builtTree())

	for _, p := range []string{"/api/v1/settings", "/healthz", "/t/abc", "/consoleX"} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 for a request outside the base path", rec.Code)
			}
		})
	}
}

func TestHandlerRejectsNonGetHead(t *testing.T) {
	h := newHandler("/console", builtTree())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/console/", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandlerRootBasePath(t *testing.T) {
	h := newHandler("/", builtTree())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable for a hashed asset served at the root base path", cc)
	}
}

// TestHandlerPlaceholderTree exercises the exact shape cmd/sendplane/console
// ships in a fresh clone (dist/ holding only the committed placeholder
// index.html, see dist/index.html), so `go build ./cmd/sendplane` works
// without Node installed. It uses an in-memory fs.FS rather than the real
// embedded distFS so the assertion holds regardless of whether
// `make console-sync` has already replaced dist/ on disk in this checkout.
func TestHandlerPlaceholderTree(t *testing.T) {
	placeholder := fstest.MapFS{
		"index.html": {Data: []byte("console not built; run make web-build console-sync")},
	}
	h := newHandler("/console", placeholder)

	for _, p := range []string{"/console", "/console/", "/console/campaigns", "/console/assets/whatever.js"} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 even with only the placeholder present", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "console not built") {
				t.Errorf("body = %q, want the placeholder message", rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
		})
	}
}

// TestHandlerPlaceholderTreeMissingIndex documents the (currently
// unreachable in practice, since dist/index.html is committed) case of an
// empty dist/: it should answer 500, not panic or serve an empty body as if
// it were the SPA shell.
func TestHandlerPlaceholderTreeMissingIndex(t *testing.T) {
	h := newHandler("/console", fstest.MapFS{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when dist/ has no index.html at all", rec.Code)
	}
}

// TestRealEmbeddedHandlerServesSomething is a light smoke test of the actual
// package-level Handler (and thus the real //go:embed distFS), unlike the
// tests above which use newHandler against a controlled fs.FS. It only
// checks that *something* well-formed comes back — either the committed
// placeholder or a real `make console-sync` build — since which one is on
// disk depends on build order, not on this package's behavior.
func TestRealEmbeddedHandlerServesSomething(t *testing.T) {
	h := Handler("/console")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("body is empty, want the placeholder or a real console build")
	}
}
