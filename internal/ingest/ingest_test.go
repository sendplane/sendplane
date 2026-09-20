package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

var testNow = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

// fixture returns an ingester bound to a fresh memstore holding one draft
// campaign.
func fixture(t testing.TB, limits host.Limits, opts ...Option) (*Ingester, store.Store, *store.Campaign) {
	t.Helper()
	ctx := context.Background()
	p := memstore.New(memstore.WithClock(func() time.Time { return testNow }))
	t.Cleanup(func() { _ = p.Close() })
	st, err := p.ForTenant(ctx, "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	c := &store.Campaign{
		Name:          "launch",
		VersionID:     "version-1",
		SenderID:      "sender-1",
		DefaultLocale: "ko",
		Status:        store.CampaignDraft,
	}
	if err := st.Campaigns().Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	return New(st, limits, func() time.Time { return testNow }, opts...), st, c
}

func ndjson(lines ...string) io.Reader { return strings.NewReader(strings.Join(lines, "\n") + "\n") }

func TestIngestHappyPath(t *testing.T) {
	ctx := context.Background()
	ing, st, c := fixture(t, host.Limits{})

	res, err := ing.Ingest(ctx, c.ID, "", ndjson(
		`{"email":"A@Example.COM","name":" Ada ","locale":"ko-KR","vars":{"plan":"pro"},"unsubscribe_url":"https://host.example/u/1"}`,
		`{"email":"Bob <bob@example.com>"}`,
		``,
		`   `,
		`{"email":"c@example.com","locale":"en"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 3 || res.Duplicates != 0 || res.Invalid != 0 || res.Total != 3 {
		t.Fatalf("got %+v", res)
	}

	page, err := st.Deliveries().ListByCampaign(ctx, c.ID, store.DeliveryFilter{EmailNorm: "a@example.com"}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(page.Items))
	}
	d := page.Items[0]
	if d.TenantID != "tenant-1" || d.CampaignID != c.ID || d.VersionID != "version-1" || d.SenderID != "sender-1" {
		t.Errorf("campaign fields not copied: %+v", d)
	}
	if d.Status != store.DeliveryPending || d.Lane != store.LaneBulk || d.Priority != 0 {
		t.Errorf("status/lane/priority = %v/%v/%d", d.Status, d.Lane, d.Priority)
	}
	if d.Email != "A@Example.COM" || d.EmailNorm != "a@example.com" {
		t.Errorf("email %q / %q", d.Email, d.EmailNorm)
	}
	if d.Name != "Ada" || d.Locale != "ko-KR" || d.Vars["plan"] != "pro" {
		t.Errorf("name/locale/vars = %q/%q/%v", d.Name, d.Locale, d.Vars)
	}
	if d.UnsubscribeURL != "https://host.example/u/1" {
		t.Errorf("unsubscribe_url = %q", d.UnsubscribeURL)
	}
	want := store.TruncateTime(testNow)
	if !d.NextAttemptAt.Equal(want) || !d.CreatedAt.Equal(want) {
		t.Errorf("timestamps = %v / %v, want %v", d.NextAttemptAt, d.CreatedAt, want)
	}

	// "Bob <bob@example.com>" is reduced to the address: the display name must
	// not end up in the To header a second time.
	got, err := st.Deliveries().ListByCampaign(ctx, c.ID, store.DeliveryFilter{EmailNorm: "bob@example.com"}, store.Page{})
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("bob: %v, %d items", err, len(got.Items))
	}
	if got.Items[0].Email != "bob@example.com" {
		t.Errorf("bob Email = %q, want the bare address", got.Items[0].Email)
	}
}

func TestIngestAppendsAcrossCalls(t *testing.T) {
	ctx := context.Background()
	ing, _, c := fixture(t, host.Limits{})

	if _, err := ing.Ingest(ctx, c.ID, "", ndjson(`{"email":"a@example.com"}`)); err != nil {
		t.Fatal(err)
	}
	res, err := ing.Ingest(ctx, c.ID, "", ndjson(`{"email":"b@example.com"}`, `{"email":"A@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 1 || res.Duplicates != 1 || res.Total != 2 {
		t.Fatalf("got %+v", res)
	}
}

func TestIngestScheduledCampaignAccepted(t *testing.T) {
	ctx := context.Background()
	ing, st, c := fixture(t, host.Limits{})
	c.Status = store.CampaignScheduled
	if err := st.Campaigns().Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := ing.Ingest(ctx, c.ID, "", ndjson(`{"email":"a@example.com"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestIngestNotEditable(t *testing.T) {
	ctx := context.Background()
	for _, s := range []store.CampaignStatus{
		store.CampaignRunning, store.CampaignPaused, store.CampaignCompleted, store.CampaignCancelled,
	} {
		ing, st, c := fixture(t, host.Limits{})
		c.Status = s
		if err := st.Campaigns().Update(ctx, c); err != nil {
			t.Fatal(err)
		}
		_, err := ing.Ingest(ctx, c.ID, "", failingReader{t})
		if !errors.Is(err, ErrCampaignNotEditable) {
			t.Errorf("%s: err = %v, want ErrCampaignNotEditable", s, err)
		}
	}
}

func TestIngestUnknownCampaign(t *testing.T) {
	ing, _, _ := fixture(t, host.Limits{})
	_, err := ing.Ingest(context.Background(), "nope", "", failingReader{t})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// failingReader fails the test if anything reads it: the chunk replay and the
// precondition checks must answer without touching the body.
type failingReader struct{ t testing.TB }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("body was read when it should not have been")
	return 0, io.EOF
}

func TestChunkIdempotency(t *testing.T) {
	ctx := context.Background()
	ing, st, c := fixture(t, host.Limits{})

	first, err := ing.Ingest(ctx, c.ID, "chunk-0007", ndjson(
		`{"email":"a@example.com"}`,
		`{"email":"a@example.com"}`,
		`{"email":"not-an-email"}`,
		`{"email":"b@example.com"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if first.Accepted != 2 || first.Duplicates != 1 || first.Invalid != 1 || first.Total != 2 {
		t.Fatalf("first = %+v", first)
	}

	ch, err := st.RecipientChunks().Get(ctx, c.ID, "chunk-0007")
	if err != nil {
		t.Fatal(err)
	}
	if ch.State != store.ChunkCompleted || ch.Accepted != 2 || ch.Duplicates != 1 || ch.Invalid != 1 {
		t.Fatalf("chunk = %+v", ch)
	}

	// Replay: the stored counts come back and the body is never read.
	replay, err := ing.Ingest(ctx, c.ID, "chunk-0007", failingReader{t})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Accepted != 2 || replay.Duplicates != 1 || replay.Invalid != 1 || replay.Total != 2 {
		t.Fatalf("replay = %+v, want the stored counts", replay)
	}
	if len(replay.Errors) != 0 {
		t.Errorf("replay carries line errors: %+v", replay.Errors)
	}
}

func TestChunkLeftInProgressOnError(t *testing.T) {
	ctx := context.Background()
	ing, st, c := fixture(t, host.Limits{}, WithBatchSize(2))

	_, err := ing.Ingest(ctx, c.ID, "chunk-1", io.MultiReader(
		ndjson(`{"email":"a@example.com"}`, `{"email":"b@example.com"}`),
		errReader{errors.New("connection reset")},
	))
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v", err)
	}
	ch, err := st.RecipientChunks().Get(ctx, c.ID, "chunk-1")
	if err != nil {
		t.Fatal(err)
	}
	if ch.State != store.ChunkPending {
		t.Fatalf("chunk state = %v, want pending so a retry re-streams", ch.State)
	}
	// The rows written before the failure survive; the retry dedupes on them.
	res, err := ing.Ingest(ctx, c.ID, "chunk-1", ndjson(
		`{"email":"a@example.com"}`, `{"email":"b@example.com"}`, `{"email":"c@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 1 || res.Duplicates != 2 || res.Total != 3 {
		t.Fatalf("retry = %+v", res)
	}
}

func TestNoChunkKeyRecordsNothing(t *testing.T) {
	ctx := context.Background()
	ing, st, c := fixture(t, host.Limits{})
	if _, err := ing.Ingest(ctx, c.ID, "", ndjson(`{"email":"a@example.com"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecipientChunks().Get(ctx, c.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestDuplicatesWithinAndAcrossBatches(t *testing.T) {
	ctx := context.Background()
	ing, _, c := fixture(t, host.Limits{}, WithBatchSize(2))

	// a, a  -> in-batch duplicate (never reaches the store)
	// b, a  -> a again, now in a later batch: the unique key catches it
	res, err := ing.Ingest(ctx, c.ID, "", ndjson(
		`{"email":"a@example.com"}`,
		`{"email":"A@EXAMPLE.com"}`,
		`{"email":"b@example.com"}`,
		`{"email":"a@example.com"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 || res.Duplicates != 2 || res.Invalid != 0 || res.Total != 2 {
		t.Fatalf("got %+v", res)
	}
}

func TestInvalidLines(t *testing.T) {
	ctx := context.Background()
	ing, _, c := fixture(t, host.Limits{MaxVarsBytes: 32, MaxRecipientLineBytes: 200})

	big := `{"email":"big@example.com","vars":{"blob":"` + strings.Repeat("x", 64) + `"}}`
	long := `{"email":"long@example.com","name":"` + strings.Repeat("y", 400) + `"}`
	res, err := ing.Ingest(ctx, c.ID, "", ndjson(
		`{"email":"ok@example.com"}`, // 1 accepted
		`{"email":"not an email"}`,   // 2 bad email
		`{"email":}`,                 // 3 bad JSON
		`"just a string"`,            // 4 bad JSON (not an object)
		`{"email":"n@example.com","name":"A\r\nBcc: x@y"}`, // 5 CR/LF in name
		big, // 6 vars too large
		`{"email":"l@example.com","locale":"korean_KOREA"}`,                 // 7 bad locale
		`{"email":"u@example.com","unsubscribe_url":"javascript:alert(1)"}`, // 8 bad URL
		long,                           // 9 line too long
		`{"email":"","name":"nobody"}`, // 10 empty email
		`{"email":"ok2@example.com"}`,  // 11 accepted
	))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 || res.Invalid != 9 || res.Duplicates != 0 || res.Total != 2 {
		t.Fatalf("got %+v", res)
	}
	if len(res.Errors) != 9 {
		t.Fatalf("errors = %d: %+v", len(res.Errors), res.Errors)
	}
	wantLines := []int{2, 3, 4, 5, 6, 7, 8, 9, 10}
	wantReason := []error{
		ErrBadEmail, ErrBadJSON, ErrBadJSON, ErrBadName, ErrVarsTooLarge,
		ErrBadLocale, ErrBadUnsubscribeURL, ErrLineTooLong, ErrBadEmail,
	}
	for i, le := range res.Errors {
		if le.Line != wantLines[i] {
			t.Errorf("errors[%d].Line = %d, want %d (%+v)", i, le.Line, wantLines[i], le)
		}
		if !strings.Contains(le.Reason, wantReason[i].Error()) {
			t.Errorf("errors[%d].Reason = %q, want %v", i, le.Reason, wantReason[i])
		}
	}
	if res.Errors[0].Email != "not an email" {
		t.Errorf("bad-email entry should echo the address, got %q", res.Errors[0].Email)
	}
	// An over-long line is skipped whole: the reader resumes at the next one.
	if res.Errors[7].Email != "" {
		t.Errorf("too-long entry email = %q, want empty", res.Errors[7].Email)
	}
}

func TestLocaleAccepted(t *testing.T) {
	ctx := context.Background()
	for _, loc := range []string{"ko", "en", "ko-KR", "zh-Hans-CN", "", "es-419"} {
		ing, _, c := fixture(t, host.Limits{})
		res, err := ing.Ingest(ctx, c.ID, "", strings.NewReader(
			fmt.Sprintf(`{"email":"a@example.com","locale":%q}`+"\n", loc)))
		if err != nil {
			t.Fatal(err)
		}
		if res.Accepted != 1 {
			t.Errorf("locale %q rejected: %+v", loc, res.Errors)
		}
	}
}

func TestLineErrorsCapped(t *testing.T) {
	ctx := context.Background()
	ing, _, c := fixture(t, host.Limits{}, WithMaxLineErrors(3))
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("{\n")
	}
	res, err := ing.Ingest(ctx, c.ID, "", strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Invalid != 20 || len(res.Errors) != 3 {
		t.Fatalf("invalid = %d, errors = %d", res.Invalid, len(res.Errors))
	}
}

func TestNoTrailingNewline(t *testing.T) {
	ctx := context.Background()
	ing, _, c := fixture(t, host.Limits{})
	res, err := ing.Ingest(ctx, c.ID, "", strings.NewReader(
		`{"email":"a@example.com"}`+"\n"+`{"email":"b@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 {
		t.Fatalf("got %+v", res)
	}
}

func TestCRLFBody(t *testing.T) {
	ctx := context.Background()
	ing, _, c := fixture(t, host.Limits{})
	res, err := ing.Ingest(ctx, c.ID, "", strings.NewReader(
		`{"email":"a@example.com"}`+"\r\n"+`{"email":"b@example.com"}`+"\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 || res.Invalid != 0 {
		t.Fatalf("got %+v: %+v", res, res.Errors)
	}
}

func TestTooManyRecipients(t *testing.T) {
	ctx := context.Background()
	limits := host.Limits{MaxRecipientsPerCampaign: 5}

	// Exactly at the limit is fine.
	ing, _, c := fixture(t, limits, WithBatchSize(2))
	res, err := ing.Ingest(ctx, c.ID, "", ndjson(lines(5)...))
	if err != nil {
		t.Fatalf("5 of 5: %v", err)
	}
	if res.Accepted != 5 || res.Total != 5 {
		t.Fatalf("got %+v", res)
	}

	// One past it aborts, and what fitted stays inserted.
	ing2, _, c2 := fixture(t, limits, WithBatchSize(2))
	res2, err := ing2.Ingest(ctx, c2.ID, "", ndjson(lines(6)...))
	if !errors.Is(err, ErrTooManyRecipients) {
		t.Fatalf("err = %v, want ErrTooManyRecipients", err)
	}
	if res2.Accepted != 5 || res2.Total != 5 {
		t.Fatalf("partial result = %+v", res2)
	}

	// A campaign already at the limit rejects without reading the body.
	if _, err := ing.Ingest(ctx, c.ID, "", failingReader{t}); !errors.Is(err, ErrTooManyRecipients) {
		t.Fatalf("err = %v, want ErrTooManyRecipients", err)
	}

	// Documented sharp edge: on a campaign sitting exactly on its cap even a
	// duplicate line is rejected, because telling it apart from a new address
	// would cost a store round trip per line.
	ing3, _, c3 := fixture(t, limits, WithBatchSize(2))
	dup := append(lines(5), `{"email":"r0@example.com"}`)
	res3, err := ing3.Ingest(ctx, c3.ID, "", ndjson(dup...))
	if !errors.Is(err, ErrTooManyRecipients) {
		t.Fatalf("err = %v, want ErrTooManyRecipients", err)
	}
	if res3.Accepted != 5 || res3.Total != 5 {
		t.Fatalf("got %+v", res3)
	}
}

func lines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`{"email":"r%d@example.com"}`, i)
	}
	return out
}

func TestIngestContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ing, _, c := fixture(t, host.Limits{})
	cancel()
	if _, err := ing.Ingest(ctx, c.ID, "", ndjson(lines(3)...)); !errors.Is(err, context.Canceled) {
		// Get may fail first; either way the call must not report success.
		if err == nil {
			t.Fatal("cancelled context reported success")
		}
	}
}
