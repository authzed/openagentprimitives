package subagentrequest

// The ASK: publishing the card, and acting on the answer.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
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

// TestALapsedWindowRetiresEachCardItAsked.
//
// channelsd parks the parent on every disclosure card and clears that only on a
// decision or a timeout report. Failing the delegation at the window is the
// controller's half; the other half is telling channelsd the cards lapsed, or
// the parent stays marked as awaiting a decision nobody can make. A click on
// the expired card used to clear it, but a card that expires on its way out is
// never delivered, so there is nothing to click.
func TestALapsedWindowRetiresEachCardItAsked(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-a": true, "ptt-b": true}}
	sr := requestWithDataSlots(t, "req-lapsed",
		v1.DataSlotRequest{Slot: "logs", TagID: "ptt-a"},
		v1.DataSlotRequest{Slot: "notes", TagID: "ptt-b"})
	longAgo := metav1.NewTime(time.Now().Add(-24 * time.Hour))
	sr.Status.DisclosureRequestedAt = &longAgo
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-a": handoff.GradeRoute, "ptt-b": handoff.GradeRoute}, nil)
	var published []channelevents.Envelope
	var targets []string
	r.PublishInteraction = func(_ context.Context, ns, name string, env channelevents.Envelope) error {
		published = append(published, env)
		targets = append(targets, ns+"/"+name)
		return nil
	}

	reconcileOnce(t, r, "req-lapsed")

	got := getRequest(t, r.Client, "req-lapsed")
	require.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase, "the window still ends the delegation")
	require.Len(t, published, 2, "one expiry report per card that was asked")
	refs := make([]string, 0, len(published))
	for i, env := range published {
		assert.Equal(t, channelevents.KindInteractionApplied, env.Kind)
		assert.Equal(t, sr.Spec.Parent.Namespace+"/"+sr.Spec.Parent.Name, targets[i],
			"reported on the PARENT's subject, where the card parked it")
		var pl channelevents.InteractionAppliedPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		assert.Equal(t, categories.DataSlotDisclosure, pl.Category)
		assert.Equal(t, channelevents.OutcomeExpired, pl.Outcome, "a lapsed window never approves")
		refs = append(refs, pl.RequestRef)
	}
	assert.ElementsMatch(t, []string{
		disclosureRequestRef(sr, "logs"), disclosureRequestRef(sr, "notes"),
	}, refs, "each report names the card it retires")
}

// lapsedRequest builds a two-slot request whose disclosure window opened
// sinceAsked ago, parked awaiting the decision, with the reconciler's clock
// pinned so the window and the retire grace are measured exactly.
func lapsedRequest(t *testing.T, name string, sinceAsked time.Duration) (*Reconciler, *v1.SubagentRequest) {
	t.Helper()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-a": true, "ptt-b": true}}
	sr := requestWithDataSlots(t, name,
		v1.DataSlotRequest{Slot: "logs", TagID: "ptt-a"},
		v1.DataSlotRequest{Slot: "notes", TagID: "ptt-b"})
	asked := metav1.NewTime(now.Add(-sinceAsked))
	sr.Status.DisclosureRequestedAt = &asked
	sr.Status.Phase = v1.SubagentRequestPhaseAwaitingDisclosure
	r, _ := reconcilerWithAuthz(t, az, sr)
	r.Now = func() time.Time { return now }
	r.Grader = gradeFunc(map[string]handoff.Grade{"ptt-a": handoff.GradeRoute, "ptt-b": handoff.GradeRoute}, nil)
	return r, sr
}

// TestALapsedWindowHoldsTheVerdictUntilItsCardsAreRetired.
//
// A Failed request is never reconciled again, so the expiry reports have one
// chance: before the verdict is written. If a report cannot be published and
// the request goes terminal anyway, the parent stays marked as awaiting a card
// nobody can click, with nothing left to clear it. So a failed report holds the
// verdict back and retries — but only for a bounded grace, because a bus that
// never recovers must not keep the delegation from ever failing.
func TestALapsedWindowHoldsTheVerdictUntilItsCardsAreRetired(t *testing.T) {
	cases := []struct {
		name        string
		sinceAsked  time.Duration
		publishErr  error
		wantPhase   string
		wantRequeue time.Duration
	}{
		{
			name:        "report publish fails inside the grace: still AwaitingDisclosure, requeued to retry",
			sinceAsked:  disclosureWaitWindow + time.Second,
			publishErr:  errors.New("nats: connection closed"),
			wantPhase:   v1.SubagentRequestPhaseAwaitingDisclosure,
			wantRequeue: disclosurePollInterval,
		},
		{
			name:        "report publish fails at the last instant of the grace: still held back",
			sinceAsked:  disclosureWaitWindow + disclosureRetireGrace,
			publishErr:  errors.New("nats: connection closed"),
			wantPhase:   v1.SubagentRequestPhaseAwaitingDisclosure,
			wantRequeue: disclosurePollInterval,
		},
		{
			name:       "report publish still failing past the grace: Failed anyway, no requeue",
			sinceAsked: disclosureWaitWindow + disclosureRetireGrace + time.Second,
			publishErr: errors.New("nats: connection closed"),
			wantPhase:  v1.SubagentRequestPhaseFailed,
		},
		{
			name:       "report published: Failed at once, no requeue",
			sinceAsked: disclosureWaitWindow + time.Second,
			wantPhase:  v1.SubagentRequestPhaseFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := lapsedRequest(t, "req-hold", tc.sinceAsked)
			var attempts int
			r.PublishInteraction = func(context.Context, string, string, channelevents.Envelope) error {
				attempts++
				return tc.publishErr
			}

			res := reconcileOnce(t, r, "req-hold")

			got := getRequest(t, r.Client, "req-hold")
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			assert.Equal(t, tc.wantRequeue, res.RequeueAfter)
			assert.Equal(t, 2, attempts, "every slot's report is attempted, even after one fails")
			if tc.wantPhase == v1.SubagentRequestPhaseFailed {
				assert.Contains(t, got.Status.Determination, "none arrived within",
					"a retire failure never changes the stated reason")
			}
		})
	}
}

// TestAHeldVerdictLandsOnceTheReportsGetOut is the recovery half: the pass
// after the bus comes back retires every card and writes the verdict. Every
// slot is reported again, including one whose report got through the first
// time, because channelsd clears an entry once and ignores the repeat.
func TestAHeldVerdictLandsOnceTheReportsGetOut(t *testing.T) {
	r, sr := lapsedRequest(t, "req-recover", disclosureWaitWindow+time.Second)
	var busUp bool
	var retired []string
	r.PublishInteraction = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		var pl channelevents.InteractionAppliedPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		if !busUp && pl.RequestRef == disclosureRequestRef(sr, "notes") {
			return errors.New("nats: connection closed")
		}
		if busUp {
			retired = append(retired, pl.RequestRef)
		}
		return nil
	}

	reconcileOnce(t, r, "req-recover")
	require.Equal(t, v1.SubagentRequestPhaseAwaitingDisclosure, getRequest(t, r.Client, "req-recover").Status.Phase,
		"one slot's report failed, so the verdict is held back")

	busUp = true
	res := reconcileOnce(t, r, "req-recover")

	assert.Equal(t, v1.SubagentRequestPhaseFailed, getRequest(t, r.Client, "req-recover").Status.Phase)
	assert.Zero(t, res.RequeueAfter)
	assert.ElementsMatch(t, []string{
		disclosureRequestRef(sr, "logs"), disclosureRequestRef(sr, "notes"),
	}, retired, "the retry reports every card, not only the one that failed")
}
