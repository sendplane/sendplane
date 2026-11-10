package sender

// Metric names. They are constants so that a dashboard and a test refer to the
// same string. They follow Prometheus naming (snake_case, _total on counters,
// a unit suffix on histograms) like internal/bounce's, so the prommetrics
// adapter can register them verbatim.
//
// Each name is always emitted with the same label key set (a Prometheus
// metric family has fixed label names):
//
//	claimed_total           lane, tenant
//	processed_total         status, class
//	render_seconds          (none)
//	smtp_seconds            transport, class
//	limiter_wait_seconds    transport
//	conn_opened_total       transport
//	conn_closed_total       (none)
//	transport_status_total  transport, status
//
// The sink itself is host.Metrics (Count/Observe), which lives in the host
// package because Config.Metrics is filled straight from the root package's
// Options: a metrics interface declared here would force the root package to
// import internal/sender just for a type.
const (
	MetricClaimed     = "sendplane_sender_claimed_total"
	MetricProcessed   = "sendplane_sender_processed_total"
	MetricRenderTime  = "sendplane_sender_render_seconds"
	MetricSendTime    = "sendplane_sender_smtp_seconds"
	MetricLimiterWait = "sendplane_sender_limiter_wait_seconds"
	MetricConnOpened  = "sendplane_sender_conn_opened_total"
	MetricConnClosed  = "sendplane_sender_conn_closed_total"
	MetricTransport   = "sendplane_sender_transport_status_total"
)
