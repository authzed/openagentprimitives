package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func hasPending(s State, rid string) bool {
	for _, p := range s.Pending {
		if p.RequestID == rid {
			return true
		}
	}
	return false
}

func TestDecisionSubMachine(t *testing.T) {
	t.Run("ask tool_call: Running → AwaitingDecision, pending recorded", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(),
			DecisionAsked{RequestID: "r1", Kind: DecisionToolCall})
		assert.Equal(t, PhaseAwaitingDecision, got.Phase)
		assert.True(t, hasPending(got, "r1"))
	})

	// Approval timeout clears the queue+condition for EVERY kind (not just tool_call).
	for _, k := range []DecisionKind{DecisionToolCall, DecisionLeakageShare, DecisionContentInspect} {
		t.Run(string(k)+" timeout clears approval queue and returns to Running (anti-strand)", func(t *testing.T) {
			in := State{Phase: PhaseAwaitingDecision, Region: RegionRunner,
				Pending: []PendingDecision{{RequestID: "r1", Kind: k}}}
			got, effs := Transition(in.OrDefault(), DecisionResolved{RequestID: "r1", TimedOut: true})
			assert.Equal(t, PhaseRunning, got.Phase, "no longer stranded in AwaitingDecision")
			assert.False(t, hasPending(got, "r1"), "queue cleared on timeout")
			var unparked bool
			for _, e := range effs {
				if u, ok := e.(Unpark); ok && u.TimedOut {
					unparked = true
				}
			}
			assert.True(t, unparked, "blocked goroutine released as timed-out (anti-confabulation)")
		})
	}

	// Join decisions stay visible while Idle (phase unchanged), and time out instead of stranding.
	t.Run("join asked while Idle stays Idle but records the pending decision", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseIdle, Region: RegionOperatorPre}.OrDefault(),
			DecisionAsked{RequestID: "j1", Kind: DecisionJoin})
		assert.Equal(t, PhaseIdle, got.Phase)
		assert.True(t, hasPending(got, "j1"), "join wait is first-class, not silently masked")
	})
	t.Run("join timeout clears the pending entry and emits Unpark{TimedOut:true}", func(t *testing.T) {
		in := State{Phase: PhaseIdle, Region: RegionOperatorPre,
			Pending: []PendingDecision{{RequestID: "j1", Kind: DecisionJoin}}}
		got, effs := Transition(in.OrDefault(), DecisionResolved{RequestID: "j1", TimedOut: true})
		assert.False(t, hasPending(got, "j1"))
		assert.Equal(t, PhaseIdle, got.Phase)
		var unparked bool
		for _, e := range effs {
			if u, ok := e.(Unpark); ok && u.TimedOut {
				unparked = true
			}
		}
		assert.True(t, unparked, "join timeout must release the blocked goroutine (anti-strand)")
	})

	t.Run("scope_review ask sets ScopeReviewPending (surface gap fix)", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(),
			DecisionAsked{RequestID: "s1", Kind: DecisionScopeReview})
		assert.True(t, got.ScopeReviewPending)
	})

	// Safety-critical: cold-start scope_review timeout fails closed — gate cannot be bypassed.
	t.Run("scope_review timeout fails closed: Phase==Failed, FailureReason==ScopeReviewFailed", func(t *testing.T) {
		in := State{
			Phase:              PhaseAwaitingDecision,
			Region:             RegionRunner,
			Pending:            []PendingDecision{{RequestID: "s1", Kind: DecisionScopeReview}},
			ScopeReviewPending: true,
		}
		got, _ := Transition(in.OrDefault(), DecisionResolved{RequestID: "s1", TimedOut: true})
		assert.Equal(t, PhaseFailed, got.Phase, "scope_review timeout must fail the session closed")
		assert.Equal(t, "ScopeReviewFailed", got.FailureReason, "cold-start gate cannot be bypassed")
	})

	// addPending idempotency: restart re-issue must not duplicate the entry.
	t.Run("addPending idempotency: DecisionAsked twice → len(Pending)==1", func(t *testing.T) {
		base := State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault()
		after1, _ := Transition(base, DecisionAsked{RequestID: "r1", Kind: DecisionToolCall})
		after2, _ := Transition(after1, DecisionAsked{RequestID: "r1", Kind: DecisionToolCall})
		assert.Equal(t, 1, len(after2.Pending), "re-issued ask must not duplicate the pending entry")
	})
}
