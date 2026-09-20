// Package host holds everything the host application provides to sendplane or
// receives back from it: authentication and authorization, tenant resolution,
// the Go hooks, the event sink, secret encryption, request limits and the
// metrics sink.
//
// It exists to break an import cycle. The root sendplane package implements
// New, Handler, RunControl and RunSender, so it has to import internal/api,
// internal/control and internal/sender; those packages in turn need the types
// the host injects. Keeping those types in a leaf package that imports nothing
// but store lets both sides depend on the same declarations instead of on each
// other (ADR-0001, docs/architecture.md 2.1, 3).
//
// Host code does not normally import this package directly: the root package
// re-exports every identifier here as a type alias, so sendplane.Hooks and
// host.Hooks are the same type. The alias is what keeps the documented API of
// architecture 3 intact while internal packages depend only on this one.
//
// Nothing here imports internal/, and nothing here has behaviour beyond
// defaulting: it is a vocabulary, not a layer.
package host
