package sender

// Metrics is the minimal surface the sender needs from a metrics backend
// (architecture 12). The default is NopMetrics; an OpenTelemetry or Prometheus
// adapter implements the two methods.
//
// Labels are alternating key/value strings, so a caller never has to allocate
// a map on a hot path.
type Metrics interface {
	// Count adds delta to a counter.
	Count(name string, delta int64, labels ...string)
	// Observe records one value in a histogram. Durations are in seconds.
	Observe(name string, value float64, labels ...string)
}

// Metric names. They are constants so that a dashboard and a test refer to the
// same string.
const (
	MetricClaimed     = "sendplane.sender.claimed"
	MetricProcessed   = "sendplane.sender.processed"
	MetricRenderTime  = "sendplane.sender.render.seconds"
	MetricSendTime    = "sendplane.sender.smtp.seconds"
	MetricLimiterWait = "sendplane.sender.limiter.wait.seconds"
	MetricConnOpened  = "sendplane.sender.conn.opened"
	MetricConnClosed  = "sendplane.sender.conn.closed"
	MetricTransport   = "sendplane.sender.transport.status"
)

// NopMetrics discards everything. It is the default so that nothing in the
// sender has to check for a nil Metrics.
type NopMetrics struct{}

func (NopMetrics) Count(string, int64, ...string)     {}
func (NopMetrics) Observe(string, float64, ...string) {}

var _ Metrics = NopMetrics{}
