// Package prommetrics is a Prometheus backend for host.Metrics (architecture
// 12): pass an *Exporter as sendplane.Options.Metrics and serve its Handler on
// a scrape endpoint.
//
// sendplane's packages emit metrics by name through Count/Observe and never
// declare them up front, so the Exporter creates each metric family lazily —
// a CounterVec on the first Count of a name, a HistogramVec on the first
// Observe — and takes the family's label names from that first call. The
// emitting packages own the names (internal/sender/metrics.go,
// internal/bounce) and keep each name's label key set fixed; a call that
// breaks that rule is dropped rather than allowed to panic or corrupt the
// family. Every dropped sample increments sendplane_metrics_dropped_total
// {metric="<name>"} and the first drop of a name is logged once.
//
// NewQueueDepthCollector adds the delivery backlog gauge read from the store.
package prommetrics

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sendplane/sendplane/host"
)

// MetricDropped counts samples the Exporter refused, by metric name.
const MetricDropped = "sendplane_metrics_dropped_total"

// DefaultBuckets are the histogram buckets used when neither Options.Buckets
// nor Options.DefaultBuckets names one. Every histogram sendplane emits today
// is a duration in seconds, from a template render (milliseconds) to an SMTP
// transaction or a limiter wait (up to tens of seconds).
var DefaultBuckets = []float64{
	.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60,
}

// Options configures New. The zero value is usable.
type Options struct {
	// Registry receives every metric family. Nil means a new registry with
	// the Go runtime and process collectors already registered.
	Registry *prometheus.Registry
	// Buckets overrides the histogram buckets per metric name.
	Buckets map[string][]float64
	// DefaultBuckets is used for a histogram Buckets does not name. Nil means
	// the package's DefaultBuckets.
	DefaultBuckets []float64
	// Logger receives the once-per-name warning for dropped samples. Nil
	// means slog.Default().
	Logger *slog.Logger
}

// Exporter implements host.Metrics on a Prometheus registry. It is safe for
// concurrent use.
type Exporter struct {
	reg            *prometheus.Registry
	buckets        map[string][]float64
	defaultBuckets []float64
	logger         *slog.Logger
	dropped        *prometheus.CounterVec

	mu       sync.RWMutex
	families map[string]*family

	warned sync.Map // metric name -> struct{}; first drop logged
}

var _ host.Metrics = (*Exporter)(nil)

type kind uint8

const (
	kindCounter kind = iota + 1
	kindHistogram
)

func (k kind) String() string {
	if k == kindCounter {
		return "counter"
	}
	return "histogram"
}

// family is one lazily created metric. Exactly one of counter/histogram is
// set unless broken is, in which case every later sample of the name is
// dropped without retrying the registration.
type family struct {
	kind      kind
	keys      []string
	counter   *prometheus.CounterVec
	histogram *prometheus.HistogramVec
	broken    error
}

// New returns an Exporter. It never fails: a problem with one metric (a bad
// name, bad buckets, a registration conflict) shows up as dropped samples of
// that metric, not as an error at startup.
func New(o Options) *Exporter {
	reg := o.Registry
	if reg == nil {
		reg = prometheus.NewRegistry()
		reg.MustRegister(
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	def := o.DefaultBuckets
	if def == nil {
		def = DefaultBuckets
	}
	e := &Exporter{
		reg:            reg,
		buckets:        o.Buckets,
		defaultBuckets: def,
		logger:         logger,
		families:       map[string]*family{},
	}
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricDropped,
		Help: "Samples the Prometheus adapter refused (invalid name or labels, label set or type mismatch, registration error), by metric name.",
	}, []string{"metric"})
	if c, err := register(reg, dropped); err == nil {
		e.dropped = c.(*prometheus.CounterVec)
	} else {
		// Still count, just unexported; the warning says why.
		e.dropped = dropped
		logger.Warn("prommetrics: cannot register the dropped-samples counter", "error", err)
	}
	return e
}

// Registry returns the registry the Exporter writes to, for registering
// additional collectors such as NewQueueDepthCollector.
func (e *Exporter) Registry() *prometheus.Registry { return e.reg }

// Handler serves the registry in the Prometheus exposition format. A failing
// collector does not fail the scrape: the rest of the metrics are still
// served and the error is logged.
func (e *Exporter) Handler() http.Handler {
	return promhttp.HandlerFor(e.reg, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(e.logger.Handler(), slog.LevelWarn),
		ErrorHandling: promhttp.ContinueOnError,
		Registry:      e.reg,
	})
}

// Count adds delta to the counter name. A negative delta is dropped.
func (e *Exporter) Count(name string, delta int64, labels ...string) {
	if delta < 0 {
		e.drop(name, "negative counter delta")
		return
	}
	f := e.family(name, kindCounter, labels)
	if f == nil {
		return
	}
	var buf [8]string
	vals, ok := f.values(buf[:0], labels)
	if !ok {
		e.drop(name, "label keys differ from the first call")
		return
	}
	c, err := f.counter.GetMetricWithLabelValues(vals...)
	if err != nil {
		e.drop(name, err.Error())
		return
	}
	c.Add(float64(delta))
}

// Observe records value in the histogram name. NaN is dropped: one NaN would
// turn the histogram's _sum into NaN for the life of the process.
func (e *Exporter) Observe(name string, value float64, labels ...string) {
	if math.IsNaN(value) {
		e.drop(name, "NaN observation")
		return
	}
	f := e.family(name, kindHistogram, labels)
	if f == nil {
		return
	}
	var buf [8]string
	vals, ok := f.values(buf[:0], labels)
	if !ok {
		e.drop(name, "label keys differ from the first call")
		return
	}
	h, err := f.histogram.GetMetricWithLabelValues(vals...)
	if err != nil {
		e.drop(name, err.Error())
		return
	}
	h.Observe(value)
}

// family returns the usable family for name, creating it on first use. It
// returns nil, having already counted the drop, when the sample cannot be
// recorded.
func (e *Exporter) family(name string, k kind, labels []string) *family {
	e.mu.RLock()
	f, ok := e.families[name]
	e.mu.RUnlock()
	if !ok {
		f = e.create(name, k, labels)
	}
	switch {
	case f.broken != nil:
		e.drop(name, f.broken.Error())
		return nil
	case f.kind != k:
		e.drop(name, fmt.Sprintf("used as a %s, first used as a %s", k, f.kind))
		return nil
	}
	return f
}

func (e *Exporter) create(name string, k kind, labels []string) *family {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f, ok := e.families[name]; ok {
		return f // created by a concurrent first call
	}
	f := &family{kind: k}
	f.keys, f.broken = labelKeys(labels)
	if f.broken == nil && !validMetricName(name) {
		f.broken = fmt.Errorf("invalid metric name")
	}
	if f.broken == nil {
		f.broken = e.build(f, name)
	}
	// A family whose first call was bad stays broken: retrying the
	// registration on every sample would be a slow path on a hot path, and
	// the name is a constant in code, so the next call would be just as bad.
	// An odd label count is the exception — it says nothing about the name
	// or the key set — so it is not remembered.
	if errors.Is(f.broken, errOddLabels) {
		return f
	}
	e.families[name] = f
	return f
}

// build constructs and registers the vec. client_golang panics on some bad
// inputs (unsorted buckets); recover turns those into a broken family rather
// than a crashed sender.
func (e *Exporter) build(f *family, name string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("creating %s: %v", f.kind, r)
		}
	}()
	var c prometheus.Collector
	switch f.kind {
	case kindCounter:
		c = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: name,
			Help: "sendplane counter " + name + " (see the emitting package for its meaning).",
		}, f.keys)
	case kindHistogram:
		b, ok := e.buckets[name]
		if !ok {
			b = e.defaultBuckets
		}
		opts := prometheus.HistogramOpts{
			Name:    name,
			Help:    "sendplane histogram " + name + " (see the emitting package for its meaning).",
			Buckets: b,
		}
		// HistogramVec only builds a histogram when it first sees a label
		// combination, so its panics would surface later, inside Observe.
		// Check what they check now: the reserved "le" label here, and the
		// buckets by building one unlabelled histogram under the recover.
		for _, k := range f.keys {
			if k == "le" {
				return fmt.Errorf(`label name "le" is reserved in histograms`)
			}
		}
		_ = prometheus.NewHistogram(opts)
		c = prometheus.NewHistogramVec(opts, f.keys)
	}
	got, err := register(e.reg, c)
	if err != nil {
		return err
	}
	switch v := got.(type) {
	case *prometheus.CounterVec:
		f.counter = v
	case *prometheus.HistogramVec:
		f.histogram = v
	}
	if f.counter == nil && f.histogram == nil {
		return fmt.Errorf("registry already holds a different collector named %s", name)
	}
	if (f.kind == kindCounter) != (f.counter != nil) {
		return fmt.Errorf("registry already holds %s as another type", name)
	}
	return nil
}

// register registers c, or returns the collector already registered under
// the same descriptors — two Exporters sharing one registry then share the
// families instead of failing.
func register(reg *prometheus.Registry, c prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(c)
	if err == nil {
		return c, nil
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		return are.ExistingCollector, nil
	}
	return nil, err
}

// values appends to dst the label values of labels in f.keys order. It
// reports false when labels does not carry exactly f.keys.
func (f *family) values(dst, labels []string) ([]string, bool) {
	if len(labels) != 2*len(f.keys) {
		return nil, false
	}
	// Fast path: the caller uses the first call's order, which is the case
	// for every call site in sendplane.
	inOrder := true
	for i, k := range f.keys {
		if labels[2*i] != k {
			inOrder = false
			break
		}
	}
	if inOrder {
		for i := range f.keys {
			dst = append(dst, labels[2*i+1])
		}
		return dst, true
	}
	for _, k := range f.keys {
		found := false
		for j := 0; j < len(labels); j += 2 {
			if labels[j] == k {
				dst = append(dst, labels[j+1])
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return dst, true
}

var errOddLabels = errors.New("odd number of label arguments")

// labelKeys validates a first call's labels and returns its keys.
func labelKeys(labels []string) ([]string, error) {
	if len(labels)%2 != 0 {
		return nil, errOddLabels
	}
	keys := make([]string, 0, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		k := labels[i]
		if !validLabelName(k) {
			return nil, fmt.Errorf("invalid label name %q", k)
		}
		for _, prev := range keys {
			if prev == k {
				return nil, fmt.Errorf("duplicate label name %q", k)
			}
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// drop counts a refused sample and logs the first one per name.
func (e *Exporter) drop(name, reason string) {
	if c, err := e.dropped.GetMetricWithLabelValues(name); err == nil {
		c.Inc()
	}
	if _, seen := e.warned.LoadOrStore(name, struct{}{}); !seen {
		e.logger.Warn("prommetrics: dropping samples of a metric (logged once per name)",
			"metric", name, "reason", reason)
	}
}

// validMetricName applies the classic Prometheus rule
// [a-zA-Z_:][a-zA-Z0-9_:]*, stricter than client_golang's UTF-8 default so
// the exposition stays readable by every scraper.
func validMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c == ':' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// validLabelName applies [a-zA-Z_][a-zA-Z0-9_]* and rejects the reserved
// "__" prefix.
func validLabelName(s string) bool {
	if s == "" || len(s) >= 2 && s[:2] == "__" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9' {
			continue
		}
		return false
	}
	return true
}
