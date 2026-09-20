package api

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

// seedDeliveries writes deliveries straight into the tenant store. The listing
// is what is under test here, not ingest, and going through the store keeps
// the fixture explicit about lane, status and created_at.
func (e *env) seedDeliveries(ds ...store.Delivery) {
	e.t.Helper()
	n, err := e.st.Deliveries().InsertBatch(e.t.Context(), ds)
	if err != nil {
		e.t.Fatalf("InsertBatch: %v", err)
	}
	if n != len(ds) {
		e.t.Fatalf("InsertBatch inserted %d of %d", n, len(ds))
	}
}

func listDeliveries(t *testing.T, e *env, query string) DeliveryList {
	t.Helper()
	return decodeInto[DeliveryList](t, e.do(http.MethodGet, "/api/v1/deliveries?"+query, nil),
		http.StatusOK)
}

// TestListDeliveries covers the tenant-wide route: the one an operator reaches
// for with an address and no campaign in hand.
func TestListDeliveries(t *testing.T) {
	e := newEnv(t)
	campaignA, campaignB := store.NewID(), store.NewID()
	old := e.now.Add(-48 * time.Hour)

	mk := func(campaign, email string, lane store.Lane, st store.DeliveryStatus,
		cl store.ErrorClass, created time.Time,
	) store.Delivery {
		return store.Delivery{
			ID: store.NewID(), CampaignID: campaign, Lane: lane, Status: st,
			LastErrorClass: cl, Email: email, EmailNorm: email,
			NextAttemptAt: e.now, CreatedAt: created,
		}
	}
	e.seedDeliveries(
		mk(campaignA, "a@example.com", store.LaneBulk, store.DeliverySent, store.ErrorClassNone, old),
		mk(campaignA, "shared@example.com", store.LaneBulk, store.DeliveryFailed, store.ErrorClassPermanent, old),
		mk(campaignB, "shared@example.com", store.LaneBulk, store.DeliverySent, store.ErrorClassNone, e.now),
		mk("", "shared@example.com", store.LaneTransactional, store.DeliverySent, store.ErrorClassNone, e.now),
		mk("", "probe@example.com", store.LaneProbe, store.DeliverySent, store.ErrorClassNone, e.now),
	)

	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"everything", "limit=100", 5},
		{"campaign", "campaign_id=" + campaignA, 2},
		// The lane is how a caller asks for the deliveries that have no
		// campaign at all, which is what the spec says instead of overloading
		// an empty campaign_id.
		{"transactional lane", "lane=transactional", 1},
		{"two lanes", "lane=transactional&lane=probe", 2},
		{"address across campaigns", "email=" + url.QueryEscape("shared@example.com"), 3},
		{"address and campaign", "email=" + url.QueryEscape("shared@example.com") +
			"&campaign_id=" + campaignB, 1},
		{"status", "status=failed", 1},
		{"two statuses", "status=failed&status=sent", 5},
		{"error class", "error_class=permanent", 1},
		{"since", "since=" + url.QueryEscape(e.now.Add(-time.Hour).Format(time.RFC3339)), 3},
		{"until", "until=" + url.QueryEscape(e.now.Add(-time.Hour).Format(time.RFC3339)), 2},
		{"unknown campaign is an empty page, not a 404", "campaign_id=" + store.NewID(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := listDeliveries(t, e, tc.query)
			if len(got.Items) != tc.want {
				t.Fatalf("%s: got %d deliveries, want %d", tc.query, len(got.Items), tc.want)
			}
		})
	}

	// The filter address is normalized like a recipient's, so the casing a
	// human types finds the stored row.
	if got := listDeliveries(t, e, "email="+url.QueryEscape("Shared@Example.COM")); len(got.Items) != 3 {
		t.Fatalf("mixed-case address matched %d deliveries, want 3", len(got.Items))
	}

	// The cursor walks every row exactly once, as everywhere else.
	seen := map[string]bool{}
	query := "limit=2"
	for {
		page := listDeliveries(t, e, query)
		for _, d := range page.Items {
			if seen[d.Id.String()] {
				t.Fatalf("delivery %s listed twice", d.Id)
			}
			seen[d.Id.String()] = true
		}
		if page.NextCursor == nil {
			break
		}
		query = "limit=2&cursor=" + url.QueryEscape(*page.NextCursor)
	}
	if len(seen) != 5 {
		t.Fatalf("paging saw %d deliveries, want 5", len(seen))
	}
}

func TestListDeliveriesRejectsBadFilters(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"unknown lane", "lane=carrier-pigeon"},
		{"unknown status", "status=posted"},
		{"unknown error class", "error_class=gremlins"},
		{"until before since", "since=2025-03-01T12:00:00Z&until=2025-02-01T12:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := e.do(http.MethodGet, "/api/v1/deliveries?"+tc.query, nil)
			if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 400 or 422; body: %s", w.Code, w.Body.String())
			}
		})
	}
}
