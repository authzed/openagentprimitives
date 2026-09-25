package subagentrequest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// newReconciler builds a Reconciler over a fake client seeded with objs.
func newReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	sch := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).
		Build()
	return &Reconciler{Client: c, Scheme: sch, Authz: &fakeAuthz{}, MaxDelegationDepth: 3}, c
}

func reconcileOnce(t *testing.T, r *Reconciler, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: name},
	})
	require.NoError(t, err, "Reconcile must not return an error for a policy refusal")
	return res
}

func TestReconcile_OffRosterClassIsDenied(t *testing.T) {
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"), // roster: coder only
		childClass(t, "demo-sre"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		request(t, "req1", "demo-parent", "demo-sre"), // asks for a class NOT on the roster
	)

	reconcileOnce(t, r, "req1")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req1"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Contains(t, got.Status.Determination, "demo-sre")
	assert.Empty(t, got.Status.ChildRef, "no child may be created for an off-roster class")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1, "only the parent session should exist")
}

func TestReconcile_OnRosterCreatesHeadlessChildWithParentSet(t *testing.T) {
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		request(t, "req2", "demo-parent", "demo-coder"),
	)

	reconcileOnce(t, r, "req2")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req2"}, &got))
	require.NotNil(t, got.Status.ChildRef, "an on-roster request must create a child")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)

	var child v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: got.Status.ChildRef.Namespace, Name: got.Status.ChildRef.Name,
	}, &child))

	require.NotNil(t, child.Spec.Parent, "the child must record its parent")
	assert.Equal(t, "demo-parent", child.Spec.Parent.Name)
	assert.Equal(t, "demo-coder", child.Spec.Class)
	assert.Nil(t, child.Spec.InputChannel, "a single_turn child is headless")
	assert.Equal(t, "do the thing", child.Spec.Prompt.Inline)
}

// childClassWithDigest is childClass plus a recorded .oap install digest —
// the fixture the digest-pin tests need to exercise a pinned roster entry
// against a class that either does or does not carry an installed bundle.
func childClassWithDigest(t *testing.T, name, digest string) *v1.AgentClass {
	t.Helper()
	c := childClass(t, name)
	if digest != "" {
		c.Status.OapInstall = &v1.OapInstallStatus{Digest: digest, SourceKind: "registry"}
	}
	return c
}

// TestReconcile_DigestPin exercises the roster-pin check: a pinned roster
// entry ("child@sha256:...") binds delegation to the exact installed bundle
// digest, and every way that binding can fail to hold must refuse the
// request rather than delegate to an unverified target.
func TestReconcile_DigestPin(t *testing.T) {
	pin := "sha256:" + strings.Repeat("aa", 32)
	other := "sha256:" + strings.Repeat("bb", 32)
	cases := []struct {
		name       string
		roster     []string
		digest     string // installed digest on the "child" AgentClass; "" means no OapInstall recorded
		wantPhase  string
		wantReason string
	}{
		{"pin matches installed digest: proceeds past pin check",
			[]string{"child@" + pin}, pin, v1.SubagentRequestPhaseRunning, ""},
		{"pin mismatch: Denied/DigestPinMismatch",
			[]string{"child@" + pin}, other, v1.SubagentRequestPhaseDenied, "DigestPinMismatch"},
		{"pinned target with no oapInstall: Denied/DigestUnrecorded",
			[]string{"child@" + pin}, "", v1.SubagentRequestPhaseDenied, "DigestUnrecorded"},
		{"two distinct pins for one name: Denied/DigestPinConflict",
			[]string{"child@" + pin, "child@" + other}, pin, v1.SubagentRequestPhaseDenied, "DigestPinConflict"},
		{"unpinned entry unaffected",
			[]string{"child"}, "", v1.SubagentRequestPhaseRunning, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newReconciler(t,
				parentClass(t, "demo-lead", tc.roster...),
				childClassWithDigest(t, "child", tc.digest),
				parentSession(t, "demo-parent", "demo-lead"),
				request(t, "req-pin", "demo-parent", "child"),
			)

			reconcileOnce(t, r, "req-pin")

			var got v1.SubagentRequest
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-pin"}, &got))
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			assert.Equal(t, tc.wantReason, got.Status.FailureReason)

			switch tc.wantReason {
			case "DigestPinMismatch":
				assert.Contains(t, got.Status.Determination, "child")
				assert.Contains(t, got.Status.Determination, pin, "mismatch determination must name the roster pin")
				assert.Contains(t, got.Status.Determination, other, "mismatch determination must name the installed digest")
			case "DigestPinConflict":
				assert.Contains(t, got.Status.Determination, "child")
				assert.Contains(t, got.Status.Determination, "2", "conflict determination must name the count of distinct pins")
			case "DigestUnrecorded":
				assert.Contains(t, got.Status.Determination, "child")
			case "":
				assert.NotNil(t, got.Status.ChildRef, "an unpinned or satisfied pin must proceed to create a child")
			}
		})
	}
}

// clusterSettingsRequiringDigestPins builds the singleton ClusterAgentSettings
// fixture with RequireSubagentDigestPins set, for
// TestReconcile_DigestPinRequired. The controller resolves the tiers itself
// (settingswiring.FetchTiers reads this exact name/scope), so the fixture
// only needs the CR present in the fake client.
func clusterSettingsRequiringDigestPins(t *testing.T, require bool) *v1.ClusterAgentSettings {
	t.Helper()
	return &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec:       v1.SettingsSpec{Limits: &v1.SettingsLimits{RequireSubagentDigestPins: &require}},
	}
}

// TestReconcile_DigestPinRequired exercises the RequireSubagentDigestPins
// settings enforcement: with the cluster tier requiring digest-pinned
// delegation, an unpinned roster entry is refused before Task 7's own pin
// check ever runs, and a pinned entry whose digest matches proceeds exactly
// as it would without the setting.
func TestReconcile_DigestPinRequired(t *testing.T) {
	pin := "sha256:" + strings.Repeat("cc", 32)
	cases := []struct {
		name       string
		roster     []string
		digest     string
		wantPhase  string
		wantReason string
	}{
		{"unpinned roster entry: Denied/DigestPinRequired",
			[]string{"child"}, "", v1.SubagentRequestPhaseDenied, "DigestPinRequired"},
		{"pinned entry with matching digest: proceeds",
			[]string{"child@" + pin}, pin, v1.SubagentRequestPhaseRunning, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newReconciler(t,
				clusterSettingsRequiringDigestPins(t, true),
				parentClass(t, "demo-lead", tc.roster...),
				childClassWithDigest(t, "child", tc.digest),
				parentSession(t, "demo-parent", "demo-lead"),
				request(t, "req-pin-required", "demo-parent", "child"),
			)

			reconcileOnce(t, r, "req-pin-required")

			var got v1.SubagentRequest
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-pin-required"}, &got))
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			assert.Equal(t, tc.wantReason, got.Status.FailureReason)
			if tc.wantReason == "DigestPinRequired" {
				assert.Contains(t, got.Status.Determination, "child")
				assert.Empty(t, got.Status.ChildRef, "a required-but-missing pin must not create a child")
			} else {
				assert.NotNil(t, got.Status.ChildRef, "a satisfied pin requirement must proceed to create a child")
			}
		})
	}
}

func TestBuildChild_Closure_StampsChildWithParentsDelegationRoot(t *testing.T) {
	r, _ := newReconciler(t)
	sr := request(t, "req-closure", "demo-mid", "demo-coder")
	// demo-mid is itself a delegated child of demo-root -- its label carries
	// the tree's root, distinct from its own name. buildChild must propagate
	// that root, not stamp the child with demo-mid's own name (which would
	// make every grandchild start a new tree).
	parent := parentSession(t, "demo-mid", "demo-lead")
	parent.Labels = map[string]string{v1.LabelDelegationRoot: "demo-root"}

	// root is unused by buildChild for single_turn (sr carries no mode, so
	// EffectiveMode() is single_turn); passing parent stands in for it.
	child := r.buildChild(sr, parent, parent)

	require.NotNil(t, child.Labels, "buildChild must stamp the delegation-root label")
	assert.Equal(t, "demo-root", child.Labels[v1.LabelDelegationRoot],
		"the child inherits the parent's tree root, not the parent's own name")
}

func TestReconcile_WritesTheLineageTuple(t *testing.T) {
	az := &fakeAuthz{}
	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(
			parentClass(t, "demo-lead", "demo-coder"),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			request(t, "req3", "demo-parent", "demo-coder"),
		).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()
	r := &Reconciler{Client: c, Scheme: sch, Authz: az, MaxDelegationDepth: 3}

	reconcileOnce(t, r, "req3")

	require.Len(t, az.lineageWrites, 1, "exactly one lineage write must be issued (it carries both tuples atomically)")
	assert.Equal(t, "demo-parent", az.lineageWrites[0].parentName)
}

func TestReconcile_TupleWriteFailureDoesNotLeaveAnUnlinkedChild(t *testing.T) {
	az := &fakeAuthz{touchLineageErr: assert.AnError}
	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(
			parentClass(t, "demo-lead", "demo-coder"),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			request(t, "req4", "demo-parent", "demo-coder"),
		).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()
	r := &Reconciler{Client: c, Scheme: sch, Authz: az, MaxDelegationDepth: 3}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req4"},
	})
	require.Error(t, err, "a failed lineage write must be retried, not swallowed")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1,
		"the child must not exist without its lineage tuples: standing would resolve to nobody")
}

// TestReconcile_RevokedDelegationIsDeniedNoChild is the kill switch's happy
// path: an operator has armed the lever, so an otherwise-valid on-roster
// delegation is refused outright and NO child session is created.
func TestReconcile_RevokedDelegationIsDeniedNoChild(t *testing.T) {
	az := &fakeAuthz{delegationBlocked: true}
	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(
			parentClass(t, "demo-lead", "demo-coder"),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			request(t, "req-revoked", "demo-parent", "demo-coder"),
		).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()
	r := &Reconciler{Client: c, Scheme: sch, Authz: az, MaxDelegationDepth: 3}

	reconcileOnce(t, r, "req-revoked")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-revoked"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, "DelegationRevoked", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "demo-parent")
	assert.Empty(t, got.Status.ChildRef, "a revoked delegation must never get a child created for it")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1, "only the parent session should exist")
	require.Len(t, az.lineageWrites, 0, "a revoked delegation writes no lineage tuple")
}

// TestReconcile_RevocationCheckErrorFailsClosed: a SpiceDB read error on the
// revocation check must NOT be read as a grant. The request is requeued (the
// reconcile returns the error) rather than terminally denied — a transient
// outage delays a delegation, it does not permanently refuse one that was never
// actually revoked — and, critically, no child is spawned in the meantime.
func TestReconcile_RevocationCheckErrorFailsClosed(t *testing.T) {
	az := &fakeAuthz{delegationBlockedErr: assert.AnError}
	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(
			parentClass(t, "demo-lead", "demo-coder"),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			request(t, "req-revoke-err", "demo-parent", "demo-coder"),
		).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()
	r := &Reconciler{Client: c, Scheme: sch, Authz: az, MaxDelegationDepth: 3}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req-revoke-err"},
	})
	require.Error(t, err, "a broken revocation check must fail closed and requeue, not silently allow the spawn")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1, "no child may be spawned while the revocation check is unresolved")
}

func TestReconcile_ParentClassMissingIsDenied(t *testing.T) {
	r, c := newReconciler(t,
		parentSession(t, "demo-parent", "demo-ghost"), // names an AgentClass that was never created
		request(t, "req5", "demo-parent", "demo-coder"),
	)

	reconcileOnce(t, r, "req5")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req5"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, "ParentClassMissing", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "demo-ghost")
	assert.Empty(t, got.Status.ChildRef, "no child may be created when the parent's class is gone")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1, "only the parent session should exist")
}

func TestReconcile_AskModeChildClassIsDenied(t *testing.T) {
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClassWithIdentityMode(t, "demo-coder", v1.IdentityModeAsk),
		parentSession(t, "demo-parent", "demo-lead"),
		request(t, "req6", "demo-parent", "demo-coder"),
	)

	reconcileOnce(t, r, "req6")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req6"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, "ChildIdentityModeUnsupported", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "only once, at delegation",
		"the refusal must state the reason that still holds -- the one-shot monotonicity check -- not headlessness, which task/chat children no longer have")
	assert.Empty(t, got.Status.ChildRef, "an ask-mode child class must never get a child created for it")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 1, "only the parent session should exist")
}

func TestReconcile_CrossNamespaceParentIsDenied(t *testing.T) {
	sr := &v1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "req7"},
		Spec: v1.SubagentRequestSpec{
			Parent: v1.NamespacedRef{Namespace: "other-ns", Name: "demo-parent"},
			Class:  "demo-coder",
			Task:   "do the thing",
		},
	}
	r, c := newReconciler(t, sr) // deliberately no parent session anywhere, to prove this fires before any lookup

	reconcileOnce(t, r, "req7")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req7"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, "ParentCrossNamespace", got.Status.FailureReason)
	assert.Empty(t, got.Status.ChildRef, "a cross-namespace parent must never get a child created for it")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions))
	assert.Empty(t, sessions.Items, "no session was ever looked up, let alone created")
}

// TestReconcile_ChildCreateInvalidNameIsFailedNotHung pins the fix for a
// permanently-invalid child Create: buildChild names the child
// sr.Name+"-child", so a SubagentRequest name within 6 characters of the
// 253-character Kubernetes name limit produces a child name the API server
// rejects as Invalid. The fake client performs no such validation, so the
// rejection is injected via an interceptor -- standing in for what a real
// apiserver does -- to prove the RECONCILER's handling, not the fake's.
// Before this fix, IsInvalid fell through to the generic wrapped-error
// return, controller-runtime backed off and retried forever, and the
// request never resolved at all -- the parent's delegate
// tool call would hang for the full poll timeout. It must instead resolve
// Failed (retryable, not a policy Denied) within this single reconcile.
func TestReconcile_ChildCreateInvalidNameIsFailedNotHung(t *testing.T) {
	// 250 'a's + "-child" (6 chars) = 256, past the 253-character limit.
	longName := strings.Repeat("a", 250)

	sch := newScheme(t)
	base := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(
			parentClass(t, "demo-lead", "demo-coder"),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			request(t, longName, "demo-parent", "demo-coder"),
		).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if s, ok := obj.(*v1.AgentSession); ok && s.Name == longName+"-child" {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: v1.SchemeGroupVersion.Group, Kind: "AgentSession"},
					s.Name,
					field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), s.Name,
						"must be no more than 253 characters")},
				)
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	r := &Reconciler{Client: c, Scheme: sch, Authz: &fakeAuthz{}, MaxDelegationDepth: 3}

	reconcileOnce(t, r, longName)

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: longName}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase,
		"a rejected Create must resolve Failed -- retryable, not a policy Denied")
	assert.Equal(t, "ChildCreateRejected", got.Status.FailureReason)
	assert.Empty(t, got.Status.ChildRef, "no child was actually created")
}

func TestReconcile_ChildSucceededResolvesRequestSucceeded(t *testing.T) {
	sr := requestWithChild(t, "req8", "demo-parent", "demo-coder", "req8-child")
	child := childSession(t, "req8-child", "demo-coder", v1.AgentSessionStatus{
		Phase:  v1.AgentSessionPhaseSucceeded,
		Result: &v1.AgentResult{Summary: "done: built the thing"},
	})
	r, c := newReconciler(t, sr, child)

	reconcileOnce(t, r, "req8")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req8"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseSucceeded, got.Status.Phase)
	assert.Equal(t, "done: built the thing", got.Status.Result, "the child's result must be copied onto the request")
}

// TestReconcile_ChildSucceededTruncatesOverlongResultOnARuneBoundary pins
// truncateResult's two properties together: the bound is a hard ceiling
// (marker included), and the cut never splits a multi-byte rune. The input
// is built to land the naive byte-count cut point exactly one byte INTO a
// 3-byte UTF-8 rune ('世', E4 B8 96) -- padding right up to budget-1 ASCII
// bytes, then a multi-byte rune, so a plain s[:maxCopiedResultLen] slice (or
// any fix that only reserves the marker without also respecting rune
// boundaries) would produce an invalid trailing byte. Making the adversarial
// case explicit is the point: a convenient all-ASCII input would pass
// whether or not the rune-boundary fix was actually applied.
func TestReconcile_ChildSucceededTruncatesOverlongResultOnARuneBoundary(t *testing.T) {
	marker := fmt.Sprintf(truncationMarker, maxCopiedResultLen)
	budget := maxCopiedResultLen - len(marker)
	padLen := budget - 1 // one byte short of the naive cut point
	overlong := strings.Repeat("a", padLen) + "世界" + strings.Repeat("b", 100)
	require.Greater(t, len(overlong), maxCopiedResultLen, "fixture must actually exceed the bound to exercise truncation")

	sr := requestWithChild(t, "req11", "demo-parent", "demo-coder", "req11-child")
	child := childSession(t, "req11-child", "demo-coder", v1.AgentSessionStatus{
		Phase:  v1.AgentSessionPhaseSucceeded,
		Result: &v1.AgentResult{Summary: overlong},
	})
	r, c := newReconciler(t, sr, child)

	reconcileOnce(t, r, "req11")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req11"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseSucceeded, got.Status.Phase)
	assert.LessOrEqual(t, len(got.Status.Result), maxCopiedResultLen,
		"the truncated result, marker included, must never exceed the bound")
	assert.Contains(t, got.Status.Result, "truncated",
		"a clipped result must say so -- a silent cut reads as a complete answer")
	assert.True(t, utf8.ValidString(got.Status.Result),
		"truncation must never split a multi-byte rune, even when the naive byte cut point lands mid-codepoint")
}

func TestReconcile_ChildFailedResolvesRequestFailedNotDenied(t *testing.T) {
	sr := requestWithChild(t, "req9", "demo-parent", "demo-coder", "req9-child")
	child := childSession(t, "req9-child", "demo-coder", v1.AgentSessionStatus{
		Phase:         v1.AgentSessionPhaseFailed,
		FailureReason: "provider timeout",
	})
	r, c := newReconciler(t, sr, child)

	reconcileOnce(t, r, "req9")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req9"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase,
		"a crashed child is retryable and must resolve Failed, never Denied -- a denial binds the delegation closure")
	assert.Contains(t, got.Status.Determination, "provider timeout", "the child's failure reason must be carried forward")
}

func TestReconcile_ChildStillRunningIsANoOp(t *testing.T) {
	sr := requestWithChild(t, "req10", "demo-parent", "demo-coder", "req10-child")
	child := childSession(t, "req10-child", "demo-coder", v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning})
	r, c := newReconciler(t, sr, child)

	reconcileOnce(t, r, "req10")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req10"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"a mid-flight child must never be prematurely resolved")
	assert.False(t, got.IsTerminal())
	assert.Empty(t, got.Status.Result)
}

// TestReconcile_TreeCeiling_AtCeilingDeniesChildCreation pins case 1 of Task
// 7: a root with maxDelegatedAgents=3 and two already-labelled descendants
// (three counting the root) is AT the ceiling, so a third descendant is
// denied before it is ever created -- not created then cleaned up after.
func TestReconcile_TreeCeiling_AtCeilingDeniesChildCreation(t *testing.T) {
	objs := treeCeilingFixture(t, &v1.BudgetConfig{MaxDelegatedAgents: 3}, 2)
	objs = append(objs, request(t, "req-ceiling1", "demo-root", "demo-coder"))
	r, c := newReconciler(t, objs...)

	reconcileOnce(t, r, "req-ceiling1")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-ceiling1"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, "TreeCeilingExceeded", got.Status.FailureReason)
	// Both numbers must be named: the tree's current count (3: root + 2
	// descendants) and the ceiling (3) it would exceed.
	assert.Contains(t, got.Status.Determination, "3 of 3", "denial must name both the current count and the ceiling")

	var sessions v1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 3, "the ceiling must refuse before creating -- no child, only the root + 2 pre-existing descendants")
}

// TestReconcile_TreeCeiling_OneUnderCeilingAllowsChildCreation pins case 2:
// the same tree one ceiling higher (4) is one agent UNDER the ceiling, so the
// child IS created. This is the row that fails on an off-by-one in either
// direction -- and it is why Task 6's doc comment about the root not being
// labelled matters: get the +1 wrong and this case denies incorrectly.
func TestReconcile_TreeCeiling_OneUnderCeilingAllowsChildCreation(t *testing.T) {
	objs := treeCeilingFixture(t, &v1.BudgetConfig{MaxDelegatedAgents: 4}, 2)
	objs = append(objs, request(t, "req-ceiling2", "demo-root", "demo-coder"))
	r, c := newReconciler(t, objs...)

	reconcileOnce(t, r, "req-ceiling2")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-ceiling2"}, &got))
	require.NotNil(t, got.Status.ChildRef, "one under the ceiling must create the child")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)
}

// TestReconcile_TreeCeiling_UnsetTierFallsBackToBuiltInDefaultNotUnlimited
// pins case 3: nothing in any settings tier sets maxDelegatedAgents (it
// resolves to 0), so an unset ceiling must NOT read as "unlimited" -- it
// must fall back to the Reconciler's own DefaultMaxDelegatedAgents and be
// enforced against that, denying the same as an explicit ceiling would.
func TestReconcile_TreeCeiling_UnsetTierFallsBackToBuiltInDefaultNotUnlimited(t *testing.T) {
	objs := treeCeilingFixture(t, nil, 3)
	objs = append(objs, request(t, "req-ceiling3", "demo-root", "demo-coder"))
	r, c := newReconcilerWithDefaultMax(t, 3, objs...)

	reconcileOnce(t, r, "req-ceiling3")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-ceiling3"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase,
		"an unset tier ceiling must fall back to the built-in default, not read as unlimited")
	assert.Equal(t, "TreeCeilingExceeded", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "4 of 3", "denial must name both the current count and the fallback ceiling")
}

// TestReconcile_TreeCeiling_ExplicitTierValueBeatsBuiltInDefault pins case
// 4: the root's own resolved maxDelegatedAgents (10) overrides the
// Reconciler's built-in default (3) -- operators keep control, and the
// built-in bound only fills a gap when nothing set an explicit ceiling.
func TestReconcile_TreeCeiling_ExplicitTierValueBeatsBuiltInDefault(t *testing.T) {
	objs := treeCeilingFixture(t, &v1.BudgetConfig{MaxDelegatedAgents: 10}, 3)
	objs = append(objs, request(t, "req-ceiling4", "demo-root", "demo-coder"))
	r, c := newReconcilerWithDefaultMax(t, 3, objs...)

	reconcileOnce(t, r, "req-ceiling4")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-ceiling4"}, &got))
	require.NotNil(t, got.Status.ChildRef, "an explicit tier ceiling of 10 must beat the built-in default of 3")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)
}

// TestReconcile_TreeCeiling_TransientRootClassLookupErrorRequeues pins the
// round-1 review's Finding 1: the four ceiling-resolution lookups split their
// errors the same way loadParent and the roster List already do -- NotFound
// or Forbidden fail() outright (permanently gone / an RBAC problem a retry
// cannot fix), but everything else -- a transient apiserver hiccup, injected
// here via an interceptor standing in for what a flaky apiserver does --
// must requeue via a wrapped error return, not resolve the request to any
// terminal phase. The request's parent is deliberately ONE HOP below the
// root (a "demo-mid" session, its own class distinct from the root's) so the
// injected failure -- keyed on the ROOT's class name only -- cannot be
// confused with loadParent's own (unrelated, untouched) class lookup earlier
// in the same Reconcile.
func TestReconcile_TreeCeiling_TransientRootClassLookupErrorRequeues(t *testing.T) {
	rootClassObj := parentClass(t, "demo-root-class", "demo-mid-class")
	rootClassObj.Spec.Budget = &v1.BudgetConfig{MaxDelegatedAgents: 3}
	midClassObj := parentClass(t, "demo-mid-class", "demo-coder")
	rootSession := parentSession(t, "demo-root", "demo-root-class")
	midSession := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "demo-mid",
			Labels: map[string]string{v1.LabelDelegationRoot: "demo-root"},
		},
		Spec: v1.AgentSessionSpec{
			Class:  "demo-mid-class",
			Parent: &v1.NamespacedRef{Namespace: "ns", Name: "demo-root"},
			Prompt: v1.PromptSource{Inline: "mid"},
		},
	}

	sch := newScheme(t)
	base := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(rootClassObj, midClassObj, childClass(t, "demo-coder"),
			rootSession, midSession,
			request(t, "req-ceiling-transient", "demo-mid", "demo-coder")).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()

	simulated := errors.New("simulated apiserver hiccup")
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1.AgentClass); ok && key.Name == "demo-root-class" {
				return simulated
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	r := &Reconciler{Client: c, Scheme: sch, Authz: &fakeAuthz{}, MaxDelegationDepth: 3}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req-ceiling-transient"},
	})
	require.Error(t, err, "a transient (non-NotFound, non-Forbidden) lookup failure must requeue, not resolve silently")
	assert.Contains(t, err.Error(), "get root session's AgentClass")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-ceiling-transient"}, &got))
	assert.Empty(t, got.Status.Phase,
		"a retryable transient error must not resolve any terminal phase -- fail() is reserved for NotFound/Forbidden")
}

// TestReconcile_TreeCeiling_ZeroDefaultSkipsCheckEntirely pins Minor 3 (zero
// coverage on the fully-disabled branch): when the ceiling resolves to 0
// from every settings tier AND the Reconciler's own DefaultMaxDelegatedAgents
// is ALSO 0, an operator has explicitly disabled the bound -- the ListClosure
// call and the check itself must be skipped entirely, even with many
// existing descendants already in the tree.
func TestReconcile_TreeCeiling_ZeroDefaultSkipsCheckEntirely(t *testing.T) {
	objs := treeCeilingFixture(t, nil, 5) // no tier ceiling, five pre-existing descendants
	objs = append(objs, request(t, "req-ceiling-nolimit", "demo-root", "demo-coder"))
	r, c := newReconcilerWithDefaultMax(t, 0, objs...) // built-in default also unset

	reconcileOnce(t, r, "req-ceiling-nolimit")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-ceiling-nolimit"}, &got))
	require.NotNil(t, got.Status.ChildRef,
		"an operator who leaves both the tier ceiling and the built-in default unset must get an unbounded tree, not a denial")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)
}

// TestReconcile_TreeCeiling_RootResolvedThroughMultiHopWalkNotParent pins
// Minor 4: every case above sets the requesting parent EQUAL to the tree
// root, so ResolveRoot's walk is always a zero-hop no-op and could never
// catch a wrong-variable bug (reading the ceiling off `parent`'s own class,
// or listing the closure under `parent`'s own name, instead of the resolved
// `root`'s). Here the requesting session is a GRANDCHILD two hops below the
// root, forcing ResolveRoot to actually walk Spec.Parent twice. Root class
// "demo-root-class" carries the explicit ceiling (4); the immediate parent's
// own class ("demo-coder") carries none -- so a `parent`-for-`root` mixup
// would read a different (unset, fallback-only) ceiling and list an empty
// closure under the grandchild's own name, wrongly ALLOWING a child that
// must be denied.
func TestReconcile_TreeCeiling_RootResolvedThroughMultiHopWalkNotParent(t *testing.T) {
	rootClassObj := parentClass(t, "demo-root-class", "demo-coder")
	rootClassObj.Spec.Budget = &v1.BudgetConfig{MaxDelegatedAgents: 4}
	rootSession := parentSession(t, "demo-root", "demo-root-class")
	child1 := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "demo-child1",
			Labels: map[string]string{v1.LabelDelegationRoot: "demo-root"},
		},
		Spec: v1.AgentSessionSpec{
			Class:  "demo-coder",
			Parent: &v1.NamespacedRef{Namespace: "ns", Name: "demo-root"},
			Prompt: v1.PromptSource{Inline: "child1"},
		},
	}
	// grandchild's own class is distinct from "demo-coder" (the class being
	// requested) so the roster check upstream of the ceiling check passes: a
	// class cannot delegate to its own name unless it lists itself, and
	// reusing "demo-coder" here would fail that check before ever reaching
	// the ceiling logic this test targets.
	grandchildClassObj := parentClass(t, "demo-grandchild-class", "demo-coder")
	grandchild := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "demo-grandchild",
			Labels: map[string]string{v1.LabelDelegationRoot: "demo-root"},
		},
		Spec: v1.AgentSessionSpec{
			Class:  "demo-grandchild-class",
			Parent: &v1.NamespacedRef{Namespace: "ns", Name: "demo-child1"},
			Prompt: v1.PromptSource{Inline: "grandchild"},
		},
	}
	// A further pre-existing descendant beyond child1 and grandchild
	// themselves, so the tree's true count (root + child1 + grandchild + this
	// one = 4) sits exactly at the ceiling -- the same boundary shape as case
	// 1, but reached through a real two-hop walk instead of parent == root.
	extra := descendantSession(t, "demo-extra", "demo-root")

	r, c := newReconciler(t,
		rootClassObj, grandchildClassObj, childClass(t, "demo-coder"),
		rootSession, child1, grandchild, extra,
		request(t, "req-ceiling-multihop", "demo-grandchild", "demo-coder"),
	)

	reconcileOnce(t, r, "req-ceiling-multihop")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-ceiling-multihop"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase,
		"the ceiling must be enforced against the RESOLVED ROOT's tree, not the immediate (grandchild) parent's own name or class")
	assert.Equal(t, "TreeCeilingExceeded", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "demo-root",
		"the denial must name the resolved root, not the immediate parent")
}

// TestReconcile_TreeCeiling_LineageCycleFailsTerminalNotRequeue pins the
// round-2 review's finding: v1.ErrLineageCycle must be treated as PERMANENT
// (fail() -> terminal Failed, nil error), not routed into the "everything
// else -> wrapped return -> retry" bucket the round-1 split otherwise sends
// non-NotFound/non-Forbidden errors through. A lineage cycle cannot clear on
// its own -- it takes a human editing a roster -- so retrying it (as the
// round-1 split alone would have) requeues with backoff FOREVER, hanging the
// parent's delegate call to its poll deadline. This is the terminal
// counterpart to TestReconcile_TreeCeiling_TransientRootClassLookupErrorRequeues,
// which proves the opposite outcome for a genuinely transient error.
func TestReconcile_TreeCeiling_LineageCycleFailsTerminalNotRequeue(t *testing.T) {
	// demo-a and demo-b parent each other. Admission DAG-validates the CLASS
	// roster graph, not that a session's OWN Spec.Parent chain is acyclic, so
	// this is a shape a corrupted or hand-edited lineage could take even
	// though the class graph above it (demo-lead -> demo-coder) is perfectly
	// valid -- exactly the gap the round-2 finding is about.
	demoA := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-a"},
		Spec: v1.AgentSessionSpec{
			Class:  "demo-lead",
			Parent: &v1.NamespacedRef{Namespace: "ns", Name: "demo-b"},
			Prompt: v1.PromptSource{Inline: "a"},
		},
	}
	demoB := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-b"},
		Spec: v1.AgentSessionSpec{
			Class:  "demo-lead",
			Parent: &v1.NamespacedRef{Namespace: "ns", Name: "demo-a"},
			Prompt: v1.PromptSource{Inline: "b"},
		},
	}

	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClass(t, "demo-coder"),
		demoA, demoB,
		request(t, "req-ceiling-cycle", "demo-a", "demo-coder"),
	)

	reconcileOnce(t, r, "req-ceiling-cycle")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-ceiling-cycle"}, &got))
	assert.True(t, got.IsTerminal(), "a lineage cycle must reach a terminal phase, not hang in Pending/Running forever")
	assert.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase,
		"a cycle is a data defect, not a policy refusal -- fail(), not deny()")
	assert.Contains(t, got.Status.Determination, "cycle")
	assert.Empty(t, got.Status.ChildRef, "no child may be created while the root is unresolvable")
}

// TestReconcile_ChildCreatedButFinalStatusWriteFailsDoesNotDenyOnRetry pins
// the whole-branch review's Finding 1 (CRITICAL): Reconcile's LAST statement
// is the Status().Update that persists ChildRef (:314-318) -- the child was
// already Created and its lineage tuples already written by the time that
// write runs. If the write itself fails (an apiserver blip, an operator
// rolling update), the retry re-enters the FULL path with ChildRef still
// nil, including the ceiling check -- and before this fix, that check would
// count the child THIS SAME REQUEST already created as one MORE agent,
// denying a delegation whose child is already running. The fixture puts the
// tree at exactly one-under-ceiling so creating precisely one more child
// reaches (not exceeds) it -- the shape that fails if the retry double-counts.
func TestReconcile_ChildCreatedButFinalStatusWriteFailsDoesNotDenyOnRetry(t *testing.T) {
	// root(1) + 1 pre-existing descendant(1) = 2, one under a ceiling of 3:
	// creating one more child reaches the ceiling exactly, so a retry that
	// miscounts the just-created child as an ADDITIONAL agent denies.
	objs := treeCeilingFixture(t, &v1.BudgetConfig{MaxDelegatedAgents: 3}, 1)
	objs = append(objs, request(t, "req-retry", "demo-root", "demo-coder"))

	sch := newScheme(t)
	base := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()

	failedOnce := false
	simulated := errors.New("simulated apiserver blip")
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if !failedOnce && subResourceName == "status" {
				if sr, ok := obj.(*v1.SubagentRequest); ok && sr.Name == "req-retry" {
					failedOnce = true
					return simulated
				}
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	})
	r := &Reconciler{Client: c, Scheme: sch, Authz: &fakeAuthz{}, MaxDelegationDepth: 3}

	// First pass: the child gets Created and its lineage tuples written, but
	// the FINAL status write (the one that persists ChildRef) fails -- the
	// error must be returned so controller-runtime retries, per
	// AGENTS.md's no-silent-errors rule.
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req-retry"},
	})
	require.Error(t, err, "a failed final status write must be retried, not swallowed")
	require.True(t, failedOnce, "the injected failure must actually have fired for this to be a meaningful test")

	var mid v1.SubagentRequest
	require.NoError(t, base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-retry"}, &mid))
	assert.Empty(t, mid.Status.ChildRef, "the status write failed, so ChildRef was never persisted")

	var sessions v1.AgentSessionList
	require.NoError(t, base.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 3, "root + 1 pre-existing descendant + the child THIS PASS already created")

	// Second pass: ChildRef is still nil, so Reconcile re-enters the FULL
	// path -- including the ceiling check -- and must not count the child it
	// already created as one more agent.
	reconcileOnce(t, r, "req-retry")

	var got v1.SubagentRequest
	require.NoError(t, base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "req-retry"}, &got))
	assert.NotEqual(t, v1.SubagentRequestPhaseDenied, got.Status.Phase,
		"a retry must not deny a delegation whose child it already created")
	require.NotNil(t, got.Status.ChildRef, "the second pass must persist ChildRef this time")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)
	assert.Equal(t, "req-retry-child", got.Status.ChildRef.Name)

	require.NoError(t, base.List(context.Background(), &sessions, client.InNamespace("ns")))
	assert.Len(t, sessions.Items, 3,
		"the retry must not create a SECOND child -- Create's AlreadyExists on the deterministic name is a no-op")
}

// treeCeilingFixture builds the shared shape every tree-ceiling test starts
// from: a root AgentClass carrying budget (nil to leave maxDelegatedAgents
// unset), its matching child class, a root AgentSession, and descendantCount
// pre-existing sessions already labelled as members of that root's closure.
// Tests differ only in the budget, the descendant count, and (for the
// fallback cases) the Reconciler's own DefaultMaxDelegatedAgents.
func treeCeilingFixture(t *testing.T, budget *v1.BudgetConfig, descendantCount int) []client.Object {
	t.Helper()
	rootClass := parentClass(t, "demo-root-class", "demo-coder")
	rootClass.Spec.Budget = budget
	objs := []client.Object{
		rootClass,
		childClass(t, "demo-coder"),
		parentSession(t, "demo-root", "demo-root-class"),
	}
	for i := 0; i < descendantCount; i++ {
		objs = append(objs, descendantSession(t, fmt.Sprintf("demo-descendant-%d", i), "demo-root"))
	}
	return objs
}

// descendantSession builds an AgentSession already labelled as a member of
// rootName's delegation closure, standing in for a session buildChild would
// have created on some earlier reconcile -- ListClosure finds it by label
// alone, with no ancestor walk needed.
func descendantSession(t *testing.T, name, rootName string) *v1.AgentSession {
	t.Helper()
	return &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      name,
			Labels:    map[string]string{v1.LabelDelegationRoot: rootName},
		},
		Spec: v1.AgentSessionSpec{Class: "demo-coder", Prompt: v1.PromptSource{Inline: "descendant"}},
	}
}

// newReconcilerWithDefaultMax is newReconciler plus a non-zero
// DefaultMaxDelegatedAgents, for the fallback-ceiling cases; newReconciler
// itself is left alone rather than growing a parameter every other test
// call site would have to pass zero for.
func newReconcilerWithDefaultMax(t *testing.T, defaultMax int, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	sch := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).
		Build()
	return &Reconciler{Client: c, Scheme: sch, Authz: &fakeAuthz{}, MaxDelegationDepth: 3, DefaultMaxDelegatedAgents: defaultMax}, c
}

type lineageWrite struct{ childNS, childName, parentNS, parentName string }

type fakeAuthz struct {
	lineageWrites   []lineageWrite
	touchLineageErr error

	// tagAccess is the parent's standing, keyed by tag id. A tag absent from
	// the map is one the parent CANNOT read — the default, so a test that
	// forgets to grant standing sees a refusal rather than an accidental bind.
	tagAccess         map[string]bool
	tagAccessErr      error
	grantedDataSlots  []authz.DataSlotBinding
	grantDataSlotsErr error
	dataSlotExpiry    time.Time

	// delegationBlocked models the operator-side revocation lever. false (the
	// zero value) is the ordinary case: delegation is permitted unless a test
	// arms the kill switch.
	delegationBlocked    bool
	delegationBlockedErr error
}

func (f *fakeAuthz) CheckDelegationBlocked(_ context.Context, _, _ string) (bool, error) {
	if f.delegationBlockedErr != nil {
		return false, f.delegationBlockedErr
	}
	return f.delegationBlocked, nil
}

func (f *fakeAuthz) SessionHasOnResource(
	_ context.Context, resourceType, resourceID, permission string, _ authz.SessionRef,
) (bool, error) {
	if f.tagAccessErr != nil {
		return false, f.tagAccessErr
	}
	if resourceType != authz.DataSlotResourceType || permission != authz.DataSlotAccessPermission {
		return false, nil
	}
	return f.tagAccess[resourceID], nil
}

func (f *fakeAuthz) GrantDataSlots(
	_ context.Context, _, _ string, bindings []authz.DataSlotBinding, expiresAt time.Time,
) error {
	if f.grantDataSlotsErr != nil {
		return f.grantDataSlotsErr
	}
	f.grantedDataSlots = append(f.grantedDataSlots, bindings...)
	f.dataSlotExpiry = expiresAt
	return nil
}

func (f *fakeAuthz) TouchLineage(_ context.Context, childNS, childName, parentNS, parentName string) error {
	if f.touchLineageErr != nil {
		return f.touchLineageErr
	}
	f.lineageWrites = append(f.lineageWrites, lineageWrite{childNS, childName, parentNS, parentName})
	return nil
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(sch))
	return sch
}

func parentClass(t *testing.T, name string, roster ...string) *v1.AgentClass {
	t.Helper()
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       v1.AgentClassSpec{Subagents: roster, IdentityMode: v1.IdentityModeAgent},
	}
}

func childClass(t *testing.T, name string) *v1.AgentClass {
	t.Helper()
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       v1.AgentClassSpec{IdentityMode: v1.IdentityModeAgent},
	}
}

func childClassWithIdentityMode(t *testing.T, name, mode string) *v1.AgentClass {
	t.Helper()
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       v1.AgentClassSpec{IdentityMode: mode},
	}
}

func parentSession(t *testing.T, name, class string) *v1.AgentSession {
	t.Helper()
	return &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       v1.AgentSessionSpec{Class: class, Prompt: v1.PromptSource{Inline: "root"}},
	}
}

func request(t *testing.T, name, parent, class string) *v1.SubagentRequest {
	t.Helper()
	return &v1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec: v1.SubagentRequestSpec{
			Parent: v1.NamespacedRef{Namespace: "ns", Name: parent},
			Class:  class,
			Task:   "do the thing",
		},
	}
}

// requestWithChild builds a request already past the create step: Running,
// with ChildRef already naming childName, the shape a completion-propagation
// reconcile starts from.
func requestWithChild(t *testing.T, name, parent, class, childName string) *v1.SubagentRequest {
	t.Helper()
	sr := request(t, name, parent, class)
	sr.Status.Phase = v1.SubagentRequestPhaseRunning
	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: "ns", Name: childName}
	return sr
}

// childSession builds the delegated child AgentSession a completion-propagation
// reconcile reads, with the given status seeded directly (fake client honors an
// initial Status even with WithStatusSubresource in play; only later plain
// Update calls are restricted to spec+metadata).
func childSession(t *testing.T, name, class string, status v1.AgentSessionStatus) *v1.AgentSession {
	t.Helper()
	return &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       v1.AgentSessionSpec{Class: class, Prompt: v1.PromptSource{Inline: "do the thing"}},
		Status:     status,
	}
}
