package subagentrequest

// The ASK: publishing the card, and acting on the answer.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestAHumansNoEndsItImmediately.
//
// Waiting out the window on a decided question reports a timeout for a
// decision that WAS made — telling the parent nobody answered when someone
// did. A denial must be as actionable as an approval.
func TestAHumansNoEndsItImmediately(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-discloses": true}}
	sr := requestWithDataSlots(t, "req-no", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"})
	sr.Status.DeniedDataSlots = []v1.DataSlotRequest{{Slot: "logs", TagID: "ptt-discloses"}}
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-discloses": handoff.GradeRoute}, nil)

	reconcileOnce(t, r, "req-no")

	got := getRequest(t, r.Client, "req-no")
	assert.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Determination, "refused by someone who can speak for that data")
	assert.Contains(t, got.Status.Determination, "no part of it ran")
	assert.Empty(t, az.grantedDataSlots)
	assert.Nil(t, got.Status.ChildRef)
}

// TestTheCardIsPublishedOnceNotOncePerPoll.
//
// The controller re-reconciles every 15s while parked. A publish on each pass
// would be a card every 15s for the whole 20-minute window; the
// disclosureRequestedAt timestamp is the at-most-once guard.
func TestTheCardIsPublishedOnceNotOncePerPoll(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-discloses": true}}
	sr := requestWithDataSlots(t, "req-once", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"})
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-discloses": handoff.GradeRoute}, nil)
	var published int
	r.PublishInteraction = func(context.Context, string, string, channelevents.Envelope) error {
		published++
		return nil
	}
	// A card with no approvers is never published — the envelope contract
	// refuses an approvers-scope prompt without them — so a decider has to
	// exist for this test to be about publishing at all.
	r.TagSources = func(context.Context, authz.SessionRef, string) ([]string, error) {
		return []string{"note:n1"}, nil
	}
	r.ResourceOwners = func(context.Context, string, string) ([]string, error) {
		return []string{"cmV2aWV3ZXItOUBleGFtcGxlLmNvbQ"}, nil
	}

	reconcileOnce(t, r, "req-once")
	reconcileOnce(t, r, "req-once")
	reconcileOnce(t, r, "req-once")

	assert.Equal(t, 1, published, "one card per slot, however many times the controller polls")
}

// TestNoPublisherStillRoutesAndStillExpires.
//
// The routing must not depend on the card reaching anyone. With no publisher
// the request still parks and still ends at the window with a stated reason.
// Binding what nobody was asked about is the one outcome worse than not
// asking, and it cannot happen here because binding reads approvedDataSlots,
// which only a real decision writes.
func TestNoPublisherStillRoutesAndStillExpires(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-discloses": true}}
	sr := requestWithDataSlots(t, "req-nopub", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"})
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-discloses": handoff.GradeRoute}, nil)
	r.PublishInteraction = nil

	reconcileOnce(t, r, "req-nopub")

	got := getRequest(t, r.Client, "req-nopub")
	assert.Equal(t, v1.SubagentRequestPhaseAwaitingDisclosure, got.Status.Phase)
	assert.Empty(t, az.grantedDataSlots, "nothing binds without a decision, publisher or not")
}

// TestTheCardNeverCarriesTheDatum.
//
// A card that showed the content would disclose it to everyone who can see the
// card, which is the very thing being asked about. The fields are structural:
// the receiving agent, the slot, the task.
func TestTheCardNeverCarriesTheDatum(t *testing.T) {
	sr := requestWithDataSlots(t, "req-fields", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-x"})
	sr.Spec.Task = "summarize it"

	fields := disclosureFields(sr, v1.DataSlotRequest{Slot: "logs", TagID: "ptt-x"})

	var labels []string
	for _, f := range fields {
		labels = append(labels, f.Label)
		assert.NotContains(t, f.Value, "ptt-x",
			"not even the tag id: it is the handle to the datum, and the decider does not need it")
	}
	assert.Equal(t, []string{"Receiving agent", "Input slot", "Task"}, labels)
}

// TestNoDeciderPublishesNothingAndStillDoesNotBind.
//
// pt_tag has no owner relation, so a card's deciders come from the OWNERS of
// the objects the tag was minted from. A datum whose sources nobody owns has
// no one who can rule on it — and the envelope contract refuses an
// approvers-scope prompt with an empty approver list anyway.
//
// The important half is the second assertion: nothing binds. A disclosure
// nobody can rule on must expire, never quietly proceed.
func TestNoDeciderPublishesNothingAndStillDoesNotBind(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-discloses": true}}
	sr := requestWithDataSlots(t, "req-noowner", v1.DataSlotRequest{Slot: "logs", TagID: "ptt-discloses"})
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-discloses": handoff.GradeRoute}, nil)
	var published int
	r.PublishInteraction = func(context.Context, string, string, channelevents.Envelope) error {
		published++
		return nil
	}
	r.TagSources = func(context.Context, authz.SessionRef, string) ([]string, error) {
		return []string{"note:n1"}, nil
	}
	r.ResourceOwners = func(context.Context, string, string) ([]string, error) { return nil, nil }

	reconcileOnce(t, r, "req-noowner")

	assert.Zero(t, published, "a card nobody could decide is not sent")
	got := getRequest(t, r.Client, "req-noowner")
	assert.Equal(t, v1.SubagentRequestPhaseAwaitingDisclosure, got.Status.Phase)
	assert.Empty(t, az.grantedDataSlots, "and it certainly does not bind")
}
