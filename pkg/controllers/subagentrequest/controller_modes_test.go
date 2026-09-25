package subagentrequest

// The delegation-mode ceiling: which SubagentRequest modes a parent's roster
// permits for a given child, and what happens to a request that asks for one
// it does not have.
//
// Its own file rather than more of controller_conversational_test.go: that file
// asks what a conversational child is PROVISIONED with once the mode is
// granted; this one asks whether the mode is granted at all. The two failures
// look nothing alike -- a provisioning bug leaves a wrongly-shaped child, a
// ceiling bug leaves a child that should never have existed -- and keeping them
// apart keeps each file's fixtures honest about what it is claiming.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
)

// parentClassWithModes is parentClass plus a spec.subagentModes ceiling. The
// roster and the ceiling are separate arguments because they are separate
// declarations: membership permits single_turn, and the map is what widens a
// member beyond it.
func parentClassWithModes(t *testing.T, name string, modes map[string][]string, roster ...string) *v1.AgentClass {
	t.Helper()
	pc := parentClass(t, name, roster...)
	pc.Spec.SubagentModes = modes
	return pc
}

// denialOf reconciles once and returns the request, requiring that it was
// denied. Every ceiling refusal below asserts the same three things about the
// aftermath -- denied, no child, no Channel -- so they are checked here once
// rather than restated per test, where one of them would eventually be dropped.
func denialOf(t *testing.T, r *Reconciler, c client.Client, name string) v1.SubagentRequest {
	t.Helper()
	reconcileOnce(t, r, name)

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, &got))
	require.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase,
		"a mode the roster does not permit is a policy refusal, not a retryable failure")

	assert.Nil(t, got.Status.ChildRef, "a denied delegation must not name a child")
	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1, "only the parent session may exist after a refusal")
	assert.Empty(t, channelsIn(t, c), "a refused mode must not leave the Channel it was asking for")
	return got
}

func TestReconcile_OldShapeRoster_NoModeAsked_YieldsSingleTurnAndDelegatesNormally(t *testing.T) {
	// A roster exactly as Track 1a wrote them: a []string and nothing else.
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		request(t, "req-old-default", "demo-parent", "demo-coder"),
	)

	reconcileOnce(t, r, "req-old-default")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-old-default"}, &got))
	require.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"a roster that predates spec.subagentModes must keep delegating")
	require.NotNil(t, got.Status.ChildRef)

	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: "ns", Name: "req-old-default-child",
	}, &child))
	assert.Nil(t, child.Spec.InputChannel, "the child stays headless: an old-shape roster means single_turn")
	assert.Empty(t, channelsIn(t, c), "no Channel may be provisioned for a single_turn child")
}

func TestReconcile_OldShapeRoster_AskingChat_IsDeniedNamingTheModeAndWhatIsAllowed(t *testing.T) {
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-old-chat", "demo-parent", "demo-coder", v1.SubagentModeChat),
	)

	got := denialOf(t, r, c, "req-old-chat")

	assert.Equal(t, "SubagentModeNotPermitted", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, v1.SubagentModeChat, "the refusal must name the mode that was asked for")
	assert.Contains(t, got.Status.Determination, "demo-coder", "the refusal must name the subagent it was asked for")
	assert.Contains(t, got.Status.Determination, v1.SubagentModeSingleTurn, "the refusal must name what the roster does allow")
}

func TestReconcile_RosterPermittingChat_AskingChat_ProvisionsTheConversationalChild(t *testing.T) {
	r, c := newReconciler(t,
		parentClassWithModes(t, "demo-lead", map[string][]string{"demo-coder": {v1.SubagentModeChat}}, "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-chat", "demo-parent", "demo-coder", v1.SubagentModeChat),
	)

	reconcileOnce(t, r, "req-chat")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-chat"}, &got))
	require.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase, "a permitted mode must not be refused")

	require.Len(t, channelsIn(t, c), 1, "a chat child gets its agent Channel")
	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: "ns", Name: "req-chat-child",
	}, &child))
	require.NotNil(t, child.Spec.InputChannel, "a chat child must be bound to its Channel")
	assert.Equal(t, agent.KindName, child.Spec.InputChannel.Kind)
}

func TestReconcile_RosterPermittingChat_AskingSingleTurn_IsAllowedAndStaysHeadless(t *testing.T) {
	// Narrowing: the ceiling is chat, and the agent may pick anything at or
	// under it. single_turn is under it, so this must not be refused -- and it
	// must actually run as single_turn, not be promoted to the ceiling.
	r, c := newReconciler(t,
		parentClassWithModes(t, "demo-lead", map[string][]string{"demo-coder": {v1.SubagentModeChat}}, "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-narrow", "demo-parent", "demo-coder", v1.SubagentModeSingleTurn),
	)

	reconcileOnce(t, r, "req-narrow")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-narrow"}, &got))
	require.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase, "narrowing below the ceiling must be allowed")

	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: "ns", Name: "req-narrow-child",
	}, &child))
	assert.Nil(t, child.Spec.InputChannel, "the agent asked for single_turn, so the child is headless")
	assert.Empty(t, channelsIn(t, c), "narrowing must not provision the ceiling's Channel")
}

func TestReconcile_RosterPermittingChat_AskingTask_IsDenied_ChatDoesNotImplyTask(t *testing.T) {
	r, c := newReconciler(t,
		parentClassWithModes(t, "demo-lead", map[string][]string{"demo-coder": {v1.SubagentModeChat}}, "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-task-unasked", "demo-parent", "demo-coder", v1.SubagentModeTask),
	)

	got := denialOf(t, r, c, "req-task-unasked")

	assert.Equal(t, "SubagentModeNotPermitted", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination,
		"["+v1.SubagentModeSingleTurn+" "+v1.SubagentModeChat+"]",
		"the allowed set is exactly what was declared plus single_turn; task is absent from it")
}

func TestReconcile_UnrecognizedMode_IsDeniedNotReadAsTheDefault(t *testing.T) {
	// The roster permits chat, so a request that fell through to the ceiling
	// check would be denied for the wrong reason and a request read as empty
	// would SUCCEED as single_turn. Only an explicit unrecognized-value refusal
	// produces the reason asserted below.
	r, c := newReconciler(t,
		parentClassWithModes(t, "demo-lead", map[string][]string{"demo-coder": {v1.SubagentModeChat}}, "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-bogus", "demo-parent", "demo-coder", "chatt"),
	)

	got := denialOf(t, r, c, "req-bogus")

	assert.Equal(t, "SubagentModeUnrecognized", got.Status.FailureReason,
		"a misspelling is a mistake to surface, never a request for single_turn")
	assert.Contains(t, got.Status.Determination, "chatt")
	assert.Contains(t, got.Status.Determination, v1.SubagentModeChat, "the refusal must list the modes that do exist")
}

func TestReconcile_OffRosterClassAskingAMode_IsDeniedForMembershipFirst(t *testing.T) {
	// Ordering guard. Membership is step 1 and the ceiling is step 2, so an
	// off-roster class must still refuse as OffRoster -- the more specific
	// answer -- rather than as "no modes permitted", which is technically true
	// (PermittedSubagentModes returns nil off-roster) and useless to read.
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClass(t, "demo-sre"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-off", "demo-parent", "demo-sre", v1.SubagentModeChat),
	)

	got := denialOf(t, r, c, "req-off")

	assert.Equal(t, "OffRoster", got.Status.FailureReason)
}

// TestDelegateToolThroughController_PermittedModeReachesTheChannelPath spans
// the tool/controller join. Each half is covered on its own above and in
// pkg/agent/tool/meta, and neither can see the seam: the tool's tests prove a
// mode lands on spec.mode, the controller's prove spec.mode provisions a
// Channel, and a field renamed on one side of that boundary passes both. This
// runs the real delegate tool against the real reconciler over one fake client,
// so nothing between "the agent named chat" and "the child is bound to an agent
// Channel" is stubbed.
func TestDelegateToolThroughController_PermittedModeReachesTheChannelPath(t *testing.T) {
	r, c := newReconciler(t,
		parentClassWithModes(t, "demo-lead", map[string][]string{"demo-coder": {v1.SubagentModeChat}}, "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
	)
	ctx := context.Background()

	var created string
	tl := meta.NewDelegateTool(meta.DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(ctx context.Context, sr *v1.SubagentRequest) error {
			// Stands in for the apiserver resolving GenerateName. The tool
			// deliberately never picks its own name, so something has to.
			sr.Name = "req-join"
			sr.GenerateName = ""
			if err := c.Create(ctx, sr); err != nil {
				return err
			}
			created = sr.Name
			return nil
		},
		Poll: func(ctx context.Context, name string) (*v1.SubagentRequest, error) {
			// The operator reconciles between the tool's Create and its first
			// poll in a real cluster; doing it here is what makes this a join
			// test rather than two halves in one function.
			reconcileOnce(t, r, name)
			var sr v1.SubagentRequest
			if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: name}, &sr); err != nil {
				return nil, err
			}
			// Resolve the tool's wait: the child would reach Succeeded on its
			// own in a cluster, and this test is about what was provisioned on
			// the way there, not about completion propagation.
			sr.Status.Phase = v1.SubagentRequestPhaseSucceeded
			sr.Status.Result = "done"
			return &sr, nil
		},
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
	})

	res, err := tl.Execute(ctx, []byte(`{"agent":"demo-coder","task":"review the thing","mode":"chat"}`), nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "a permitted mode must not come back to the model as an error: %s", res.Content)
	require.Equal(t, "req-join", created)

	var sr v1.SubagentRequest
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "req-join"}, &sr))
	assert.Equal(t, v1.SubagentModeChat, sr.Spec.Mode, "the mode the agent named must reach spec.mode unaltered")

	chs := channelsIn(t, c)
	require.Len(t, chs, 1, "the tool's chat request must provision the agent Channel")
	assert.Equal(t, agent.KindName, chs[0].Spec.Kind)

	var child v1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "req-join-child"}, &child))
	require.NotNil(t, child.Spec.InputChannel, "the child must be bound to the Channel the tool's mode asked for")
	assert.Equal(t, chs[0].Name, child.Spec.InputChannel.Name)
}

// TestDelegateToolThroughController_RefusedModeStopsAtTheControllerAndTellsTheAgent
// is the other half of the join: what an agent actually SEES when it asks for a
// mode its roster does not carry. The tool-side tests can only assert on a
// Determination they wrote themselves; this one asserts on the text the
// controller really produced.
func TestDelegateToolThroughController_RefusedModeStopsAtTheControllerAndTellsTheAgent(t *testing.T) {
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"), // old-shape roster: single_turn only
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
	)
	ctx := context.Background()

	tl := meta.NewDelegateTool(meta.DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(ctx context.Context, sr *v1.SubagentRequest) error {
			sr.Name, sr.GenerateName = "req-join-denied", ""
			return c.Create(ctx, sr)
		},
		Poll: func(ctx context.Context, name string) (*v1.SubagentRequest, error) {
			reconcileOnce(t, r, name)
			var sr v1.SubagentRequest
			err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: name}, &sr)
			return &sr, err
		},
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
	})

	res, err := tl.Execute(ctx, []byte(`{"agent":"demo-coder","task":"review the thing","mode":"chat"}`), nil)
	require.NoError(t, err)
	require.True(t, res.IsError, "a refusal must reach the model as an error")

	assert.Contains(t, res.Content, v1.SubagentModeChat, "the agent must be told which mode was refused")
	assert.Contains(t, res.Content, "demo-coder", "the agent must be told which subagent it was refused for")
	assert.Contains(t, res.Content, v1.SubagentModeSingleTurn, "the agent must be told what it may use instead")
	assert.Contains(t, res.Content, "Do not retry",
		"a ceiling refusal is a policy denial, so it carries the same non-retryable framing as any other")

	assert.Empty(t, channelsIn(t, c), "no Channel may exist for a refused mode")
	var sessions v1.AgentSessionList
	require.NoError(t, c.List(ctx, &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1, "no child may exist for a refused mode")
}
