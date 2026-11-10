package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/prommetrics"
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

func TestParseFlagsMetricsListen(t *testing.T) {
	t.Run("default off", func(t *testing.T) {
		t.Setenv("SENDPLANE_METRICS_LISTEN", "")
		f, err := parseFlags(nil)
		if err != nil {
			t.Fatal(err)
		}
		if f.metricsListen != "" {
			t.Errorf("metricsListen = %q, want empty (disabled) by default", f.metricsListen)
		}
	})
	t.Run("flag", func(t *testing.T) {
		t.Setenv("SENDPLANE_METRICS_LISTEN", "")
		f, err := parseFlags([]string{"--metrics-listen=:9090"})
		if err != nil {
			t.Fatal(err)
		}
		if f.metricsListen != ":9090" {
			t.Errorf("metricsListen = %q, want :9090", f.metricsListen)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("SENDPLANE_METRICS_LISTEN", ":9191")
		f, err := parseFlags(nil)
		if err != nil {
			t.Fatal(err)
		}
		if f.metricsListen != ":9191" {
			t.Errorf("metricsListen = %q, want :9191 from SENDPLANE_METRICS_LISTEN", f.metricsListen)
		}
	})
	t.Run("flag overrides env", func(t *testing.T) {
		t.Setenv("SENDPLANE_METRICS_LISTEN", ":9191")
		f, err := parseFlags([]string{"--metrics-listen=:9292"})
		if err != nil {
			t.Fatal(err)
		}
		if f.metricsListen != ":9292" {
			t.Errorf("metricsListen = %q, want :9292", f.metricsListen)
		}
	})
}

func TestParseFlagsMetricsListenCollision(t *testing.T) {
	t.Setenv("SENDPLANE_METRICS_LISTEN", "")
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"--metrics-listen=:8080"}, true}, // --listen defaults to :8080
		{[]string{"--listen=:9090", "--metrics-listen=:9090"}, true},
		{[]string{"--listen=:9090", "--metrics-listen=0.0.0.0:9090"}, true},
		{[]string{"--listen=127.0.0.1:9090", "--metrics-listen=:9090"}, true},
		{[]string{"--listen=[::]:9090", "--metrics-listen=127.0.0.1:9090"}, true},
		{[]string{"--listen=127.0.0.1:9090", "--metrics-listen=10.0.0.1:9090"}, false},
		{[]string{"--listen=:8080", "--metrics-listen=:9090"}, false},
		{[]string{"--listen=:8080"}, false}, // metrics off: nothing to collide
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			_, err := parseFlags(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--metrics-listen") {
				t.Errorf("error %q does not name --metrics-listen", err)
			}
		})
	}
}

func testExporter(t *testing.T) *prommetrics.Exporter {
	t.Helper()
	logger, err := newLogger(LogConfig{}, io.Discard)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	e := prommetrics.New(prommetrics.Options{Registry: prometheus.NewRegistry(), Logger: logger})
	e.Count("sendplane_sender_claimed_total", 1, "lane", "bulk", "tenant", "t1")
	return e
}

func TestBuildMetricsMuxServesMetrics(t *testing.T) {
	mux := buildMetricsMux(testExporter(t))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `sendplane_sender_claimed_total{lane="bulk",tenant="t1"} 1`) {
		t.Errorf("GET /metrics body is missing the counter:\n%s", body)
	}

	for _, p := range []string{"/", "/healthz", "/api/v1/campaigns"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("metrics mux served GET %s with 200, want only /metrics", p)
		}
	}
}

// The main listener sits behind the Ingress; the metrics carry tenant IDs, so
// /metrics must never be reachable there, whatever the roles.
func TestBuildMuxDoesNotServeMetrics(t *testing.T) {
	on := true
	cfg := &Config{Console: ConsoleConfig{Enabled: &on, Path: "/console"}}
	for name, roles := range map[string]roleSet{
		"control": {control: true},
		"sender":  {sender: true},
		"all":     {control: true, sender: true, bounce: true},
	} {
		t.Run(name, func(t *testing.T) {
			mux := buildMux(cfg, roles, testSendplane(t))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "# TYPE") {
				t.Errorf("main mux GET /metrics = %d %q, want no metrics", rec.Code, rec.Body.String())
			}
		})
	}
}
