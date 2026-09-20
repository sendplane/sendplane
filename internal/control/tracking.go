package control

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sendplane/sendplane/store"
)

// finalFlushTimeout bounds the last flush after Close, so a shutdown cannot
// hang on an unreachable database.
const finalFlushTimeout = 10 * time.Second

// TrackingStats is the counter set a metrics exporter reads off the buffer.
type TrackingStats struct {
	// Buffered is what is waiting for the next flush right now.
	Buffered int64
	// Flushed is how many events reached the store.
	Flushed int64
	// Dropped is how many were thrown away because the buffer was full.
	Dropped int64
	// Failed is how many were lost to a store error during a flush.
	Failed int64
}

// TrackingBuffer batches open/click/unsubscribe records (architecture 9.3).
//
// A large campaign produces a burst of opens the moment it lands, and a public
// pixel endpoint must answer in microseconds, so records are appended to
// memory and written once a second in one InsertEvents call per tenant. The
// documented cost is that a crash loses up to one second of tracking.
//
// Unlike the leader loops this runs on every control replica: it belongs to
// the HTTP handlers that receive the pixel and redirect requests, not to the
// singleton.
type TrackingBuffer struct {
	provider store.Provider
	log      *slog.Logger
	clock    func() time.Time

	flushEvery time.Duration
	flushSize  int
	maxBuffer  int

	mu      sync.Mutex
	buf     []store.TrackingEvent
	started bool
	closed  bool
	stats   TrackingStats

	wake     chan struct{}
	stop     chan struct{}
	loopDone chan struct{}
	closedCh chan struct{}

	startOnce sync.Once
	closeOnce sync.Once
}

func newTrackingBuffer(provider store.Provider, log *slog.Logger, clock func() time.Time, cfg config) *TrackingBuffer {
	return &TrackingBuffer{
		provider:   provider,
		log:        log,
		clock:      clock,
		flushEvery: cfg.trackFlushEvery,
		flushSize:  cfg.trackFlushSize,
		maxBuffer:  cfg.trackMaxBuffer,
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		loopDone:   make(chan struct{}),
		closedCh:   make(chan struct{}),
	}
}

// Record buffers one tracking event. It never blocks on the store and never
// returns an error: the caller is an HTTP handler that must answer with a
// pixel or a redirect whatever happens here.
//
// ev.TenantID must be set; it is what the flush groups by.
func (b *TrackingBuffer) Record(ev store.TrackingEvent) {
	if ev.ID == "" {
		ev.ID = store.NewID()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = store.TruncateTime(b.clock())
	}

	b.mu.Lock()
	if b.closed {
		b.stats.Dropped++
		b.mu.Unlock()
		return
	}
	// Bounded on purpose: if the store is down, tracking must not turn into
	// an out-of-memory. The oldest records go first, because the freshest
	// ones are the ones still worth having.
	if over := len(b.buf) - b.maxBuffer + 1; over > 0 {
		b.buf = b.buf[over:]
		b.stats.Dropped += int64(over)
	}
	b.buf = append(b.buf, ev)
	n := len(b.buf)
	b.mu.Unlock()

	if n >= b.flushSize {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
}

// Stats returns the counters. Buffered is a snapshot, the rest are totals.
func (b *TrackingBuffer) Stats() TrackingStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stats
	s.Buffered = int64(len(b.buf))
	return s
}

// start launches the flusher. Calling it more than once is a no-op.
func (b *TrackingBuffer) start(ctx context.Context) {
	b.startOnce.Do(func() {
		b.mu.Lock()
		b.started = true
		b.mu.Unlock()
		go b.loop(ctx)
	})
}

func (b *TrackingBuffer) loop(ctx context.Context) {
	defer close(b.loopDone)
	t := time.NewTicker(b.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			b.finalFlush(ctx)
			return
		case <-b.stop:
			b.finalFlush(ctx)
			return
		case <-t.C:
		case <-b.wake:
		}
		b.flush(ctx)
	}
}

// Close stops the flusher and writes whatever is still buffered. It is safe to
// call more than once and from several goroutines.
func (b *TrackingBuffer) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		started := b.started
		b.closed = true
		b.mu.Unlock()

		close(b.stop)
		if started {
			<-b.loopDone
		} else {
			b.finalFlush(context.Background())
		}
		close(b.closedCh)
	})
	<-b.closedCh
	return nil
}

// finalFlush runs outside the cancelled context: the records are already in
// memory and dropping them because the process is shutting down is the one
// loss this design can actually avoid.
func (b *TrackingBuffer) finalFlush(ctx context.Context) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
	defer cancel()
	b.flush(fctx)
}

// flush writes the buffered events, one InsertEvents call per tenant, and then
// derives the first-interaction columns.
func (b *TrackingBuffer) flush(ctx context.Context) {
	b.mu.Lock()
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.buf
	b.buf = make([]store.TrackingEvent, 0, b.flushSize)
	b.mu.Unlock()

	byTenant := map[string][]store.TrackingEvent{}
	for _, ev := range batch {
		byTenant[ev.TenantID] = append(byTenant[ev.TenantID], ev)
	}
	for tenantID, events := range byTenant {
		st, err := b.provider.ForTenant(ctx, tenantID)
		if err != nil {
			b.countFailed(len(events))
			b.log.Error("control: tracking flush cannot open tenant store",
				"tenant", tenantID, "count", len(events), "err", err)
			continue
		}
		if err := st.Tracking().InsertEvents(ctx, events); err != nil {
			b.countFailed(len(events))
			b.log.Error("control: tracking flush failed",
				"tenant", tenantID, "count", len(events), "err", err)
			continue
		}
		b.countFlushed(len(events))
		b.deriveFirsts(ctx, st, events)
	}
}

// deriveFirsts writes the delivery summary columns that unique counts are
// based on (architecture 9.3). Each setter is a conditional update that only
// writes a NULL column and reports whether it changed anything, so uniqueness
// is decided by the store, not by counting here.
//
// Suspected bots are recorded but never derived from: letting a scanner set
// first_opened_at would make every unique count wrong, and there is no way to
// take it back.
func (b *TrackingBuffer) deriveFirsts(ctx context.Context, st store.Store, events []store.TrackingEvent) {
	type key struct {
		kind store.TrackingKind
		id   string
	}
	seen := make(map[key]bool, len(events))
	deliveries := st.Deliveries()
	for _, ev := range events {
		if ev.SuspectedBot || ev.DeliveryID == "" {
			continue
		}
		var set func(context.Context, string, time.Time) (bool, error)
		switch ev.Kind {
		case store.TrackingOpen:
			set = deliveries.SetFirstOpened
		case store.TrackingClick:
			set = deliveries.SetFirstClicked
		case store.TrackingUnsubscribed:
			set = deliveries.SetUnsubscribed
		default:
			// unsubscribe_clicked is the unconfirmed GET (ADR-0011); it must
			// not mark the recipient as unsubscribed.
			continue
		}
		k := key{ev.Kind, ev.DeliveryID}
		if seen[k] {
			continue // the first one in this batch already decided it
		}
		seen[k] = true
		if _, err := set(ctx, ev.DeliveryID, ev.CreatedAt); err != nil {
			b.log.Error("control: cannot derive first interaction",
				"delivery", ev.DeliveryID, "kind", ev.Kind, "err", err)
		}
	}
}

func (b *TrackingBuffer) countFlushed(n int) {
	b.mu.Lock()
	b.stats.Flushed += int64(n)
	b.mu.Unlock()
}

func (b *TrackingBuffer) countFailed(n int) {
	b.mu.Lock()
	b.stats.Failed += int64(n)
	b.mu.Unlock()
}
