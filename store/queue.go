package store

import (
	"context"
	"errors"
)

// QueueDepth is the number of deliveries in one (lane, status) cell, summed
// over every tenant the Provider serves.
type QueueDepth struct {
	Lane   Lane
	Status DeliveryStatus
	Count  int64
}

// QueueDepthReader is an optional Provider capability: the cluster-wide
// backlog a metrics exporter publishes and an autoscaler (KEDA) scales the
// sender on (architecture 12).
//
// It is optional rather than a Provider method so that a custom or routed
// Provider written before it existed keeps compiling; sendplane type-asserts
// for it and reports errors.ErrUnsupported when it is missing.
//
// QueueDepth counts only non-terminal deliveries — pending, queued, leased and
// deferred — across all tenants, in one round trip on a shared-mode store.
// Cells with a zero count may be omitted. The result is sorted by Lane, then
// Status. Pending includes the rows of a campaign that is scheduled or paused,
// and deferred includes rows whose next attempt is still in the future: the
// caller picks which statuses it treats as backlog.
//
// It is a monitoring query, not a hot-path one: callers poll it on a scrape
// interval and should cache the result rather than call it per request.
type QueueDepthReader interface {
	QueueDepth(ctx context.Context) ([]QueueDepth, error)
}

// ReadQueueDepth calls p.QueueDepth when p implements QueueDepthReader and
// returns errors.ErrUnsupported otherwise.
func ReadQueueDepth(ctx context.Context, p Provider) ([]QueueDepth, error) {
	r, ok := p.(QueueDepthReader)
	if !ok {
		return nil, errors.ErrUnsupported
	}
	return r.QueueDepth(ctx)
}
