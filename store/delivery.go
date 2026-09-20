package store

import (
	"context"
	"time"
)

// Delivery is one recipient of one MessageVersion, and at the same time the
// queue item senders claim (ADR-0002, ADR-0003). It has no Version field: all
// of its transitions are CAS on status/lease, not optimistic concurrency.
type Delivery struct {
	ID       string
	TenantID string
	// CampaignID is empty for transactional and probe deliveries. Only
	// campaign deliveries take part in the (campaign_id, email_norm) unique
	// key.
	CampaignID string
	VersionID  string
	SenderID   string

	Lane     Lane
	Priority int
	Status   DeliveryStatus

	Email     string
	EmailNorm string
	Name      string
	Locale    string
	Vars      map[string]any
	// UnsubscribeURL is the per-recipient host destination, when the caller
	// supplied one.
	UnsubscribeURL string

	// AttemptCount is consumed by transient/rate_limited attempts only.
	AttemptCount int
	// RetryGen is bumped by a manual retry so that history stays intact.
	RetryGen      int
	NextAttemptAt time.Time

	LeaseOwner string
	LeaseUntil time.Time

	LastErrorClass ErrorClass
	LastSMTPCode   int
	LastError      string

	MessageID  string
	SentAt     time.Time
	FinishedAt time.Time

	// First-interaction summary columns, written by conditional NULL-only
	// updates so they are the basis of unique tracking counts (architecture 9.3).
	FirstOpenedAt  time.Time
	FirstClickedAt time.Time
	UnsubscribedAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeliveryAttempt is one SMTP attempt. Response codes, errors, the transport
// used and the duration live here, never on the Delivery.
type DeliveryAttempt struct {
	ID         string
	TenantID   string
	DeliveryID string

	// AttemptNo is the value of Delivery.AttemptCount this attempt produced.
	AttemptNo   int
	RetryGen    int
	TransportID string

	StartedAt  time.Time
	FinishedAt time.Time

	SMTPCode     int
	EnhancedCode string
	ErrorClass   ErrorClass
	Error        string

	CreatedAt time.Time
}

// ClaimRequest is one sender's request for work.
type ClaimRequest struct {
	Lane Lane
	// CampaignIDs filters bulk claims to the running campaign set:
	//   nil            no filter at all
	//   empty non-nil  only deliveries without a campaign
	//   non-empty      deliveries of those campaigns, plus deliveries
	//                  without a campaign
	// This mirrors "campaign_id IS NULL OR campaign_id = ANY($3)"
	// (architecture 5.2).
	CampaignIDs []string
	Limit       int
	LeaseFor    time.Duration
	WorkerID    string
	// Now is the claim timestamp: only queued/deferred rows whose
	// NextAttemptAt is at or before it are eligible, and the lease runs to
	// Now+LeaseFor.
	Now time.Time
}

// DeliveryResult is one finished attempt, committed in batches by Complete.
type DeliveryResult struct {
	DeliveryID string
	// LeaseOwner must still hold the lease, otherwise the result is dropped.
	LeaseOwner string

	NewStatus DeliveryStatus
	// NextAttemptAt is only meaningful for NewStatus deferred.
	NextAttemptAt time.Time

	ErrorClass ErrorClass
	SMTPCode   int
	Error      string
	MessageID  string

	// IncrementAttempt tells the store to consume one retry. The sender sets
	// it for transient and rate_limited results only: transport-level faults
	// (auth, TLS) are recorded but must not exhaust a delivery (ADR-0003).
	IncrementAttempt bool

	// Attempt is inserted alongside the transition. Nil records no attempt.
	Attempt *DeliveryAttempt
}

// RetryFilter selects deliveries for a manual retry.
type RetryFilter struct {
	// CampaignID is empty to target deliveries without a campaign.
	CampaignID string
	// DeliveryIDs restricts the filter to specific deliveries (single-delivery
	// retry). Nil means "every match".
	DeliveryIDs  []string
	Statuses     []DeliveryStatus
	ErrorClasses []ErrorClass
	// Now is the timestamp the requeued deliveries become eligible at.
	Now time.Time
}

// DeliveryFilter narrows a campaign delivery listing.
type DeliveryFilter struct {
	Statuses     []DeliveryStatus
	ErrorClasses []ErrorClass
	// EmailNorm matches one recipient exactly.
	EmailNorm string
}

// DeliveryRepo is the queue. Every transition is conditional; nothing here
// requires a transaction spanning another repository.
type DeliveryRepo interface {
	// InsertBatch inserts deliveries, skipping campaign deliveries whose
	// (campaign_id, email_norm) already exists, and returns how many rows were
	// actually inserted. Deliveries without a campaign are always inserted.
	// EmailNorm must already be normalized (see NormalizeEmail); an empty one
	// is ErrInvalid.
	InsertBatch(ctx context.Context, ds []Delivery) (inserted int, err error)

	Get(ctx context.Context, id string) (*Delivery, error)
	ListByCampaign(ctx context.Context, campaignID string, f DeliveryFilter, p Page) (Result[Delivery], error)

	// Claim leases up to Limit eligible deliveries, ordered by priority
	// descending then NextAttemptAt ascending, and returns them as leased.
	Claim(ctx context.Context, req ClaimRequest) ([]Delivery, error)

	// Complete applies finished attempts and clears the lease. A result whose
	// delivery is no longer held by LeaseOwner is ignored, including its
	// attempt row, which also makes a repeated Complete a no-op.
	Complete(ctx context.Context, results []DeliveryResult) error

	// MarkSent is the fast path recorded right after SMTP 250, before the
	// batched Complete (architecture 8.1). It returns ErrLeaseLost if the
	// delivery is not leased by owner. It keeps the lease, so the later
	// Complete still attaches the attempt; ReleaseExpiredLeases never touches
	// a row that is already sent.
	MarkSent(ctx context.Context, id, owner, messageID string, at time.Time) error

	// ReleaseExpiredLeases returns leased deliveries whose lease has expired to
	// deferred (or queued, when nothing was attempted), keeping AttemptCount.
	ReleaseExpiredLeases(ctx context.Context, now time.Time, limit int) (int, error)

	// CountByStatus counts one campaign's deliveries, or, for an empty
	// campaignID, the deliveries without a campaign.
	CountByStatus(ctx context.Context, campaignID string) (map[DeliveryStatus]int64, error)

	// BulkTransition moves up to limit deliveries between statuses, so cancel
	// can walk a million rows in chunks.
	BulkTransition(ctx context.Context, campaignID string, from []DeliveryStatus, to DeliveryStatus, limit int) (int, error)

	// Requeue is the manual retry: matching deliveries go back to queued with
	// RetryGen bumped and AttemptCount kept.
	Requeue(ctx context.Context, f RetryFilter, limit int) (int, error)

	// SetFirstOpened records the first open. It reports changed=false when the
	// column was already set (or the delivery is gone), which is what makes
	// unique counts correct.
	SetFirstOpened(ctx context.Context, id string, at time.Time) (changed bool, err error)
	SetFirstClicked(ctx context.Context, id string, at time.Time) (changed bool, err error)
	SetUnsubscribed(ctx context.Context, id string, at time.Time) (changed bool, err error)

	// DeleteBefore enforces retention in chunks.
	DeleteBefore(ctx context.Context, campaignID string, before time.Time, limit int) (int, error)
}

// AttemptRepo stores the per-attempt history. Attempts are written by
// DeliveryRepo.Complete as well; Insert exists for the paths that record an
// attempt without a transition (probe, transport faults).
type AttemptRepo interface {
	Insert(ctx context.Context, as []DeliveryAttempt) error
	ListByDelivery(ctx context.Context, deliveryID string, p Page) (Result[DeliveryAttempt], error)
}
