// pkg/controllers/agentsession/workshop_hook_test.go
//
// Unit tests for workshopIdentityFor (the read side of the sidecar identity
// seam, spec §13) and ensureWorkshop (the sanction hook, spec §1.1). Uses a
// fake controller-runtime client, mirroring credential_grants_sidecar_test.go's
// style.
package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// workshopSessionOwnerRef returns a controller owner reference to sess, the
// same shape sessionOwnerRef/cosidecar.OwnerRef stamps on every object the
// AgentSession reconciler creates.
func workshopSessionOwnerRef(sess *spiceboxv1alpha1.AgentSession) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         spiceboxv1alpha1.SchemeGroupVersion.String(),
		Kind:               "AgentSession",
		Name:               sess.Name,
		UID:                sess.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func TestWorkshopIdentityFor(t *testing.T) {
	const ns = "default"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: ns, UID: types.UID("sess-uid-abc")},
	}
	identity := &spiceboxv1alpha1.WorkshopSidecarIdentity{
		ServiceAccount: "s1-workshop-sa",
		TokenSecret:    "s1-workshop-token",
	}

	cases := []struct {
		name          string
		workshop      *spiceboxv1alpha1.Workshop
		wantNil       bool
		wantRef       string
		wantNamespace string
		description   string
	}{
		{
			name: "Ready + owned Workshop returns the identity, sidecar ref, and workshop namespace",
			workshop: &spiceboxv1alpha1.Workshop{
				ObjectMeta: metav1.ObjectMeta{
					Name:            spiceboxv1alpha1.WorkshopName(sess.Name),
					Namespace:       ns,
					OwnerReferences: []metav1.OwnerReference{workshopSessionOwnerRef(sess)},
				},
				Spec:   spiceboxv1alpha1.WorkshopSpec{SidecarToolbox: "workshop-tb"},
				Status: spiceboxv1alpha1.WorkshopStatus{Phase: spiceboxv1alpha1.WorkshopPhaseReady, SidecarIdentity: identity, Namespace: "ws-abc123def456"},
			},
			wantNil:       false,
			wantRef:       "workshop-tb",
			wantNamespace: "ws-abc123def456",
		},
		{
			name: "Provisioning phase yields no identity",
			workshop: &spiceboxv1alpha1.Workshop{
				ObjectMeta: metav1.ObjectMeta{
					Name:            spiceboxv1alpha1.WorkshopName(sess.Name),
					Namespace:       ns,
					OwnerReferences: []metav1.OwnerReference{workshopSessionOwnerRef(sess)},
				},
				Spec:   spiceboxv1alpha1.WorkshopSpec{SidecarToolbox: "workshop-tb"},
				Status: spiceboxv1alpha1.WorkshopStatus{Phase: spiceboxv1alpha1.WorkshopPhaseProvisioning, SidecarIdentity: identity},
			},
			wantNil: true,
		},
		{
			name: "owner-ref UID mismatch (a planted CR) yields no identity",
			workshop: &spiceboxv1alpha1.Workshop{
				ObjectMeta: metav1.ObjectMeta{
					Name:      spiceboxv1alpha1.WorkshopName(sess.Name),
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         spiceboxv1alpha1.SchemeGroupVersion.String(),
						Kind:               "AgentSession",
						Name:               sess.Name,
						UID:                types.UID("some-other-uid"),
						Controller:         ptr.To(true),
						BlockOwnerDeletion: ptr.To(true),
					}},
				},
				Spec:   spiceboxv1alpha1.WorkshopSpec{SidecarToolbox: "workshop-tb"},
				Status: spiceboxv1alpha1.WorkshopStatus{Phase: spiceboxv1alpha1.WorkshopPhaseReady, SidecarIdentity: identity},
			},
			wantNil: true,
		},
		{
			name: "no owner ref at all (unowned) yields no identity",
			workshop: &spiceboxv1alpha1.Workshop{
				ObjectMeta: metav1.ObjectMeta{
					Name:      spiceboxv1alpha1.WorkshopName(sess.Name),
					Namespace: ns,
				},
				Spec:   spiceboxv1alpha1.WorkshopSpec{SidecarToolbox: "workshop-tb"},
				Status: spiceboxv1alpha1.WorkshopStatus{Phase: spiceboxv1alpha1.WorkshopPhaseReady, SidecarIdentity: identity},
			},
			wantNil: true,
		},
		{
			name:     "no Workshop at all yields nil, no error",
			workshop: nil,
			wantNil:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t))
			if tc.workshop != nil {
				builder = builder.WithObjects(tc.workshop)
			}
			r := &Reconciler{Client: builder.Build()}

			gotIdentity, gotRef, gotNamespace := r.workshopIdentityFor(context.Background(), sess)

			if tc.wantNil {
				assert.Nil(t, gotIdentity, tc.name)
				assert.Empty(t, gotRef, tc.name)
				assert.Empty(t, gotNamespace, tc.name)
				return
			}
			require.NotNil(t, gotIdentity, tc.name)
			assert.Equal(t, identity, gotIdentity)
			assert.Equal(t, tc.wantRef, gotRef)
			assert.Equal(t, tc.wantNamespace, gotNamespace, "wsNamespace must be Workshop.status.namespace (W)")
		})
	}
}

// --- ensureWorkshop (spec §1.1) ---

// workshopBuilderClass returns an AgentClass whose SidecarToolboxes
// reference refs. Namespace/name are the two halves BuilderClassFor matches
// against — no other class fields matter to the sanction hook.
func workshopBuilderClass(ns, name string, refs ...spiceboxv1alpha1.AgentClassSidecarToolboxRef) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spiceboxv1alpha1.AgentClassSpec{SidecarToolboxes: refs},
	}
}

// workshopClusterSettings builds a ClusterAgentSettings sanctioning exactly
// one (ns, class) pair for sidecar, optionally capped at maxPerStarter (nil
// leaves the default-3 ceiling in force).
func workshopClusterSettings(ns, class, sidecar string, maxPerStarter *int32) *spiceboxv1alpha1.ClusterAgentSettings {
	return &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				BuilderClasses: &[]spiceboxv1alpha1.BuilderClassRef{
					{Namespace: ns, Name: class, SidecarToolbox: sidecar},
				},
				MaxWorkshopsPerStarter: maxPerStarter,
			},
		},
	}
}

// workshopSession builds a minimal AgentSession with a human starter
// annotation (StartedByCanonical's source), or none when starterCanonical
// is "".
func workshopSession(ns, name string, uid types.UID, starterCanonical string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: uid},
	}
	if starterCanonical != "" {
		s.Annotations = map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:" + starterCanonical}
	}
	return s
}

// workshopOwnedBy builds a Workshop CR named for session, controller-owned
// by owner, sanctioned for sidecar. Used to pre-seed a Workshop the test
// then asserts gets deleted (or survives).
func workshopOwnedBy(ns, session, sidecar, starterCanonical string, owner *spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:            spiceboxv1alpha1.WorkshopName(session),
			Namespace:       ns,
			OwnerReferences: sessionOwnerRef(owner),
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:          spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: session},
			StarterCanonical: starterCanonical,
			SidecarToolbox:   sidecar,
			Limits:           defaultWorkshopLimits(),
		},
	}
}

func newWorkshopReconciler(t *testing.T, objs ...client.Object) (*Reconciler, *[]string) {
	t.Helper()
	builder := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.Workshop{}).
		WithObjects(objs...)
	var notices []string
	r := &Reconciler{
		Client: builder.Build(),
		StartRefusedNoticePublish: func(_ context.Context, ns, name, requester, body string) error {
			notices = append(notices, ns+"/"+name+"|"+requester+"|"+body)
			return nil
		},
	}
	return r, &notices
}

func getWorkshop(t *testing.T, r *Reconciler, ns, session string) (*spiceboxv1alpha1.Workshop, error) {
	t.Helper()
	var ws spiceboxv1alpha1.Workshop
	key := types.NamespacedName{Namespace: ns, Name: spiceboxv1alpha1.WorkshopName(session)}
	err := r.Client.Get(context.Background(), key, &ws)
	if err != nil {
		return nil, err
	}
	return &ws, nil
}

func TestEnsureWorkshop_UnsanctionedClassCreatesNoWorkshop(t *testing.T) {
	const ns = "default"

	t.Run("no settings object at all", func(t *testing.T) {
		sess := workshopSession(ns, "s1", types.UID("sess-uid-1a"), "builder-user")
		ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
		r, _ := newWorkshopReconciler(t, sess, ac)

		_, halted, err := r.ensureWorkshop(context.Background(), sess, ac)
		require.NoError(t, err)
		assert.False(t, halted)

		_, gerr := getWorkshop(t, r, ns, "s1")
		assert.True(t, apierrors.IsNotFound(gerr), "no Workshop should exist")
	})

	t.Run("settings object names a different class", func(t *testing.T) {
		sess := workshopSession(ns, "s1", types.UID("sess-uid-1b"), "builder-user")
		ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
		cluster := workshopClusterSettings(ns, "some-other-class", "workshop", nil)
		r, _ := newWorkshopReconciler(t, sess, ac, cluster)

		_, halted, err := r.ensureWorkshop(context.Background(), sess, ac)
		require.NoError(t, err)
		assert.False(t, halted)

		_, gerr := getWorkshop(t, r, ns, "s1")
		assert.True(t, apierrors.IsNotFound(gerr), "no Workshop should exist")
	})
}

// The tenant-cannot-self-sanction property: a namespace-tier AgentSettings
// naming the class is inert. Only the CLUSTER tier's BuilderClasses counts.
func TestEnsureWorkshop_NamespaceTierSanctionIsIgnored(t *testing.T) {
	const ns = "default"
	sess := workshopSession(ns, "s1", types.UID("sess-uid-2"), "builder-user")
	ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
	nsSettings := &spiceboxv1alpha1.AgentSettings{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: spiceboxv1alpha1.AgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				BuilderClasses: &[]spiceboxv1alpha1.BuilderClassRef{
					{Namespace: ns, Name: "builder-class", SidecarToolbox: "workshop"},
				},
			},
		},
	}
	r, _ := newWorkshopReconciler(t, sess, ac, nsSettings)

	_, halted, err := r.ensureWorkshop(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.False(t, halted)

	_, gerr := getWorkshop(t, r, ns, "s1")
	assert.True(t, apierrors.IsNotFound(gerr), "a namespace-tier sanction must not create a Workshop")
}

// Sanctioned at the cluster tier, but the class's own spec never wired in
// the named sidecar: no Workshop is created, and a pre-existing OWNED one
// is torn down — the class dropping its sidecar ref behaves exactly like a
// revoked sanction.
func TestEnsureWorkshop_SanctionedButSidecarNotReferenced_DeletesExisting(t *testing.T) {
	const ns = "default"
	sess := workshopSession(ns, "s1", types.UID("sess-uid-3"), "builder-user")
	ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "other"})
	cluster := workshopClusterSettings(ns, "builder-class", "workshop", nil)
	existing := workshopOwnedBy(ns, "s1", "workshop", "builder-user", sess)
	r, _ := newWorkshopReconciler(t, sess, ac, cluster, existing)

	_, halted, err := r.ensureWorkshop(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.False(t, halted)

	_, gerr := getWorkshop(t, r, ns, "s1")
	assert.True(t, apierrors.IsNotFound(gerr), "an existing owned Workshop must be deleted when the class stops referencing the sanctioned sidecar")
}

// Both facts hold: the class is cluster-sanctioned AND references the named
// sidecar. A Workshop is created with the exact fields the spec promises,
// and a second call is idempotent (no duplicate, no error).
func TestEnsureWorkshop_SanctionedAndReferenced_CreatesOnceWithCorrectFields(t *testing.T) {
	const ns = "default"
	sess := workshopSession(ns, "s1", types.UID("sess-uid-4"), "builder-user")
	ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
	cluster := workshopClusterSettings(ns, "builder-class", "workshop", nil)
	r, _ := newWorkshopReconciler(t, sess, ac, cluster)

	ctx := context.Background()
	_, halted, err := r.ensureWorkshop(ctx, sess, ac)
	require.NoError(t, err)
	assert.False(t, halted, "an in-pod workshop sidecar needs no minted identity, so create does not halt")

	got, gerr := getWorkshop(t, r, ns, "s1")
	require.NoError(t, gerr)
	assert.Equal(t, spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: "s1"}, got.Spec.Session)
	assert.Equal(t, "workshop", got.Spec.SidecarToolbox)
	assert.Equal(t, "builder-user", got.Spec.StarterCanonical)
	assert.Equal(t, defaultWorkshopLimits(), got.Spec.Limits)
	owner := metav1.GetControllerOf(got)
	require.NotNil(t, owner)
	assert.Equal(t, sess.UID, owner.UID)
	assert.Equal(t, "AgentSession", owner.Kind)
	// Second call: idempotent — no error, no duplicate. Mark the Workshop
	// Ready first, so the readiness gate releases and the halt state does not
	// mask a duplicate-create bug.
	ready, gerr2 := getWorkshop(t, r, ns, "s1")
	require.NoError(t, gerr2)
	ready.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, r.Client.Status().Update(ctx, ready))
	_, halted, err = r.ensureWorkshop(ctx, sess, ac)
	require.NoError(t, err)
	assert.False(t, halted, "once Ready, the sanctioned path proceeds")
	var list spiceboxv1alpha1.WorkshopList
	require.NoError(t, r.Client.List(ctx, &list))
	assert.Len(t, list.Items, 1, "the second call must not create a duplicate")
	assert.Len(t, list.Items, 1, "the second call must not create a duplicate")
}

// A revoked sanction — the cluster settings object still exists, but its
// builderClasses list no longer names this class — deletes the existing
// owned Workshop exactly like never having been sanctioned.
func TestEnsureWorkshop_SanctionRevoked_DeletesExisting(t *testing.T) {
	const ns = "default"
	sess := workshopSession(ns, "s1", types.UID("sess-uid-5"), "builder-user")
	ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
	cluster := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		// Limits present, but builderClasses now empty/absent: revoked.
		Spec: spiceboxv1alpha1.SettingsSpec{Limits: &spiceboxv1alpha1.SettingsLimits{}},
	}
	existing := workshopOwnedBy(ns, "s1", "workshop", "builder-user", sess)
	r, _ := newWorkshopReconciler(t, sess, ac, cluster, existing)

	_, halted, err := r.ensureWorkshop(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.False(t, halted)

	_, gerr := getWorkshop(t, r, ns, "s1")
	assert.True(t, apierrors.IsNotFound(gerr), "a revoked sanction must delete the existing owned Workshop")
}

// The per-starter cap: with MaxWorkshopsPerStarter=1 and ONE live workshop
// already held by the same starter (in a different namespace), the
// (cap+1)th session is refused, told in plain words, and no new Workshop is
// created. Control: a workshop held by a DIFFERENT starter does not count.
func TestEnsureWorkshop_PerStarterCapExceeded_RefusesAndTells(t *testing.T) {
	const ns = "default"
	one := int32(1)
	sess := workshopSession(ns, "s2", types.UID("sess-uid-6"), "builder-user")
	ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
	cluster := workshopClusterSettings(ns, "builder-class", "workshop", &one)

	// The starter's one existing live workshop, in ANOTHER namespace.
	other := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.WorkshopName("s1"), Namespace: "other-ns"},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:          spiceboxv1alpha1.NamespacedRef{Namespace: "other-ns", Name: "s1"},
			StarterCanonical: "builder-user",
			SidecarToolbox:   "workshop",
			Limits:           defaultWorkshopLimits(),
		},
	}
	// Control: a workshop with a DIFFERENT starter must not count toward the cap.
	control := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.WorkshopName("s3"), Namespace: "third-ns"},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:          spiceboxv1alpha1.NamespacedRef{Namespace: "third-ns", Name: "s3"},
			StarterCanonical: "someone-else",
			SidecarToolbox:   "workshop",
			Limits:           defaultWorkshopLimits(),
		},
	}
	r, notices := newWorkshopReconciler(t, sess, ac, cluster, other, control)

	_, halted, err := r.ensureWorkshop(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.True(t, halted)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "s2"}, &got))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
	failed := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionWorkshopLimitExceeded, failed.Reason)

	require.Len(t, *notices, 1, "the person is told exactly once")
	assert.Contains(t, (*notices)[0], "build")
	for _, banned := range []string{"namespace", "YAML", "kubectl"} {
		assert.NotContains(t, (*notices)[0], banned, "no internal vocabulary in the notice")
	}

	var list spiceboxv1alpha1.WorkshopList
	require.NoError(t, r.Client.List(context.Background(), &list))
	assert.Len(t, list.Items, 2, "no new Workshop was created; only the two pre-existing ones remain")
}

// A same-named Workshop left over from a PRIOR session — a different UID
// under the same controller kind — must never be silently adopted.
func TestEnsureWorkshop_NameCollisionWithForeignOwner_Errors(t *testing.T) {
	const ns = "default"
	sess := workshopSession(ns, "s1", types.UID("sess-uid-7"), "builder-user")
	ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
	cluster := workshopClusterSettings(ns, "builder-class", "workshop", nil)
	priorOwner := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "s1", UID: types.UID("some-prior-session-uid")},
	}
	leftover := workshopOwnedBy(ns, "s1", "workshop", "", priorOwner)
	r, _ := newWorkshopReconciler(t, sess, ac, cluster, leftover)

	_, halted, err := r.ensureWorkshop(context.Background(), sess, ac)
	require.Error(t, err, "a same-named Workshop owned by a different session must never be adopted")
	assert.True(t, halted)
}

// TestEnsureWorkshop_HaltsUntilWorkshopReady pins the ordering fix for the
// live race on oap-desktop 2026-09-11: ensureWorkshop created the Workshop and
// returned halted=false, so the reconcile went on to build the workshop
// sidecar pod and the runner while the Workshop was still Provisioning — no
// identity yet. The identity-less sidecar (a fail-closed image) crashed, the
// operator marked the session SidecarBootFailed, and the ~1s-later
// rebuild-with-identity came too late. Nothing downstream may be created until
// the Workshop is Ready, so ensureWorkshop must HALT (requeue) while it
// provisions and only release once Ready.
func TestEnsureWorkshop_HaltsUntilWorkshopReady(t *testing.T) {
	const ns = "default"
	sess := workshopSession(ns, "s1", types.UID("sess-uid-ready"), "builder-user")
	ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
	cluster := workshopClusterSettings(ns, "builder-class", "workshop", nil)
	// A separate-pod (isolation: isolated) workshop toolbox is what makes the
	// gate wait: only that mode receives a minted identity. Named "workshop"
	// to match the Workshop.spec.SidecarToolbox the sanction sets.
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "workshop", Namespace: ns},
		Spec:       spiceboxv1alpha1.SidecarToolboxSpec{Isolation: spiceboxv1alpha1.SidecarToolboxIsolationIsolated},
	}
	r, _ := newWorkshopReconciler(t, sess, ac, cluster, tb)
	ctx := context.Background()

	// First pass CREATES the Workshop, which has no status yet → must halt.
	res, halted, err := r.ensureWorkshop(ctx, sess, ac)
	require.NoError(t, err)
	assert.True(t, halted, "a just-created, unprovisioned Workshop must halt the session reconcile")
	assert.Positive(t, res.RequeueAfter, "the halt must requeue so the session resumes once the Workshop is Ready")

	// Still Provisioning → still halt.
	ws, gerr := getWorkshop(t, r, ns, "s1")
	require.NoError(t, gerr)
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseProvisioning
	require.NoError(t, r.Client.Status().Update(ctx, ws))
	_, halted, err = r.ensureWorkshop(ctx, sess, ac)
	require.NoError(t, err)
	assert.True(t, halted, "a Provisioning Workshop must still halt")

	// Ready → release.
	ws, gerr = getWorkshop(t, r, ns, "s1")
	require.NoError(t, gerr)
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, r.Client.Status().Update(ctx, ws))
	_, halted, err = r.ensureWorkshop(ctx, sess, ac)
	require.NoError(t, err)
	assert.False(t, halted, "once the Workshop is Ready the session reconcile proceeds to build the sidecar WITH identity")
}

// The live race on oap-desktop 2026-09-13: ensureWorkshop CREATED the Workshop
// and, in the same reconcile, re-read it through the manager's informer cache,
// which had not seen the write yet. That NotFound was read as "no workshop,
// nothing to wait for", the reconcile went on to build the sidecar pod
// identity-less, the fail-closed image crashed, and the session was marked
// SidecarBootFailed — the gate TestEnsureWorkshop_HaltsUntilWorkshopReady pins
// never ran. A Workshop this reconcile just created cannot be absent: a
// NotFound after Create is cache lag, and the only safe answer is to halt and
// requeue until the cache catches up and the Workshop is Ready.
func TestEnsureWorkshop_HaltsWhenCacheLagsBehindCreate(t *testing.T) {
	const ns = "default"
	sess := workshopSession(ns, "s1", types.UID("sess-uid-lag"), "builder-user")
	ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
	cluster := workshopClusterSettings(ns, "builder-class", "workshop", nil)
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "workshop", Namespace: ns},
		Spec:       spiceboxv1alpha1.SidecarToolboxSpec{Isolation: spiceboxv1alpha1.SidecarToolboxIsolationIsolated},
	}
	// Every Get of a Workshop answers NotFound while lagging is set — the
	// informer cache between a Create and its watch event.
	lagging := true
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.Workshop{}).
		WithObjects(sess, ac, cluster, tb).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isWS := obj.(*spiceboxv1alpha1.Workshop); isWS && lagging {
					return apierrors.NewNotFound(spiceboxv1alpha1.SchemeGroupVersion.WithResource("workshops").GroupResource(), key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &Reconciler{Client: c}
	ctx := context.Background()

	res, halted, err := r.ensureWorkshop(ctx, sess, ac)
	require.NoError(t, err)
	assert.True(t, halted, "a Workshop this reconcile just created cannot be absent: NotFound is cache lag and must halt, never fall through to build an identity-less sidecar")
	assert.Positive(t, res.RequeueAfter, "the halt must requeue so the session resumes once the cache catches up")

	// The cache catches up and the Workshop is Ready → release, exactly as
	// the non-lagging path does.
	lagging = false
	ws, gerr := getWorkshop(t, r, ns, "s1")
	require.NoError(t, gerr)
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, r.Client.Status().Update(ctx, ws))
	_, halted, err = r.ensureWorkshop(ctx, sess, ac)
	require.NoError(t, err)
	assert.False(t, halted, "once the cache sees the Ready Workshop the session reconcile proceeds")
}

// A finished session must never get a workshop — not even a sanctioned one.
// Release deletes the workshop of a session that reached a terminal phase; with
// the failed-pod reap grace disabled a Failed session still reconciles past the
// reap short-circuit and reaches ensureWorkshop, which finds nothing and would
// create one, which release then deletes again, forever. The guard belongs here
// rather than at the caller: this is the only path to a Workshop CR.
func TestEnsureWorkshop_AFinishedSessionIsNeverGivenAWorkshop(t *testing.T) {
	const ns = "default"
	cases := []struct {
		name       string
		phase      string
		wantCreate bool
	}{
		{name: "Failed: no workshop is created, and the release loop cannot restart", phase: spiceboxv1alpha1.AgentSessionPhaseFailed},
		{name: "Succeeded: no workshop is created, the build is over", phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
		{name: "Running: the ordinary path still provisions a workshop", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, wantCreate: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := workshopSession(ns, "s1", types.UID("sess-uid-term-"+tc.phase), "builder-user")
			sess.Status.Phase = tc.phase
			ac := workshopBuilderClass(ns, "builder-class", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ws", Ref: "workshop"})
			cluster := workshopClusterSettings(ns, "builder-class", "workshop", nil)
			r, notices := newWorkshopReconciler(t, sess, ac, cluster)

			_, halted, err := r.ensureWorkshop(context.Background(), sess, ac)
			require.NoError(t, err, "a finished session is not an error; it simply gets nothing")
			assert.False(t, halted, "the rest of the reconcile must still run")

			_, gerr := getWorkshop(t, r, ns, "s1")
			if tc.wantCreate {
				assert.NoError(t, gerr, "a live sanctioned session still gets its workshop")
				return
			}
			assert.True(t, apierrors.IsNotFound(gerr), "a finished session must not be given a workshop")
			assert.Empty(t, *notices, "nothing is said to a person whose session is already over")
		})
	}
}
