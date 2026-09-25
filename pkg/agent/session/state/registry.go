// Package state is the per-session in-memory state framework. Kinds register at
// init() time; the runner constructs one Registry per AgentSession that
// materializes one Store per registered Kind.
//
// Memory replay uses a self-describing wrapper for system_note turns:
//
//	{"kind": "<name>", "v": <int>, "data": <kind-defined>}
//
// On resume, DispatchSystemNote routes each wrapped note to the matching Kind's
// Store.ReplayNote. Unwrapped notes (respond_to_user's "delivered" set) fall
// through to the caller's own system_note processing.
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// Kind defines one variety of session-scoped state.
type Kind interface {
	// Name is the system_note discriminator. Stable; bump v in the
	// wrapper when the data shape evolves.
	Name() string
	// NewStore is called once per session to materialize this Kind's
	// per-session Store.
	NewStore(deps Deps) tool.StateStore
}

// Deps is what the runner passes through NewRegistry to each Kind's
// NewStore. Kinds use only the fields they need; nil values are
// permitted for unused dependencies.
type Deps struct {
	// Operations is the per-session audit registry; plans uses it to
	// auto-Begin/Close operations on item transitions.
	Operations tool.OperationRegistry
	// AppendSystemNote writes a Store-marshaled {kind, v, data} envelope as a
	// system_note turn. Nil skips persistence entirely — kubectl-driven sessions
	// and test fixtures run without it.
	AppendSystemNote func(ctx context.Context, content map[string]any) error
}

// Registry is the per-session typed-store registry.
type Registry struct {
	stores map[string]tool.StateStore
}

// Get returns the registered Store for kindName.
func (r *Registry) Get(kindName string) (tool.StateStore, bool) {
	s, ok := r.stores[kindName]
	return s, ok
}

// Compile-time check that *Registry satisfies tool.StateRegistry.
var _ tool.StateRegistry = (*Registry)(nil)

var (
	regMu sync.RWMutex
	kinds = map[string]Kind{}
)

// Register adds k to the global Kind registry. Panics on duplicate
// names so build/init ordering bugs are loud rather than silent.
func Register(k Kind) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := kinds[k.Name()]; exists {
		panic(fmt.Sprintf("state.Register: duplicate kind name %q", k.Name()))
	}
	kinds[k.Name()] = k
}

// NewRegistry materializes one Store per registered Kind and returns
// the per-session Registry. Kinds register at init() time; each Kind's
// NewStore MUST NOT call Register, since NewRegistry holds the
// registry's read lock while invoking NewStore — a re-entrant Register
// would deadlock against the read-held RWMutex.
func NewRegistry(deps Deps) *Registry {
	regMu.RLock()
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic Store creation order
	stores := make(map[string]tool.StateStore, len(names))
	for _, name := range names {
		stores[name] = kinds[name].NewStore(deps)
	}
	regMu.RUnlock()
	return &Registry{stores: stores}
}

// ResetForTest clears the global Kind registry for the caller's test and
// returns the func that puts back what was registered before the call.
//
// Call it as t.Cleanup(state.ResetForTest()), in the same discipline as
// fake.EnableDeliverySurfaces. Kinds register from init(), so the registry is
// populated once per process and never again: a reset that only cleared would
// leave every test that ran afterwards in the same binary looking at an empty
// registry — passing alone and failing in company, which reads as flakiness
// rather than as a leak. The restore is what scopes the clear to one test.
//
// Restoring is repeatable: each call reinstates the snapshot taken at reset
// time, so a Kind registered after a restore is dropped by the next one.
//
// Tests only.
func ResetForTest() (restore func()) {
	regMu.Lock()
	defer regMu.Unlock()
	snapshot := maps.Clone(kinds)
	kinds = map[string]Kind{}
	return func() {
		regMu.Lock()
		defer regMu.Unlock()
		// Clone per call rather than handing back the snapshot itself: a
		// Register against the restored registry would otherwise mutate the
		// snapshot, and a second restore would reinstate a Kind the first
		// never held.
		kinds = maps.Clone(snapshot)
	}
}

// noteWrapper is the on-the-wire framework envelope for a system_note's
// JSON content. The runner reads this on memory replay to route notes
// to the right Store.
type noteWrapper struct {
	// Kind is the registered Kind name the note routes to.
	Kind string `json:"kind"`
	// V is the Kind's own data-shape version; parsed here, interpreted only by
	// that Kind's ReplayNote.
	V int `json:"v"`
	// Data is the kind-defined payload handed to ReplayNote.
	Data json.RawMessage `json:"data"`
}

// DispatchSystemNote interprets payload as a wrapped state note and routes it to
// r.Get(wrapper.Kind).ReplayNote(wrapper.Data), returning (matched, err):
//
//   - (false, nil) — benign fall-through: not a wrapped note for any registered
//     Kind (invalid JSON, no "kind" field, or an unregistered kind). The caller
//     falls through to its own system_note processing.
//   - (true, nil) — the store replayed successfully.
//   - (true, err) — the note matched but ReplayNote FAILED on corrupt or
//     version-mismatched data. It must NOT fall through, and the caller MUST
//     surface the error: dropping it means the agent resumes with that Kind's
//     state (e.g. the user's tracked plan) missing and no diagnostic.
//
// Schema versioning is per-Kind — each Store's ReplayNote owns compatibility for
// its own data shape.
func DispatchSystemNote(r *Registry, payload []byte) (matched bool, err error) {
	var w noteWrapper
	if jsonErr := json.Unmarshal(payload, &w); jsonErr != nil {
		return false, nil
	}
	if w.Kind == "" {
		return false, nil
	}
	store, ok := r.Get(w.Kind)
	if !ok {
		return false, nil
	}
	if replayErr := store.ReplayNote(w.Data); replayErr != nil {
		// Matched stays true so the caller does not re-process the note, and the
		// error propagates so it can be surfaced rather than resuming silently
		// without this Kind's state.
		return true, fmt.Errorf("replay note for kind %q (v%d): %w", w.Kind, w.V, replayErr)
	}
	return true, nil
}
