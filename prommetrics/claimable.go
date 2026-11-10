package prommetrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sendplane/sendplane/store"
)

// ClaimableOptions configures NewClaimableCollector; the fields mean the same
// as in QueueDepthOptions.
type ClaimableOptions QueueDepthOptions

type claimableCollector struct {
	*cachedRead[store.LaneCount]
	desc *prometheus.Desc
}

// NewClaimableCollector returns a collector exposing
// sendplane_delivery_claimable{lane}: per lane, the deliveries a sender could
// claim right now (queued or deferred and due, plus pending and due in a
// running campaign), from read — typically (*sendplane.Sendplane).Claimable.
// This, not queue depth, is the autoscaling signal: pending rows of scheduled
// or paused campaigns are not counted.
//
// Like NewQueueDepthCollector it is a cluster-wide, cached read: register it
// on one role only and aggregate across replicas with max, not sum. It is
// zero-filled for bulk, transactional and probe; ErrUnsupported and read
// errors behave as for queue depth, independently of it, and failures count
// in sendplane_delivery_claimable_errors_total.
func NewClaimableCollector(read func(context.Context) ([]store.LaneCount, error), o ClaimableOptions) prometheus.Collector {
	return &claimableCollector{
		cachedRead: newCachedRead("claimable", MetricClaimable, MetricClaimableErrors,
			"Failed store reads of the claimable delivery count (excluding stores that do not support it).", read, QueueDepthOptions(o)),
		desc: prometheus.NewDesc(MetricClaimable,
			"Deliveries a sender could claim now across all tenants by lane (queued/deferred and due, plus pending and due in running campaigns), read from the store and cached. "+
				"Every replica reporting it sees the same value: aggregate with max, not sum.",
			[]string{"lane"}, nil),
	}
}

func (c *claimableCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
	c.errors.Describe(ch)
}

func (c *claimableCollector) Collect(ch chan<- prometheus.Metric) {
	if cells, ok := c.snapshot(); ok {
		counts := make(map[string]int64, len(queueDepthLanes))
		order := make([]string, 0, len(queueDepthLanes))
		for _, l := range queueDepthLanes {
			counts[l.String()] = 0
			order = append(order, l.String())
		}
		for _, cell := range cells {
			k := cell.Lane.String()
			if _, ok := counts[k]; !ok {
				order = append(order, k)
			}
			counts[k] += cell.Count
		}
		for _, k := range order {
			if m, err := prometheus.NewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[k]), k); err == nil {
				ch <- m
			}
		}
	}
	c.errors.Collect(ch)
}
