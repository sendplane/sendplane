package store

import "github.com/google/uuid"

// NewID returns a UUIDv7 string: time-ordered, so it doubles as a stable
// pagination cursor, and portable across Postgres and MongoDB.
func NewID() string {
	return uuid.Must(uuid.NewV7()).String()
}
