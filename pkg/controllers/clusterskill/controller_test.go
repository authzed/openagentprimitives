package clusterskill

import (
	"context"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// pinStrengthOf reads the recorded pin strength, tolerating the nil PinRecord
// a skill whose canonical name did not parse is left with.
func pinStrengthOf(pin *v1.PinRecord) string {
	if pin == nil {
		return ""
	}
	return pin.Strength
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&v1.ClusterSkill{}).
		Build()
}

func reconcileOnce(t *testing.T, c client.Client, name string) {
	t.Helper()
	r := &Reconciler{Client: c}
	// ClusterSkill is cluster-scoped: no namespace on the request.
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	require.NoError(t, err)
}

// skillFixture builds a ClusterSkill. A git-authority canonical name needs
// MATERIALIZATION — a controller owner-ref to a ClusterSkillSource whose
// authority matches, per pkg/tools/skills/materialize — to pass the provenance
// check. materializedOwnerRef + clusterSkillSourceFixture below supply a
// realistic one. spec.Source is set alongside (mirroring what the
// ClusterSkillSource controller actually writes) but is deliberately NOT what
// the check gates on: it is user-writable and must not be trusted on its own
// (that was the bug).
func skillFixture(name, canonical, fmName, desc, body string, source *v1.SkillProvenance) *v1.ClusterSkill {
	return &v1.ClusterSkill{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.SkillSpec{
			CanonicalName: canonical,
			Description:   desc,
			Body:          body,
			Frontmatter:   v1.SkillFrontmatter{Name: fmName},
			Source:        source,
		},
	}
}

// gitProvenance is a stand-in ClusterSkillSource-materialized provenance, so a
// git-authority canonical name is realistic (spec.Source alone no longer
// grants materialization — see skillFixture's doc).
func gitProvenance() *v1.SkillProvenance {
	return &v1.SkillProvenance{RepoLocator: "github.com/o/r", Subpath: "skills/skillone"}
}

// materializedOwnerRef builds the controller owner-ref shape the
// ClusterSkillSource controller stamps on the ClusterSkills it materializes
// (see pkg/controllers/clusterskillsource/controller.go upsertClusterSkill).
func materializedOwnerRef(sourceName string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         v1.SchemeGroupVersion.String(),
		Kind:               "ClusterSkillSource",
		Name:               sourceName,
		UID:                "uid-src",
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

// clusterSkillSourceFixture builds a ClusterSkillSource whose RepoURL
// normalizes to the authority "github.com/o/r" — the authority
// gitProvenance's canonical names claim.
func clusterSkillSourceFixture(name string) *v1.ClusterSkillSource {
	return &v1.ClusterSkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: "uid-src"},
		Spec:       v1.ClusterSkillSourceSpec{RepoURL: "https://github.com/o/r"},
	}
}

func TestSetPinnedWritesPinRecord(t *testing.T) {
	r := &Reconciler{}
	sk := &v1.ClusterSkill{}
	sk.Spec.CanonicalName = "github.com/org/repo//skills/x@deadbee"
	r.setPinned(sk)
	require.NotNil(t, sk.Status.Pin)
	assert.Equal(t, "skill", sk.Status.Pin.Kind)
	assert.Equal(t, "frozen", sk.Status.Pin.Strength)
	assert.Equal(t, "deadbee", sk.Status.Pin.Digest)
	assert.NotNil(t, sk.Status.Pin.ObservedAt)
}

func TestReconcile(t *testing.T) {
	cases := []struct {
		name            string
		skill           *v1.ClusterSkill
		extraObjs       []client.Object
		wantValid       metav1.ConditionStatus
		wantPinned      metav1.ConditionStatus
		wantPinReason   string
		wantPinStrength string
	}{
		{
			// LEGIT / regression guard: this is exactly what the ClusterSkillSource
			// controller produces — a git-authority name, a controller owner-ref to
			// the ClusterSkillSource, and that source's RepoURL normalizing to the
			// same authority. It MUST stay Valid=True after gating provenance on the
			// owner-ref instead of spec.Source.
			name: "frozen valid skill, MATERIALIZED via owner-ref: Valid=True, Pinned=True/Frozen",
			skill: func() *v1.ClusterSkill {
				sk := skillFixture("a", "github.com/o/r//skills/skillone@a1b2c3d", "skillone", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("src")
				return sk
			}(),
			extraObjs: []client.Object{clusterSkillSourceFixture("src")},
			wantValid: metav1.ConditionTrue, wantPinned: metav1.ConditionTrue,
			wantPinReason: v1.ReasonSkillPinFrozen, wantPinStrength: "frozen",
		},
		{
			name: "unpinned valid skill, MATERIALIZED via owner-ref: Valid=True, Pinned=False/UnpinnedRolling",
			skill: func() *v1.ClusterSkill {
				sk := skillFixture("b", "github.com/o/r//skills/skillone", "skillone", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("src")
				return sk
			}(),
			extraObjs: []client.Object{clusterSkillSourceFixture("src")},
			wantValid: metav1.ConditionTrue, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
		{
			name: "invalid spec, unpinned: Valid=False, Pinned=False",
			skill: func() *v1.ClusterSkill {
				sk := skillFixture("c", "github.com/o/r//skills/skillone", "MISMATCH", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("src")
				return sk
			}(),
			extraObjs: []client.Object{clusterSkillSourceFixture("src")},
			wantValid: metav1.ConditionFalse, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
		{
			name:      "hand-authored skill, local name: Valid=True, Pinned=False/UnpinnedRolling",
			skill:     skillFixture("d", "local//skillone", "skillone", "Use for X.", "Body.", nil),
			wantValid: metav1.ConditionTrue, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
		{
			name:      "provenance spoof, git name without Source and without owner-ref: Valid=False",
			skill:     skillFixture("e", "github.com/o/r//skills/skillone", "skillone", "Use for X.", "Body.", nil),
			wantValid: metav1.ConditionFalse, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
		{
			// THE BYPASS, at the durable backstop: fabricated spec.Source AND a
			// trusted git-authority canonical name, but NO owner-ref at all. Before
			// gating on the owner-ref this was Valid=True (spec.Source != nil was
			// enough); it must now be denied.
			name:      "THE BYPASS: fabricated Source, git name, no owner-ref: Valid=False",
			skill:     skillFixture("f", "github.com/o/r//skills/skillone", "skillone", "Use for X.", "Body.", gitProvenance()),
			wantValid: metav1.ConditionFalse, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
		{
			// Owner-ref present but the ClusterSkillSource it names doesn't exist.
			name: "owner-ref to non-existent ClusterSkillSource: Valid=False",
			skill: func() *v1.ClusterSkill {
				sk := skillFixture("g", "github.com/o/r//skills/skillone", "skillone", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("does-not-exist")
				return sk
			}(),
			wantValid: metav1.ConditionFalse, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
		{
			// Owner-ref present, the ClusterSkillSource exists, but its authority is
			// a DIFFERENT org than the canonical name claims.
			name: "owner-ref to real ClusterSkillSource with mismatched authority: Valid=False",
			skill: func() *v1.ClusterSkill {
				sk := skillFixture("h", "github.com/trusted-org/skills//evil", "evil", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("src")
				return sk
			}(),
			extraObjs: []client.Object{clusterSkillSourceFixture("src")}, // authority "github.com/o/r", not "github.com/trusted-org/skills"
			wantValid: metav1.ConditionFalse, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, append([]client.Object{tc.skill}, tc.extraObjs...)...)
			reconcileOnce(t, c, tc.skill.Name)

			var got v1.ClusterSkill
			require.NoError(t, c.Get(context.Background(),
				types.NamespacedName{Name: tc.skill.Name}, &got))

			valid := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillConditionValid)
			require.NotNil(t, valid, "Valid condition must be set")
			assert.Equal(t, tc.wantValid, valid.Status)

			pinned := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillConditionPinned)
			require.NotNil(t, pinned, "Pinned condition must be set")
			assert.Equal(t, tc.wantPinned, pinned.Status)
			if tc.wantPinReason != "" {
				assert.Equal(t, tc.wantPinReason, pinned.Reason)
			}
			assert.Equal(t, tc.wantPinStrength, pinStrengthOf(got.Status.Pin))
		})
	}
}
