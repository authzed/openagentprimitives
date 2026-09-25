package precondition

// Rule is one DECLARED precondition: the compiled predicate together with the
// two messages an unsatisfied verdict hands the agent.
//
// The three travel as one value rather than as a compiled program beside a
// parallel slice of messages, because the message is chosen BY the verdict —
// the hint for Undetermined, the refusal for Refused — and two slices keyed by
// index go out of step the first time a slot's `requires[]` is reordered or one
// entry fails to compile. That drift is silent in the worst direction: the
// agent is told, in the author's own words, about a rule that is not the one
// holding its slot shut.
//
// The messages are carried rather than derived because neither can be: an
// author's "call get_pr_info first" and "reviewing a fork means running
// untrusted code" are the two things CEL cannot say about itself.
type Rule struct {
	// Compiled is the predicate. A nil Compiled is not "no rule" — Evaluate
	// refuses it with an error, and every caller here holds the slot on an
	// error, so a Rule that lost its program fails closed rather than binding.
	Compiled *Compiled
	// UndeterminedHint is what the AGENT is told while some referenced fact is
	// not yet recorded. It is the only party that can fix that, by making the
	// call that establishes the fact.
	UndeterminedHint string
	// RefusalMessage explains what was refused and why, for a predicate the
	// recorded facts decided against.
	RefusalMessage string
	// Approvers are the subject-set expressions the waiver card for a Refused
	// verdict is routed to. Unset means the slot's resolved standing decides;
	// carried here so a later human-in-the-loop path can ask the risk-answerer
	// rather than the resource owner (whose set is often empty for a userless
	// session). Not consumed by Evaluate — it selects nothing about the verdict.
	Approvers []string
}

// Message returns what this rule says about a verdict.
//
// It is the ONE place the verdict→message mapping is written. The bind-time
// filter and the dispatch-time explanation both need it, and a second copy is
// how the gate that holds a candidate and the sentence explaining that hold
// come to disagree.
//
// Satisfied — and any value that is not a verdict at all — returns the empty
// string: a precondition that is met explains nothing, and inventing a sentence
// for an unknown verdict would put words in an author's mouth. Callers that
// must show the agent something check for empty and say so themselves rather
// than handing it a denial with no reason.
func (r Rule) Message(v Verdict) string {
	switch v {
	case Undetermined:
		return r.UndeterminedHint
	case Refused:
		return r.RefusalMessage
	default:
		return ""
	}
}

// Unsatisfied is the first rule of a slot's `requires[]` that did not evaluate
// Satisfied, with the verdict it reached and — when that verdict is
// Undetermined — the references still missing.
type Unsatisfied struct {
	Rule    Rule
	Verdict Verdict
	Missing []FactRef
}

// FirstUnsatisfied evaluates rules in order and returns the first that is not
// Satisfied. ok is false when every rule is Satisfied, which is the only state
// in which an instance may occupy the slot.
//
// It stops at the first non-Satisfied verdict because one hold is a hold: the
// slot does not bind, and evaluating on would only produce a second answer
// about a predicate that changes nothing. The one that stopped it is the one an
// operator — and the agent — needs.
//
// # Why this is a function and not a loop written at each call site
//
// The same question is asked in TWO places, for two different reasons: the
// bind-time filter asks it to decide whether a candidate may occupy its slot,
// and the dispatch-time explanation asks it to tell the agent why the slot it
// named is empty. If the two iterated differently — a different stopping rule,
// a different treatment of an eval error — the filter would hold the slot on
// one rule while the agent was told about another, and nothing anywhere would
// report a fault. Same argument as LookupSubject: one derivation, two
// consumers.
//
// An error is returned rather than folded into a verdict, for the reason
// Evaluate states at length: "this expression is broken" must stay
// distinguishable from "no fact yet", or a broken predicate presents as a gate
// quietly doing its job. Callers hold the slot on an error AND say so.
func FirstUnsatisfied(rules []Rule, f Facts, slotType, slotID string) (Unsatisfied, bool, error) {
	for _, r := range rules {
		verdict, missing, err := Evaluate(r.Compiled, f, slotType, slotID)
		if err != nil {
			return Unsatisfied{}, false, err
		}
		if verdict == Satisfied {
			continue
		}
		return Unsatisfied{Rule: r, Verdict: verdict, Missing: missing}, true, nil
	}
	return Unsatisfied{}, false, nil
}
