package host

// Metrics is the minimal surface sendplane needs from a metrics backend
// (architecture 12). The default is NopMetrics; an OpenTelemetry or Prometheus
// adapter implements the two methods.
//
// Labels are alternating key/value strings, so a caller never has to allocate
// a map on a hot path. The metric names themselves belong to the packages that
// emit them, not here.
type Metrics interface {
	// Count adds delta to a counter.
	Count(name string, delta int64, labels ...string)
	// Observe records one value in a histogram. Durations are in seconds.
	Observe(name string, value float64, labels ...string)
}

// NopMetrics discards everything. It is the default so that nothing on a hot
// path has to check for a nil Metrics.
type NopMetrics struct{}

func (NopMetrics) Count(string, int64, ...string)     {}
func (NopMetrics) Observe(string, float64, ...string) {}

var _ Metrics = NopMetrics{}
