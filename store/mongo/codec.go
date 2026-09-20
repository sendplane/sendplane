package mongo

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/sendplane/sendplane/store"
)

// --- times -------------------------------------------------------------

// encTime stores a time.Time as nanoseconds since the Unix epoch, or null for
// the zero time (the contract's NULL). BSON dates are milliseconds, which
// would round timestamps the caller handed in, so they are not used.
func encTime(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	n := t.UTC().UnixNano()
	return &n
}

// decTime is the inverse of encTime.
func decTime(n *int64) time.Time {
	if n == nil {
		return time.Time{}
	}
	return time.Unix(0, *n).UTC()
}

// ns is encTime for fields that are never NULL (created_at, updated_at) and
// for the right-hand side of range filters.
func ns(t time.Time) int64 { return t.UTC().UnixNano() }

// --- enums -------------------------------------------------------------

// openStatuses are the delivery states that still have work scheduled.
var openStatuses = []store.DeliveryStatus{
	store.DeliveryPending, store.DeliveryQueued,
	store.DeliveryLeased, store.DeliveryDeferred,
}

// claimable are the states Claim moves to leased.
var claimable = []store.DeliveryStatus{store.DeliveryQueued, store.DeliveryDeferred}

func statusInts(ss []store.DeliveryStatus) []int32 {
	out := make([]int32, len(ss))
	for i, s := range ss {
		out[i] = int32(s)
	}
	return out
}

func classInts(cs []store.ErrorClass) []int32 {
	out := make([]int32, len(cs))
	for i, c := range cs {
		out[i] = int32(c)
	}
	return out
}

func campaignStatusInts(ss []store.CampaignStatus) []int32 {
	out := make([]int32, len(ss))
	for i, s := range ss {
		out[i] = int32(s)
	}
	return out
}

func laneInts(ls []store.Lane) []int32 {
	out := make([]int32, len(ls))
	for i, l := range ls {
		out[i] = int32(l)
	}
	return out
}

func lanesFrom(vs []int32) []store.Lane {
	if vs == nil {
		return nil
	}
	out := make([]store.Lane, len(vs))
	for i, v := range vs {
		out[i] = store.Lane(v)
	}
	return out
}

// encStatusMap renders a map keyed by an enum as a BSON document: BSON field
// names are strings, and the enum's text form is the one the JSON API uses.
func encStatusMap(m map[store.DeliveryStatus]int64) map[string]int64 {
	if m == nil {
		return nil
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k.String()] = v
	}
	return out
}

func decStatusMap(m map[string]int64) map[store.DeliveryStatus]int64 {
	if m == nil {
		return nil
	}
	out := make(map[store.DeliveryStatus]int64, len(m))
	for k, v := range m {
		var s store.DeliveryStatus
		if err := s.UnmarshalText([]byte(k)); err != nil {
			continue // a status this build does not know: drop it
		}
		out[s] = v
	}
	return out
}

// --- errors ------------------------------------------------------------

func notFound(kind, id string) error {
	return fmt.Errorf("%w: %s %s", store.ErrNotFound, kind, id)
}

func conflict(kind, id string) error {
	return fmt.Errorf("%w: %s %s", store.ErrConflict, kind, id)
}

// wrap turns a driver error into a store error where the mapping is
// unambiguous and otherwise keeps it as an operational failure.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("%w: %s", store.ErrNotFound, op)
	}
	if mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("%w: %s", store.ErrConflict, op)
	}
	return fmt.Errorf("mongo: %s: %w", op, err)
}

// duplicateKeyCode is the server error for a unique-index violation.
const duplicateKeyCode = 11000

// countDuplicates splits an InsertMany error into the duplicate-key writes,
// which idempotent ingest ignores, and anything else, which propagates.
func countDuplicates(err error) (dups int, rest error) {
	var bwe mongo.BulkWriteException
	if !errors.As(err, &bwe) {
		return 0, err
	}
	for _, we := range bwe.WriteErrors {
		if we.Code != duplicateKeyCode {
			return 0, err
		}
		dups++
	}
	if bwe.WriteConcernError != nil {
		return 0, err
	}
	return dups, nil
}

// --- cursors -----------------------------------------------------------

// Listings are keyset-paginated on (created_at, _id): a row inserted after a
// page was read sorts after the cursor and is never repeated or skipped.
type cursor struct {
	createdAt int64
	id        string
}

func encodeCursor(c cursor) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(c.createdAt, 10) + "|" + c.id))
}

func decodeCursor(s string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: cursor %q", store.ErrInvalid, s)
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return cursor{}, fmt.Errorf("%w: cursor %q", store.ErrInvalid, s)
	}
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: cursor %q", store.ErrInvalid, s)
	}
	return cursor{createdAt: n, id: id}, nil
}

// afterCursor is the keyset predicate for everything that sorts after c.
func afterCursor(c cursor) bson.D {
	return bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "created_at", Value: bson.D{{Key: "$gt", Value: c.createdAt}}}},
		bson.D{
			{Key: "created_at", Value: c.createdAt},
			{Key: "_id", Value: bson.D{{Key: "$gt", Value: c.id}}},
		},
	}}}
}

// listSort is the order every cursor-paginated listing uses.
var listSort = bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}

// and combines filters without assuming any of them is free of $or.
func and(parts ...bson.D) bson.D {
	all := make(bson.A, 0, len(parts))
	for _, p := range parts {
		if len(p) > 0 {
			all = append(all, p)
		}
	}
	if len(all) == 0 {
		return bson.D{}
	}
	if len(all) == 1 {
		return all[0].(bson.D)
	}
	return bson.D{{Key: "$and", Value: all}}
}
