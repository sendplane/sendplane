package store_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/sendplane/sendplane/store"
)

func TestEnumStrings(t *testing.T) {
	cases := []struct {
		v    interface{ String() string }
		want string
	}{
		{store.DeliveryPending, "pending"},
		{store.DeliveryCancelled, "cancelled"},
		{store.LaneTransactional, "transactional"},
		{store.LaneProbe, "probe"},
		{store.ErrorClassRateLimited, "rate_limited"},
		{store.CampaignScheduled, "scheduled"},
		{store.TransportUnhealthy, "unhealthy"},
		{store.BounceComplaint, "complaint"},
		{store.TrackingUnsubscribeClicked, "unsubscribe_clicked"},
		{store.HealthYellow, "yellow"},
		{store.SecretNone, "none"},
		{store.SecretPending, "pending"},
		{store.SecretConsumed, "consumed"},
		{store.SecretDiscarded, "discarded"},
		{store.SecretExpired, "expired"},
	}
	for _, tc := range cases {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestEnumJSON(t *testing.T) {
	type payload struct {
		Status store.DeliveryStatus
		Lane   store.Lane
		Class  store.ErrorClass
	}
	in := payload{store.DeliveryDeferred, store.LaneBulk, store.ErrorClassPolicy}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"Status":"deferred","Lane":"bulk","Class":"policy"}`
	if string(b) != want {
		t.Fatalf("Marshal = %s, want %s", b, want)
	}
	var out payload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}

	// Status maps (CountByStatus) render their keys as strings too.
	b, err = json.Marshal(map[store.DeliveryStatus]int64{store.DeliverySent: 2})
	if err != nil {
		t.Fatalf("Marshal map: %v", err)
	}
	if string(b) != `{"sent":2}` {
		t.Fatalf("Marshal map = %s, want {\"sent\":2}", b)
	}

	var bad store.DeliveryStatus
	if err := json.Unmarshal([]byte(`"nope"`), &bad); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Unmarshal unknown = %v, want ErrInvalid", err)
	}
	if _, err := json.Marshal(store.DeliveryStatus(99)); err == nil {
		t.Fatal("Marshal out-of-range value: want an error")
	}
}

func TestDeliveryStatusTerminal(t *testing.T) {
	for _, s := range []store.DeliveryStatus{
		store.DeliveryPending, store.DeliveryQueued, store.DeliveryLeased, store.DeliveryDeferred,
	} {
		if s.Terminal() {
			t.Errorf("%s must not be terminal", s)
		}
	}
	for _, s := range []store.DeliveryStatus{
		store.DeliverySent, store.DeliveryFailed, store.DeliveryBounced,
		store.DeliveryComplained, store.DeliverySuppressed, store.DeliveryCancelled,
	} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
}

// The numeric values are the on-disk format (delivery.secret_state), so they
// are pinned here alongside the wire names.
func TestSecretStateValues(t *testing.T) {
	for v, name := range map[store.SecretState]string{
		0: "none", 1: "pending", 2: "consumed", 3: "discarded", 4: "expired",
	} {
		b, err := v.MarshalText()
		if err != nil || string(b) != name {
			t.Errorf("MarshalText(%d) = %q, %v; want %q", v, b, err, name)
		}
		var back store.SecretState
		if err := back.UnmarshalText([]byte(name)); err != nil || back != v {
			t.Errorf("UnmarshalText(%q) = %d, %v; want %d", name, back, err, v)
		}
	}
	var bad store.SecretState
	if err := bad.UnmarshalText([]byte("used")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("UnmarshalText unknown = %v, want ErrInvalid", err)
	}
	if (&store.Delivery{}).Sensitive() {
		t.Error("a delivery without a secret state is sensitive")
	}
	for _, s := range []store.SecretState{
		store.SecretPending, store.SecretConsumed, store.SecretDiscarded, store.SecretExpired,
	} {
		if !(&store.Delivery{SecretState: s}).Sensitive() {
			t.Errorf("a delivery in secret state %s is not sensitive", s)
		}
	}
}
