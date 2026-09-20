package store

import "time"

// TruncateTime reduces t to the resolution every implementation stores: UTC,
// truncated to whole milliseconds. The zero time stays zero, so "no value"
// keeps meaning SQL NULL.
//
// Millisecond is the greatest common resolution of the supported backends:
// PostgreSQL timestamptz keeps microseconds, a BSON datetime keeps
// milliseconds. Rather than let each implementation round differently,
// the contract picks the coarser one and every implementation applies this
// function on write (store/doc.go). Callers that compare a timestamp they
// handed in with the one they read back must compare through it.
func TruncateTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Millisecond)
}
