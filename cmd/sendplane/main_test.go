package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/store/memstore"
)

// testSendplane builds a minimal *sendplane.Sendplane good enough to mount
// (buildMux never calls into it beyond Handler()), so buildMux's routing can
// be tested without a real store or config file.
func testSendplane(t *testing.T) *sendplane.Sendplane {
	t.Helper()
	logger, err := newLogger(LogConfig{}, io.Discard)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	sp, err := sendplane.New(sendplane.Options{
		Store: memstore.New(),
		Auth:  newNoneAuthenticator(logger),
	})
	if err != nil {
		t.Fatalf("sendplane.New: %v", err)
	}
	return sp
}

func TestBuildMuxMountsConsoleUnderConfiguredPath(t *testing.T) {
	on := true
	cfg := &Config{Console: ConsoleConfig{Enabled: &on, Path: "/console"}}
	mux := buildMux(cfg, roleSet{control: true}, testSendplane(t))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/campaigns", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /console/campaigns = %d, want 200 (console SPA fallback)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" || ct == "application/json" {
		t.Errorf("Content-Type = %q, want the console's html shell, not the API", ct)
	}
}

func TestBuildMuxConsoleDoesNotShadowAPI(t *testing.T) {
	on := true
	cfg := &Config{Console: ConsoleConfig{Enabled: &on, Path: "/console"}}
	mux := buildMux(cfg, roleSet{control: true}, testSendplane(t))

	for _, p := range []string{"/api/v1/campaigns", "/healthz", "/t/abc"} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			// The console's own tests (cmd/sendplane/console) confirm it 404s
			// requests outside its base path; what matters here is that these
			// well-known API/tracking/health routes still reach sp.Handler()
			// rather than the console's SPA fallback, i.e. they must not come
			// back as the console's HTML shell.
			if rec.Code == http.StatusOK && rec.Header().Get("Content-Type") == "text/html; charset=utf-8" {
				t.Errorf("GET %s was served as the console's HTML shell, want sp.Handler()", p)
			}
		})
	}
}

func TestBuildMuxConsoleDisabled(t *testing.T) {
	off := false
	cfg := &Config{Console: ConsoleConfig{Enabled: &off, Path: "/console"}}
	mux := buildMux(cfg, roleSet{control: true}, testSendplane(t))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/campaigns", nil))
	// With no console.Handler registered under "/console/", the request
	// falls through to sp.Handler()'s "/" mount instead of the console SPA;
	// it must not come back as the console's shell.
	if rec.Header().Get("Content-Type") == "text/html; charset=utf-8" && rec.Code == http.StatusOK {
		t.Errorf("console.Enabled=false but /console/campaigns still served the console shell")
	}
}

func TestBuildMuxSenderOnlyMountsNoConsole(t *testing.T) {
	on := true
	cfg := &Config{Console: ConsoleConfig{Enabled: &on, Path: "/console"}}
	mux := buildMux(cfg, roleSet{sender: true}, testSendplane(t))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/campaigns", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("sender-only role served /console/campaigns with 200, want no console mount (roles.control is false)")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("sender-only /healthz = %d, want 200 (plain healthz still registered)", rec.Code)
	}
}
