package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/store"
)

// allCampaignStatuses is the closed set of store.CampaignStatus, so the
// transition table below is exhaustive by construction.
var allCampaignStatuses = []store.CampaignStatus{
	store.CampaignDraft, store.CampaignScheduled, store.CampaignRunning,
	store.CampaignPaused, store.CampaignCompleted, store.CampaignCancelled,
}

// readyCampaign creates a campaign in `from` that satisfies every StartCampaign
// precondition, so a rejected transition can only be the state machine's doing.
func readyCampaign(t *testing.T, st store.Store, from store.CampaignStatus) *store.Campaign {
	t.Helper()
	sender := seedSender(t, st)
	cam := seedCampaign(t, st, from, func(c *store.Campaign) { c.SenderID = sender.ID })
	seedDeliveries(t, st, cam.ID, store.DeliveryFailed, 1)
	return cam
}

func TestCampaignStateMachine(t *testing.T) {
	// want is the status after the action, or "" when the action must be
	// rejected with ErrInvalidTransition.
	type row struct {
		action string
		from   store.CampaignStatus
		want   store.CampaignStatus
		ok     bool
	}
	var table []row
	add := func(action string, allowed map[store.CampaignStatus]store.CampaignStatus) {
		for _, from := range allCampaignStatuses {
			to, ok := allowed[from]
			table = append(table, row{action: action, from: from, want: to, ok: ok})
		}
	}
	add("start", map[store.CampaignStatus]store.CampaignStatus{
		store.CampaignDraft: store.CampaignRunning,
	})
	add("schedule", map[store.CampaignStatus]store.CampaignStatus{
		store.CampaignDraft: store.CampaignScheduled,
	})
	add("pause", map[store.CampaignStatus]store.CampaignStatus{
		store.CampaignRunning: store.CampaignPaused,
	})
	add("resume", map[store.CampaignStatus]store.CampaignStatus{
		store.CampaignPaused: store.CampaignRunning,
	})
	add("cancel", map[store.CampaignStatus]store.CampaignStatus{
		store.CampaignDraft:     store.CampaignCancelled,
		store.CampaignScheduled: store.CampaignCancelled,
		store.CampaignRunning:   store.CampaignCancelled,
		store.CampaignPaused:    store.CampaignCancelled,
	})
	add("retry", map[store.CampaignStatus]store.CampaignStatus{
		store.CampaignRunning:   store.CampaignRunning,
		store.CampaignPaused:    store.CampaignPaused,
		store.CampaignCompleted: store.CampaignRunning,
	})

	for _, tc := range table {
		name := tc.action + "/" + tc.from.String()
		t.Run(name, func(t *testing.T) {
			_, st, _, c := newFixture(t, sendplane.Hooks{})
			ctx := context.Background()
			cam := readyCampaign(t, st, tc.from)

			var err error
			switch tc.action {
			case "start":
				err = c.StartCampaign(ctx, st, cam.ID, time.Time{})
			case "schedule":
				err = c.StartCampaign(ctx, st, cam.ID, baseTime.Add(time.Hour))
			case "pause":
				err = c.PauseCampaign(ctx, st, cam.ID)
			case "resume":
				err = c.ResumeCampaign(ctx, st, cam.ID)
			case "cancel":
				err = c.CancelCampaign(ctx, st, cam.ID)
			case "retry":
				_, err = c.RetryCampaign(ctx, st, cam.ID, store.RetryFilter{})
			}

			if !tc.ok {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("err = %v, want ErrInvalidTransition", err)
				}
				if got := getCampaign(t, st, cam.ID); got.Status != tc.from {
					t.Fatalf("a rejected %s changed the status to %s", tc.action, got.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", tc.action, err)
			}
			if got := getCampaign(t, st, cam.ID); got.Status != tc.want {
				t.Fatalf("status = %s, want %s", got.Status, tc.want)
			}
		})
	}
}

func TestStartCampaignSchedulesAndStarts(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{})

	later := seedAndStart(t, st, c, baseTime.Add(time.Hour))
	scheduled := getCampaign(t, st, later.ID)
	if scheduled.Status != store.CampaignScheduled {
		t.Fatalf("status %s, want scheduled", scheduled.Status)
	}
	if !scheduled.ScheduleAt.Equal(baseTime.Add(time.Hour)) {
		t.Errorf("ScheduleAt = %v", scheduled.ScheduleAt)
	}
	if !scheduled.StartedAt.IsZero() {
		t.Errorf("StartedAt set on a scheduled campaign: %v", scheduled.StartedAt)
	}
	if n := len(listOutbox(t, st, store.OutboxPending)); n != 0 {
		t.Errorf("%d events for a campaign that has not started", n)
	}

	// A zero or past time starts immediately and emits campaign.started.
	immediate := seedAndStart(t, st, c, time.Time{})
	got := getCampaign(t, st, immediate.ID)
	if got.Status != store.CampaignRunning {
		t.Fatalf("status %s, want running", got.Status)
	}
	if !got.StartedAt.Equal(baseTime) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, baseTime)
	}
	events := listOutbox(t, st, store.OutboxPending)
	if len(events) != 1 || events[0].Type != EventCampaignStarted {
		t.Fatalf("outbox = %+v, want one campaign.started", events)
	}
}

// seedAndStart creates a ready draft campaign and starts it at `at`.
func seedAndStart(t *testing.T, st store.Store, c *Control, at time.Time) *store.Campaign {
	t.Helper()
	cam := readyCampaign(t, st, store.CampaignDraft)
	if err := c.StartCampaign(context.Background(), st, cam.ID, at); err != nil {
		t.Fatalf("StartCampaign: %v", err)
	}
	return cam
}

func TestStartCampaignPreconditions(t *testing.T) {
	ctx := context.Background()

	t.Run("no recipients", func(t *testing.T) {
		_, st, _, c := newFixture(t, sendplane.Hooks{})
		sender := seedSender(t, st)
		cam := seedCampaign(t, st, store.CampaignDraft, func(c *store.Campaign) { c.SenderID = sender.ID })
		if err := c.StartCampaign(ctx, st, cam.ID, time.Time{}); !errors.Is(err, ErrNoRecipients) {
			t.Fatalf("err = %v, want ErrNoRecipients", err)
		}
	})

	t.Run("no version", func(t *testing.T) {
		_, st, _, c := newFixture(t, sendplane.Hooks{})
		sender := seedSender(t, st)
		cam := seedCampaign(t, st, store.CampaignDraft, func(c *store.Campaign) {
			c.SenderID, c.VersionID = sender.ID, ""
		})
		seedDeliveries(t, st, cam.ID, store.DeliveryPending, 1)
		if err := c.StartCampaign(ctx, st, cam.ID, time.Time{}); !errors.Is(err, ErrNoVersion) {
			t.Fatalf("err = %v, want ErrNoVersion", err)
		}
	})

	t.Run("sender missing", func(t *testing.T) {
		_, st, _, c := newFixture(t, sendplane.Hooks{})
		cam := seedCampaign(t, st, store.CampaignDraft, func(c *store.Campaign) { c.SenderID = "" })
		seedDeliveries(t, st, cam.ID, store.DeliveryPending, 1)
		if err := c.StartCampaign(ctx, st, cam.ID, time.Time{}); !errors.Is(err, ErrNoSender) {
			t.Fatalf("err = %v, want ErrNoSender", err)
		}
	})

	t.Run("sender does not exist", func(t *testing.T) {
		_, st, _, c := newFixture(t, sendplane.Hooks{})
		cam := seedCampaign(t, st, store.CampaignDraft, func(c *store.Campaign) { c.SenderID = "ghost" })
		seedDeliveries(t, st, cam.ID, store.DeliveryPending, 1)
		if err := c.StartCampaign(ctx, st, cam.ID, time.Time{}); !errors.Is(err, ErrNoSender) {
			t.Fatalf("err = %v, want ErrNoSender", err)
		}
	})

	t.Run("campaign missing", func(t *testing.T) {
		_, st, _, c := newFixture(t, sendplane.Hooks{})
		if err := c.StartCampaign(ctx, st, "ghost", time.Time{}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want store.ErrNotFound", err)
		}
	})
}

func TestCancelCampaignRecordsFinishTime(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{})
	ctx := context.Background()

	cam := readyCampaign(t, st, store.CampaignRunning)
	if err := c.CancelCampaign(ctx, st, cam.ID); err != nil {
		t.Fatalf("CancelCampaign: %v", err)
	}
	got := getCampaign(t, st, cam.ID)
	if !got.CompletedAt.Equal(baseTime) {
		t.Errorf("CompletedAt = %v, want %v so retention has a date to key off", got.CompletedAt, baseTime)
	}
	// The delivery rows are the canceller loop's job, not the request's.
	counts, _ := st.Deliveries().CountByStatus(ctx, cam.ID)
	if counts[store.DeliveryCancelled] != 0 {
		t.Errorf("CancelCampaign touched %d delivery rows synchronously", counts[store.DeliveryCancelled])
	}
	events := listOutbox(t, st, store.OutboxPending)
	if len(events) != 1 || events[0].Type != EventCampaignCancelled {
		t.Fatalf("outbox = %+v, want one campaign.cancelled", events)
	}
}

func TestRetryCampaignRequeuesAndResumes(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{}, WithBatches(Batches{RetryChunk: 2}))
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignCompleted, func(c *store.Campaign) {
		c.CompletedAt = baseTime.Add(-time.Hour)
	})
	seedDeliveries(t, st, cam.ID, store.DeliveryFailed, 5, func(d *store.Delivery) {
		d.LastErrorClass = store.ErrorClassTransient
	})
	seedDeliveries(t, st, cam.ID, store.DeliverySent, 2, func(d *store.Delivery) {
		d.Email, d.EmailNorm = "ok"+d.Email, "ok"+d.EmailNorm
	})

	n, err := c.RetryCampaign(ctx, st, cam.ID, store.RetryFilter{
		Statuses:     []store.DeliveryStatus{store.DeliveryFailed},
		ErrorClasses: []store.ErrorClass{store.ErrorClassTransient},
	})
	if err != nil {
		t.Fatalf("RetryCampaign: %v", err)
	}
	if n != 5 {
		t.Fatalf("requeued %d, want 5 across chunks of 2", n)
	}

	counts, _ := st.Deliveries().CountByStatus(ctx, cam.ID)
	if counts[store.DeliveryQueued] != 5 || counts[store.DeliverySent] != 2 {
		t.Fatalf("counts = %v, want 5 queued and 2 sent", counts)
	}
	got := getCampaign(t, st, cam.ID)
	if got.Status != store.CampaignRunning {
		t.Fatalf("status = %s, want running so the sender claims the rows again", got.Status)
	}
	if !got.CompletedAt.IsZero() {
		t.Errorf("CompletedAt = %v, want cleared", got.CompletedAt)
	}
	events := listOutbox(t, st, store.OutboxPending)
	if len(events) != 1 || events[0].Type != EventCampaignStarted {
		t.Fatalf("outbox = %+v, want one campaign.started", events)
	}
}

func TestRetryCampaignEmptyIDListMatchesNothing(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{})
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignCompleted)
	seedDeliveries(t, st, cam.ID, store.DeliveryFailed, 3)

	n, err := c.RetryCampaign(ctx, st, cam.ID, store.RetryFilter{DeliveryIDs: []string{}})
	if err != nil {
		t.Fatalf("RetryCampaign: %v", err)
	}
	if n != 0 {
		t.Fatalf("requeued %d, want 0 for an explicitly empty selection", n)
	}
	if got := getCampaign(t, st, cam.ID); got.Status != store.CampaignCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
}

func TestRetryCampaignDefaultsToFailed(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{})
	ctx := context.Background()

	cam := seedCampaign(t, st, store.CampaignRunning)
	seedDeliveries(t, st, cam.ID, store.DeliveryFailed, 2)
	seedDeliveries(t, st, cam.ID, store.DeliverySent, 2, func(d *store.Delivery) {
		d.Email, d.EmailNorm = "ok"+d.Email, "ok"+d.EmailNorm
	})

	n, err := c.RetryCampaign(ctx, st, cam.ID, store.RetryFilter{})
	if err != nil {
		t.Fatalf("RetryCampaign: %v", err)
	}
	if n != 2 {
		t.Fatalf("requeued %d, want only the 2 failed rows", n)
	}
	if counts, _ := st.Deliveries().CountByStatus(ctx, cam.ID); counts[store.DeliverySent] != 2 {
		t.Errorf("a default retry touched sent rows: %v", counts)
	}
}

func TestPauseResumeKeepsDeliveriesUntouched(t *testing.T) {
	_, st, _, c := newFixture(t, sendplane.Hooks{})
	ctx := context.Background()

	cam := readyCampaign(t, st, store.CampaignRunning)
	seedDeliveries(t, st, cam.ID, store.DeliveryQueued, 3, func(d *store.Delivery) {
		d.Email, d.EmailNorm = "q"+d.Email, "q"+d.EmailNorm
	})
	before, _ := st.Deliveries().CountByStatus(ctx, cam.ID)

	if err := c.PauseCampaign(ctx, st, cam.ID); err != nil {
		t.Fatalf("PauseCampaign: %v", err)
	}
	if err := c.ResumeCampaign(ctx, st, cam.ID); err != nil {
		t.Fatalf("ResumeCampaign: %v", err)
	}

	after, _ := st.Deliveries().CountByStatus(ctx, cam.ID)
	if len(before) != len(after) || before[store.DeliveryQueued] != after[store.DeliveryQueued] {
		t.Fatalf("pause/resume rewrote delivery rows: %v -> %v", before, after)
	}
	types := []string{}
	for _, e := range listOutbox(t, st, store.OutboxPending) {
		types = append(types, e.Type)
	}
	if len(types) != 2 || types[0] != EventCampaignPaused || types[1] != EventCampaignStarted {
		t.Fatalf("events = %v, want [campaign.paused campaign.started]", types)
	}
}
