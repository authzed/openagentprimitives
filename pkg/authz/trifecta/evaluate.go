package trifecta

import (
	"fmt"
	"strings"
)

// Verdict is the judgement over a derived set of legs.
type Verdict struct {
	// Refused is true only when ALL THREE legs hold.
	//
	// The zero value is NOT a refusal, deliberately, and this is the opposite
	// choice from handoff.GradeRoute. There the conservative default is to ask
	// a human, so zero means route. Here a zero Verdict means NOTHING WAS
	// JUDGED, and a caller that forgot to evaluate must not thereby block every
	// delegation while looking like a working gate. The fail-safe lives
	// upstream instead: Derive returns an ERROR rather than empty legs when a
	// fact cannot be established.
	Refused bool

	// LegCount is how many legs held, so a near-miss is countable without
	// parsing Reason. Two-leg handoffs are what logging mode exists to
	// collect.
	LegCount int

	// Reason names which legs combined, in operator-facing words.
	Reason string
}

// Evaluate judges derived legs.
//
// ONLY all three refuses. Each pair is permitted deliberately, and the design
// depends on that being true rather than merely tolerated:
//
//   - untrusted + sensitive — hostile content and private data, but a child
//     that cannot act on either;
//   - untrusted + consequential — hostile content and a capable child, but
//     nothing private for it to take;
//   - sensitive + consequential — private data and a capable child, but nothing
//     hostile driving what it does with them.
//
// Refusing on two would make the control fire on ordinary work constantly, and
// a control that fires constantly is one an operator turns off.
func Evaluate(l Legs) Verdict {
	var held []string
	if l.Untrusted {
		held = append(held, "untrusted input")
	}
	if l.Sensitive {
		held = append(held, "sensitive access")
	}
	if l.Consequential {
		held = append(held, "consequential action")
	}

	v := Verdict{LegCount: len(held)}
	if !l.All() {
		if len(held) == 0 {
			v.Reason = "no trifecta legs hold for this delegation"
			return v
		}
		v.Reason = fmt.Sprintf("%d of 3 trifecta legs hold (%s); a delegation is refused only when all three do",
			len(held), strings.Join(held, ", "))
		return v
	}

	v.Refused = true
	// Names all three rather than saying "the trifecta", because the remedy
	// differs by leg: narrow the child's tools, narrow the data bound to it, or
	// fix where the content came from. "Refused" alone leaves an operator
	// guessing which of three things to change.
	v.Reason = fmt.Sprintf(
		"this delegation combines all three: %s. Refused because untrusted content could determine an "+
			"authority-bearing argument on data the child's own audience is not entitled to. Remove any one leg — "+
			"narrow the child's tools, unbind the private input, or clean the source — and it proceeds",
		strings.Join(held, ", "))
	return v
}
