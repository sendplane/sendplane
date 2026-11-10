package prommetrics

import (
	"bytes"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestExporter(t *testing.T) (*Exporter, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	e := New(Options{
		Registry: prometheus.NewRegistry(),
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	})
	return e, &logs
}

func droppedCount(t *testing.T, e *Exporter, name string) float64 {
	t.Helper()
	return testutil.ToFloat64(e.dropped.WithLabelValues(name))
}

func TestCountExposition(t *testing.T) {
	e, _ := newTestExporter(t)
	e.Count("sendplane_sender_claimed_total", 3, "lane", "bulk", "tenant", "t1")
	e.Count("sendplane_sender_claimed_total", 2, "lane", "bulk", "tenant", "t1")
	e.Count("sendplane_sender_claimed_total", 1, "lane", "transactional", "tenant", "t2")
	e.Count("sendplane_sender_conn_closed_total", 1)

	want := `
# HELP sendplane_sender_claimed_total sendplane counter sendplane_sender_claimed_total (see the emitting package for its meaning).
# TYPE sendplane_sender_claimed_total counter
sendplane_sender_claimed_total{lane="bulk",tenant="t1"} 5
sendplane_sender_claimed_total{lane="transactional",tenant="t2"} 1
# HELP sendplane_sender_conn_closed_total sendplane counter sendplane_sender_conn_closed_total (see the emitting package for its meaning).
# TYPE sendplane_sender_conn_closed_total counter
sendplane_sender_conn_closed_total 1
`
	if err := testutil.GatherAndCompare(e.Registry(), strings.NewReader(want),
		"sendplane_sender_claimed_total", "sendplane_sender_conn_closed_total"); err != nil {
		t.Fatal(err)
	}
}

func TestObserveExpositionAndBuckets(t *testing.T) {
	var logs bytes.Buffer
	e := New(Options{
		Registry:       prometheus.NewRegistry(),
		Buckets:        map[string][]float64{"custom_seconds": {1, 2}},
		DefaultBuckets: []float64{0.5},
		Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
	})
	e.Observe("custom_seconds", 1.5, "transport", "x")
	e.Observe("plain_seconds", 0.25)

	want := `
# HELP custom_seconds sendplane histogram custom_seconds (see the emitting package for its meaning).
# TYPE custom_seconds histogram
custom_seconds_bucket{transport="x",le="1"} 0
custom_seconds_bucket{transport="x",le="2"} 1
custom_seconds_bucket{transport="x",le="+Inf"} 1
custom_seconds_sum{transport="x"} 1.5
custom_seconds_count{transport="x"} 1
# HELP plain_seconds sendplane histogram plain_seconds (see the emitting package for its meaning).
# TYPE plain_seconds histogram
plain_seconds_bucket{le="0.5"} 1
plain_seconds_bucket{le="+Inf"} 1
plain_seconds_sum 0.25
plain_seconds_count 1
`
	if err := testutil.GatherAndCompare(e.Registry(), strings.NewReader(want),
		"custom_seconds", "plain_seconds"); err != nil {
		t.Fatal(err)
	}
}

func TestLabelOrderIndependence(t *testing.T) {
	e, _ := newTestExporter(t)
	e.Count("c_total", 1, "status", "sent", "class", "none")
	e.Count("c_total", 2, "class", "none", "status", "sent")
	e.Observe("h_seconds", 1, "transport", "a", "class", "none")
	e.Observe("h_seconds", 1, "class", "none", "transport", "a")

	if got := testutil.ToFloat64(e.families["c_total"].counter.WithLabelValues("sent", "none")); got != 3 {
		t.Errorf("c_total{status=sent,class=none} = %v, want 3", got)
	}
	if n := testutil.CollectAndCount(e.families["h_seconds"].histogram); n != 1 {
		t.Errorf("h_seconds has %d series, want 1", n)
	}
	if got := droppedCount(t, e, "c_total") + droppedCount(t, e, "h_seconds"); got != 0 {
		t.Errorf("dropped = %v, want 0", got)
	}
}

func TestDrops(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(e *Exporter)
		emit   func(e *Exporter)
		metric string // the dropped{metric} label; "" means "m"
	}{
		{"invalid metric name", nil, func(e *Exporter) { e.Count("bad.name", 1) }, "bad.name"},
		{"metric name starting with a digit", nil, func(e *Exporter) { e.Count("1bad", 1) }, "1bad"},
		{"invalid label name", nil, func(e *Exporter) { e.Count("m", 1, "bad-key", "v") }, ""},
		{"reserved label name", nil, func(e *Exporter) { e.Count("m", 1, "__name", "v") }, ""},
		{"duplicate label name", nil, func(e *Exporter) { e.Count("m", 1, "a", "1", "a", "2") }, ""},
		{"odd label count on first call", nil, func(e *Exporter) { e.Count("m", 1, "a") }, ""},
		{"odd label count later", func(e *Exporter) { e.Count("m", 1, "a", "1") }, func(e *Exporter) { e.Count("m", 1, "a", "1", "b") }, ""},
		{"fewer keys than first call", func(e *Exporter) { e.Count("m", 1, "a", "1", "b", "2") }, func(e *Exporter) { e.Count("m", 1, "a", "1") }, ""},
		{"more keys than first call", func(e *Exporter) { e.Count("m", 1, "a", "1") }, func(e *Exporter) { e.Count("m", 1, "a", "1", "b", "2") }, ""},
		{"different keys", func(e *Exporter) { e.Count("m", 1, "a", "1") }, func(e *Exporter) { e.Count("m", 1, "b", "1") }, ""},
		{"repeated key standing in for a missing one", func(e *Exporter) { e.Count("m", 1, "a", "1", "b", "2") }, func(e *Exporter) { e.Count("m", 1, "a", "1", "a", "2") }, ""},
		{"negative delta", nil, func(e *Exporter) { e.Count("m", -1) }, ""},
		{"counter then histogram", func(e *Exporter) { e.Count("m", 1) }, func(e *Exporter) { e.Observe("m", 1) }, ""},
		{"histogram then counter", func(e *Exporter) { e.Observe("m", 1) }, func(e *Exporter) { e.Count("m", 1) }, ""},
		{"histogram label le", nil, func(e *Exporter) { e.Observe("m", 1, "le", "x") }, ""},
		{"unsorted buckets", func(e *Exporter) { e.buckets = map[string][]float64{"m": {2, 1}} }, func(e *Exporter) { e.Observe("m", 1, "a", "x") }, ""},
		{"NaN observation", nil, func(e *Exporter) { e.Observe("m", math.NaN()) }, ""},
		{"invalid UTF-8 label value", nil, func(e *Exporter) { e.Count("m", 1, "a", "\xff") }, ""},
		{"collides with a registered collector", func(e *Exporter) {
			e.Registry().MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "m", Help: "h"}))
		}, func(e *Exporter) { e.Count("m", 1) }, ""},
		{"collides with the dropped counter's labels", nil, func(e *Exporter) { e.Count(MetricDropped, 1, "other", "x") }, MetricDropped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, logs := newTestExporter(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			name := tc.metric
			if name == "" {
				name = "m"
			}
			before := droppedCount(t, e, name)
			tc.emit(e) // must not panic
			tc.emit(e)
			if got := droppedCount(t, e, name) - before; got != 2 {
				t.Errorf("dropped{metric=%q} rose by %v, want 2", name, got)
			}
			if n := strings.Count(logs.String(), "dropping samples"); n != 1 {
				t.Errorf("logged %d drop warnings, want exactly 1:\n%s", n, logs.String())
			}
			// The scrape still succeeds.
			if _, err := e.Registry().Gather(); err != nil {
				t.Errorf("Gather after drop: %v", err)
			}
		})
	}
}

func TestDropsDoNotBreakTheFirstFamily(t *testing.T) {
	e, _ := newTestExporter(t)
	e.Count("m", 1, "a", "1")
	e.Count("m", 1, "b", "1") // dropped
	e.Count("m", 1, "a", "1")
	if got := testutil.ToFloat64(e.families["m"].counter.WithLabelValues("1")); got != 2 {
		t.Errorf("m{a=1} = %v, want 2", got)
	}
}

func TestOddLabelsOnFirstCallDoNotPinTheFamily(t *testing.T) {
	e, _ := newTestExporter(t)
	e.Count("m", 1, "a")      // dropped, not remembered
	e.Count("m", 1, "a", "1") // creates the family
	if got := testutil.ToFloat64(e.families["m"].counter.WithLabelValues("1")); got != 1 {
		t.Errorf("m{a=1} = %v, want 1", got)
	}
}

func TestSharedRegistryReusesFamilies(t *testing.T) {
	reg := prometheus.NewRegistry()
	a := New(Options{Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	b := New(Options{Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a.Count("m_total", 1, "k", "v")
	b.Count("m_total", 1, "k", "v")
	if got := testutil.ToFloat64(a.families["m_total"].counter.WithLabelValues("v")); got != 2 {
		t.Errorf("m_total = %v, want 2 (both exporters on one family)", got)
	}
}

func TestDefaultRegistryHasRuntimeCollectors(t *testing.T) {
	e := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	e.Count("sendplane_x_total", 1)
	body := scrape(t, e.Handler())
	for _, want := range []string{"go_goroutines", "sendplane_x_total 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

func TestHandlerServesExposition(t *testing.T) {
	e, _ := newTestExporter(t)
	e.Count("sendplane_bounce_handled_total", 1, "type", "hard", "status", "bounced")
	e.Count("bad.name", 1)
	body := scrape(t, e.Handler())
	for _, want := range []string{
		`sendplane_bounce_handled_total{status="bounced",type="hard"} 1`,
		`sendplane_metrics_dropped_total{metric="bad.name"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q:\n%s", want, body)
		}
	}
}

func TestConcurrentFirstUse(t *testing.T) {
	e, _ := newTestExporter(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				e.Count("c_total", 1, "a", "x", "b", "y")
			} else {
				e.Count("c_total", 1, "b", "y", "a", "x")
			}
			e.Observe("h_seconds", 0.1)
		}(i)
	}
	wg.Wait()
	if got := testutil.ToFloat64(e.families["c_total"].counter.With(prometheus.Labels{"a": "x", "b": "y"})); got != 32 {
		t.Errorf("c_total = %v, want 32", got)
	}
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape status %d: %s", resp.StatusCode, b)
	}
	return string(b)
}

func BenchmarkCount(b *testing.B) {
	e := New(Options{Registry: prometheus.NewRegistry(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e.Count("sendplane_sender_processed_total", 1, "status", "sent", "class", "none")
		}
	})
}
