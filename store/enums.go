package store

import (
	"fmt"
	"strconv"
)

// The enums below are stored as small ints (the numeric values are part of the
// on-disk format, see docs/architecture.md 5.2) and marshalled as strings.
// They implement encoding.TextMarshaler so that they also render as strings
// when used as JSON map keys (CountByStatus).

func enumString[T ~int8](v T, names []string) string {
	if i := int(v); i >= 0 && i < len(names) {
		return names[i]
	}
	return "invalid(" + strconv.Itoa(int(v)) + ")"
}

func enumText[T ~int8](v T, names []string) ([]byte, error) {
	i := int(v)
	if i < 0 || i >= len(names) {
		return nil, fmt.Errorf("%w: enum value %d out of range", ErrInvalid, i)
	}
	return []byte(names[i]), nil
}

func enumParse[T ~int8](b []byte, names []string, out *T) error {
	s := string(b)
	for i, n := range names {
		if n == s {
			*out = T(i)
			return nil
		}
	}
	return fmt.Errorf("%w: unknown enum value %q", ErrInvalid, s)
}

// DeliveryStatus is the delivery state machine (architecture 4.1).
type DeliveryStatus int8

const (
	DeliveryPending DeliveryStatus = iota
	DeliveryQueued
	DeliveryLeased
	DeliveryDeferred
	DeliverySent
	DeliveryFailed
	DeliveryBounced
	DeliveryComplained
	DeliverySuppressed
	DeliveryCancelled
)

var deliveryStatusNames = []string{
	"pending", "queued", "leased", "deferred", "sent",
	"failed", "bounced", "complained", "suppressed", "cancelled",
}

func (v DeliveryStatus) String() string               { return enumString(v, deliveryStatusNames) }
func (v DeliveryStatus) MarshalText() ([]byte, error) { return enumText(v, deliveryStatusNames) }
func (v *DeliveryStatus) UnmarshalText(b []byte) error {
	return enumParse(b, deliveryStatusNames, v)
}

// Terminal reports whether no further work is scheduled for the delivery.
func (v DeliveryStatus) Terminal() bool {
	switch v {
	case DeliverySent, DeliveryFailed, DeliveryBounced, DeliveryComplained,
		DeliverySuppressed, DeliveryCancelled:
		return true
	}
	return false
}

// Lane separates scheduling classes. Transactional is always filled first and
// probe traffic is excluded from campaign statistics, tracking and
// suppression (ADR-0012).
type Lane int8

const (
	LaneBulk Lane = iota
	LaneTransactional
	LaneProbe
)

var laneNames = []string{"bulk", "transactional", "probe"}

func (v Lane) String() string                { return enumString(v, laneNames) }
func (v Lane) MarshalText() ([]byte, error)  { return enumText(v, laneNames) }
func (v *Lane) UnmarshalText(b []byte) error { return enumParse(b, laneNames, v) }

// ErrorClass is the normalized SMTP/transport error classification
// (architecture 4.2). ErrorClassNone means no error was recorded.
type ErrorClass int8

const (
	ErrorClassNone ErrorClass = iota
	ErrorClassTransient
	ErrorClassRateLimited
	ErrorClassPermanent
	ErrorClassPolicy
	ErrorClassAuth
)

var errorClassNames = []string{"none", "transient", "rate_limited", "permanent", "policy", "auth"}

func (v ErrorClass) String() string                { return enumString(v, errorClassNames) }
func (v ErrorClass) MarshalText() ([]byte, error)  { return enumText(v, errorClassNames) }
func (v *ErrorClass) UnmarshalText(b []byte) error { return enumParse(b, errorClassNames, v) }

// CampaignStatus is the campaign lifecycle.
type CampaignStatus int8

const (
	CampaignDraft CampaignStatus = iota
	CampaignScheduled
	CampaignRunning
	CampaignPaused
	CampaignCompleted
	CampaignCancelled
)

var campaignStatusNames = []string{
	"draft", "scheduled", "running", "paused", "completed", "cancelled",
}

func (v CampaignStatus) String() string               { return enumString(v, campaignStatusNames) }
func (v CampaignStatus) MarshalText() ([]byte, error) { return enumText(v, campaignStatusNames) }
func (v *CampaignStatus) UnmarshalText(b []byte) error {
	return enumParse(b, campaignStatusNames, v)
}

// TransportStatus is the SMTP transport circuit state (architecture 8.3).
// Cooldown is a temporary slowdown after rate_limited responses; unhealthy is
// set after repeated auth/TLS/connection failures and cleared by a probe.
type TransportStatus int8

const (
	TransportHealthy TransportStatus = iota
	TransportCooldown
	TransportUnhealthy
)

var transportStatusNames = []string{"healthy", "cooldown", "unhealthy"}

func (v TransportStatus) String() string               { return enumString(v, transportStatusNames) }
func (v TransportStatus) MarshalText() ([]byte, error) { return enumText(v, transportStatusNames) }
func (v *TransportStatus) UnmarshalText(b []byte) error {
	return enumParse(b, transportStatusNames, v)
}

// BounceType classifies a parsed bounce or feedback report.
type BounceType int8

const (
	BounceUnknown BounceType = iota
	BounceHard
	BounceSoft
	BounceComplaint
)

var bounceTypeNames = []string{"unknown", "hard", "soft", "complaint"}

func (v BounceType) String() string                { return enumString(v, bounceTypeNames) }
func (v BounceType) MarshalText() ([]byte, error)  { return enumText(v, bounceTypeNames) }
func (v *BounceType) UnmarshalText(b []byte) error { return enumParse(b, bounceTypeNames, v) }

// TrackingKind is the recorded tracking interaction. UnsubscribeClicked is the
// GET redirect (a scanner may have caused it); Unsubscribed is confirmed
// (one-click POST or host notification) and is the one ratios use (ADR-0011).
type TrackingKind int8

const (
	TrackingOpen TrackingKind = iota
	TrackingClick
	TrackingUnsubscribeClicked
	TrackingUnsubscribed
)

var trackingKindNames = []string{"open", "click", "unsubscribe_clicked", "unsubscribed"}

func (v TrackingKind) String() string               { return enumString(v, trackingKindNames) }
func (v TrackingKind) MarshalText() ([]byte, error) { return enumText(v, trackingKindNames) }
func (v *TrackingKind) UnmarshalText(b []byte) error {
	return enumParse(b, trackingKindNames, v)
}

// MailboxStatus is the reachability of a probe or bounce mailbox account
// (MailboxHealth). It is a separate enum from HealthStatus on purpose: a
// mailbox is reachable or it is not, there is no "yellow" version of a
// rejected login, and mixing the two would make a broken credential look like
// a deliverability verdict (ADR-0015).
type MailboxStatus int8

const (
	// MailboxUnknown is a mailbox nothing has checked yet.
	MailboxUnknown MailboxStatus = iota
	MailboxOK
	MailboxError
)

var mailboxStatusNames = []string{"unknown", "ok", "error"}

func (v MailboxStatus) String() string               { return enumString(v, mailboxStatusNames) }
func (v MailboxStatus) MarshalText() ([]byte, error) { return enumText(v, mailboxStatusNames) }
func (v *MailboxStatus) UnmarshalText(b []byte) error {
	return enumParse(b, mailboxStatusNames, v)
}

// HealthStatus summarizes a sending domain or sender health check
// (architecture 11.4).
type HealthStatus int8

const (
	HealthUnknown HealthStatus = iota
	HealthGreen
	HealthYellow
	HealthRed
)

var healthStatusNames = []string{"unknown", "green", "yellow", "red"}

func (v HealthStatus) String() string               { return enumString(v, healthStatusNames) }
func (v HealthStatus) MarshalText() ([]byte, error) { return enumText(v, healthStatusNames) }
func (v *HealthStatus) UnmarshalText(b []byte) error {
	return enumParse(b, healthStatusNames, v)
}
