package prommetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sendplane/sendplane/store"
)

type fakeClaimable struct {
	cells []store.LaneCount
	err   error
	calls atomic.Int32
}

func (r *fakeClaimable) read(context.Context) ([]store.LaneCount, error) {
	r.calls.Add(1)
	return r.cells, r.err
}

func newTestClaimable(r *fakeClaimable) (prometheus.Collector, *fakeClock, *bytes.Buffer) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var logs bytes.Buffer
	c := NewClaimableCollector(r.read, ClaimableOptions{
		MinInterval: 15 * time.Second,
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
		now:         clk.now,
	})
	return c, clk, &logs
}

func claimableExposition(overrides map[string]int64) string {
	var b strings.Builder
	b.WriteString("# HELP " + MetricClaimable + " Deliveries a sender could claim now across all tenants by lane (queued/deferred and due, plus pending and due in running campaigns), read from the store and cached. Every replica reporting it sees the same value: aggregate with max, not sum.\n")
	b.WriteString("# TYPE " + MetricClaimable + " gauge\n")
	for _, l := range []string{"bulk", "probe", "transactional"} {
		fmt.Fprintf(&b, "%s{lane=%q} %d\n", MetricClaimable, l, overrides[l])
	}
	return b.String()
}

func TestClaimableZeroFillAndSum(t *testing.T) {
	r := &fakeClaimable{cells: []store.LaneCount{
		{Lane: store.LaneBulk, Count: 7},
		{Lane: store.LaneBulk, Count: 3},
		{Lane: store.LaneTransactional, Count: 2},
	}}
	c, _, _ := newTestClaimable(r)
	want := claimableExposition(map[string]int64{"bulk": 10, "transactional": 2})
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), MetricClaimable); err != nil {
		t.Fatal(err)
	}
	r2 := &fakeClaimable{}
	c2, _, _ := newTestClaimable(r2)
	if err := testutil.CollectAndCompare(c2, strings.NewReader(claimableExposition(nil)), MetricClaimable); err != nil {
		t.Fatal(err)
	}
}

func TestClaimableCachesForMinInterval(t *testing.T) {
	r := &fakeClaimable{cells: []store.LaneCount{{Lane: store.LaneBulk, Count: 1}}}
	c, clk, _ := newTestClaimable(r)
	testutil.CollectAndCount(c)
	r.cells = []store.LaneCount{{Lane: store.LaneBulk, Count: 9}}
	clk.add(14 * time.Second)
	if err := testutil.CollectAndCompare(c, strings.NewReader(claimableExposition(map[string]int64{"bulk": 1})), MetricClaimable); err != nil {
		t.Errorf("inside MinInterval: %v", err)
	}
	clk.add(time.Second)
	if err := testutil.CollectAndCompare(c, strings.NewReader(claimableExposition(map[string]int64{"bulk": 9})), MetricClaimable); err != nil {
		t.Errorf("after MinInterval: %v", err)
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("reads = %d, want 2", got)
	}
}

func TestClaimableUnsupported(t *testing.T) {
	r := &fakeClaimable{err: fmt.Errorf("wrapped: %w", errors.ErrUnsupported)}
	c, clk, logs := newTestClaimable(r)
	for i := 0; i < 3; i++ {
		if n := testutil.CollectAndCount(c, MetricClaimable); n != 0 {
			t.Errorf("unsupported store exported %d series, want 0", n)
		}
		clk.add(time.Minute)
	}
	if got := testutil.ToFloat64(c.(*claimableCollector).errors); got != 0 {
		t.Errorf("errors_total = %v, want 0", got)
	}
	if n := strings.Count(logs.String(), "does not report claimable"); n != 1 {
		t.Errorf("unsupported notice logged %d times, want once", n)
	}
}

func TestClaimableErrorServesStaleThenNothing(t *testing.T) {
	r := &fakeClaimable{cells: []store.LaneCount{{Lane: store.LaneProbe, Count: 3}}}
	c, clk, _ := newTestClaimable(r)
	testutil.CollectAndCount(c)
	r.err = errors.New("db down")
	for i := 0; i < 5; i++ {
		clk.add(15 * time.Second)
		if err := testutil.CollectAndCompare(c, strings.NewReader(claimableExposition(map[string]int64{"probe": 3})), MetricClaimable); err != nil {
			t.Fatalf("want stale values: %v", err)
		}
	}
	clk.add(15 * time.Second)
	if n := testutil.CollectAndCount(c, MetricClaimable); n != 0 {
		t.Errorf("past MaxStale exported %d series, want 0", n)
	}
	if got := testutil.ToFloat64(c.(*claimableCollector).errors); got != 6 {
		t.Errorf("errors_total = %v, want 6", got)
	}
}

// One failing or unsupported read must not hide the other gauge.
func TestQueueDepthAndClaimableAreIndependent(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	build := func(d *fakeReader, cl *fakeClaimable) *prometheus.Registry {
		reg := prometheus.NewRegistry()
		reg.MustRegister(
			NewQueueDepthCollector(d.read, QueueDepthOptions{Logger: logger, now: clk.now}),
			NewClaimableCollector(cl.read, ClaimableOptions{Logger: logger, now: clk.now}),
		)
		return reg
	}
	count := func(reg *prometheus.Registry, name string) int {
		n, err := testutil.GatherAndCount(reg, name)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	for name, failure := range map[string]error{"error": errors.New("boom"), "unsupported": errors.ErrUnsupported} {
		// claimable fails, depth works
		d := &fakeReader{}
		reg := build(d, &fakeClaimable{err: failure})
		if n := count(reg, MetricQueueDepth); n != 12 {
			t.Errorf("%s: depth series = %d with failing claimable, want 12", name, n)
		}
		if n := count(reg, MetricClaimable); n != 0 {
			t.Errorf("%s: claimable series = %d, want 0", name, n)
		}
		// depth fails, claimable works
		reg = build(&fakeReader{err: failure}, &fakeClaimable{})
		if n := count(reg, MetricClaimable); n != 3 {
			t.Errorf("%s: claimable series = %d with failing depth, want 3", name, n)
		}
		if n := count(reg, MetricQueueDepth); n != 0 {
			t.Errorf("%s: depth series = %d, want 0", name, n)
		}
	}

	// Errors are counted on the failing read's own counter only.
	reg := build(&fakeReader{}, &fakeClaimable{err: errors.New("boom")})
	count(reg, MetricClaimable)
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		v := mf.GetMetric()[0].GetCounter().GetValue()
		switch mf.GetName() {
		case MetricClaimableErrors:
			if v != 1 {
				t.Errorf("claimable errors = %v, want 1", v)
			}
		case MetricQueueDepthErrors:
			if v != 0 {
				t.Errorf("depth errors = %v, want 0", v)
			}
		}
	}
}
