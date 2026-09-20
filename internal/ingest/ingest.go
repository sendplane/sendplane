// Package ingest appends recipients to a campaign from a streaming NDJSON
// body (architecture.md 7.2, ADR-0005).
//
// One line is one recipient. The body is decoded line by line and inserted in
// batches, so the memory an ingest call uses is proportional to the batch
// size, never to the body: a 1M-recipient campaign is a 150 MB request that
// the server never holds.
//
// Idempotency has two layers, as in the architecture doc:
//
//   - chunk level: the caller's Idempotency-Key is recorded as a
//     store.RecipientChunk. Re-sending a completed chunk returns the stored
//     counts without reading the body at all.
//   - row level: store.DeliveryRepo.InsertBatch skips a delivery whose
//     (campaign_id, email_norm) already exists, so re-sending a chunk that
//     failed halfway through only inserts what is missing.
//
// A malformed line never fails the call: it is counted in Result.Invalid,
// described in Result.Errors and skipped. Only conditions that make the whole
// request meaningless (campaign not editable, recipient limit exceeded, store
// or transport failure) return an error.
package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// Errors that abort the whole call. The API layer maps ErrTooManyRecipients to
// 413 and ErrCampaignNotEditable to 409.
var (
	// ErrCampaignNotEditable is returned when the campaign is past the point
	// where recipients may be appended (architecture 7.1: draft or scheduled).
	ErrCampaignNotEditable = errors.New("ingest: campaign does not accept recipients")
	// ErrTooManyRecipients is returned when the campaign would exceed
	// Limits.MaxRecipientsPerCampaign. Deliveries accepted before the limit
	// was reached stay inserted and are reported in the returned Result.
	ErrTooManyRecipients = errors.New("ingest: campaign recipient limit exceeded")
)

// Per-line rejections. They are reported through Result.Errors and never
// returned from Ingest.
var (
	ErrLineTooLong       = errors.New("ingest: line exceeds the line length limit")
	ErrBadJSON           = errors.New("ingest: line is not a JSON object")
	ErrBadEmail          = errors.New("ingest: invalid email")
	ErrBadName           = errors.New("ingest: name contains CR or LF")
	ErrBadLocale         = errors.New("ingest: invalid locale")
	ErrVarsTooLarge      = errors.New("ingest: vars exceed the vars size limit")
	ErrBadUnsubscribeURL = errors.New("ingest: unsubscribe_url must be an absolute http(s) URL")
)

// DefaultBatchSize is the insert batch size from architecture 7.2.
const DefaultBatchSize = 2000

// DefaultMaxLineErrors caps Result.Errors. Invalid keeps counting past it: a
// client that sent a broken file needs the count and a sample, not a million
// messages.
const DefaultMaxLineErrors = 100

// maxReportedBytes truncates the values echoed back in a LineError, so one
// absurd line cannot inflate the response.
const maxReportedBytes = 200

// clip shortens a value quoted into a rejection message.
func clip(s string) string {
	if len(s) > maxReportedBytes {
		return s[:maxReportedBytes] + "..."
	}
	return s
}

// readBufferBytes is the bufio.Reader size. Lines longer than it are still
// read (in several ReadSlice calls) up to the line limit.
const readBufferBytes = 64 << 10

// localeRe is the BCP47-shaped subset sendplane accepts: a 2-3 letter language
// and any number of alphanumeric subtags. It is deliberately not a full RFC
// 5646 parser; the locale is only ever used as a lookup key into the
// translation bundles (internal/render), so what matters is that it cannot
// carry anything but tag characters.
var localeRe = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// RecipientLine is one line of the NDJSON body.
//
//	{"email":"a@x.com","name":"A","locale":"ko","vars":{"plan":"pro"},"unsubscribe_url":"https://..."}
type RecipientLine struct {
	Email          string         `json:"email"`
	Name           string         `json:"name,omitempty"`
	Locale         string         `json:"locale,omitempty"`
	Vars           map[string]any `json:"vars,omitempty"`
	UnsubscribeURL string         `json:"unsubscribe_url,omitempty"`
}

// wireLine is what the decoder actually uses: vars stay raw so their size can
// be checked against the limit as they arrived on the wire, before a map is
// built for them.
type wireLine struct {
	Email          string          `json:"email"`
	Name           string          `json:"name"`
	Locale         string          `json:"locale"`
	Vars           json.RawMessage `json:"vars"`
	UnsubscribeURL string          `json:"unsubscribe_url"`
}

// LineError describes one rejected line. Line is 1-based and counts every line
// of the body, including blank ones.
type LineError struct {
	Line   int    `json:"line"`
	Email  string `json:"email,omitempty"`
	Reason string `json:"reason"`
}

// Result is the ingest summary, returned to the caller as the response body of
// POST /campaigns/{id}/recipients.
type Result struct {
	// Accepted is how many deliveries this call inserted.
	Accepted int `json:"accepted"`
	// Duplicates counts lines whose address was already a recipient of the
	// campaign, or appeared twice in this body.
	Duplicates int `json:"duplicates"`
	// Invalid counts rejected lines; the first DefaultMaxLineErrors of them
	// are described in Errors.
	Invalid int `json:"invalid"`
	// Total is the campaign's recipient count after the call.
	Total  int         `json:"total"`
	Errors []LineError `json:"errors,omitempty"`
}

// Option configures an Ingester.
type Option func(*Ingester)

// WithBatchSize replaces the number of deliveries per InsertBatch call
// (DefaultBatchSize). It is the knob that trades memory for round trips.
func WithBatchSize(n int) Option {
	return func(i *Ingester) {
		if n > 0 {
			i.batchSize = n
		}
	}
}

// WithMaxLineErrors replaces the cap on Result.Errors (DefaultMaxLineErrors).
func WithMaxLineErrors(n int) Option {
	return func(i *Ingester) {
		if n >= 0 {
			i.maxLineErrors = n
		}
	}
}

// Ingester appends recipients to campaigns of one tenant. It holds no state
// between calls and is safe for concurrent use.
type Ingester struct {
	st            store.Store
	limits        host.Limits
	clock         func() time.Time
	batchSize     int
	maxLineErrors int
}

// New returns an Ingester bound to a tenant store. Zero fields of limits fall
// back to host.DefaultLimits; a nil clock is time.Now.
func New(st store.Store, limits host.Limits, clock func() time.Time, opts ...Option) *Ingester {
	if clock == nil {
		clock = time.Now
	}
	i := &Ingester{
		st:            st,
		limits:        limits.WithDefaults(),
		clock:         clock,
		batchSize:     DefaultBatchSize,
		maxLineErrors: DefaultMaxLineErrors,
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

// Ingest streams an NDJSON body into the campaign's deliveries.
//
// chunkKey is the caller's Idempotency-Key (the API layer falls back to a hash
// of the body). When it is not empty the call is recorded as a
// store.RecipientChunk: a completed chunk replays its stored counts without
// reading r, and a chunk left in progress by a failed call is simply
// re-streamed, which the (campaign_id, email_norm) unique key makes safe. An
// empty key records nothing.
//
// On error the returned Result still holds what was accepted before the error,
// so the API can report partial progress; the chunk is then left in progress.
func (i *Ingester) Ingest(ctx context.Context, campaignID, chunkKey string, r io.Reader) (Result, error) {
	c, err := i.st.Campaigns().Get(ctx, campaignID)
	if err != nil {
		return Result{}, err
	}
	switch c.Status {
	case store.CampaignDraft, store.CampaignScheduled:
	default:
		return Result{}, fmt.Errorf("%w: campaign %s is %s", ErrCampaignNotEditable, c.ID, c.Status)
	}

	existing, err := i.count(ctx, campaignID)
	if err != nil {
		return Result{}, err
	}

	if chunkKey != "" {
		prev, err := i.st.RecipientChunks().Get(ctx, campaignID, chunkKey)
		switch {
		case err == nil && prev.State == store.ChunkCompleted:
			return Result{
				Accepted:   prev.Accepted,
				Duplicates: prev.Duplicates,
				Invalid:    prev.Invalid,
				Total:      existing,
			}, nil
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return Result{}, err
		}
		if err := i.st.RecipientChunks().Put(ctx, &store.RecipientChunk{
			CampaignID: campaignID,
			Key:        chunkKey,
			State:      store.ChunkPending,
		}); err != nil {
			return Result{}, err
		}
	}

	rn := &run{
		ing:      i,
		camp:     c,
		now:      store.TruncateTime(i.clock()),
		existing: existing,
		batch:    make([]store.Delivery, 0, i.batchSize),
		seen:     make(map[string]struct{}, i.batchSize),
	}
	res, err := rn.stream(ctx, r)

	total, cerr := i.count(ctx, campaignID)
	if cerr == nil {
		res.Total = total
	} else if err == nil {
		err = cerr
	}
	if err != nil {
		return res, err
	}

	if chunkKey != "" {
		if err := i.st.RecipientChunks().Put(ctx, &store.RecipientChunk{
			CampaignID: campaignID,
			Key:        chunkKey,
			State:      store.ChunkCompleted,
			Accepted:   res.Accepted,
			Duplicates: res.Duplicates,
			Invalid:    res.Invalid,
		}); err != nil {
			return res, err
		}
	}
	return res, nil
}

// count sums CountByStatus, which is how the campaign's recipient count is
// defined everywhere else (architecture 7.3): no counter is kept on the
// campaign row.
func (i *Ingester) count(ctx context.Context, campaignID string) (int, error) {
	byStatus, err := i.st.Deliveries().CountByStatus(ctx, campaignID)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, n := range byStatus {
		total += int(n)
	}
	return total, nil
}

// run is the state of one Ingest call.
type run struct {
	ing      *Ingester
	camp     *store.Campaign
	now      time.Time
	existing int

	batch []store.Delivery
	// seen deduplicates within the current batch only. Duplicates that span
	// batches are caught by the store's unique key, which is what keeps this
	// map O(batch) instead of O(recipients).
	seen map[string]struct{}
	res  Result
}

func (r *run) stream(ctx context.Context, src io.Reader) (Result, error) {
	max := r.ing.limits.MaxRecipientsPerCampaign
	if r.existing >= max {
		return r.res, fmt.Errorf("%w: campaign %s already has %d of %d recipients",
			ErrTooManyRecipients, r.camp.ID, r.existing, max)
	}

	lr := &lineReader{br: bufio.NewReaderSize(src, readBufferBytes), max: r.ing.limits.MaxRecipientLineBytes}
	for lineNo := 1; ; lineNo++ {
		if err := ctx.Err(); err != nil {
			return r.res, err
		}
		line, err := lr.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, ErrLineTooLong) {
			r.reject(lineNo, "", err)
			continue
		}
		if err != nil {
			return r.res, err
		}
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}
		d, err := r.parse(line)
		if err != nil {
			r.reject(lineNo, d.Email, err)
			continue
		}
		if _, dup := r.seen[d.EmailNorm]; dup {
			r.res.Duplicates++
			continue
		}
		// Flush early rather than overshoot the limit: only after the flush
		// is it known how many of the pending rows were really new.
		//
		// Sharp edge: once the budget is exhausted, any further line is
		// rejected, including one that would have turned out to be a
		// duplicate. Telling the two apart costs a store round trip per
		// line, and the case only arises on a campaign sitting exactly on
		// its cap.
		if r.existing+r.res.Accepted+len(r.batch) >= max {
			if err := r.flush(ctx); err != nil {
				return r.res, err
			}
			if r.existing+r.res.Accepted >= max {
				return r.res, fmt.Errorf("%w: campaign %s would exceed %d recipients",
					ErrTooManyRecipients, r.camp.ID, max)
			}
		}
		r.seen[d.EmailNorm] = struct{}{}
		r.batch = append(r.batch, d)
		if len(r.batch) >= r.ing.batchSize {
			if err := r.flush(ctx); err != nil {
				return r.res, err
			}
		}
	}
	if err := r.flush(ctx); err != nil {
		return r.res, err
	}
	return r.res, nil
}

func (r *run) flush(ctx context.Context) error {
	if len(r.batch) == 0 {
		return nil
	}
	inserted, err := r.ing.st.Deliveries().InsertBatch(ctx, r.batch)
	r.res.Accepted += inserted
	if err != nil {
		return err
	}
	r.res.Duplicates += len(r.batch) - inserted
	r.batch = r.batch[:0]
	clear(r.seen)
	return nil
}

func (r *run) reject(lineNo int, email string, err error) {
	r.res.Invalid++
	if len(r.res.Errors) >= r.ing.maxLineErrors {
		return
	}
	email = clip(email)
	r.res.Errors = append(r.res.Errors, LineError{Line: lineNo, Email: email, Reason: err.Error()})
}

// parse validates one line and builds the delivery to insert. On error the
// returned delivery still carries Email when the line parsed as JSON, so the
// rejection can name the address.
func (r *run) parse(line []byte) (store.Delivery, error) {
	i := r.ing
	var w wireLine
	if err := json.Unmarshal(line, &w); err != nil {
		return store.Delivery{}, fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	d := store.Delivery{Email: strings.TrimSpace(w.Email)}

	norm, err := store.NormalizeEmail(w.Email)
	if err != nil {
		return d, fmt.Errorf("%w: %v", ErrBadEmail, err)
	}
	d.EmailNorm = norm
	if strings.ContainsAny(d.Email, "<>") {
		// The line used the "Name <a@b>" form. Only the address is kept:
		// Email is what the sender puts in the To header and what the
		// {{ email }} binding renders (internal/render), so it must not carry
		// a second display name. The original casing of a plain address is
		// kept, which is why this is not just EmailNorm everywhere.
		d.Email = norm
	}

	d.Name = strings.TrimSpace(w.Name)
	if strings.ContainsAny(d.Name, "\r\n") {
		return d, ErrBadName
	}
	d.Locale = strings.TrimSpace(w.Locale)
	if d.Locale != "" && !localeRe.MatchString(d.Locale) {
		return d, fmt.Errorf("%w: %q", ErrBadLocale, clip(d.Locale))
	}
	if len(w.Vars) > 0 {
		if len(w.Vars) > i.limits.MaxVarsBytes {
			return d, fmt.Errorf("%w: %d > %d bytes", ErrVarsTooLarge, len(w.Vars), i.limits.MaxVarsBytes)
		}
		if err := json.Unmarshal(w.Vars, &d.Vars); err != nil {
			return d, fmt.Errorf("%w: vars: %v", ErrBadJSON, err)
		}
	}
	d.UnsubscribeURL = strings.TrimSpace(w.UnsubscribeURL)
	if d.UnsubscribeURL != "" {
		u, err := url.Parse(d.UnsubscribeURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return d, fmt.Errorf("%w: %q", ErrBadUnsubscribeURL, clip(d.UnsubscribeURL))
		}
	}

	d.ID = store.NewID()
	r.fill(&d)
	return d, nil
}

// fill copies the campaign-derived fields onto a parsed delivery.
func (r *run) fill(d *store.Delivery) {
	d.TenantID = r.camp.TenantID
	d.CampaignID = r.camp.ID
	d.VersionID = r.camp.VersionID
	d.SenderID = r.camp.SenderID
	d.Lane = store.LaneBulk
	d.Priority = 0
	d.Status = store.DeliveryPending
	d.NextAttemptAt = r.now
	d.CreatedAt = r.now
	d.UpdatedAt = r.now
}

// lineReader reads newline-delimited records with a hard per-line byte budget.
//
// bufio.Scanner cannot be used: it fails the whole stream with
// bufio.ErrTooLong on the first oversized token, and architecture 7.2 wants an
// oversized line to be one invalid row among a million good ones. This reader
// drains the rest of an over-long line and keeps going, so the memory it holds
// stays bounded by max.
type lineReader struct {
	br  *bufio.Reader
	max int
	buf []byte
}

// next returns the next line without its line terminator. It returns io.EOF at
// the end of the stream and ErrLineTooLong for a line over the budget (which
// it consumes, so the next call resumes at the following line).
func (lr *lineReader) next() ([]byte, error) {
	lr.buf = lr.buf[:0]
	tooLong := false
	for {
		chunk, err := lr.br.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return nil, err
		}
		n := len(chunk)
		if n > 0 && chunk[n-1] == '\n' {
			n--
		}
		if !tooLong {
			if len(lr.buf)+n > lr.max {
				tooLong = true
				lr.buf = lr.buf[:0]
			} else {
				lr.buf = append(lr.buf, chunk[:n]...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(chunk) == 0 && !tooLong {
			return nil, io.EOF
		}
		if tooLong {
			return nil, ErrLineTooLong
		}
		return trimCR(lr.buf), nil
	}
}

// trimCR drops the carriage return of a CRLF body. A stray CR inside the line
// is left alone: the JSON decoder and NormalizeEmail reject what matters.
func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}
