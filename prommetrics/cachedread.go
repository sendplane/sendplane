package prommetrics

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// cachedRead is the store-read machinery shared by the queue-depth and
// claimable collectors: a MinInterval cache, one read in flight at a time,
// stale fallback up to MaxStale, ErrUnsupported handling, an errors counter
// and rate-limited logging. Each collector owns one, so a failure of one
// read never suppresses the other.
type cachedRead[T any] struct {
	what     string // for logs, e.g. "queue depth"
	metric   string // gauge name, for the unsupported notice
	read     func(context.Context) ([]T, error)
	interval time.Duration
	timeout  time.Duration
	maxStale time.Duration
	logger   *slog.Logger
	now      func() time.Time

	errors prometheus.Counter

	// mu is held across the store read, which is what makes concurrent
	// scrapes share one query: the second Collect waits, then finds the
	// result the first one just cached.
	mu          sync.Mutex
	attempted   time.Time // last read, successful or not
	good        []T
	goodAt      time.Time
	lastErr     error
	unsupported bool
	warnedAt    time.Time
	infoOnce    sync.Once
}

func newCachedRead[T any](what, metric, errorsName, errorsHelp string, read func(context.Context) ([]T, error), o QueueDepthOptions) *cachedRead[T] {
	c := &cachedRead[T]{
		what: what, metric: metric, read: read,
		interval: o.MinInterval, timeout: o.Timeout, maxStale: o.MaxStale,
		logger: o.Logger, now: o.now,
		errors: prometheus.NewCounter(prometheus.CounterOpts{Name: errorsName, Help: errorsHelp}),
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

// snapshot returns the cells to publish, reading the store when the cached
// attempt is older than MinInterval. ok is false when nothing should be
// published.
func (c *cachedRead[T]) snapshot() ([]T, bool) {
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
func (c *cachedRead[T]) refresh(now time.Time) {
	c.attempted = now
	// Collect has no context of its own; the timeout is what keeps a stuck
	// database from holding the scrape (and every scrape queued behind it).
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	cells, err := c.read(ctx)

	switch {
	case err == nil:
		if c.lastErr != nil && !c.unsupported {
			c.logger.Info("prommetrics: " + c.what + " read recovered")
		}
		c.good, c.goodAt, c.lastErr, c.unsupported = cells, now, nil, false
	case errors.Is(err, errors.ErrUnsupported):
		c.lastErr, c.unsupported = err, true
		c.infoOnce.Do(func() {
			c.logger.Info("prommetrics: the store does not report " + c.what + "; " +
				c.metric + " is not exported")
		})
	default:
		c.lastErr, c.unsupported = err, false
		c.errors.Inc()
		if c.warnedAt.IsZero() || now.Sub(c.warnedAt) >= errorLogEvery {
			c.warnedAt = now
			c.logger.Warn("prommetrics: "+c.what+" read failed (logged at most every "+errorLogEvery.String()+")",
				"error", err, "serving_stale", !c.goodAt.IsZero() && now.Sub(c.goodAt) <= c.maxStale)
		}
	}
}
