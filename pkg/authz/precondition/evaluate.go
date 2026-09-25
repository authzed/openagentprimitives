package precondition

import "fmt"

// Verdict is the outcome of evaluating a precondition against the facts
// recorded so far. It is TRI-state, and the third state is the point: a
// predicate over facts nobody has written yet has not answered "no", it has not
// answered at all.
//
// Collapsing Undetermined into Refused raises a waiver card on every gated
// session before the agent can establish anything — asking a human to approve a
// fork review for pull requests that are not forks. Collapsing it into
// Satisfied is the bypass. Both directions are wrong, which is why there is a
// third value rather than a bool plus a comment.
type Verdict int

const (
	// Undetermined is the zero value on purpose. A Verdict nobody set must be
	// the one that never binds: a caller who drops the error and reads the
	// verdict anyway then holds the slot, which is the survivable failure.
	// Making Satisfied the zero would make a forgotten assignment open a gate.
	Undetermined Verdict = iota
	// Satisfied: every referenced fact is recorded and the predicate is true.
	Satisfied
	// Refused: every referenced fact is recorded and the predicate is false.
	// A human may still waive this; that is what makes it distinct from
	// Undetermined, which no human can act on because the missing input is an
	// observation, not a decision.
	Refused
)

// String renders the verdict for logs, status and hold reasons. The strings are
// lowercase because they read as a clause in the messages that carry them.
func (v Verdict) String() string {
	switch v {
	case Undetermined:
		return "undetermined"
	case Satisfied:
		return "satisfied"
	case Refused:
		return "refused"
	default:
		// Never silently render an unknown verdict as one of the three: a bad
		// cast reading as "satisfied" in a log is how an operator concludes the
		// gate opened legitimately.
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// Facts holds the facts recorded about ONE candidate instance, split by the
// provenance that addresses them. The split is not organizational: provenance
// is a trust grade (see the constants in compile.go), and an author who gated
// on an envelope fact must not be answered from the observed map.
//
// A nil map is a legitimate value meaning nothing of that provenance has been
// recorded — which is undetermined for any reference into it, never false.
type Facts struct {
	Envelope map[string]any
	Observed map[string]any
}

// forProvenance selects the map a FactRef addresses.
//
// The switch is over this package's own closed constant pair, in the package
// that declares them — not a kind dispatch that belongs on a registry. The
// unknown arm cannot fire through Compile, which refuses an unknown provenance
// at admission with a message naming the typo; it errors rather than reporting
// the fact absent because an unnameable namespace would otherwise hold the slot
// undetermined forever while the author hunted for a missing observation.
func (f Facts) forProvenance(provenance string) (map[string]any, error) {
	switch provenance {
	case ProvenanceEnvelope:
		return f.Envelope, nil
	case ProvenanceObserved:
		return f.Observed, nil
	default:
		return nil, fmt.Errorf(
			"precondition: unknown fact provenance %q: only %q and %q exist",
			provenance, ProvenanceEnvelope, ProvenanceObserved)
	}
}

// Evaluate decides one precondition against the facts recorded so far, binding
// the candidate instance as `slot`.
//
// The returned []FactRef names the references that are MISSING, and is
// non-empty exactly when the verdict is Undetermined — it is what a caller
// hands the agent so it knows which fact to establish. It comes back in
// References' order (provenance, then name), so a hold reason recorded from it
// is stable across re-evaluations rather than churning with map order.
//
// # Presence is checked BEFORE evaluation, and that order is the design
//
// Evaluating first and inspecting the result cannot work, because CEL
// short-circuits, and it does so in BOTH directions:
//
//   - `facts.observed.a && facts.observed.b` with a=false yields false without
//     ever touching b, so a predicate the recorded facts cannot yet decide
//     would report Refused. That silently turns "nobody has answered" into
//     "the answer is no".
//   - `facts.observed.a || facts.observed.b` with a=true yields true the same
//     way, reporting Satisfied — which BINDS the slot on a predicate no
//     observation has cleared. This is the bypass, and it is strictly worse
//     than the refusal above: one wrongly holds a slot shut, the other wrongly
//     opens it.
//
// A conditional does it too, evaluating only the taken branch. Both directions
// are order-dependent — call the tool before the observation and you get one
// answer, after it and you get another — and both breach the monotonicity the
// whole gate rests on, since a verdict reached that way can later be
// contradicted rather than only narrowed.
//
// Checking every reference up front makes the short-circuit unreachable for
// every such shape at once, rather than shape by shape: a predicate is
// evaluated only once its entire input is present, at which point whatever CEL
// skips cannot change the answer.
func Evaluate(c *Compiled, f Facts, slotType, slotID string) (Verdict, []FactRef, error) {
	if c == nil {
		// A nil precondition is not "no precondition" — a caller reaching here
		// with one has lost the compiled program that decides whether the slot
		// binds, and answering that as any verdict would invent a decision.
		return Undetermined, nil, fmt.Errorf("precondition: evaluate: nil compiled precondition")
	}

	var missing []FactRef
	// c.refs directly rather than References(): this loop runs per candidate per
	// dispatch and again on every re-bind signal, and the accessor allocates a
	// clone each call. The clone is right for callers OUTSIDE the package — this
	// set is what decides undetermined, and a caller who could shrink it in
	// place could bind a slot no observation has cleared — but the loop below
	// only reads, so it has no need to pay for one.
	for _, ref := range c.refs {
		recorded, err := f.forProvenance(ref.Provenance)
		if err != nil {
			return Undetermined, nil, err
		}
		// Comma-ok, never `v := recorded[ref.Name]; v == nil`. A present key
		// holding nil is a fact the tool actually reported whose value is null,
		// and a gate may act on it; an absent key is undetermined. The bare form
		// conflates the two and fails closed on real evidence, which looks like
		// nothing at all rather than like a bug — see the accessor doc comment on
		// memory/kinds/factcontent.ForSubject, which writes the wrong shape down.
		if _, ok := recorded[ref.Name]; !ok {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		return Undetermined, missing, nil
	}

	// The activation's shape is spec surface: these key names are how every
	// authored precondition addresses its inputs, so they match compile.go's
	// bindings and the provenance constants exactly.
	out, _, err := c.Program().Eval(map[string]any{
		factsVar: map[string]any{
			ProvenanceEnvelope: f.Envelope,
			ProvenanceObserved: f.Observed,
		},
		slotVar: map[string]any{
			"resourceType": slotType,
			"resourceID":   slotID,
		},
	})
	if err != nil {
		// An eval error is an ERROR, never Undetermined. `slot` is cel.DynType,
		// so a typo'd `slot.resourceId` type-checks and fails only here; folding
		// that into Undetermined would shut the slot forever with no signal,
		// indistinguishable from a fact nobody has recorded — fail-closed, but
		// permanently and invisibly. Surfacing it is what lets the author see it.
		return Undetermined, nil, fmt.Errorf("precondition: evaluate %q: %w", c.Expression(), err)
	}

	// Compile refuses a non-bool expression, so neither arm below is reachable
	// through it today. They are spelled out rather than assumed because the
	// alternative to an explicit error is a silent default, and the only safe
	// default — Undetermined — is exactly the verdict that must stay reserved
	// for "no fact yet" and never mean "this expression is broken".
	if out == nil {
		return Undetermined, nil, fmt.Errorf(
			"precondition: evaluate %q: produced neither a value nor an error", c.Expression())
	}
	decided, ok := out.Value().(bool)
	if !ok {
		return Undetermined, nil, fmt.Errorf(
			"precondition: evaluate %q: produced %T, not bool: a precondition that may not be a decision cannot decide whether a slot binds",
			c.Expression(), out.Value())
	}
	if decided {
		return Satisfied, nil, nil
	}
	return Refused, nil, nil
}
