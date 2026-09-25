package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The INITIATIVE half of SubagentRequestSpec.Mode: a `task` child may ask its
// parent something without being drivable by anyone, while a `chat` child may
// be addressed freely. Every test here turns on one axis — who is speaking, and
// in what mode the child was delegated.

// subagentRequestFor builds the delegation record the gate reads, in the given
// mode, for the child delegationPair creates.
func subagentRequestFor(name, parent, mode string) *spiceboxv1alpha1.SubagentRequest {
	return &spiceboxv1alpha1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.SubagentRequestSpec{
			Parent: spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: parent},
			Class:  "ac1",
			Task:   "do the thing",
			Mode:   mode,
		},
	}
}

// delegatedChildIn returns the pair Channel and the child of one delegation in
// the given mode, with the child carrying spec.parent and the controller owner
// reference that links it to its SubagentRequest — the two facts the gate needs
// and the SubagentRequest controller writes together.
//
// ownedBy is the request name; passing "" drops the owner reference, which is
// the broken-link shape the fail-closed arm exists for.
func delegatedChildIn(t *testing.T, parent, child, ownedBy string) (*spiceboxv1alpha1.Channel, *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	ch, sess := delegationPair(t, parent, child)
	sess.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: parent}
	if ownedBy != "" {
		yes := true
		sess.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
			Kind:       "SubagentRequest",
			Name:       ownedBy,
			Controller: &yes,
		}}
	}
	return ch, sess
}

// humanTurnInto delivers a message a PERSON typed into the session bound to ch.
// ExternalIDs carries a per-user identity and AuthzSubject is empty, which is
// exactly what Deliver reads to decide the acting subject is a human — the same
// shape a browser session view produces (HandleViewMessage).
func humanTurnInto(t *testing.T, p *Pipeline, ch *spiceboxv1alpha1.Channel, key, text string) channelkinds.InboundDecision {
	t.Helper()
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ChannelKey:  key,
		MessageText: text,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U-LEAD", Email: "lead@example.test"},
	})
	require.NoError(t, err, "Deliver")
	return dec
}

// Brief test 5: a person cannot open a turn into a `task` child. The refusal is
// visible to them and the message never reaches the agent.
func TestDeliver_TaskChild_RefusesATurnAPersonOpened(t *testing.T) {
	ch, child := delegatedChildIn(t, "parent-1", "child-1", "req1")
	p, az, mem, _, _ := newPipeline(t, ch, child, subagentRequestFor("req1", "parent-1", spiceboxv1alpha1.SubagentModeTask))

	dec := humanTurnInto(t, p, ch, "agent:child-1", "actually, do it differently")

	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)
	assert.Empty(t, mem.appends, "a refused turn must never reach the child's transcript")
	assert.Zero(t, az.checkCalls,
		"the gate is a property of the delegation, not of the sender's standing, so it refuses before the check")

	require.False(t, dec.Notice.IsSuppressed(),
		"the person is looking at a live agent that just ignored them; silence is the one unacceptable answer")
	assert.Equal(t, categories.SubagentNotAddressable, dec.Notice.Category())
	assert.Contains(t, dec.Notice.Args().NextStep, "parent-1",
		"the remedy is to address the delegating agent, so the copy has to name it")
}

// Brief test 6, and the regression guard the resumable loop depends on: the
// PARENT's reply is admitted to a `task` child. It arrives with an
// "agentsession:" acting subject and no per-user identity, so it is not a person
// opening a turn — if the gate caught it, a task child could never be answered
// and the whole conversational delegation would break on the first reply.
func TestDeliver_TaskChild_AdmitsTheParentsReply(t *testing.T) {
	ch, child := delegatedChildIn(t, "parent-1", "child-1", "req1")
	rootCh := newSlackChannel("root-slack", "")
	root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
	p, az, mem, _, _ := newPipeline(t, ch, child, rootCh, root,
		subagentRequestFor("req1", "parent-1", spiceboxv1alpha1.SubagentModeTask))
	seedDelegation(t, az, "default/parent-1", "default/child-1")

	env := agentMessageSendEnvelope(t, "default", "parent-1", "default", "child-1", "use the staging cluster")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env), "HandleAgentMessageSend")

	require.Len(t, mem.appends, 1, "the parent's answer must reach its task child")
	assert.Equal(t, "child-1", mem.appends[0].name)
	require.NotEmpty(t, mem.appends[0].turn.Content)
	assert.Equal(t, "use the staging cluster", mem.appends[0].turn.Content[0].Text)
	assert.Equal(t, 1, az.converseCalls,
		"the agent-to-agent check still runs: the gate skips a non-human sender, it does not authorize one")
}

// A `chat` child declares a full back-and-forth up front, so a person may
// address it. This is what keeps the gate a MODE distinction rather than a
// blanket refusal of every conversational child.
func TestDeliver_ChatChild_AdmitsATurnAPersonOpened(t *testing.T) {
	ch, child := delegatedChildIn(t, "parent-1", "child-1", "req1")
	p, az, mem, _, _ := newPipeline(t, ch, child, subagentRequestFor("req1", "parent-1", spiceboxv1alpha1.SubagentModeChat))

	dec := humanTurnInto(t, p, ch, "agent:child-1", "one more thing")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, 1, len(mem.appends), "a chat child may be addressed, so the turn is delivered")
	assert.Positive(t, az.checkCalls, "admitted by the gate still means checked by the authorization gate")
}

// An ordinary session nobody delegated to has no delegation terms, and must not
// be gated on the ones it does not have. Without this arm the gate would refuse
// every human message in the cluster.
func TestDeliver_UndelegatedSession_IsNotGatedOnDelegationTerms(t *testing.T) {
	ch := newChannel("c1")
	sess := existingSession(t, "s1", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, _, mem, _, _ := newPipeline(t, ch, sess)

	dec := humanTurnInto(t, p, ch, "thread:C1:1", "hello")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Len(t, mem.appends, 1)
}

// Fail-closed: a session that says it was delegated but whose delegation cannot
// be read is refused. What could not be established is precisely the thing that
// would have permitted the turn, so admitting it would decide the security
// question by guessing.
func TestDeliver_DelegatedChildWithUnreadableTerms_RefusesLoudly(t *testing.T) {
	ch, child := delegatedChildIn(t, "parent-1", "child-1", "") // no owner reference
	p, _, mem, _, _ := newPipeline(t, ch, child)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ChannelKey:  "agent:child-1",
		MessageText: "hello?",
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U-LEAD", Email: "lead@example.test"},
	})

	require.Error(t, err, "an unreadable delegation must not be swallowed")
	assert.Equal(t, channelkinds.OutcomeInternalError, dec.Outcome)
	assert.Empty(t, mem.appends)
}

// PermitsHumanInitiative is the whole mode table for this half of spec.mode, so
// its answers are pinned directly as well as through Deliver.
func TestPermitsHumanInitiative_PerMode(t *testing.T) {
	cases := []struct {
		mode string
		want bool
		why  string
	}{
		{spiceboxv1alpha1.SubagentModeChat, true, "a full back-and-forth is what chat declares"},
		{spiceboxv1alpha1.SubagentModeTask, false, "task may ask; it may not be driven"},
		{spiceboxv1alpha1.SubagentModeSingleTurn, false, "headless: there is no surface to address"},
		{"", false, "empty resolves to single_turn"},
		{"nonsense", false, "an unrecognized mode grants nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.mode+": "+tc.why, func(t *testing.T) {
			sr := &spiceboxv1alpha1.SubagentRequest{Spec: spiceboxv1alpha1.SubagentRequestSpec{Mode: tc.mode}}
			assert.Equal(t, tc.want, sr.PermitsHumanInitiative())
		})
	}
}

// OwningSubagentRequest is what links a child to its terms, and its three
// outcomes are kept apart on purpose: reading "no owner reference" as "not
// delegated" would drop every term of the delegation silently.
func TestOwningSubagentRequest_Outcomes(t *testing.T) {
	sr := subagentRequestFor("req1", "parent-1", spiceboxv1alpha1.SubagentModeTask)
	_, delegated := delegatedChildIn(t, "parent-1", "child-1", "req1")
	_, orphan := delegatedChildIn(t, "parent-1", "child-2", "")
	plain := existingSession(t, "s1", "", spiceboxv1alpha1.AgentSessionPhaseRunning)

	_, _, _, _, cli := newPipeline(t, sr, delegated, orphan, plain)

	t.Run("a delegated child resolves its request", func(t *testing.T) {
		got, err := spiceboxv1alpha1.OwningSubagentRequest(context.Background(), cli, delegated)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "req1", got.Name)
	})
	t.Run("an undelegated session resolves to nothing, and that is not an error", func(t *testing.T) {
		got, err := spiceboxv1alpha1.OwningSubagentRequest(context.Background(), cli, plain)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
	t.Run("a delegated child with no owner reference is an error, never 'undelegated'", func(t *testing.T) {
		got, err := spiceboxv1alpha1.OwningSubagentRequest(context.Background(), cli, orphan)
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "child-2")
	})
	t.Run("a request that no longer exists is an error, not an ungated child", func(t *testing.T) {
		_, child := delegatedChildIn(t, "parent-1", "child-3", "no-such-request")
		got, err := spiceboxv1alpha1.OwningSubagentRequest(context.Background(), cli, child)
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "no-such-request")
	})
}
