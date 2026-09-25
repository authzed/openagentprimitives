// Package capability holds the metaagent's capability registry: the closed set
// of session changes an utterance can be classified into.
//
// Registration follows the repo's standard shape — `func init() { Register(&X{}) }`
// in the capability's own package, with a blank import in the binaries that want
// it. Adding a capability touches its own package plus one import line.
package capability

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
)

// Bound is the administrative ceiling a capability moves WITHIN.
//
// The rule the whole module rests on: a capability moves a value inside a bound
// an admin already set; it never raises one. A request above the bound is
// clamped before any card renders, so approving the card grants the clamped
// value and never the requested one.
type Bound struct {
	// Min and Max bound a numeric capability (a budget). Zero Max means the
	// capability is not numeric, or is unbounded above by policy.
	Min int64
	Max int64
	// Allowed bounds an enumerated capability (a model name). Empty means the
	// capability is not enumerated.
	Allowed []string
	// Permitted reports whether the capability may be exercised at all. A
	// capability an admin has switched off is refused regardless of standing —
	// `model` with allowModelOverride unset, for instance.
	Permitted bool
}

// Capability is one class of session change the metaagent can effect.
type Capability interface {
	// Name is the registry key ("scope", "budget", "model", "lifecycle").
	Name() string
	// DefaultOn reports whether it is active without being listed.
	DefaultOn() bool
	// Permission is the SpiceDB permission the SPEAKER must hold for this
	// capability to be offered at all.
	Permission() string
	// Actions is the closed vocabulary the classifier may emit. Never
	// free-form: a model that hallucinates an action names nothing the runtime
	// will act on.
	Actions() []metaagent.Action
	// Ceiling reports the administrative bound this capability moves within,
	// for rendering on the approval card and for enforcement.
	Ceiling(metaagent.StateSnapshot) Bound
	// Apply effects an approved (or auto-applied) decision, through the
	// per-session effector surface.
	//
	// Env is a deliberate departure from the spec's Apply(ctx, d): the registry
	// holds init-time singletons, so a capability has nowhere to keep a session
	// reference, and Apply(ctx, d) has no way to name which session to act on.
	// The tool-capability registry (pkg/agent/tool/meta/capability) solved the
	// same problem the same way — Offer(OfferContext{Env}) — so this follows an
	// established shape rather than inventing one.
	Apply(ctx context.Context, env Env, d metaagent.Decision) error
}

var (
	mu       sync.RWMutex
	registry = map[string]Capability{}
)

// Register adds a capability. Panics on a duplicate name: two capabilities
// answering to one key would make which-one-wins depend on init order, and the
// loser would be silently unreachable.
func Register(c Capability) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[c.Name()]; dup {
		panic("metaagent/capability: duplicate registration for " + c.Name())
	}
	registry[c.Name()] = c
}

// Unregister removes a capability. For tests only — production registration is
// init-time and permanent.
func Unregister(name string) {
	mu.Lock()
	defer mu.Unlock()
	delete(registry, name)
}

// Get returns the registered capability by name.
func Get(name string) (Capability, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[name]
	return c, ok
}

// All returns every registered capability, ordered by name so callers that
// render or iterate are deterministic across runs.
func All() []Capability {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Capability, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// DirectionOf resolves what a decision COSTS, from the registered action.
//
// This is the single place direction is decided, and it consults only the
// registry. Nothing the model emitted reaches it: metaagent.Decision has no
// direction to carry, so there is no claim to weigh and no path by which a
// poisoned classification could price a widening as a narrowing.
//
// An unregistered capability or action is an ERROR, never a default. A
// hallucinated action treated as narrowing would apply with no approval, which
// is the one outcome that must be unreachable — so this fails closed and loudly
// enough for the caller to surface it.
func DirectionOf(d metaagent.Decision) (metaagent.Direction, error) {
	c, ok := Get(d.Capability)
	if !ok {
		return "", fmt.Errorf("metaagent: no capability %q is registered; refusing to act on it", d.Capability)
	}
	for _, a := range c.Actions() {
		if a.Name == d.Action {
			return a.Direction, nil
		}
	}
	return "", fmt.Errorf("metaagent: capability %q registers no action %q; refusing to act on it",
		d.Capability, d.Action)
}

// RequiresApproval reports whether this decision may apply on its own.
//
// Narrowing applies immediately: reducing what a session may do can never need
// consent, and taxing the safe direction would push users away from it.
// Widening ALWAYS requires approval — including when the turn text instructs
// otherwise, because the turn is the untrusted half and "no approval needed" is
// exactly what an attacker writes.
func RequiresApproval(d metaagent.Decision) (bool, error) {
	dir, err := DirectionOf(d)
	if err != nil {
		return false, err
	}
	return dir == metaagent.Widening, nil
}

// SessionRef names the session a capability acts on.
type SessionRef struct{ Namespace, Name string }

func (s SessionRef) String() string { return s.Namespace + "/" + s.Name }

// Env is the per-session effector surface Apply acts through.
//
// Every field is a resolved function the host supplies — never a client a
// capability could use to go fetch something. That is the same discipline
// metaagent.Input enforces on the way in: a capability must not reach for
// history, and the cleanest way to guarantee it is to hand over only the exact
// verbs it may perform.
//
// A nil effector means the host did not wire that verb. A capability whose verb
// is missing must report it rather than silently succeed — losing an effect the
// user asked for and saying nothing is the failure mode the no-silent-errors
// rule exists to prevent.
type Env struct {
	Session SessionRef

	// CancelSession ends the run. Subtractive: it can only stop work.
	CancelSession func(ctx context.Context, reason string) error

	// UnapprovePhase appends a revocation for one phase of the approved plan.
	// An APPEND, never a deletion — the plan-gate log is append-only and
	// replay is latest-wins per phase.
	UnapprovePhase func(ctx context.Context, index int) error

	// ApplyScope runs the existing scope chain; WidenActivePhase carries an
	// approved widening to the second bound. Both nil on a session with no
	// scope capability wired.
	ApplyScope       ApplyScopeChange
	WidenActivePhase WidenActivePhaseForTypes

	// Notify surfaces a user-visible notice. Every auto-applied narrowing
	// produces one: a session that quietly shrank is indistinguishable from a
	// broken one.
	Notify func(ctx context.Context, body string)
}

// ApplyScopeChange runs the EXISTING scope extract → classify → apply chain for
// the current turn, and reports the resource types it added.
//
// The scope capability delegates rather than reimplements, which is what the
// spec means by "the existing behavior, unchanged except for its new trigger":
// the module contributes the ambient trigger, the standing gate and the
// direction rule, and the delta itself never travels on a Decision. That keeps
// resource ids — strings derived from untrusted turn text — on the path that
// already validates them, where ClassifySkipped checks the type against the
// closed envelope AND requires the REQUESTER to already hold the permission on
// that id.
//
// It RETURNS the added types because the caller needs them for the second bound
// (see WidenActivePhaseForTypes) and has no other way to learn them — the
// capability deliberately never sees the delta.
type ApplyScopeChange func(ctx context.Context) (addedTypes []string, err error)

// WidenActivePhaseForTypes widens the ACTIVE phase's ceiling to admit the
// handles that reach these resource types, and carries the human's approval to
// the resulting plan.
//
// This is the second half of "one intent, one approval". A user who said "you
// can also read the deploy logs" and approved it should not then be asked again
// the moment the agent tries — but the two bounds speak different vocabularies
// (scope names resource types; a phase ceiling names permission handles), so
// the host derives the handles from the types against the live surface. The
// capability stays out of it.
//
// Mechanically this supersedes the plan and writes the phase approval for the
// new digest, bounded to the active phase and to the handles the human was
// shown. Superseding re-keys the digest, but the other phases widen nothing, so
// carry-over keeps their approvals and nobody is re-asked about them.
type WidenActivePhaseForTypes func(ctx context.Context, resourceTypes []string) error
