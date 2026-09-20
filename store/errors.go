package store

import "errors"

var (
	// ErrNotFound is returned when an object does not exist, or exists in a
	// different tenant.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when an optimistic-concurrency Version does not
	// match, or a unique key is violated.
	ErrConflict = errors.New("store: conflict")
	// ErrInvalid is returned for malformed input (bad email, bad enum value,
	// missing required field).
	ErrInvalid = errors.New("store: invalid")
	// ErrLeaseLost is returned when a delivery is no longer leased by the
	// worker that is trying to finish it.
	ErrLeaseLost = errors.New("store: lease lost")
)
