package subagentrequest

// Attenuation and grading answer different questions, and the controller
// enforced only the first.
//
//   - Attenuation: may the PARENT delegate this tag at all? It bounds the
//     envelope, so a compromised parent can choose badly within it and no worse.
//   - Grading: would binding it DISCLOSE anything to the CHILD's audience, and
//     can the datum be TRUSTED? A parent holding a tag legitimately can still
//     hand it somewhere its readers were never authorized.
//
// A tag the parent may read is not thereby a tag this child may receive.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
)

// gradeFunc builds a Grader returning a fixed verdict per tag. A tag absent
// from the map grades as GradeRoute — the zero value, which is "ask a human"
// and therefore "do not auto-grant".
func gradeFunc(verdicts map[string]handoff.Grade, err error) func(context.Context, authz.SessionRef, string) (handoff.Grade, string, error) {
	return func(_ context.Context, _ authz.SessionRef, tagID string) (handoff.Grade, string, error) {
		if err != nil {
			return handoff.GradeRoute, "", err
		}
		return verdicts[tagID], "test verdict", nil
	}
}

// TestATagThePARENTCanReadIsStillNotAutoBound is the gap this closed.
//
// Both tags pass attenuation — the parent holds access on both — and only
// grading can see that one of them would disclose to the child's audience or
// carries untrusted content.
//
// What it asserts changed when the decision moved BEFORE child creation: with
// one slot awaiting a person, NOTHING binds and no child starts. The
// delegation waits whole. Binding the clean tag now would mean either starting
// a child on a partial handoff — which it cannot tell from a parent that chose
// to send less — or writing a grant for a child that does not exist yet.
func TestATagThePARENTCanReadIsStillNotAutoBound(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-clean": true, "ptt-discloses": true}}
	r, c := reconcilerWithAuthz(t, az, requestWithDataSlots(t, "req-grade",
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-clean"},
		v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"},
	))
	_ = c
	r.Grader = gradeFunc(map[string]handoff.Grade{
		"ptt-clean":     handoff.GradeAutoGrant,
		"ptt-discloses": handoff.GradeRoute,
	}, nil)

	reconcileOnce(t, r, "req-grade")

	assert.Empty(t, az.grantedDataSlots,
		"one slot awaiting a person holds the whole delegation; a half-bound child "+
			"cannot tell that from a parent that sent less")
	got := getRequest(t, r.Client, "req-grade")
	require.Len(t, got.Status.PendingDataSlots, 1, "and the one being asked about is named")
	assert.Equal(t, "ptt-discloses", got.Status.PendingDataSlots[0].TagID)
	assert.Nil(t, got.Status.ChildRef, "no child is created while a slot is undecided")
}

// TestAnUngradedSlotIsROUTEDNotDropped.
//
// A slot that would disclose is a QUESTION, not a verdict: the spec routes it
// to someone who can speak for the datum. It used to be refused outright,
// which both denied a disclosure nobody had ruled on and made the trifecta
// unreachable — every bound slot was, by construction, neither untrusted nor
// disclosing.
//
// What must never happen is the slot being dropped: a child waiting on data
// that never arrives, with nothing saying why, is the failure this whole area
// exists to prevent. Pending is that record now.
func TestAnUngradedSlotIsROUTEDNotDropped(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-discloses": true}}
	sr := requestWithDataSlots(t, "req-ref", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"})
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-discloses": handoff.GradeRoute}, nil)

	reconcileOnce(t, r, "req-ref")

	assert.Empty(t, az.grantedDataSlots, "nothing may be bound before a human answers")
	got := getRequest(t, r.Client, "req-ref")
	require.Len(t, got.Status.PendingDataSlots, 1,
		"the parent must be able to read back that the slot is awaiting a decision")
	assert.Equal(t, "logs", got.Status.PendingDataSlots[0].Slot)
	assert.Equal(t, v1.SubagentRequestPhaseAwaitingDisclosure, got.Status.Phase)
	assert.Nil(t, got.Status.ChildRef,
		"the child must NOT be started while a slot it was promised is undecided")
}

// TestAnApprovedDisclosureBinds is the other half: once a human has cleared
// the exact slot for this request, the grade that declined to auto-answer is
// answered and the binding proceeds.
func TestAnApprovedDisclosureBinds(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-discloses": true}}
	sr := requestWithDataSlots(t, "req-ok", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"})
	sr.Status.ApprovedDataSlots = []v1.DataSlotRequest{{Slot: "logs", TagID: "ptt-discloses"}}
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-discloses": handoff.GradeRoute}, nil)

	reconcileOnce(t, r, "req-ok")

	got := getRequest(t, r.Client, "req-ok")
	assert.Empty(t, got.Status.PendingDataSlots, "the question is settled")
	require.Len(t, got.Status.BoundDataSlots, 1)
	assert.Equal(t, "logs", got.Status.BoundDataSlots[0].Slot)
	assert.NotEmpty(t, az.grantedDataSlots, "the grant must actually be written")
}

// TestAnApprovalDoesNotTravelToADifferentTag.
//
// Consent was given to disclose ONE datum. A slot re-pointed at another tag is
// a different disclosure, and riding the first approval would let a parent
// swap the payload after the human agreed to it — the same repoint-reasks rule
// the plan gate's slots already follow.
func TestAnApprovalDoesNotTravelToADifferentTag(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-other": true}}
	sr := requestWithDataSlots(t, "req-swap", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-other"})
	sr.Status.ApprovedDataSlots = []v1.DataSlotRequest{{Slot: "logs", TagID: "ptt-approved"}}
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-other": handoff.GradeRoute}, nil)

	reconcileOnce(t, r, "req-swap")

	got := getRequest(t, r.Client, "req-swap")
	assert.Empty(t, az.grantedDataSlots, "the approval named a different datum")
	require.Len(t, got.Status.PendingDataSlots, 1, "the swapped tag must be asked about afresh")
	assert.Equal(t, "ptt-other", got.Status.PendingDataSlots[0].TagID)
}

// TestAnUnanswerableGradeDoesNotBind.
//
// GradeRoute is the zero value precisely so an error path cannot silently
// disclose. A fact we could not establish is not a fact in the request's
// favour, and this asserts the error path behaves like the refusal rather than
// like a grant.
func TestAnUnanswerableGradeDoesNotBind(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-x": true}}
	r, _ := reconcilerWithAuthz(t, az, requestWithDataSlots(t, "req-err",
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-x"}))
	r.Grader = gradeFunc(nil, errors.New("spicedb unavailable"))

	reconcileOnce(t, r, "req-err")

	assert.Empty(t, az.grantedDataSlots,
		"a grade that could not be computed must not be treated as a yes")
}

// TestGradingRunsAFTERAttenuation, so a tag the parent cannot read is never
// even graded.
//
// Order matters for a reason beyond efficiency: grading asks SpiceDB about a
// tag's readers, and doing that for a tag the parent has no standing on would
// answer a question the parent was never entitled to ask.
func TestGradingRunsAFTERAttenuation(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-mine": true}}
	var graded []string
	r, _ := reconcilerWithAuthz(t, az, requestWithDataSlots(t, "req-order",
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-mine"},
		v1.DataSlotRequest{Slot: "logs", TagID: "ptt-theirs"},
	))
	r.Grader = func(_ context.Context, _ authz.SessionRef, tagID string) (handoff.Grade, string, error) {
		graded = append(graded, tagID)
		return handoff.GradeAutoGrant, "", nil
	}

	reconcileOnce(t, r, "req-order")

	assert.Equal(t, []string{"ptt-mine"}, graded,
		"a tag the parent cannot read must never reach the grader — asking about its readers answers a question the parent was not entitled to ask")
}

// TestNoGraderKeepsAttenuationOnlyBehaviour: the field is nil in any deployment
// that has not wired grading, and must behave exactly as it did before.
func TestNoGraderKeepsAttenuationOnlyBehaviour(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-mine": true}}
	r, _ := reconcilerWithAuthz(t, az, requestWithDataSlots(t, "req-nograder",
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-mine"}))
	// r.Grader deliberately nil.

	reconcileOnce(t, r, "req-nograder")

	require.Len(t, az.grantedDataSlots, 1,
		"without a grader, attenuation alone decides — the behaviour that shipped before per-datum provenance")
}

// TestAnUnansweredDisclosureFailsTheDelegationRatherThanHanging.
//
// The wait must END. A question nobody answers, left open, blocks the parent's
// delegate call to its own timeout and reports nothing about why — which reads
// to a user as the agent hanging rather than as a decision nobody made. It
// also must NOT quietly proceed without the data: that is the partial handoff
// the child cannot distinguish from a parent that chose to send less.
func TestAnUnansweredDisclosureFailsTheDelegationRatherThanHanging(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-discloses": true}}
	sr := requestWithDataSlots(t, "req-stale", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"})
	long_ago := metav1.NewTime(time.Now().Add(-24 * time.Hour))
	sr.Status.DisclosureRequestedAt = &long_ago
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-discloses": handoff.GradeRoute}, nil)

	reconcileOnce(t, r, "req-stale")

	got := getRequest(t, r.Client, "req-stale")
	assert.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Determination, "no part of it ran",
		"the parent must be told the delegation did not happen at all")
	assert.Empty(t, az.grantedDataSlots, "an unanswered question never binds")
	assert.Nil(t, got.Status.ChildRef)
}
