package prommetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sendplane/sendplane/store"
)

// fakeClock is advanced by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeReader returns its current result and counts calls.
type fakeReader struct {
	mu    sync.Mutex
	cells []store.QueueDepth
	err   error
	calls atomic.Int32
	gate  chan struct{} // when non-nil, read blocks until it is closed
}

func (r *fakeReader) set(cells []store.QueueDepth, err error) {
	r.mu.Lock()
	r.cells, r.err = cells, err
	r.mu.Unlock()
}

func (r *fakeReader) read(ctx context.Context) ([]store.QueueDepth, error) {
	r.calls.Add(1)
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cells, r.err
}

func newTestQueueDepth(r *fakeReader) (prometheus.Collector, *fakeClock, *bytes.Buffer) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var logs bytes.Buffer
	c := NewQueueDepthCollector(r.read, QueueDepthOptions{
		MinInterval: 15 * time.Second,
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
		now:         clk.now,
	})
	return c, clk, &logs
}

// depthExposition renders the full zero-filled grid with overrides.
func depthExposition(overrides map[string]int64) string {
	var b strings.Builder
	b.WriteString("# HELP " + MetricQueueDepth + " Non-terminal deliveries across all tenants by lane and status, read from the store and cached. Every replica reporting it sees the same value: aggregate with max, not sum.\n")
	b.WriteString("# TYPE " + MetricQueueDepth + " gauge\n")
	for _, l := range []string{"bulk", "probe", "transactional"} { // exposition sorts
		for _, s := range []string{"deferred", "leased", "pending", "queued"} {
			fmt.Fprintf(&b, "%s{lane=%q,status=%q} %d\n", MetricQueueDepth, l, s, overrides[l+"/"+s])
		}
	}
	return b.String()
}

func TestQueueDepthZeroFill(t *testing.T) {
	r := &fakeReader{}
	r.set([]store.QueueDepth{
		{Lane: store.LaneBulk, Status: store.DeliveryPending, Count: 7},
		{Lane: store.LaneTransactional, Status: store.DeliveryLeased, Count: 2},
	}, nil)
	c, _, _ := newTestQueueDepth(r)
	want := depthExposition(map[string]int64{"bulk/pending": 7, "transactional/leased": 2})
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), MetricQueueDepth); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(c, MetricQueueDepth); n != 12 {
		t.Errorf("%d series, want 12 (3 lanes x 4 statuses)", n)
	}
}

func TestQueueDepthEmptyStoreStillExportsZeros(t *testing.T) {
	r := &fakeReader{}
	c, _, _ := newTestQueueDepth(r)
	if err := testutil.CollectAndCompare(c, strings.NewReader(depthExposition(nil)), MetricQueueDepth); err != nil {
		t.Fatal(err)
	}
}

func TestQueueDepthSumsDuplicateCells(t *testing.T) {
	r := &fakeReader{}
	r.set([]store.QueueDepth{
		{Lane: store.LaneBulk, Status: store.DeliveryQueued, Count: 1},
		{Lane: store.LaneBulk, Status: store.DeliveryQueued, Count: 4},
	}, nil)
	c, _, _ := newTestQueueDepth(r)
	want := depthExposition(map[string]int64{"bulk/queued": 5})
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), MetricQueueDepth); err != nil {
		t.Fatal(err)
	}
}

func TestQueueDepthCachesForMinInterval(t *testing.T) {
	r := &fakeReader{}
	r.set([]store.QueueDepth{{Lane: store.LaneBulk, Status: store.DeliveryPending, Count: 1}}, nil)
	c, clk, _ := newTestQueueDepth(r)

	testutil.CollectAndCount(c)
	r.set([]store.QueueDepth{{Lane: store.LaneBulk, Status: store.DeliveryPending, Count: 9}}, nil)
	clk.add(14 * time.Second)
	if err := testutil.CollectAndCompare(c, strings.NewReader(depthExposition(map[string]int64{"bulk/pending": 1})), MetricQueueDepth); err != nil {
		t.Errorf("inside MinInterval: %v", err)
	}
	if got := r.calls.Load(); got != 1 {
		t.Errorf("reads = %d, want 1 inside MinInterval", got)
	}

	clk.add(time.Second)
	if err := testutil.CollectAndCompare(c, strings.NewReader(depthExposition(map[string]int64{"bulk/pending": 9})), MetricQueueDepth); err != nil {
		t.Errorf("after MinInterval: %v", err)
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("reads = %d, want 2 after MinInterval", got)
	}
}

func TestQueueDepthUnsupported(t *testing.T) {
	r := &fakeReader{}
	r.set(nil, fmt.Errorf("wrapped: %w", errors.ErrUnsupported))
	c, clk, logs := newTestQueueDepth(r)

	for i := 0; i < 3; i++ {
		if n := testutil.CollectAndCount(c, MetricQueueDepth); n != 0 {
			t.Errorf("unsupported store exported %d depth series, want 0", n)
		}
		clk.add(time.Minute)
	}
	if n := testutil.CollectAndCount(c, MetricQueueDepthErrors); n != 1 {
		t.Errorf("errors counter series = %d, want 1 (always present)", n)
	}
	if got := testutil.ToFloat64(c.(*queueDepthCollector).errors); got != 0 {
		t.Errorf("errors_total = %v, want 0: unsupported is not an error", got)
	}
	if n := strings.Count(logs.String(), "does not report queue depth"); n != 1 {
		t.Errorf("logged the unsupported notice %d times, want once", n)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("unsupported should log at info, got:\n%s", logs.String())
	}
}

func TestQueueDepthErrorServesStaleThenNothing(t *testing.T) {
	r := &fakeReader{}
	r.set([]store.QueueDepth{{Lane: store.LaneProbe, Status: store.DeliveryDeferred, Count: 3}}, nil)
	c, clk, logs := newTestQueueDepth(r) // MinInterval 15s → MaxStale 75s
	qc := c.(*queueDepthCollector)
	stale := depthExposition(map[string]int64{"probe/deferred": 3})

	testutil.CollectAndCount(c) // good read at t0
	r.set(nil, errors.New("db down"))

	// Failed reads at t0+15s ... t0+75s: still within MaxStale of the good read.
	for i := 1; i <= 5; i++ {
		clk.add(15 * time.Second)
		if err := testutil.CollectAndCompare(c, strings.NewReader(stale), MetricQueueDepth); err != nil {
			t.Fatalf("t0+%ds: want the last good values: %v", 15*i, err)
		}
	}
	if got := testutil.ToFloat64(qc.errors); got != 5 {
		t.Errorf("errors_total = %v, want 5 (one per read attempt, not per scrape)", got)
	}

	// Past MaxStale the gauge disappears.
	clk.add(15 * time.Second)
	if n := testutil.CollectAndCount(c, MetricQueueDepth); n != 0 {
		t.Errorf("past MaxStale exported %d series, want 0", n)
	}
	if n := strings.Count(logs.String(), "queue depth read failed"); n != 1 {
		t.Errorf("logged %d read failures in 90s, want 1 (rate-limited)", n)
	}

	// Recovery brings it back.
	r.set(nil, nil)
	clk.add(15 * time.Second)
	if err := testutil.CollectAndCompare(c, strings.NewReader(depthExposition(nil)), MetricQueueDepth); err != nil {
		t.Errorf("after recovery: %v", err)
	}
	if !strings.Contains(logs.String(), "recovered") {
		t.Errorf("no recovery log:\n%s", logs.String())
	}
}

func TestQueueDepthErrorBeforeAnySuccessExportsNothing(t *testing.T) {
	r := &fakeReader{}
	r.set(nil, errors.New("db down"))
	c, _, _ := newTestQueueDepth(r)
	if n := testutil.CollectAndCount(c, MetricQueueDepth); n != 0 {
		t.Errorf("exported %d series with no good read, want 0", n)
	}
}

func TestQueueDepthSingleflight(t *testing.T) {
	r := &fakeReader{gate: make(chan struct{})}
	c, _, _ := newTestQueueDepth(r)

	const scrapes = 8
	var wg sync.WaitGroup
	counts := make([]int, scrapes)
	for i := 0; i < scrapes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			counts[i] = testutil.CollectAndCount(c, MetricQueueDepth)
		}(i)
	}
	// Let the first read start, then release it.
	for r.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // give the other scrapes time to queue up
	close(r.gate)
	wg.Wait()

	if got := r.calls.Load(); got != 1 {
		t.Errorf("reads = %d for %d concurrent scrapes, want 1", got, scrapes)
	}
	for i, n := range counts {
		if n != 12 {
			t.Errorf("scrape %d saw %d series, want 12", i, n)
		}
	}
}

func TestQueueDepthTimeout(t *testing.T) {
	r := &fakeReader{gate: make(chan struct{})} // never released
	c := NewQueueDepthCollector(r.read, QueueDepthOptions{
		Timeout: 20 * time.Millisecond,
		Logger:  slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	done := make(chan int)
	go func() { done <- testutil.CollectAndCount(c, MetricQueueDepth) }()
	select {
	case n := <-done:
		if n != 0 {
			t.Errorf("timed-out read exported %d series, want 0", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Collect did not honour Timeout")
	}
	if got := testutil.ToFloat64(c.(*queueDepthCollector).errors); got != 1 {
		t.Errorf("errors_total = %v, want 1", got)
	}
}

func TestQueueDepthRegistersAlongsideExporter(t *testing.T) {
	e, _ := newTestExporter(t)
	r := &fakeReader{}
	if err := e.Registry().Register(NewQueueDepthCollector(r.read, QueueDepthOptions{})); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, e.Handler())
	for _, want := range []string{
		`sendplane_delivery_queue_depth{lane="transactional",status="pending"} 0`,
		`sendplane_delivery_queue_depth_errors_total 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}
