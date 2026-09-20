package mongo

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/sendplane/sendplane/store"
)

// --- times -------------------------------------------------------------
//
// Times are BSON dates, which is what makes them comparable, indexable and
// readable from the shell. A BSON date holds milliseconds, which is exactly
// the contract's resolution (store/doc.go), so every instant is truncated on
// the way in and nothing is silently rounded.

// encTime encodes a nullable timestamp: the zero time (the contract's NULL)
// becomes BSON null, everything else a BSON date truncated to milliseconds.
func encTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := store.TruncateTime(t)
	return &u
}

// decTime is the inverse of encTime.
func decTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// ts is encTime for fields that are never NULL (created_at, updated_at) and
// for the right-hand side of range filters, where the bound has to be
// truncated the same way the stored value was.
func ts(t time.Time) time.Time { return store.TruncateTime(t) }

// --- integer narrowing -------------------------------------------------

// i32 narrows an int to the int32 a BSON document stores. Everything that
// reaches it is a port, a count or a byte size the API already bounds far
// below int32, so the clamp is a guard rail, not a behaviour: a value that hit
// it would be a bug upstream, and wrapping silently would be worse than
// storing the bound.
func i32(v int) int32 {
	switch {
	case v > math.MaxInt32:
		return math.MaxInt32
	case v < math.MinInt32:
		return math.MinInt32
	}
	return int32(v)
}

// enum8 narrows a stored int32 back to the int8 every sendplane enum is
// (store/enums.go). A document written by another version could hold any
// number; one outside int8 becomes 0, which each enum spells as its zero value
// and String() renders as "unknown", rather than wrapping into a neighbouring
// state.
func enum8(v int32) int8 {
	if v < math.MinInt8 || v > math.MaxInt8 {
		return 0
	}
	return int8(v)
}

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
		out[i] = store.Lane(enum8(v))
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
	createdAt time.Time
	id        string
}

// encodeCursor renders a cursor as Unix milliseconds plus the _id. The unit is
// the stored resolution, so decoding it back gives exactly the created_at of
// the row the cursor came from.
func encodeCursor(c cursor) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(c.createdAt.UnixMilli(), 10) + "|" + c.id))
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
	return cursor{createdAt: time.UnixMilli(n).UTC(), id: id}, nil
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
