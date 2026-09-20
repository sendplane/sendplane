package store

// DefaultPageLimit is used when Page.Limit is zero.
const DefaultPageLimit = 50

// MaxPageLimit caps Page.Limit.
const MaxPageLimit = 1000

// Page is a cursor pagination request. Cursor is empty for the first page and
// is otherwise a NextCursor returned by a previous call on the same list.
type Page struct {
	Limit  int
	Cursor string
}

// Normalize clamps Limit into [1, MaxPageLimit]. Implementations call it so
// that every backend agrees on page sizes.
func (p Page) Normalize() Page {
	if p.Limit <= 0 {
		p.Limit = DefaultPageLimit
	}
	if p.Limit > MaxPageLimit {
		p.Limit = MaxPageLimit
	}
	return p
}

// Result is one page of a list. NextCursor is empty on the last page.
type Result[T any] struct {
	Items      []T
	NextCursor string
}
