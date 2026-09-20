package sender

// Metric names. They are constants so that a dashboard and a test refer to the
// same string.
//
// The sink itself is host.Metrics (Count/Observe), which lives in the host
// package because Config.Metrics is filled straight from the root package's
// Options: a metrics interface declared here would force the root package to
// import internal/sender just for a type.
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
