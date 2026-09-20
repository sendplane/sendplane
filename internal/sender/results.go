package sender

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sendplane/sendplane/store"
)

// batcher commits DeliveryResults in batches (architecture 8.1): every
// ResultBatchSize results, or every ResultFlushInterval, whichever comes
// first.
//
// Batching is what keeps a 1M campaign from doing 1M individual commits. The
// cost is the at-least-once window ADR-0002 documents: a crash between the
// SMTP 250 and the commit re-sends the message. MarkSent narrows that window
// to the messages whose 250 arrived in the last flush interval and whose
// MarkSent also failed.
type batcher struct {
	st       store.Store
	size     int
	interval time.Duration
	log      *slog.Logger
	metrics  Metrics

	mu  sync.Mutex
	buf []store.DeliveryResult

	wake   chan struct{}
	done   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newBatcher(st store.Store, size int, interval time.Duration, log *slog.Logger, m Metrics) *batcher {
	b := &batcher{
		st: st, size: size, interval: interval, log: log, metrics: m,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	go b.loop()
	return b
}

// add queues one result. It never blocks on the store.
func (b *batcher) add(r store.DeliveryResult) {
	if r.DeliveryID == "" {
		return
	}
	b.mu.Lock()
	b.buf = append(b.buf, r)
	full := len(b.buf) >= b.size
	b.mu.Unlock()
	if full {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
}

func (b *batcher) loop() {
	defer close(b.done)
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-b.closed:
			b.flush()
			return
		case <-b.wake:
			b.flush()
		case <-t.C:
			b.flush()
		}
	}
}

// flush commits whatever has accumulated. The buffer is swapped under the lock
// and the store call happens outside it, so adding never waits for a commit.
func (b *batcher) flush() {
	b.mu.Lock()
	batch := b.buf
	b.buf = nil
	b.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	// A background context: the results describe work that already happened,
	// so they must be committed even while the sender is shutting down.
	if err := b.st.Deliveries().Complete(context.Background(), batch); err != nil {
		b.log.Error("sendplane: committing delivery results failed",
			"count", len(batch), "err", err)
		return
	}
	for _, r := range batch {
		b.metrics.Count(MetricProcessed, 1, "status", r.NewStatus.String(), "class", r.ErrorClass.String())
	}
}

// close flushes and stops the goroutine.
func (b *batcher) close() {
	b.once.Do(func() { close(b.closed) })
	<-b.done
}
