package ingest

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

const streamN = 200_000

// lineGen synthesizes n NDJSON recipient lines without ever holding more than
// one of them, so a 200k-line body can be produced by the test itself.
type lineGen struct {
	n, i int
	buf  []byte
	off  int
}

func (g *lineGen) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if g.off >= len(g.buf) {
			if g.i >= g.n {
				if n == 0 {
					return 0, io.EOF
				}
				return n, nil
			}
			g.buf = fmt.Appendf(g.buf[:0],
				`{"email":"user%07d@example.com","name":"User %d","locale":"ko","vars":{"plan":"pro","n":%d}}`+"\n",
				g.i, g.i, g.i)
			g.off, g.i = 0, g.i+1
		}
		c := copy(p[n:], g.buf[g.off:])
		g.off += c
		n += c
	}
	return n, nil
}

// countingDeliveries stands in for a real DeliveryRepo: it counts rows and
// drops them. Keeping 200k deliveries alive would measure the store's memory,
// not the ingester's, which is the thing under test here. Cross-batch
// deduplication is covered by TestDuplicatesWithinAndAcrossBatches against
// memstore; this body has no duplicates.
type countingDeliveries struct {
	store.DeliveryRepo
	mu       sync.Mutex
	inserted int
	maxBatch int
}

func (r *countingDeliveries) InsertBatch(_ context.Context, ds []store.Delivery) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range ds {
		if ds[i].EmailNorm == "" {
			return 0, store.ErrInvalid
		}
	}
	r.inserted += len(ds)
	if len(ds) > r.maxBatch {
		r.maxBatch = len(ds)
	}
	return len(ds), nil
}

func (r *countingDeliveries) CountByStatus(context.Context, string) (map[store.DeliveryStatus]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[store.DeliveryStatus]int64{store.DeliveryPending: int64(r.inserted)}, nil
}

type countingStore struct {
	store.Store
	d *countingDeliveries
}

func (s countingStore) Deliveries() store.DeliveryRepo { return s.d }

// countingFixture is fixture with the delivery repo swapped for a counter.
func countingFixture(t testing.TB) (*Ingester, *countingDeliveries, *store.Campaign) {
	_, st, c := fixture(t, host.Limits{})
	d := &countingDeliveries{DeliveryRepo: st.Deliveries()}
	cs := countingStore{Store: st, d: d}
	return New(cs, host.Limits{}, func() time.Time { return testNow }), d, c
}

// maxHeapDuring samples the live heap while fn runs and returns the peak. It
// is a sampler, not a hook, so it can miss a spike shorter than the sampling
// interval; for a claim as coarse as "the body is not buffered" that is
// enough.
func maxHeapDuring(fn func()) uint64 {
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	peak := ms.HeapAlloc

	done := make(chan struct{})
	var mu sync.Mutex
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		var s runtime.MemStats
		for {
			select {
			case <-done:
				return
			case <-t.C:
				runtime.ReadMemStats(&s)
				mu.Lock()
				if s.HeapAlloc > peak {
					peak = s.HeapAlloc
				}
				mu.Unlock()
			}
		}
	}()
	fn()
	close(done)
	mu.Lock()
	defer mu.Unlock()
	return peak
}

// TestIngestStreams200k is the O(batch) memory claim of architecture 7.2: a
// 200k-line body (~20 MB on the wire) is pushed through an io.Pipe, so nothing
// but the ingester's own buffers can hold it.
func TestIngestStreams200k(t *testing.T) {
	if testing.Short() {
		t.Skip("200k lines")
	}
	ctx := context.Background()
	ing, repo, c := countingFixture(t)

	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, &lineGen{n: streamN})
		_ = pw.CloseWithError(err)
	}()

	var res Result
	var err error
	peak := maxHeapDuring(func() { res, err = ing.Ingest(ctx, c.ID, "chunk-big", pr) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != streamN || res.Invalid != 0 || res.Duplicates != 0 || res.Total != streamN {
		t.Fatalf("got %+v", res)
	}
	if repo.inserted != streamN {
		t.Fatalf("store received %d rows, want %d", repo.inserted, streamN)
	}
	if repo.maxBatch != DefaultBatchSize {
		t.Fatalf("largest batch = %d, want %d", repo.maxBatch, DefaultBatchSize)
	}

	const budget = 64 << 20
	t.Logf("peak live heap during a %d-line ingest: %.1f MiB", streamN, float64(peak)/(1<<20))
	if peak > budget {
		t.Fatalf("peak heap %d bytes exceeds %d: the body is being buffered", peak, budget)
	}
}

// TestIngestStoresEveryRow is the same shape against memstore, at a size where
// keeping every row is affordable: it is the dedupe-and-persist check the
// streaming test above deliberately does not make.
func TestIngestStoresEveryRow(t *testing.T) {
	if testing.Short() {
		t.Skip("20k lines")
	}
	ctx := context.Background()
	const n = 20_000
	ing, st, c := fixture(t, host.Limits{})

	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, &lineGen{n: n})
		_ = pw.CloseWithError(err)
	}()
	res, err := ing.Ingest(ctx, c.ID, "", pr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != n || res.Total != n {
		t.Fatalf("got %+v", res)
	}
	counts, err := st.Deliveries().CountByStatus(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[store.DeliveryPending] != n {
		t.Fatalf("memstore holds %d pending deliveries, want %d", counts[store.DeliveryPending], n)
	}
	// The whole body again: every line is now a duplicate.
	again, err := ing.Ingest(ctx, c.ID, "", &lineGen{n: n})
	if err != nil {
		t.Fatal(err)
	}
	if again.Accepted != 0 || again.Duplicates != n || again.Total != n {
		t.Fatalf("replay = %+v", again)
	}
}

// BenchmarkIngest200k reports the per-call cost of a 200k-line body with the
// store stubbed out, so what it measures is decode + validate + batch.
func BenchmarkIngest200k(b *testing.B) {
	ctx := context.Background()
	ing, _, c := countingFixture(b)
	b.ReportAllocs()
	start := time.Now()
	for b.Loop() {
		res, err := ing.Ingest(ctx, c.ID, "", &lineGen{n: streamN})
		if err != nil {
			b.Fatal(err)
		}
		if res.Accepted != streamN {
			b.Fatalf("accepted %d", res.Accepted)
		}
	}
	b.ReportMetric(float64(time.Since(start))/float64(b.N*streamN), "ns/line")
}
