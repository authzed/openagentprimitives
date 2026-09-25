package pipeline

// The credit RULE is pinned in agentwake_test.go against the pure functions.
// These tests pin the WIRING, which is where the bound actually lives or dies:
// the rule can be perfectly correct and the feature still unbounded if nobody
// spends, if the spend is not persisted, or if a failed spend still wakes.
//
// That is not hypothetical. This package carried DecideWake, RefillOnHumanTurn
// and SpendForAgentWake — with tests — and ZERO non-test callers, while the
// listener admitted cross-agent messages. Every pure-function test passed and
// the budget bounded nothing.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// alwaysFailStatusPatchClient fails every status patch, to prove the two
// failure directions differ by author. It leaves ordinary patches alone so a
// test can still observe the annotation path.
type alwaysFailStatusPatchClient struct {
	client.Client
}

func (c *alwaysFailStatusPatchClient) Status() client.SubResourceWriter {
	// Embed the real writer so methods this test does not care about keep
	// working (and so a controller-runtime bump that adds one does not break
	// the file).
	return failingStatusWriter{SubResourceWriter: c.Client.Status()}
}

type failingStatusWriter struct {
	client.SubResourceWriter
}

func (failingStatusWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return apierrors.NewInternalError(fmt.Errorf("status patch refused by the test"))
}

// wakeCreditSession builds a session carrying the given credit (nil for
// "never considered").
func wakeCreditSession(t *testing.T, name string, credit *int) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", ResourceVersion: "10",
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:           spiceboxv1alpha1.AgentSessionPhaseIdle,
			AgentWakeCredit: credit,
		},
	}
}

func newCreditPipeline(t *testing.T, sess *spiceboxv1alpha1.AgentSession) (*Pipeline, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(newWakeTestScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(sess).
		Build()
	return &Pipeline{K8s: c}, c
}

func storedCredit(t *testing.T, c client.Client, name string) *int {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &got))
	return got.Status.AgentWakeCredit
}

// TestAnAgentMessageSpendsAndPersists is the wiring the whole track rests on:
// the spend must reach the API server, because the next message is handled by
// a different Deliver call that reads it back. A spend held only in memory
// makes every message the first one.
func TestAnAgentMessageSpendsAndPersists(t *testing.T) {
	two := 2
	sess := wakeCreditSession(t, "spender", &two)
	p, c := newCreditPipeline(t, sess)

	wake := p.decideAndRecordWake(context.Background(), sess,
		channelkinds.InboundEvent{FromAgent: true, WakeBudget: 3})

	assert.True(t, wake, "credit remained, so this message may drive a turn")
	require.NotNil(t, storedCredit(t, c, "spender"))
	assert.Equal(t, 1, *storedCredit(t, c, "spender"),
		"the spend must be durable — the next inbound is a different Deliver call that reads it back")
}

// TestAnExhaustedBudgetRefusesTheWake: the message is already appended by the
// time this runs, so refusing here is "sees it, will not answer yet" — not a
// dropped message.
func TestAnExhaustedBudgetRefusesTheWake(t *testing.T) {
	zero := 0
	sess := wakeCreditSession(t, "spent", &zero)
	p, _ := newCreditPipeline(t, sess)

	wake := p.decideAndRecordWake(context.Background(), sess,
		channelkinds.InboundEvent{FromAgent: true, WakeBudget: 3})

	assert.False(t, wake,
		"out of credit, an agent message must not drive a turn — it was appended, and a person's next message resumes the exchange")
}

// TestAHumanTurnRefillsThroughTheWiring — and does so from zero, which is the
// state a bounded thread is actually in when the person speaks again.
func TestAHumanTurnRefillsThroughTheWiring(t *testing.T) {
	zero := 0
	sess := wakeCreditSession(t, "refilled", &zero)
	p, c := newCreditPipeline(t, sess)

	wake := p.decideAndRecordWake(context.Background(), sess,
		channelkinds.InboundEvent{FromAgent: false, WakeBudget: 3})

	assert.True(t, wake, "a person is never rate-limited by the agent budget")
	require.NotNil(t, storedCredit(t, c, "refilled"))
	assert.Equal(t, 3, *storedCredit(t, c, "refilled"),
		"the human turn must restore the budget durably, or the pause never ends")
}

// TestAnOrdinarySessionIsNeverWritten is the churn pin.
//
// Every human turn of every session in the cluster passes through here. A
// session with no cross-agent configuration computes the value it already has,
// and writing zero over nil would be a status write per message, cluster-wide,
// recording nothing — plus a reconcile per write for anything watching status.
func TestAnOrdinarySessionIsNeverWritten(t *testing.T) {
	sess := wakeCreditSession(t, "ordinary", nil)
	p, c := newCreditPipeline(t, sess)

	wake := p.decideAndRecordWake(context.Background(), sess,
		channelkinds.InboundEvent{FromAgent: false, WakeBudget: 0})

	assert.True(t, wake)
	assert.Nil(t, storedCredit(t, c, "ordinary"),
		"nil and a written zero are the same state; writing one over the other is churn recording nothing")
}

// TestAFailedSpendRefusesTheWake, and TestAFailedRefillStillWakes below, are
// the pair. The SAME write failure produces opposite answers depending on who
// sent the message, and that asymmetry is deliberate:
//
//   - An unrecordable SPEND that still woke would repeat forever — every
//     retry finds the same un-decremented credit — which is the unbounded loop
//     itself, produced by an error path.
//   - An unrecordable REFILL that refused to wake would leave a PERSON
//     unanswered because of bookkeeping. The budget simply stays where it was,
//     which is the conservative direction.
func TestAFailedSpendRefusesTheWake(t *testing.T) {
	two := 2
	sess := wakeCreditSession(t, "unrecordable", &two)
	base := fake.NewClientBuilder().
		WithScheme(newWakeTestScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(sess).
		Build()
	p := &Pipeline{K8s: &alwaysFailStatusPatchClient{Client: base}}

	wake := p.decideAndRecordWake(context.Background(), sess,
		channelkinds.InboundEvent{FromAgent: true, WakeBudget: 3})

	assert.False(t, wake,
		"a spend that cannot be recorded must not wake: every retry would find the same credit, and the error path becomes the loop")
}

func TestAFailedRefillStillWakes(t *testing.T) {
	zero := 0
	sess := wakeCreditSession(t, "unrecordable-human", &zero)
	base := fake.NewClientBuilder().
		WithScheme(newWakeTestScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(sess).
		Build()
	p := &Pipeline{K8s: &alwaysFailStatusPatchClient{Client: base}}

	wake := p.decideAndRecordWake(context.Background(), sess,
		channelkinds.InboundEvent{FromAgent: false, WakeBudget: 3})

	assert.True(t, wake,
		"a person must not go unanswered because a status write failed; the budget keeps its old value, which is the safe direction")
}

// TestBothWakeMechanismsAnswerToOneDecision is the pin that the earlier
// wiring bug taught: it is not enough for the decision to be correct, every
// mechanism that can start a turn has to be behind it.
//
// The annotation respawns an EXITED runner and the NATS wakeup nudges a LIVE
// one. Gating only the annotation reads as working — a parked session
// correctly declines — while a running session, the one actually able to
// sustain a loop, keeps being driven.
func TestBothWakeMechanismsAnswerToOneDecision(t *testing.T) {
	t.Run("out of credit: neither the annotation nor the NATS wakeup fires", func(t *testing.T) {
		zero := 0
		sess := wakeCreditSession(t, "no-drive", &zero)
		p, c := newCreditPipeline(t, sess)
		nats := &fakeNATS{}
		p.NATS = nats
		p.Now = func() time.Time { return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) }

		require.NoError(t, p.applyWake(context.Background(), sess,
			channelkinds.InboundEvent{FromAgent: true, WakeBudget: 3}))

		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "no-drive"}, &got))
		assert.Empty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
			"an exhausted budget must not respawn the runner")
		assert.Empty(t, nats.subjects,
			"nor nudge a live one — a wakeup on the bus starts a turn just as surely as the annotation does")
	})

	t.Run("with credit: both fire", func(t *testing.T) {
		two := 2
		sess := wakeCreditSession(t, "drive", &two)
		p, c := newCreditPipeline(t, sess)
		nats := &fakeNATS{}
		p.NATS = nats
		p.Now = func() time.Time { return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) }

		require.NoError(t, p.applyWake(context.Background(), sess,
			channelkinds.InboundEvent{FromAgent: true, WakeBudget: 3}))

		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "drive"}, &got))
		assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
			"with credit the parked runner must be respawned exactly as before")
		assert.Len(t, nats.subjects, 1,
			"and the live-runner nudge must still be published")
	})
}

// TestABudgetlessKindCannotGrantWakes pins the cross-kind default.
//
// Every channel kind other than slack leaves WakeBudget zero, and FromAgent
// false, because none of them classify bot authorship. That combination must
// behave exactly as it did before this existed: humans through, no writes, no
// agent wakes possible.
func TestABudgetlessKindCannotGrantWakes(t *testing.T) {
	sess := wakeCreditSession(t, "other-kind", nil)
	p, c := newCreditPipeline(t, sess)

	assert.True(t, p.decideAndRecordWake(context.Background(), sess, channelkinds.InboundEvent{}),
		"a kind that knows nothing about cross-agent participation must deliver human turns unchanged")
	assert.Nil(t, storedCredit(t, c, "other-kind"))

	// And if such a kind ever DID mark a message as agent-authored without
	// declaring a budget, the answer is no wake — never unlimited.
	assert.False(t, p.decideAndRecordWake(context.Background(), sess,
		channelkinds.InboundEvent{FromAgent: true}),
		"an undeclared budget must read as OFF, not as no ceiling")
}
