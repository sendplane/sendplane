// Package console serves the embedded Vue ops console SPA
// (web/apps/console/dist, copied here by `make console-sync`) as an
// http.Handler mounted under a configurable base path.
//
// In a fresh clone dist/ holds only the committed placeholder dist/index.html,
// so `go build ./cmd/sendplane` succeeds without Node or pnpm installed; the
// real console ships once `make console-sync` (or the Docker build's
// node:22-alpine stage, see deploy/dev/Dockerfile) has copied
// web/apps/console/dist over it. See cmd/sendplane/README.md.
package console

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed dist/*
var distFS embed.FS

// extraContentTypes covers extensions Vite's output uses that Go's mime
// package does not reliably map on every platform (mime.TypeByExtension
// falls back to the OS's mime.types on some systems, which may not list
// these).
var extraContentTypes = map[string]string{
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ico":         "image/x-icon",
	".map":         "application/json",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
}

// asset is one file served from the embedded dist/ tree, preloaded into
// memory once at Handler construction: the whole console is a few MB at
// most, and it turns every request into a map lookup with no filesystem or
// path-traversal concerns (request paths never touch the embedded fs.FS,
// only this map's keys).
type asset struct {
	data        []byte
	contentType string
	// immutable marks dist/assets/*, which Vite names with a content hash
	// (e.g. assets/index-abc123.js): safe to cache forever.
	immutable bool
}

// Handler serves the embedded console SPA mounted at basePath (for example
// "/console"). A request under basePath for a known dist/ file — most
// importantly a content-hashed dist/assets/* entry — gets long-lived,
// immutable caching. Every other GET/HEAD under basePath, including unknown
// routes, serves dist/index.html with Cache-Control: no-store, so client-side
// routing (vue-router's history mode) works on a hard refresh and a new
// deploy is never stuck behind a cached shell. Requests outside basePath are
// not found; other methods get 405.
//
// basePath defaults to "/console" when empty. It is normalized to start with
// "/" and, unless it is "/" itself, not end with one.
func Handler(basePath string) http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Can't happen: dist/* is always embedded, guaranteed by the
		// committed placeholder dist/index.html.
		panic("console: fs.Sub(dist): " + err.Error())
	}
	return newHandler(basePath, sub)
}

// newHandler is Handler's implementation, taking the dist tree as an fs.FS
// so tests can exercise it (SPA fallback, cache headers, a placeholder-only
// tree) against an in-memory fstest.MapFS instead of whatever the real
// embedded dist/ happens to hold when the test binary was built.
func newHandler(basePath string, fsys fs.FS) http.Handler {
	base := normalizeBasePath(basePath)
	assets := loadAssets(fsys)
	index, haveIndex := assets["index.html"]

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		rest, ok := stripBasePath(base, r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}

		if a, found := assets[rest]; found && rest != "index.html" {
			serveAsset(w, r, a)
			return
		}
		if !haveIndex {
			// Should only happen if dist/ was embedded empty, which the
			// committed placeholder prevents; kept as a clear error instead
			// of a panic in case someone ever strips it.
			http.Error(w, "console not built: cmd/sendplane/console/dist has no index.html", http.StatusInternalServerError)
			return
		}
		serveAsset(w, r, index)
	})
}

// normalizeBasePath ensures a leading slash and, except for "/" itself, no
// trailing one, so stripBasePath can do plain string comparisons.
func normalizeBasePath(p string) string {
	if p == "" {
		p = "/console"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// stripBasePath reports whether reqPath falls under base and, if so, returns
// the cleaned, slash-trimmed remainder to look up in the assets map (""
// means the base path itself, i.e. the SPA's index).
func stripBasePath(base, reqPath string) (rest string, ok bool) {
	switch {
	case base == "/":
		rest = reqPath
	case reqPath == base:
		rest = ""
	case strings.HasPrefix(reqPath, base+"/"):
		rest = reqPath[len(base):]
	default:
		return "", false
	}
	cleaned := path.Clean("/" + rest)
	if cleaned == "/" {
		return "", true
	}
	return strings.TrimPrefix(cleaned, "/"), true
}

// serveAsset writes a's headers (and, for GET, its body).
func serveAsset(w http.ResponseWriter, r *http.Request, a asset) {
	h := w.Header()
	if a.contentType != "" {
		h.Set("Content-Type", a.contentType)
	}
	if a.immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Covers index.html itself and every SPA-fallback response: the
		// shell must always be revalidated, or a new deploy's hashed asset
		// references can point at files a stale cached shell never learns
		// about.
		h.Set("Cache-Control", "no-store")
	}
	h.Set("Content-Length", strconv.Itoa(len(a.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(a.data)
}

// loadAssets reads every file in fsys into memory once.
func loadAssets(fsys fs.FS) map[string]asset {
	out := map[string]asset{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort: skip an unreadable embedded entry rather than fail the whole tree
		}
		data, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return nil //nolint:nilerr // same as above
		}
		out[p] = asset{
			data:        data,
			contentType: contentTypeFor(p, data),
			immutable:   strings.HasPrefix(p, "assets/"),
		}
		return nil
	})
	return out
}

func contentTypeFor(name string, data []byte) string {
	ext := path.Ext(name)
	if ct, ok := extraContentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return http.DetectContentType(data)
}
