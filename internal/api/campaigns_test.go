package api

import (
	"net/http"
	"testing"

	"github.com/sendplane/sendplane/store"
)

// TestCampaignStatsDenominators pins the two totals the spec now states
// explicitly. They are derived rather than stored, so the risk they guard
// against is a client and the server disagreeing about which statuses add up
// to "sent" — in particular that a bounce arriving days later must not shrink
// the denominator of a rate that was already published.
func TestCampaignStatsDenominators(t *testing.T) {
	e := newEnv(t)
	sender := e.seedSender()
	tpl := e.seedTemplate()
	c := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "spring", SenderId: *sender.Id, TemplateId: tpl.Id,
	}), http.StatusCreated)

	err := e.st.Campaigns().UpdateStats(t.Context(), c.Id.String(), store.CampaignStats{
		ByStatus: map[store.DeliveryStatus]int64{
			store.DeliveryPending:    1,
			store.DeliveryQueued:     2,
			store.DeliverySent:       10,
			store.DeliveryBounced:    3,
			store.DeliveryComplained: 1,
			store.DeliveryFailed:     4,
			store.DeliveryCancelled:  5,
		},
		UniqueOpens: 7,
	})
	if err != nil {
		t.Fatalf("UpdateStats: %v", err)
	}

	got := decodeInto[Campaign](t,
		e.do(http.MethodGet, "/api/v1/campaigns/"+c.Id.String(), nil), http.StatusOK)
	if got.Stats == nil || got.Stats.Total == nil || got.Stats.Sent == nil {
		t.Fatalf("stats = %+v, want total and sent", got.Stats)
	}
	if *got.Stats.Total != 26 {
		t.Fatalf("total = %d, want 26 (every status)", *got.Stats.Total)
	}
	// sent + bounced + complained: the MTA accepted all 14.
	if *got.Stats.Sent != 14 {
		t.Fatalf("sent = %d, want 14 (sent + bounced + complained)", *got.Stats.Sent)
	}
	if got.Stats.ByStatus == nil || (*got.Stats.ByStatus)["bounced"] != 3 {
		t.Fatalf("by_status = %+v, want it kept alongside the totals", got.Stats.ByStatus)
	}
}
