package subagentrequest

// Task 5 of the agent-builder delegation framework: buildChild's provisioning
// for mode=attended -- no `agent` Channel, both bindings stamped from the
// ROOT's own outbound binding, the LabelAttendedParent* watch relationship --
// and the "one attended child at a time" ceiling (spec §lifecycle).
//
// Its own file rather than more of controller_conversational_test.go: that
// file is task/chat's OWN regression guard, and attended's provisioning is
// deliberately NOT the same shape (no agent Channel, a stamped OutputChannel)
// -- keeping the two apart keeps each file's fixtures honest about which mode
// it is claiming something about.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/utils/clock"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
)

// attendedRoot is a channel-attached root session an attended child can be
// stamped from: a "local" binding (a human kind -- chregistry has no
// SessionCounterparty registration for it, unlike "agent"), carrying an
// established thread_ts so the join test below can prove it survives the
// copy onto the child.
func attendedRoot(t *testing.T, name string) *v1.AgentSession {
	t.Helper()
	root := parentSession(t, name, "demo-lead")
	root.Spec.InputChannel = &v1.ChannelBinding{
		Name: name + "-local", Kind: local.KindName, Key: "tui:" + name,
		Capabilities: []string{"text"},
		External:     map[string]string{"thread_ts": "root-thread-1"},
	}
	return root
}

// attendedParentClass is parentClass whose roster also PERMITS attended for
// demo-coder -- a plain []string roster (parentClass alone) permits
// single_turn only, per PermittedSubagentModes.
func attendedParentClass(t *testing.T) *v1.AgentClass {
	t.Helper()
	return parentClassWithModes(t, "demo-lead",
		map[string][]string{"demo-coder": {v1.SubagentModeAttended}},
		"demo-coder")
}

// TestReconcile_ModeAttended_ChildGetsRootBoundBindingsAndNoAgentChannel pins
// buildChild's whole attended shape in one place: owner-refed to the
// request (unchanged from every other mode), both InputChannel and
// OutputChannel copied from the ROOT's own binding (destination fields
// verbatim, NATSSubjectPrefix overridden to the child's own -- see
// attendedChildBinding's doc for why that field specifically must not be
// copied), the watch-relationship labels, and NO `agent` Channel.
func TestReconcile_ModeAttended_ChildGetsRootBoundBindingsAndNoAgentChannel(t *testing.T) {
	root := attendedRoot(t, "demo-root")
	r, c := newReconciler(t,
		attendedParentClass(t),
		childClass(t, "demo-coder"),
		root,
		requestWithMode(t, "req-attended", "demo-root", "demo-coder", v1.SubagentModeAttended),
	)

	reconcileOnce(t, r, "req-attended")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-attended"}, &got))
	require.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase, "a permitted attended request must not be refused")
	require.NotNil(t, got.Status.ChildRef)

	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-attended-child"}, &child))

	owner := metav1.GetControllerOf(&child)
	require.NotNil(t, owner, "the child must be owner-refed, same as every other mode")
	assert.Equal(t, "SubagentRequest", owner.Kind)
	assert.Equal(t, "req-attended", owner.Name)

	wantPrefix := channelevents.SubjectPrefix("ns", "req-attended-child")

	require.NotNil(t, child.Spec.InputChannel, "channel_interaction/subagent_conversation are both inactive with a nil InputChannel")
	assert.Equal(t, local.KindName, child.Spec.InputChannel.Kind, "a HUMAN kind, not the parent-facing agent kind")
	assert.Equal(t, "demo-root-local", child.Spec.InputChannel.Name, "the SAME Channel CR the root uses")
	assert.Equal(t, "root-thread-1", child.Spec.InputChannel.External["thread_ts"],
		"the root's destination metadata is copied so replies land in the SAME thread the person is reading")
	assert.Equal(t, wantPrefix, child.Spec.InputChannel.NATSSubjectPrefix,
		"the CHILD's own subject prefix -- copying the root's would have two runners share one session's NATS traffic")

	require.NotNil(t, child.Spec.OutputChannel, "outBindingFor prefers OutputChannel; respond_to_user is wired from it")
	assert.Equal(t, local.KindName, child.Spec.OutputChannel.Kind)
	assert.Equal(t, "demo-root-local", child.Spec.OutputChannel.Name)
	assert.Equal(t, wantPrefix, child.Spec.OutputChannel.NATSSubjectPrefix)

	assert.Equal(t, "ns", child.Labels[v1.LabelAttendedParentNamespace])
	assert.Equal(t, "demo-root", child.Labels[v1.LabelAttendedParentName])
	assert.Equal(t, "demo-root", child.Labels[v1.LabelDelegationRoot], "the tree-root label is unaffected by attended provisioning")

	assert.Empty(t, channelsIn(t, c), "an attended child's counterparty is a human, not its parent -- no `agent` Channel is provisioned")
}

// TestReconcile_ModeChat_Unchanged_NoOutputChannelOrAttendedLabels is the
// invariant this task must not disturb: task/chat's OWN provisioning
// (buildAgentChannel, InputChannel = the agent Channel) is untouched, and
// nothing attended-specific leaks onto a non-attended child.
func TestReconcile_ModeChat_Unchanged_NoOutputChannelOrAttendedLabels(t *testing.T) {
	r, c := newReconciler(t,
		conversationalParentClass(t),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-chat-unchanged", "demo-parent", "demo-coder", v1.SubagentModeChat),
	)

	reconcileOnce(t, r, "req-chat-unchanged")

	require.Len(t, channelsIn(t, c), 1, "a chat child still gets its agent Channel, unaffected by attended's own provisioning")

	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-chat-unchanged-child"}, &child))
	assert.NotNil(t, child.Spec.InputChannel, "chat is still channel-bound")
	assert.Nil(t, child.Spec.OutputChannel,
		"task/chat share ONE Channel for both directions; attended's OutputChannel stamp must not leak onto it")
	assert.Empty(t, child.Labels[v1.LabelAttendedParentNamespace])
	assert.Empty(t, child.Labels[v1.LabelAttendedParentName])
}

// TestReconcile_ModeAttended_BuiltChildGetsRespondToUserNotAskParent is the
// cross-task join: Task 4 shipped the gates that offer respond_to_user and
// withhold ask_parent from an attended child, keyed on the SHAPE of its
// bindings (isAttendedChild / respondToUserSkip / IsDelegatedChild); this
// task is what has to PRODUCE that shape. Run the real capabilities against
// the child buildChild actually built -- not a hand-built fixture -- so a
// regression on either side of the seam (Task 4's gate, or Task 5's
// provisioning) fails this test.
func TestReconcile_ModeAttended_BuiltChildGetsRespondToUserNotAskParent(t *testing.T) {
	root := attendedRoot(t, "demo-root")
	r, c := newReconciler(t,
		attendedParentClass(t),
		childClass(t, "demo-coder"),
		root,
		requestWithMode(t, "req-join", "demo-root", "demo-coder", v1.SubagentModeAttended),
	)

	reconcileOnce(t, r, "req-join")

	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-join-child"}, &child))
	require.NotNil(t, child.Spec.InputChannel, "precondition: the join has nothing to prove without a real binding")
	require.NotNil(t, child.Spec.OutputChannel)

	subagentConv, ok := capability.Lookup("subagent_conversation")
	require.True(t, ok, "the capability must be registered")
	channelInt, ok := capability.Lookup("channel_interaction")
	require.True(t, ok, "the capability must be registered")

	octx := capability.OfferContext{
		Ctx:        context.Background(),
		Session:    &child,
		Binding:    child.Spec.InputChannel,
		OutBinding: child.Spec.OutputChannel,
		Env: capability.RunnerEnv{
			NATSPublish:   func(context.Context, string, []byte) error { return nil },
			SubjectPrefix: child.Spec.InputChannel.NATSSubjectPrefix,
			InboundCh:     make(chan struct{}, 1),
			IdleTTL:       time.Minute,
			Clock:         clock.RealClock{},
			// AskParent deliberately left nil: an attended child must never
			// reach ask_parent's "no way to record a question" skip either --
			// the tool must be entirely ABSENT, not merely unavailable.
		},
	}

	convTools, convSkip := subagentConv.Offer(octx)
	intTools, intSkip := channelInt.Offer(octx)
	assert.Nil(t, convSkip, "an attended child's ask_parent absence is a normal inactive state, not a granted-but-unavailable skip")
	assert.Nil(t, intSkip, "nothing is withheld once both bindings reach a person")

	all := append(append([]tool.Tool{}, convTools...), intTools...)
	var names []string
	for _, tl := range all {
		names = append(names, tl.Name())
	}
	assert.NotContains(t, names, "ask_parent",
		"Task 5's InputChannel must be a HUMAN kind, or Task 4's IsDelegatedChild/respondToUserSkip gate stops withholding ask_parent")
	assert.Contains(t, names, "respond_to_user",
		"Task 5's OutputChannel must be a channel a person reads, or Task 4's gate stops offering respond_to_user -- the child would be mute")
}

// TestReconcile_AttendedMode_RootWithNoChannelIsDenied pins
// ReasonAttendedRootNotChannelAttached: a root with no channel binding at
// all has no human to hand an attended child, so the request is refused
// rather than silently building a mute one.
func TestReconcile_AttendedMode_RootWithNoChannelIsDenied(t *testing.T) {
	r, c := newReconciler(t,
		attendedParentClass(t),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-root", "demo-lead"), // no InputChannel/OutputChannel
		requestWithMode(t, "req-nochan", "demo-root", "demo-coder", v1.SubagentModeAttended),
	)

	got := denialOf(t, r, c, "req-nochan")
	assert.Equal(t, ReasonAttendedRootNotChannelAttached, got.Status.FailureReason)
}

// TestReconcile_AttendedMode_SecondConcurrentRequestIsDeniedWhileFirstIsLive
// pins the "one attended child at a time" ceiling (spec §lifecycle): a
// second attended request against a tree that already has a LIVE (non-
// terminal) attended child is denied outright, and no second child is ever
// created.
func TestReconcile_AttendedMode_SecondConcurrentRequestIsDeniedWhileFirstIsLive(t *testing.T) {
	root := attendedRoot(t, "demo-root")
	liveChild := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-first-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder", Prompt: v1.PromptSource{Inline: "first test"}},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning},
	}
	r, c := newReconciler(t,
		attendedParentClass(t),
		childClass(t, "demo-coder"),
		root, liveChild,
		requestWithMode(t, "req-second", "demo-root", "demo-coder", v1.SubagentModeAttended),
	)

	reconcileOnce(t, r, "req-second")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-second"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, ReasonAttendedChildInProgress, got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "already running")
	assert.Nil(t, got.Status.ChildRef, "a denied delegation must not name a child")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 2, "no second child may be created: only the root and the pre-existing live attended child")
}

// TestReconcile_AttendedMode_TerminalPriorChildDoesNotBlockANewOne is the
// refusing check's complement: a PREVIOUS attended child that already
// resolved (Succeeded/Failed) is no longer "in progress" and must not block
// a fresh attended delegation from the same tree.
func TestReconcile_AttendedMode_TerminalPriorChildDoesNotBlockANewOne(t *testing.T) {
	root := attendedRoot(t, "demo-root")
	priorChild := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-prior-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder", Prompt: v1.PromptSource{Inline: "earlier test"}},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseSucceeded},
	}
	r, c := newReconciler(t,
		attendedParentClass(t),
		childClass(t, "demo-coder"),
		root, priorChild,
		requestWithMode(t, "req-fresh", "demo-root", "demo-coder", v1.SubagentModeAttended),
	)

	reconcileOnce(t, r, "req-fresh")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-fresh"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"a RESOLVED prior attended child is no longer in progress and must not block a new one")
	require.NotNil(t, got.Status.ChildRef)
}

// TestReconcile_AttendedMode_RetryAfterStatusWriteFailureDoesNotDenyItself
// mirrors TestReconcile_ChildCreatedButFinalStatusWriteFailsDoesNotDenyOnRetry
// for the attended ceiling: the request's OWN child, already created on an
// earlier pass whose final status write failed (ChildRef stays nil, so
// Reconcile retries the full create path including this check), must not
// count -- let alone deny -- against itself.
func TestReconcile_AttendedMode_RetryAfterStatusWriteFailureDoesNotDenyItself(t *testing.T) {
	root := attendedRoot(t, "demo-root")
	ownChild := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-retryatt-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder", Prompt: v1.PromptSource{Inline: "do the thing"}},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning},
	}
	r, c := newReconciler(t,
		attendedParentClass(t),
		childClass(t, "demo-coder"),
		root, ownChild,
		requestWithMode(t, "req-retryatt", "demo-root", "demo-coder", v1.SubagentModeAttended),
	)

	reconcileOnce(t, r, "req-retryatt")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-retryatt"}, &got))
	assert.NotEqual(t, v1.SubagentRequestPhaseDenied, got.Status.Phase,
		"a retry must not deny a delegation whose own attended child it already created")
	require.NotNil(t, got.Status.ChildRef)
	assert.Equal(t, "req-retryatt-child", got.Status.ChildRef.Name)
}
