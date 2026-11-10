package prommetrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sendplane/sendplane/store"
)

// Queue depth and claimable metric names.
const (
	MetricQueueDepth       = "sendplane_delivery_queue_depth"
	MetricQueueDepthErrors = "sendplane_delivery_queue_depth_errors_total"
	MetricClaimable        = "sendplane_delivery_claimable"
	MetricClaimableErrors  = "sendplane_delivery_claimable_errors_total"
)

// QueueDepthOptions configures NewQueueDepthCollector (and, as
// ClaimableOptions, NewClaimableCollector). The zero value is
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
	*cachedRead[store.QueueDepth]
	desc *prometheus.Desc
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
//
// Pending includes the rows of scheduled and paused campaigns, so this is a
// status breakdown for dashboards; use NewClaimableCollector to autoscale.
func NewQueueDepthCollector(read func(context.Context) ([]store.QueueDepth, error), o QueueDepthOptions) prometheus.Collector {
	return &queueDepthCollector{
		cachedRead: newCachedRead("queue depth", MetricQueueDepth, MetricQueueDepthErrors,
			"Failed store reads of the delivery queue depth (excluding stores that do not support it).", read, o),
		desc: prometheus.NewDesc(MetricQueueDepth,
			"Non-terminal deliveries across all tenants by lane and status, read from the store and cached. "+
				"Every replica reporting it sees the same value: aggregate with max, not sum.",
			[]string{"lane", "status"}, nil),
	}
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
