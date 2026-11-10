package bronzethread

import (
	"fmt"
	"strings"
)

// A bundle normally drives ONE session: every user turn lands on the same
// thread of the fixture's one conversational Channel, and the transcript is a
// single positional stream. A scenario about how several sessions share the
// cluster — the admission line — cannot be written that way: it needs several
// sessions, of more than one AgentClass, alive at once.
//
// Three optional fields open that up, and a bundle that sets none of them is
// exactly the single-session bundle it always was:
//
//   - UserTurn.Thread (and Channel) start or continue a NAMED session. Each
//     thread is its own channel key, so its own AgentSession.
//   - LLMStep.Thread scopes a transcript step to one thread's session, the
//     way AgentClass scopes it to a class. Several sessions of one class
//     cannot be told apart by class, and an unscoped step is answered by
//     whichever session asks next — which is a different transcript every
//     run once sessions overlap.
//   - UserTurn.WaitFor holds a turn until other sessions reach a state, so a
//     scenario can say "while A is still working" without guessing at timing.

// Barrier states a TurnBarrier can wait for.
const (
	// UntilAwaitingInput: the session is parked in await_user_message, still
	// Running. The only state a follow-up turn can be delivered INTO rather
	// than starting a fresh turn.
	UntilAwaitingInput = "awaitingInput"
	// UntilIdle: the session's turn ended (phase Idle).
	UntilIdle = "idle"
)

// TurnBarrier holds a user turn until the session on Thread reaches Until.
type TurnBarrier struct {
	Thread string `json:"thread"`
	Until  string `json:"until"`
}

// Admission turns on whole-set session admission for the run, against a
// fixed capacity, so a bundle can exercise the line itself. Absent, the run is
// ungated exactly as before — the harness then has no capacity to measure.
type Admission struct {
	// CapacitySessions is how many sessions' whole pod sets the cluster can
	// hold at once. The harness has no nodes, so this stands in for them.
	CapacitySessions int `json:"capacitySessions"`
}

// AdmissionAssertions are claims about the order the line admitted sessions
// in, read from what the run actually did. Threads are named as in userTurns.
//
// Ordering is by FIRST OBSERVATION: the driver watches every session for the
// whole run and records when each was first seen admitted and first seen
// finished. Two sessions admitted in one pass may be seen together, so only
// STRICT orderings are claimable — which is all a line promises.
type AdmissionAssertions struct {
	// AdmittedBefore: each pair's First was admitted strictly before Then.
	AdmittedBefore []ThreadOrder `json:"admittedBefore,omitempty"`
	// CompletedBefore: each pair's First finished its work (reached Idle)
	// strictly before Then did.
	CompletedBefore []ThreadOrder `json:"completedBefore,omitempty"`
	// PeakAdmitted is the EXACT largest number of sessions the run held
	// admitted at once. Equal to the capacity proves the line actually
	// queued; above it is the over-admission the line exists to prevent.
	PeakAdmitted int `json:"peakAdmitted,omitempty"`
}

// ThreadOrder is one strict ordering claim between two threads.
type ThreadOrder struct {
	First string `json:"first"`
	Then  string `json:"then"`
}

// Threads lists every named thread the bundle's user turns open, in the order
// they first appear.
func (b Bundle) Threads() []string {
	var out []string
	seen := map[string]bool{}
	for _, ut := range b.UserTurns {
		if ut.Thread != "" && !seen[ut.Thread] {
			seen[ut.Thread] = true
			out = append(out, ut.Thread)
		}
	}
	return out
}

// OpeningText is the text of the first user turn on thread: the message that
// opened that thread's session, and so the first user message of every model
// request the session makes. It is how a thread-scoped step recognizes its
// session's requests.
func (b Bundle) OpeningText(thread string) string {
	for _, ut := range b.UserTurns {
		if ut.Thread == thread {
			return ut.Text
		}
	}
	return ""
}

// validateSessions refuses a multi-session bundle the driver could not route.
func (b Bundle) validateSessions() error {
	threads := map[string]bool{}
	for _, th := range b.Threads() {
		threads[th] = true
	}
	if len(threads) > 0 && b.Trigger != nil {
		return fmt.Errorf("named threads start sessions from userTurns; a triggered bundle has one session")
	}
	for i, ut := range b.UserTurns {
		if len(threads) > 0 && ut.Thread == "" {
			return fmt.Errorf("userTurns[%d] names no thread, but others do; once one turn names its session every turn must", i)
		}
		if ut.Channel != "" && ut.Thread == "" {
			return fmt.Errorf("userTurns[%d] names a channel but no thread", i)
		}
		for j, w := range ut.WaitFor {
			if !threads[w.Thread] {
				return fmt.Errorf("userTurns[%d].waitFor[%d] names thread %q, which no user turn opens", i, j, w.Thread)
			}
			if w.Until != UntilAwaitingInput && w.Until != UntilIdle {
				return fmt.Errorf("userTurns[%d].waitFor[%d].until is %q; want %q or %q",
					i, j, w.Until, UntilAwaitingInput, UntilIdle)
			}
		}
	}
	// Each thread's opening text is how its steps find its requests, so no
	// opening may contain another's: a request would then match both scopes.
	for a := range threads {
		for c := range threads {
			if a != c && strings.Contains(b.OpeningText(a), b.OpeningText(c)) {
				return fmt.Errorf("thread %q's opening text contains thread %q's, so their transcript steps cannot be told apart", a, c)
			}
		}
	}
	for i, step := range b.LLM {
		if step.Thread != "" && !threads[step.Thread] {
			return fmt.Errorf("llm[%d] is scoped to thread %q, which no user turn opens", i, step.Thread)
		}
	}
	if b.Admission != nil && b.Admission.CapacitySessions < 1 {
		return fmt.Errorf("admission.capacitySessions must be at least 1; a cluster with room for no session admits nothing")
	}
	if a := b.Assert.Admission; a != nil {
		if b.Admission == nil {
			return fmt.Errorf("assert.admission makes claims about a line, but the bundle turns admission on nowhere (admission)")
		}
		for _, list := range [][]ThreadOrder{a.AdmittedBefore, a.CompletedBefore} {
			for _, o := range list {
				if !threads[o.First] || !threads[o.Then] {
					return fmt.Errorf("assert.admission orders %q before %q, but each must be a thread a user turn opens", o.First, o.Then)
				}
			}
		}
	}
	return nil
}
