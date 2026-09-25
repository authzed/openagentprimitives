package subagentrequest

// Task 7 of the agent-builder delegation framework: the other half of the
// attended lifecycle -- what the CHILD's watching parent is told once the
// child ends. The parent is never blocked in a tool call while it watches
// (attended_watch.go's own doc, on the channelsd side), so the only way to
// reach it is a fixed turn in its own inbox plus a forced wake, produced
// here from the controller side: resolve() on the child's own terminal
// transition (Succeeded/Failed/ChildVanished, all reached through
// reconcileChild), and finalizeAttended on a PREMATURE delete (a stop) that
// catches the request before deletion cascades its child away.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// recordedPublish is one call notifyAttendedParent's forced wake made
// through PublishInteraction.
type recordedPublish struct {
	ns, name string
	env      channelevents.Envelope
}

// newAttendedNotifyReconciler builds a Reconciler exactly like newReconciler,
// plus ParentMemory (a bare in-memory facade -- no authorizer/provenance
// verifier wired, matching every other unit test in this repo that touches
// *memory.Local directly, e.g. agentsession's incarnation_test.go) and a
// recording PublishInteraction.
func newAttendedNotifyReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, memory.Memory, *[]recordedPublish) {
	t.Helper()
	sch := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).
		Build()
	mem := memory.NewLocal(inmem.NewBackend())
	var published []recordedPublish
	r := &Reconciler{
		Client: c, Scheme: sch, Authz: &fakeAuthz{}, MaxDelegationDepth: 3,
		ParentMemory: mem,
		PublishInteraction: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			published = append(published, recordedPublish{ns: ns, name: name, env: env})
			return nil
		},
	}
	return r, c, mem, &published
}

// idleParentSession is attendedRoot but parked Idle -- WakeEligible, so the
// forced wake's annotation stamp is observable.
func idleParentSession(t *testing.T, name string) *v1.AgentSession {
	t.Helper()
	root := attendedRoot(t, name)
	root.Status.Phase = v1.AgentSessionPhaseIdle
	return root
}

// parentTranscriptText returns the text of the single turn appended to
// (ns, name)'s own memory scope, requiring exactly one turn exists. A system
// approval is minted for the READ the same way notifyAttendedParent mints
// one for its own reach — this helper is reading back what the controller
// wrote, not exercising the capability door itself.
func parentTranscriptText(t *testing.T, mem memory.Memory, ns, name string) string {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	turns, err := turn.ReadAll(ctx, mem, memory.Scope{Kind: "session", ID: ns + "/" + name})
	require.NoError(t, err)
	require.Len(t, turns, 1, "exactly one notice turn is expected")
	require.NotEmpty(t, turns[0].Content)
	return turns[0].Content[0].Text
}

// TestReconcile_AttendedChild_Succeeds_NotifiesParentWithCompletionLine pins
// the Succeeded arm: the parent's own inbox gets the fixed completion line,
// and its wake-requested-at annotation is stamped (it is parked Idle, so
// WakeEligible), plus the KindUserMessage NATS nudge PublishInteraction
// carries.
func TestReconcile_AttendedChild_Succeeds_NotifiesParentWithCompletionLine(t *testing.T) {
	root := idleParentSession(t, "demo-root")
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-succ-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef("req-succ")},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder"},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseSucceeded},
	}
	sr := requestWithMode(t, "req-succ", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-succ-child"}
	sr.Status.Phase = v1.SubagentRequestPhaseRunning

	r, c, mem, published := newAttendedNotifyReconciler(t, root, child, sr)
	reconcileOnce(t, r, "req-succ")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-succ"}, &got))
	require.Equal(t, v1.SubagentRequestPhaseSucceeded, got.Status.Phase)

	assert.Equal(t, attendedCompletedLine, parentTranscriptText(t, mem, "ns", "demo-root"))

	var gotRoot v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "demo-root"}, &gotRoot))
	assert.NotEmpty(t, gotRoot.Annotations[v1.AnnotationWakeRequestedAt], "the parent must be forced awake")

	require.Len(t, *published, 1)
	assert.Equal(t, "demo-root", (*published)[0].name)
	assert.Equal(t, channelevents.KindUserMessage, (*published)[0].env.Kind)
}

// TestReconcile_AttendedChild_Failed_NotifiesParentWithStoppedLine pins the
// Failed arm: the SAME notification mechanism, the "stopped" line instead of
// the completion one.
func TestReconcile_AttendedChild_Failed_NotifiesParentWithStoppedLine(t *testing.T) {
	root := idleParentSession(t, "demo-root")
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-fail-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef("req-fail")},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder"},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseFailed, FailureReason: "RunnerCrash"},
	}
	sr := requestWithMode(t, "req-fail", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-fail-child"}
	sr.Status.Phase = v1.SubagentRequestPhaseRunning

	r, c, mem, published := newAttendedNotifyReconciler(t, root, child, sr)
	reconcileOnce(t, r, "req-fail")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-fail"}, &got))
	require.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase)

	assert.Equal(t, attendedStoppedLine, parentTranscriptText(t, mem, "ns", "demo-root"))
	require.Len(t, *published, 1)
}

// TestReconcile_AttendedChild_Vanished_NotifiesParentWithStoppedLine pins the
// third resolve() call site: a child that no longer exists at all reaches
// fail("ChildVanished", ...) exactly like an ordinary crash, and is notified
// the same way.
func TestReconcile_AttendedChild_Vanished_NotifiesParentWithStoppedLine(t *testing.T) {
	root := idleParentSession(t, "demo-root")
	sr := requestWithMode(t, "req-vanish", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-vanish-child"} // never seeded
	sr.Status.Phase = v1.SubagentRequestPhaseRunning

	r, c, mem, published := newAttendedNotifyReconciler(t, root, sr)
	reconcileOnce(t, r, "req-vanish")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-vanish"}, &got))
	require.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase)
	assert.Equal(t, "ChildVanished", got.Status.FailureReason)

	assert.Equal(t, attendedStoppedLine, parentTranscriptText(t, mem, "ns", "demo-root"))
	require.Len(t, *published, 1)
}

// TestReconcile_AttendedRequest_GetsFinalizerOnFirstPass pins the finalizer
// prologue: a fresh attended request carries FinalizerSubagentRequest after
// ONE reconcile, and still makes progress in that SAME pass (the child gets
// created) — no separate requeue is needed to observe the finalizer.
func TestReconcile_AttendedRequest_GetsFinalizerOnFirstPass(t *testing.T) {
	root := attendedRoot(t, "demo-root")
	r, c, _, _ := newAttendedNotifyReconciler(t,
		attendedParentClass(t), childClass(t, "demo-coder"), root,
		requestWithMode(t, "req-finalizer", "demo-root", "demo-coder", v1.SubagentModeAttended),
	)
	reconcileOnce(t, r, "req-finalizer")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-finalizer"}, &got))
	assert.True(t, controllerutil.ContainsFinalizer(&got, v1.FinalizerSubagentRequest),
		"an attended request must carry the finalizer that guarantees its parent gets notified")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"adding the finalizer must not cost a whole extra pass before the child is created")
	require.NotNil(t, got.Status.ChildRef)
}

// TestReconcile_ChatRequest_NeverGetsTheAttendedFinalizer is the byte-
// identical guarantee for every non-attended mode: chat's own deletion path
// (deny/fail/resolve, reclaimTerminal's cascade) is completely untouched by
// this task.
func TestReconcile_ChatRequest_NeverGetsTheAttendedFinalizer(t *testing.T) {
	r, c, _, _ := newAttendedNotifyReconciler(t,
		conversationalParentClass(t), childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		requestWithMode(t, "req-chat", "demo-parent", "demo-coder", v1.SubagentModeChat),
	)
	reconcileOnce(t, r, "req-chat")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-chat"}, &got))
	assert.Empty(t, got.Finalizers, "a non-attended request must never carry the attended finalizer")
}

// TestFinalizeAttended_PrematureDelete_NotifiesParentAndClearsFinalizer is
// the brief's "test was stopped" case: an attended request, still non-
// terminal with a live child, is deleted out from under the delegation (the
// mechanism a future "stop" tool uses — controller.go's own doc). The
// finalizer intercepts it, notifies the parent with the fixed stop line, and
// then releases the object.
func TestFinalizeAttended_PrematureDelete_NotifiesParentAndClearsFinalizer(t *testing.T) {
	root := idleParentSession(t, "demo-root")
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-stop-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef("req-stop")},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder"},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning}, // still LIVE
	}
	sr := requestWithMode(t, "req-stop", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-stop-child"}
	sr.Status.Phase = v1.SubagentRequestPhaseRunning
	sr.Finalizers = []string{v1.FinalizerSubagentRequest} // as if an earlier pass already added it

	r, c, mem, published := newAttendedNotifyReconciler(t, root, child, sr)

	require.NoError(t, c.Delete(context.Background(), sr.DeepCopy()))
	reconcileOnce(t, r, "req-stop")

	assert.Equal(t, attendedStoppedLine, parentTranscriptText(t, mem, "ns", "demo-root"))
	require.Len(t, *published, 1)

	var gone v1.SubagentRequest
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-stop"}, &gone)
	require.Error(t, err, "removing the last finalizer must release the deletion-timestamped object")
}

// TestFinalizeAttended_AlreadyTerminalDelete_DoesNotDoubleNotify is
// finalizeAttended's own complement: a request already resolved (Succeeded,
// notified once when resolve() first reached it) that is later deleted by
// reclaimTerminal's routine retention cleanup must NOT be notified a second
// time — that delete is reclaim, not a stop.
func TestFinalizeAttended_AlreadyTerminalDelete_DoesNotDoubleNotify(t *testing.T) {
	root := idleParentSession(t, "demo-root")
	sr := requestWithMode(t, "req-reclaim", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-reclaim-child"}
	sr.Status.Phase = v1.SubagentRequestPhaseSucceeded // already resolved, presumably already notified earlier
	sr.Finalizers = []string{v1.FinalizerSubagentRequest}

	r, c, _, published := newAttendedNotifyReconciler(t, root, sr)

	require.NoError(t, c.Delete(context.Background(), sr.DeepCopy()))
	reconcileOnce(t, r, "req-reclaim")

	assert.Empty(t, *published, "a request that was already terminal before this delete must not be notified again")

	var gone v1.SubagentRequest
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-reclaim"}, &gone)
	require.Error(t, err, "the finalizer must still be released even when no notification is due")
}

// TestFinalizeAttended_UpdateConflict_RetriesAndStillNotifiesExactlyOnce
// covers the review fix: the finalizer-removal Update is wrapped in
// RetryOnConflict, so an ordinary optimistic-lock conflict on the FIRST
// attempt (cache lag / a concurrent writer -- the routine reason a
// finalizer removal needs a retry at all) is resolved inside THIS call
// rather than bubbling up as a Reconcile error that would re-enter
// finalizeAttended with the notify-guard still true. The interceptor fails
// exactly the first Update of the SubagentRequest and succeeds on the
// second, standing in for what a real apiserver does under contention --
// same technique TestReconcile_TreeCeiling_TransientRootClassLookupErrorRequeues
// uses for a Get. The crash-window residual RetryOnConflict does NOT close
// (a process death between a successful notify and a successful persist)
// is not unit-testable and is documented on finalizeAttended's own comment
// instead.
func TestFinalizeAttended_UpdateConflict_RetriesAndStillNotifiesExactlyOnce(t *testing.T) {
	root := idleParentSession(t, "demo-root")
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-conflict-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef("req-conflict")},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder"},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning}, // still LIVE
	}
	sr := requestWithMode(t, "req-conflict", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-conflict-child"}
	sr.Status.Phase = v1.SubagentRequestPhaseRunning
	sr.Finalizers = []string{v1.FinalizerSubagentRequest}

	sch := newScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(root, child, sr).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).
		Build()

	var updateAttempts int
	c := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			sreq, ok := obj.(*v1.SubagentRequest)
			if !ok || sreq.Name != "req-conflict" {
				return cl.Update(ctx, obj, opts...)
			}
			updateAttempts++
			if updateAttempts == 1 {
				return apierrors.NewConflict(
					schema.GroupResource{Group: v1.SchemeGroupVersion.Group, Resource: "subagentrequests"},
					sreq.Name, errors.New("simulated optimistic-lock conflict"))
			}
			return cl.Update(ctx, obj, opts...)
		},
	})

	mem := memory.NewLocal(inmem.NewBackend())
	var published []recordedPublish
	r := &Reconciler{
		Client: c, Scheme: sch, Authz: &fakeAuthz{}, MaxDelegationDepth: 3,
		ParentMemory: mem,
		PublishInteraction: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			published = append(published, recordedPublish{ns: ns, name: name, env: env})
			return nil
		},
	}

	require.NoError(t, c.Delete(context.Background(), sr.DeepCopy()))
	reconcileOnce(t, r, "req-conflict")

	assert.GreaterOrEqual(t, updateAttempts, 2, "the conflicting first Update must have been retried")

	assert.Equal(t, attendedStoppedLine, parentTranscriptText(t, mem, "ns", "demo-root"),
		"the retry must not re-run notifyAttendedParent -- exactly one turn, not two")
	require.Len(t, published, 1, "exactly one forced wake, not one per Update attempt")

	var gone v1.SubagentRequest
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-conflict"}, &gone)
	require.Error(t, err, "the finalizer must still end up removed once the retry succeeds")
	assert.True(t, apierrors.IsNotFound(err))
}

// midTurnParentSession is attendedRoot in the middle of its own turn: the pod
// is alive at Running with nothing awaited, so its runner holds a transcript
// index it is about to write at.
func midTurnParentSession(t *testing.T, name string) *v1.AgentSession {
	t.Helper()
	root := attendedRoot(t, name)
	root.Status.Phase = v1.AgentSessionPhaseRunning
	return root
}

// A child that ends while its parent is in the middle of its own turn is
// reported straight away: the line takes the "inbox" role, which shares no
// (index, role) key with the turns the parent's runner writes, so there is
// nothing to wait for. The parent's runner promotes it at its next yield.
func TestReconcile_AttendedChild_MidTurnParent_IsToldWithoutWaiting(t *testing.T) {
	root := midTurnParentSession(t, "demo-root")
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-live-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef("req-live")},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder"},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseSucceeded},
	}
	sr := requestWithMode(t, "req-live", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-live-child"}
	sr.Status.Phase = v1.SubagentRequestPhaseRunning

	r, c, mem, published := newAttendedNotifyReconciler(t, root, child, sr)

	reconcileOnce(t, r, "req-live")

	assert.Equal(t, attendedCompletedLine, parentTranscriptText(t, mem, "ns", "demo-root"),
		"a busy parent is told on the pass that resolved its child, not a later one")
	require.Len(t, *published, 1, "exactly one wake")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-live"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseSucceeded, got.Status.Phase, "the request resolves in the same pass")

	var after v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "demo-root"}, &after))
	assert.Empty(t, after.Annotations[v1.AnnotationWakeRequestedAt],
		"a parent whose runner is alive must not be re-spawned; the nudge alone reaches it")
}

// The same on the deletion arm: a stop whose parent is mid-turn is delivered
// and the finalizer released in one pass.
func TestFinalizeAttended_MidTurnParent_NotifiesAndReleasesTheFinalizer(t *testing.T) {
	root := midTurnParentSession(t, "demo-root")
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-livestop-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef("req-livestop")},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder"},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning}, // still LIVE
	}
	sr := requestWithMode(t, "req-livestop", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-livestop-child"}
	sr.Status.Phase = v1.SubagentRequestPhaseRunning
	sr.Finalizers = []string{v1.FinalizerSubagentRequest}

	r, c, mem, published := newAttendedNotifyReconciler(t, root, child, sr)
	require.NoError(t, c.Delete(context.Background(), sr.DeepCopy()))

	reconcileOnce(t, r, "req-livestop")

	assert.Equal(t, attendedStoppedLine, parentTranscriptText(t, mem, "ns", "demo-root"))
	require.Len(t, *published, 1)
	var gone v1.SubagentRequest
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-livestop"}, &gone)),
		"the notice landed, so the finalizer is released and the object goes")
}

// A parent that has FINISHED is appended nothing: no runner will come back to
// read it. The request resolves on its own retention clock regardless — a
// notice nobody can receive must not keep a resolved child unresolved.
func TestReconcile_AttendedChild_FinishedParent_ResolvesWithoutNotifying(t *testing.T) {
	root := attendedRoot(t, "demo-root")
	root.Status.Phase = v1.AgentSessionPhaseFailed
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "req-dead-child",
			Labels: map[string]string{
				v1.LabelDelegationRoot:          "demo-root",
				v1.LabelAttendedParentNamespace: "ns",
				v1.LabelAttendedParentName:      "demo-root",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef("req-dead")},
		},
		Spec:   v1.AgentSessionSpec{Class: "demo-coder"},
		Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseSucceeded},
	}
	sr := requestWithMode(t, "req-dead", "demo-root", "demo-coder", v1.SubagentModeAttended)
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: "req-dead-child"}
	sr.Status.Phase = v1.SubagentRequestPhaseRunning

	r, c, _, published := newAttendedNotifyReconciler(t, root, child, sr)
	res := reconcileOnce(t, r, "req-dead")

	assert.Positive(t, res.RequeueAfter, "the resolved request goes onto its retention clock")
	assert.Empty(t, *published, "nothing is appended to a session that is over, and nobody is woken")
	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-dead"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseSucceeded, got.Status.Phase, "the request still resolves")
}

// ownerRef builds the controller owner reference buildChild stamps a child
// with, naming srName as the owning SubagentRequest.
func ownerRef(srName string) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{
		APIVersion: v1.SchemeGroupVersion.String(),
		Kind:       "SubagentRequest",
		Name:       srName,
		Controller: &yes,
	}
}
