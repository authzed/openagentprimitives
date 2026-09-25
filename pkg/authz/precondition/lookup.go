package precondition

import "fmt"

// FactSubject is the pair a fact read is keyed by: the resource type, and the
// RAW pre-transform identifier the observation recorded.
//
// It is a struct rather than two loose strings so that the two halves travel
// together the way factcontent.Record wrote them, and so a caller cannot pass
// them in the wrong order without the compiler noticing the type.
type FactSubject struct {
	// ResourceType is the slot's declared resource type, verbatim.
	ResourceType string
	// ResourceID is the RAW provider identifier — `demo-org/demo-repo#6`, not
	// the `spicedb_escape`d authz.ObjectID a slot's ValueTransforms produce.
	ResourceID string
}

// LookupSubject derives the fact-lookup subject for one candidate instance.
//
// # Why this is a function and not two arguments spelled at each call site
//
// The verdict for a candidate is computed in TWO places, and they must agree
// about which instance they are talking about: the bind-time filter that holds
// a candidate out of its slot, and the dispatch-time recomputation that
// explains the resulting denial to the agent. If the two derived the lookup id
// differently, the filter would hold candidate X while dispatch explained
// candidate Y — and the agent would be told about an instance that was never
// the problem, with nothing anywhere reporting a fault.
//
// A two-sided shape spelled inline on one side is exactly how the two drift, so
// there is one derivation and both sides call it.
//
// # The id is used VERBATIM, and that is the derivation
//
// No transform chain runs here, and none may be added without changing
// factcontent.Record on the writing side in the same commit. Facts are written
// keyed by the raw provider identifier, so a reader holding a slot's transform
// chain must look up with the value from BEFORE it runs and transform only for
// the SpiceDB call. A post-transform lookup cannot match — `spicedb_escape`
// exists because `#` is illegal in a SpiceDB object id — and it fails as an
// EMPTY result, which reads as undetermined: the gate holds closed forever with
// nothing anywhere reporting a fault. See the contract on
// factcontent.ForSubject, which states the same rule at the storage layer.
//
// Doing nothing to the value is therefore the correct derivation, not a missing
// one. This function exists so that the day a normalization IS correct, it
// lands in one place that both sides already call.
//
// An empty type or id is an error rather than a lookup that returns nothing.
// The lookup would fail closed either way, but it would fail closed as
// "undetermined" — indistinguishable from an observation that has not happened
// yet — and an author would go hunting for a missing tool call instead of a
// missing field.
func LookupSubject(resourceType, rawID string) (FactSubject, error) {
	if resourceType == "" {
		return FactSubject{}, fmt.Errorf("precondition: fact lookup for %q names no resource type", rawID)
	}
	if rawID == "" {
		return FactSubject{}, fmt.Errorf("precondition: fact lookup for resource type %q names no instance id", resourceType)
	}
	return FactSubject{ResourceType: resourceType, ResourceID: rawID}, nil
}
