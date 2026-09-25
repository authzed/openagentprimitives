// Package revocation is the pluggable in-flight revocation mechanism. The
// operator publishes a KindRevoked envelope; each runner runs one subscriber
// that scope-filters and dispatches to the Invalidator registered for the
// envelope's Kind. Adding a revocable kind = register one Invalidator + add a
// publisher trigger in that CR's controller; no consumer edits.
package revocation

import "fmt"

// Invalidator invalidates the live use of one revocable kind. Implementations
// are concurrency-safe: Invalidate may be called from the subscriber goroutine
// at any time.
type Invalidator interface {
	// Kind is the registry key (matches RevokedPayload.Kind).
	Kind() string
	// Noun names what this kind withdraws, as an indefinite noun phrase a
	// person would recognise: "a connected account", "a set of tools". Kind()
	// cannot serve — "tool-origin" is a wire token, not something a reader in
	// a chat thread has ever seen.
	//
	// It exists so a consumer that has to TELL somebody a withdrawal did not
	// take effect can name what it was without switching on Kind(), which
	// outside this package would be the `if kind == "x"` the repo forbids: the
	// noun is a property of the kind, so it belongs on the kind.
	//
	// Deliberately no session, key, or count in it: the phrase is substituted
	// into publisher-authored copy that a channel surface renders as TRUSTED
	// markup, so it must be a fixed string owned by this repo and never
	// anything derived from a revoke key off the bus. Consumers must stay
	// grammatical when it is empty — a future Invalidator that has nothing
	// useful to say may return "".
	Noun() string
	// Invalidate drops/denies the live capability identified by key. Must be a
	// safe no-op when key is not present (idempotent, at-most-once delivery).
	Invalidate(key string) error
}

// Registry maps a revocable-kind name to its Invalidator.
type Registry struct{ m map[string]Invalidator }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Invalidator{}} }

// Register adds inv. Errors on empty or duplicate Kind().
func (r *Registry) Register(inv Invalidator) error {
	k := inv.Kind()
	if k == "" {
		return fmt.Errorf("revocation.Register: empty Kind()")
	}
	if _, dup := r.m[k]; dup {
		return fmt.Errorf("revocation.Register: duplicate kind %q", k)
	}
	r.m[k] = inv
	return nil
}

// Lookup returns the Invalidator for kind.
func (r *Registry) Lookup(kind string) (Invalidator, bool) { inv, ok := r.m[kind]; return inv, ok }
