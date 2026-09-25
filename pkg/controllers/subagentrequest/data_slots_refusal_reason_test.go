package subagentrequest

// A refusal's REASON, carried on status rather than only in a log line.
//
// Three causes reach the refused list, and they point a reader at three
// different actions: re-ask for something you hold, get a human to authorize
// the disclosure, or simply retry. The controller logged them apart from the
// start; the parent, which reads status and not logs, could not tell them
// apart at all.

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
)

// reasonFor returns the recorded reason for one slot.
func reasonFor(t *testing.T, refused []v1.RefusedDataSlot, slot string) v1.DataSlotRefusalReason {
	t.Helper()
	for _, r := range refused {
		if r.Slot == slot {
			return r.Reason
		}
	}
	t.Fatalf("slot %q is not in the refused list at all", slot)
	return ""
}

// TestARefusalAndAPendingQuestionAreDifferentOutcomes.
//
// One request carrying both at once, because the bug was a COLLAPSE: every
// cause landed in the same undifferentiated list, so a test exercising one at
// a time would have passed throughout.
//
// A slot the parent cannot read is OVER — a mistake in what it asked for. A
// slot that would disclose is a question still open, and reporting it as a
// refusal would have the parent tell a user it was denied when nobody has yet
// been asked.
func TestARefusalAndAPendingQuestionAreDifferentOutcomes(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-discloses": true}}
	sr := requestWithDataSlots(t, "req-reasons",
		v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"},
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-unheld"},
	)
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-discloses": handoff.GradeRoute}, nil)

	reconcileOnce(t, r, "req-reasons")

	got := getRequest(t, r.Client, "req-reasons")
	require.Len(t, got.Status.RefusedDataSlots, 1)
	assert.Equal(t, v1.DataSlotNotDelegable, reasonFor(t, got.Status.RefusedDataSlots, "diff"),
		"the parent cannot read this one at all; re-asking for it is pointless")
	require.Len(t, got.Status.PendingDataSlots, 1)
	assert.Equal(t, "logs", got.Status.PendingDataSlots[0].Slot,
		"the parent CAN read this one, so it is a question for a human rather than a refusal")
	assert.Empty(t, az.grantedDataSlots)
}

// TestAnUnanswerableGradeIsTransientNotAJudgement.
//
// The distinction that matters most of the three. Recording a failed lookup as
// WouldDisclose reports a judgement nobody made, and sends a parent to re-ask
// differently when the right move was to retry the same request.
func TestAnUnanswerableGradeIsTransientNotAJudgement(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-x": true}}
	sr := requestWithDataSlots(t, "req-transient", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-x"})
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(nil, errors.New("spicedb unavailable"))

	reconcileOnce(t, r, "req-transient")

	got := getRequest(t, r.Client, "req-transient")
	require.Len(t, got.Status.RefusedDataSlots, 1)
	assert.Equal(t, v1.DataSlotGradingUnavailable, reasonFor(t, got.Status.RefusedDataSlots, "logs"))
	assert.Empty(t, az.grantedDataSlots, "an unanswerable grade still binds nothing")
}
