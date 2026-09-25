package subagentrequest

// Conversational-mode provisioning (spec.mode = task | chat): the agent
// Channel, the child's binding to it, the rollback and retry properties of a
// SECOND non-idempotent create, and the two cross-cutting decisions the modes
// reopened -- where a human-directed prompt from a bound child goes, and
// whether an ask/dynamic child class may now be delegated.
//
// Its own file rather than more of controller_test.go: that file is the
// single_turn surface Track 1a was built on, and keeping the regression guard
// for it separate from the feature that must not disturb it is worth one extra
// file.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"

	// The lineage walk under test resolves a channel kind through the shared
	// registry, so both kinds it must tell apart have to be REGISTERED in this
	// test binary. `agent` (which no human reads) arrives with the controller's
	// own import; importing `local` here registers a real kind a human DOES
	// read. Two genuine kinds rather than a stub is what makes this an
	// end-to-end check of Task 6's guard instead of a restatement of it.
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// conversationalParentClass is `parentClass` whose roster also PERMITS the
// conversational modes for demo-coder.
//
// A plain []string roster entry permits single_turn and nothing else
// (AgentClassSpec.Subagents), so a fixture built with parentClass alone is
// refused at the mode ceiling — step 2 of Reconcile, before any of the
// provisioning these tests are about. Every test in this file asks for task or
// chat, so every one of them needs the grant; sharing it in one helper is what
// stops a future test being added with a roster that cannot reach the code it
// claims to cover.
func conversationalParentClass(t *testing.T) *v1.AgentClass {
	t.Helper()
	return parentClassWithModes(t, "demo-lead",
		map[string][]string{"demo-coder": {v1.SubagentModeTask, v1.SubagentModeChat}},
		"demo-coder")
}

// grantConversationalModes widens the named AgentClass in an already-built
// fixture slice so its roster permits task and chat for demo-coder.
//
// treeCeilingFixture builds its delegating class with a plain []string roster,
// which permits single_turn only — correct for the single_turn tests that
// share it, and refused at the mode ceiling by the conversational one. Widening
// in place here keeps that grant visible in the test that needs it instead of
// hiding it in a shared builder whose other callers never ask for it.
func grantConversationalModes(t *testing.T, objs []client.Object, className string) {
	t.Helper()
	for _, o := range objs {
		ac, ok := o.(*v1.AgentClass)
		if !ok || ac.Name != className {
			continue
		}
		ac.Spec.SubagentModes = map[string][]string{
			"demo-coder": {v1.SubagentModeTask, v1.SubagentModeChat},
		}
		return
	}
	t.Fatalf("fixture contains no AgentClass %q to widen", className)
}

// requestWithMode is `request` plus a spec.mode. Conversational tests need the
// mode and nothing else that differs, so it wraps rather than growing `request`
// a parameter every other call site would have to pass "" for.
func requestWithMode(t *testing.T, name, parent, class, mode string) *v1.SubagentRequest {
	t.Helper()
	sr := request(t, name, parent, class)
	sr.Spec.Mode = mode
	return sr
}

// channelsIn lists every Channel in the fixture namespace. These tests assert
// on the WHOLE set rather than Get-ing the expected name: the regression that
// matters most is an EXTRA Channel from a double-create, which a Get by name
// cannot see.
func channelsIn(t *testing.T, c client.Client) []v1.Channel {
	t.Helper()
	var chs v1.ChannelList
	require.NoError(t, c.List(context.Background(), &chs, client.InNamespace("ns")))
	return chs.Items
}

func TestReconcile_ModeEmpty_ChildStaysHeadlessAndNoChannelIsCreated(t *testing.T) {
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		request(t, "req-headless", "demo-parent", "demo-coder"), // no spec.mode
	)

	reconcileOnce(t, r, "req-headless")

	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: "ns", Name: "req-headless-child",
	}, &child), "the child must exist")

	assert.Nil(t, child.Spec.InputChannel, "an omitted spec.mode is single_turn: the child stays headless")
	assert.Empty(t, child.Labels[v1.LabelChannelName], "a headless child carries no channel-correlation label")
	assert.Empty(t, child.Labels[v1.LabelChannelKey], "a headless child carries no channel-correlation label")
	assert.Empty(t, channelsIn(t, c), "no Channel may be created for a single_turn child")
}

// TestReconcile_ModeTask_CreatesAnAgentChannelNamingTheParentAndBindsTheChild
// pins the three things a later change could break independently: the Channel
// exists with kind=agent under a name derived from the request; its
// authzSubject names the PARENT (naming the child would attribute the parent's
// own messages to the child); and the child's spec.inputChannel AND its
// correlation labels point at it -- without the labels, channelsd's Deliver
// resolves no session and every parent->child message dies as "no active
// session".
func TestReconcile_ModeTask_CreatesAnAgentChannelNamingTheParentAndBindsTheChild(t *testing.T) {
	r, c := newReconciler(t,
		conversationalParentClass(t),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-task", "demo-parent", "demo-coder", v1.SubagentModeTask),
	)

	reconcileOnce(t, r, "req-task")

	chs := channelsIn(t, c)
	require.Len(t, chs, 1, "a task child gets exactly one Channel")
	ch := chs[0]
	assert.Equal(t, "req-task-inbox", ch.Name, "the Channel name must be derived from the request, not minted")
	assert.Equal(t, agent.KindName, ch.Spec.Kind)
	assert.Equal(t, "agentsession:ns/demo-parent", ch.Spec.AuthzSubject,
		"the counterparty is the PARENT session, never the child")
	assert.Equal(t, "demo-coder", ch.Spec.AgentClass, "the Channel is bound to the CHILD's class")
	assert.Equal(t, v1.ChannelRoleBoth, ch.Spec.Role, "the child both receives from and answers its parent")

	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: "ns", Name: "req-task-child",
	}, &child))
	require.NotNil(t, child.Spec.InputChannel, "a task child must be bound to its Channel")
	assert.Equal(t, ch.Name, child.Spec.InputChannel.Name)
	assert.Equal(t, agent.KindName, child.Spec.InputChannel.Kind)
	assert.Equal(t, "agent:req-task-child", child.Spec.InputChannel.Key)
	assert.Equal(t, []string{"text"}, child.Spec.InputChannel.Capabilities,
		"capabilities come from the kind, so respond_to_user's schema matches what the kind renders")
	assert.Equal(t, "ap.session.ns.req-task-child", child.Spec.InputChannel.NATSSubjectPrefix)

	assert.Equal(t, ch.Name, child.Labels[v1.LabelChannelName])
	assert.Equal(t, agent.KindName, child.Labels[v1.LabelChannelKind])
	assert.Equal(t, channelkey.LabelValue("agent:req-task-child"), child.Labels[v1.LabelChannelKey],
		"the correlation label must be the hash of the binding key channelsd looks up")
	assert.Equal(t, "demo-parent", child.Labels[v1.LabelDelegationRoot],
		"conversational provisioning must not disturb the delegation-root label")
}

// TestReconcile_ModeChat_ProvisionsIdenticallyToTask pins a deliberate ABSENCE
// of difference. What separates task from chat is DECLARED — task may ask one
// bounded clarifying question, chat may hold a conversation
// (SubagentRequestSpec.Mode) — and no code enforces it yet; nothing in the
// runner reads spec.mode. What is certain is that the difference is not in what
// this controller creates. A reader hunting for a provisioning difference
// should find this test instead of hunting further.
func TestReconcile_ModeChat_ProvisionsIdenticallyToTask(t *testing.T) {
	build := func(t *testing.T, mode, reqName string) (v1.Channel, v1.AgentSession) {
		t.Helper()
		r, c := newReconciler(t,
			conversationalParentClass(t),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			requestWithMode(t, reqName, "demo-parent", "demo-coder", mode),
		)
		reconcileOnce(t, r, reqName)
		chs := channelsIn(t, c)
		require.Len(t, chs, 1, "%s must provision exactly one Channel", mode)
		var child v1.AgentSession
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{
			Namespace: "ns", Name: reqName + "-child",
		}, &child))
		return chs[0], child
	}

	// The same request name for both, so every derived value is comparable
	// field-for-field; each build runs against its own fake client.
	taskCh, taskChild := build(t, v1.SubagentModeTask, "req-same")
	chatCh, chatChild := build(t, v1.SubagentModeChat, "req-same")

	assert.Equal(t, taskCh.Spec, chatCh.Spec, "chat and task provision the same Channel spec")
	assert.Equal(t, taskCh.Name, chatCh.Name)
	assert.Equal(t, taskChild.Spec.InputChannel, chatChild.Spec.InputChannel,
		"chat and task bind the child identically")
	assert.Equal(t, taskChild.Labels, chatChild.Labels)
}

// TestReconcile_ChannelCreateFails_ChildIsRolledBackNotLeftUnbound pins the
// rollback. A conversational child whose Channel never landed can neither be
// spoken to nor answer, yet it would still hold a slot against the tree ceiling
// and standing in SpiceDB -- so it must not survive the failed pass. The retry
// rebuilds both from scratch.
func TestReconcile_ChannelCreateFails_ChildIsRolledBackNotLeftUnbound(t *testing.T) {
	sch := newScheme(t)
	base := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(
			conversationalParentClass(t),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			requestWithMode(t, "req-chrollback", "demo-parent", "demo-coder", v1.SubagentModeChat),
		).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()

	simulated := errors.New("simulated channel create failure")
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1.Channel); ok {
				return simulated
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	az := &fakeAuthz{}
	r := &Reconciler{Client: c, Scheme: sch, Authz: az, MaxDelegationDepth: 3}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req-chrollback"},
	})
	require.Error(t, err, "a transient Channel create failure must be retried, not swallowed")

	var sessions v1.AgentSessionList
	require.NoError(t, base.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1,
		"only the parent may remain: an unbound conversational child has a surface nobody can reach")
	assert.Empty(t, az.lineageWrites, "lineage must not be written for a child that was rolled back")

	var got v1.SubagentRequest
	require.NoError(t, base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-chrollback"}, &got))
	assert.Empty(t, got.Status.ChildRef, "the failed pass persisted no child reference")
}

// TestReconcile_ChannelCreateLandsButErrors_ChannelIsRolledBackToo covers the
// lost-response shape the test above does not: the write REACHED the apiserver
// and the response did not, so Create returns something other than
// AlreadyExists while the object exists. Handing rollback a nil Channel there
// deletes the child and leaves the Channel behind.
//
// The injected error is IsInvalid, which is the terminal branch: the request
// resolves Failed and is never reconciled again, so nothing later comes along
// to notice the orphan. It survives for the life of the SubagentRequest.
func TestReconcile_ChannelCreateLandsButErrors_ChannelIsRolledBackToo(t *testing.T) {
	sch := newScheme(t)
	base := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(
			conversationalParentClass(t),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			requestWithMode(t, "req-chlost", "demo-parent", "demo-coder", v1.SubagentModeChat),
		).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()

	landed := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			ch, ok := obj.(*v1.Channel)
			if !ok {
				return cl.Create(ctx, obj, opts...)
			}
			// The write lands; only the answer is lost.
			if err := cl.Create(ctx, obj, opts...); err != nil {
				return err
			}
			landed = true
			return apierrors.NewInvalid(
				schema.GroupKind{Group: v1.SchemeGroupVersion.Group, Kind: "Channel"},
				ch.Name,
				field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), ch.Name, "simulated rejection")},
			)
		},
	})
	az := &fakeAuthz{}
	r := &Reconciler{Client: c, Scheme: sch, Authz: az, MaxDelegationDepth: 3}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req-chlost"},
	})
	require.NoError(t, err, "a terminal rejection is recorded on the request, not returned for retry")
	require.True(t, landed, "the injected create must actually have persisted the Channel, or this test proves nothing")

	assert.Empty(t, channelsIn(t, base),
		"a Channel that landed despite the error must be rolled back, not orphaned on a request that never reconciles again")

	var sessions v1.AgentSessionList
	require.NoError(t, base.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1, "only the parent may remain: the child is rolled back with its Channel")
	assert.Empty(t, az.lineageWrites, "lineage must not be written for a child that was rolled back")

	var got v1.SubagentRequest
	require.NoError(t, base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-chlost"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase,
		"a rejected Channel create resolves Failed -- retryable, not a policy Denied")
	assert.Equal(t, "ChildChannelCreateRejected", got.Status.FailureReason)
	assert.Empty(t, got.Status.ChildRef, "the failed pass persisted no child reference")
}

// TestReconcile_ConversationalRetryAfterStatusWriteFails_LeavesExactlyOneChannel
// is the retry-safety guard, modelled on
// TestReconcile_ChildCreatedButFinalStatusWriteFailsDoesNotDenyOnRetry. The
// final status write is Reconcile's last statement, so a failure there leaves
// ChildRef nil and the retry re-enters the whole create path. The child
// survives that because its name is derived from the request; the Channel is a
// SECOND non-idempotent create carrying the identical hazard, so it has to be
// derived the same way. The fixture also sits one under the ceiling, so a retry
// that double-counted anything would deny instead of succeed.
func TestReconcile_ConversationalRetryAfterStatusWriteFails_LeavesExactlyOneChannel(t *testing.T) {
	objs := treeCeilingFixture(t, &v1.BudgetConfig{MaxDelegatedAgents: 3}, 1)
	grantConversationalModes(t, objs, "demo-root-class")
	objs = append(objs, requestWithMode(t, "req-chretry", "demo-root", "demo-coder", v1.SubagentModeChat))

	sch := newScheme(t)
	base := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()

	failedOnce := false
	simulated := errors.New("simulated apiserver blip")
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if !failedOnce && subResourceName == "status" {
				if sr, ok := obj.(*v1.SubagentRequest); ok && sr.Name == "req-chretry" {
					failedOnce = true
					return simulated
				}
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	})
	r := &Reconciler{Client: c, Scheme: sch, Authz: &fakeAuthz{}, MaxDelegationDepth: 3}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req-chretry"},
	})
	require.Error(t, err, "a failed final status write must be retried, not swallowed")
	require.True(t, failedOnce, "the injected failure must actually have fired for this to be a meaningful test")
	require.Len(t, channelsIn(t, base), 1, "the first pass created the Channel")

	reconcileOnce(t, r, "req-chretry")

	var got v1.SubagentRequest
	require.NoError(t, base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-chretry"}, &got))
	assert.NotEqual(t, v1.SubagentRequestPhaseDenied, got.Status.Phase,
		"a retry must not deny a delegation whose child and Channel it already created")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)

	chs := channelsIn(t, base)
	assert.Len(t, chs, 1, "the retry must reuse the deterministically-named Channel, never create a second")
	assert.Equal(t, "req-chretry-inbox", chs[0].Name)
}

// TestReconcile_ConversationalChild_HumanDirectedBindingResolvesToTheRoot is
// the cross-task guard for Task 6's routing walk, run for the first time
// against a REALLY provisioned conversational child: the child and the Channel
// this asserts on are whatever Reconcile just created, not a hand-built
// fixture.
//
// The child's own outbound binding is non-nil now (it IS the agent Channel), so
// the lineage fallback that used to fire on a nil binding no longer does. The
// walk must still climb PAST it: an `agent` Channel's far side is the parent
// SESSION, and a prompt whose answer is a person's decision must not be put in
// front of another agent.
func TestReconcile_ConversationalChild_HumanDirectedBindingResolvesToTheRoot(t *testing.T) {
	ctx := context.Background()

	// A root a human actually reads, and an intermediate parent delegated from
	// it that is itself headless -- so the walk climbs two links: past the
	// child's own agent binding, then past a session with no binding at all.
	root := parentSession(t, "demo-root", "demo-lead")
	root.Spec.InputChannel = &v1.ChannelBinding{
		Name: "demo-root-tui", Kind: local.KindName, Key: "tui:demo-root",
	}
	mid := parentSession(t, "demo-parent", "demo-lead")
	mid.Spec.Parent = &v1.NamespacedRef{Namespace: "ns", Name: "demo-root"}

	r, c := newReconciler(t,
		conversationalParentClass(t),
		childClass(t, "demo-coder"),
		root, mid,
		requestWithMode(t, "req-route", "demo-parent", "demo-coder", v1.SubagentModeChat),
	)

	reconcileOnce(t, r, "req-route")

	var child v1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "req-route-child"}, &child))
	own := v1.OutboundBinding(&child)
	require.NotNil(t, own, "precondition: the child IS bound now, which is what makes this case new")
	require.Equal(t, agent.KindName, own.Kind, "precondition: its own outbound binding is the agent Channel")

	// registry.DeliversToHuman, not a stand-in: this is the predicate the
	// outbound relay passes.
	b, err := v1.ResolveHumanDirectedBinding(ctx, c, &child, registry.DeliversToHuman)
	require.NoError(t, err, "the walk must resolve for a really-provisioned conversational child")
	require.NotNil(t, b, "a human-directed prompt from this child has somewhere to go")
	assert.Equal(t, "demo-root-tui", b.Binding.Name, "it resolves to the ROOT's binding")
	assert.Equal(t, local.KindName, b.Binding.Kind, "and not to the child's own agent Channel")
	// The owner is what makes that binding deliverable. A client-hosted host
	// asks whether it serves the session it was handed; handed this CHILD's
	// name it refuses a card it is the right reader for, and the child parks
	// until its approval times out.
	assert.Equal(t, "demo-root", b.Owner.Name,
		"the walk must report WHICH session owns the binding — the ancestor the host is asked about, not the child")
}

// TestReconcile_AskModeChildClassInChatModeIsStillDenied pins the decision the
// conversational modes reopened. Having a channel to render the identity prompt
// into is no longer the reason for this refusal; the reason is that
// CheckMonotonicIdentity runs once, at delegation, against the provisional mode
// an ask/dynamic class resolves to, and nothing re-runs it when the human's
// real choice lands on status.effectiveIdentityMode.
func TestReconcile_AskModeChildClassInChatModeIsStillDenied(t *testing.T) {
	r, c := newReconciler(t,
		conversationalParentClass(t),
		childClassWithIdentityMode(t, "demo-coder", v1.IdentityModeAsk),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-askchat", "demo-parent", "demo-coder", v1.SubagentModeChat),
	)

	reconcileOnce(t, r, "req-askchat")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-askchat"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase,
		"a conversational surface does not make a late identity choice safe")
	assert.Equal(t, "ChildIdentityModeUnsupported", got.Status.FailureReason)
	assert.Empty(t, got.Status.ChildRef)
	assert.Empty(t, channelsIn(t, c), "a denied request provisions nothing")
}
