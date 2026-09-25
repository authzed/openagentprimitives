package memory

import (
	"encoding/json"
	"time"
)

// SignalKind is a free-form, namespaced signal identifier owned by some Kind's
// package (convention: "<owning-kind>/<event>"). Deliberately not an enum, so a
// Kind can introduce a signal without a framework change.
type SignalKind string

// Signal is a lifecycle event fanned out to EVERY registered Kind's ScopeHooks
// for the scope — a hook that does not recognize the Kind ignores it.
type Signal struct {
	// Kind selects which hooks react and what Payload's shape is.
	Kind SignalKind `json:"kind"`
	// Scope selects which (Scope, Kind) hooks receive the signal.
	Scope Scope `json:"scope"`
	// At is the event time as the sender observed it, not receipt time.
	At time.Time `json:"at"`
	// Payload is opaque to the framework; the Kind owning this SignalKind
	// defines its shape. Empty when the signal carries no data.
	Payload json.RawMessage `json:"payload,omitempty"`
}
