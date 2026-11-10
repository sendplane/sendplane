package prommetrics

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sendplane/sendplane/store"
)

// Queue depth metric names.
const (
	MetricQueueDepth       = "sendplane_delivery_queue_depth"
	MetricQueueDepthErrors = "sendplane_delivery_queue_depth_errors_total"
)

// QueueDepthOptions configures NewQueueDepthCollector. The zero value is
// usable.
type QueueDepthOptions struct {
	// MinInterval is how long one store read is reused. Scrapes inside it —
	// several Prometheus replicas, a KEDA scaler, someone curling /metrics —
	// are served from the cached result. Default 15s.
	MinInterval time.Duration
	// Timeout bounds one store read. Default 5s.
	Timeout time.Duration
	// MaxStale is how long the last good result keeps being served while
	// reads fail. Past it the gauge disappears rather than reporting a
	// backlog that may no longer be true. Default 5 × MinInterval.
	MaxStale time.Duration
	// Logger receives the unsupported notice and read failures. Nil means
	// slog.Default().
	Logger *slog.Logger

	now func() time.Time // tests; nil means time.Now
}

// errorLogEvery rate-limits the read-failure warning: a broken database
// should not turn every scrape into a log line.
const errorLogEvery = 5 * time.Minute

// queueDepthLanes and queueDepthStatuses are the zero-filled cells: every
// non-terminal status in every lane, so that a series exists even when its
// count is zero. An absent series makes KEDA and "for: 10m" alerts misbehave
// in ways a zero does not.
var (
	queueDepthLanes    = []store.Lane{store.LaneBulk, store.LaneTransactional, store.LaneProbe}
	queueDepthStatuses = []store.DeliveryStatus{
		store.DeliveryPending, store.DeliveryQueued, store.DeliveryLeased, store.DeliveryDeferred,
	}
)

type queueDepthCollector struct {
	read     func(context.Context) ([]store.QueueDepth, error)
	interval time.Duration
	timeout  time.Duration
	maxStale time.Duration
	logger   *slog.Logger
	now      func() time.Time

	desc   *prometheus.Desc
	errors prometheus.Counter

	// mu is held across the store read, which is what makes concurrent
	// scrapes share one query: the second Collect waits, then finds the
	// result the first one just cached.
	mu          sync.Mutex
	attempted   time.Time // last read, successful or not
	good        []store.QueueDepth
	goodAt      time.Time
	lastErr     error
	unsupported bool
	warnedAt    time.Time
	infoOnce    sync.Once
}

// NewQueueDepthCollector returns a collector exposing
// sendplane_delivery_queue_depth{lane,status}, the cluster-wide count of
// non-terminal deliveries, from read — typically (*sendplane.Sendplane)
// .QueueDepth. Register it on one role only (the reference binary uses
// control). Every replica that registers it reports the same cluster-wide
// value, so consumers must aggregate across replicas with max, not sum.
//
// The gauge is zero-filled for every lane × {pending, queued, leased,
// deferred}. A read that returns errors.ErrUnsupported (a store without
// store.QueueDepthReader) makes the gauge absent; any other error keeps the
// last good values for up to MaxStale, after which the gauge is absent too,
// and increments sendplane_delivery_queue_depth_errors_total.
func NewQueueDepthCollector(read func(context.Context) ([]store.QueueDepth, error), o QueueDepthOptions) prometheus.Collector {
	c := &queueDepthCollector{
		read:     read,
		interval: o.MinInterval,
		timeout:  o.Timeout,
		maxStale: o.MaxStale,
		logger:   o.Logger,
		now:      o.now,
		desc: prometheus.NewDesc(MetricQueueDepth,
			"Non-terminal deliveries across all tenants by lane and status, read from the store and cached. "+
				"Every replica reporting it sees the same value: aggregate with max, not sum.",
			[]string{"lane", "status"}, nil),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: MetricQueueDepthErrors,
			Help: "Failed store reads of the delivery queue depth (excluding stores that do not support it).",
		}),
	}
	if c.interval <= 0 {
		c.interval = 15 * time.Second
	}
	if c.timeout <= 0 {
		c.timeout = 5 * time.Second
	}
	if c.maxStale <= 0 {
		c.maxStale = 5 * c.interval
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

func (c *queueDepthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
	c.errors.Describe(ch)
}

func (c *queueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	if cells, ok := c.snapshot(); ok {
		for _, m := range c.metrics(cells) {
			ch <- m
		}
	}
	c.errors.Collect(ch)
}

// snapshot returns the cells to publish, reading the store when the cached
// attempt is older than MinInterval. ok is false when nothing should be
// published.
func (c *queueDepthCollector) snapshot() ([]store.QueueDepth, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.attempted.IsZero() || now.Sub(c.attempted) >= c.interval {
		c.refresh(now)
	}
	if c.unsupported {
		return nil, false
	}
	if c.goodAt.IsZero() || (c.lastErr != nil && now.Sub(c.goodAt) > c.maxStale) {
		return nil, false
	}
	return c.good, true
}

// refresh reads the store once. c.mu is held.
func (c *queueDepthCollector) refresh(now time.Time) {
	c.attempted = now
	// Collect has no context of its own; the timeout is what keeps a stuck
	// database from holding the scrape (and every scrape queued behind it).
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	cells, err := c.read(ctx)

	switch {
	case err == nil:
		if c.lastErr != nil && !c.unsupported {
			c.logger.Info("prommetrics: queue depth read recovered")
		}
		c.good, c.goodAt, c.lastErr, c.unsupported = cells, now, nil, false
	case errors.Is(err, errors.ErrUnsupported):
		c.lastErr, c.unsupported = err, true
		c.infoOnce.Do(func() {
			c.logger.Info("prommetrics: the store does not report queue depth; " +
				MetricQueueDepth + " is not exported")
		})
	default:
		c.lastErr, c.unsupported = err, false
		c.errors.Inc()
		if c.warnedAt.IsZero() || now.Sub(c.warnedAt) >= errorLogEvery {
			c.warnedAt = now
			c.logger.Warn("prommetrics: queue depth read failed (logged at most every "+errorLogEvery.String()+")",
				"error", err, "serving_stale", !c.goodAt.IsZero() && now.Sub(c.goodAt) <= c.maxStale)
		}
	}
}

// metrics builds the gauge samples: the zero-filled grid plus anything else
// the store returned, with duplicate cells summed so the registry never sees
// two samples with the same labels.
func (c *queueDepthCollector) metrics(cells []store.QueueDepth) []prometheus.Metric {
	type key struct{ lane, status string }
	counts := make(map[key]int64, len(queueDepthLanes)*len(queueDepthStatuses))
	order := make([]key, 0, len(queueDepthLanes)*len(queueDepthStatuses))
	for _, l := range queueDepthLanes {
		for _, s := range queueDepthStatuses {
			k := key{l.String(), s.String()}
			counts[k] = 0
			order = append(order, k)
		}
	}
	for _, cell := range cells {
		k := key{cell.Lane.String(), cell.Status.String()}
		if _, ok := counts[k]; !ok {
			order = append(order, k)
		}
		counts[k] += cell.Count
	}
	out := make([]prometheus.Metric, 0, len(order))
	for _, k := range order {
		m, err := prometheus.NewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[k]), k.lane, k.status)
		if err != nil {
			continue // unreachable: the label values are enum names
		}
		out = append(out, m)
	}
	return out
}
