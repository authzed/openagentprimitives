package skill

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
		WithStatusSubresource(&v1.Skill{}).
		Build()
}

func reconcileOnce(t *testing.T, c client.Client, name string) {
	t.Helper()
	r := &Reconciler{Client: c}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: name},
	})
	require.NoError(t, err)
}

// skillFixture builds a Skill. A git-authority canonical name needs
// MATERIALIZATION — a controller owner-ref to a SkillSource whose authority
// matches, per pkg/tools/skills/materialize — to pass the provenance check.
// materializedOwnerRef + skillSourceFixture below supply a realistic one.
// spec.Source is set alongside (mirroring what the SkillSource controller
// actually writes) but is deliberately NOT what the check gates on: it is
// user-writable and must not be trusted on its own (that was the bug).
func skillFixture(name, canonical, fmName, desc, body string, source *v1.SkillProvenance) *v1.Skill {
	return &v1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: v1.SkillSpec{
			CanonicalName: canonical,
			Description:   desc,
			Body:          body,
			Frontmatter:   v1.SkillFrontmatter{Name: fmName},
			Source:        source,
		},
	}
}

// gitProvenance is a stand-in SkillSource-materialized provenance, so a
// git-authority canonical name is realistic (spec.Source alone no longer
// grants materialization — see skillFixture's doc).
func gitProvenance() *v1.SkillProvenance {
	return &v1.SkillProvenance{RepoLocator: "github.com/o/r", Subpath: "skills/skillone"}
}

// materializedOwnerRef builds the controller owner-ref shape the SkillSource
// controller stamps on the Skills it materializes (see
// pkg/controllers/skillsource/controller.go upsertSkill).
func materializedOwnerRef(sourceName string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         v1.SchemeGroupVersion.String(),
		Kind:               "SkillSource",
		Name:               sourceName,
		UID:                "uid-src",
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

// skillSourceFixture builds a same-namespace SkillSource whose RepoURL
// normalizes to the authority "github.com/o/r" — the authority gitProvenance's
// canonical names claim.
func skillSourceFixture(name string) *v1.SkillSource {
	return &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: "uid-src"},
		Spec:       v1.SkillSourceSpec{RepoURL: "https://github.com/o/r"},
	}
}

func TestSetPinnedWritesPinRecord(t *testing.T) {
	r := &Reconciler{}
	cases := []struct {
		name      string
		canonical string
		strength  string
		digest    string
		version   string
	}{
		{name: "frozen sha: digest+version are the sha", canonical: "github.com/org/repo//skills/x@deadbee", strength: "frozen", digest: "deadbee", version: "deadbee"},
		{name: "named tag: version only", canonical: "github.com/org/repo//skills/x@v1.2.0", strength: "named", digest: "", version: "v1.2.0"},
		{name: "unpinned: empty digest+version", canonical: "github.com/org/repo//skills/x", strength: "unpinned", digest: "", version: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sk := &v1.Skill{}
			sk.Spec.CanonicalName = tc.canonical
			r.setPinned(sk)
			require.NotNil(t, sk.Status.Pin)
			assert.Equal(t, "skill", sk.Status.Pin.Kind)
			assert.Equal(t, tc.strength, sk.Status.Pin.Strength)
			assert.Equal(t, tc.digest, sk.Status.Pin.Digest)
			assert.Equal(t, tc.version, sk.Status.Pin.Version)
			assert.NotNil(t, sk.Status.Pin.ObservedAt)
		})
	}
}

func TestSetPinnedPreservesObservedAtWhenUnchanged(t *testing.T) {
	r := &Reconciler{}
	sk := &v1.Skill{}
	sk.Spec.CanonicalName = "github.com/org/repo//skills/x@deadbee"
	r.setPinned(sk)
	first := sk.Status.Pin.ObservedAt
	require.NotNil(t, first)
	r.setPinned(sk)
	assert.Equal(t, first, sk.Status.Pin.ObservedAt, "re-reconcile of an identical pin must not churn ObservedAt")
}

func TestSetPinnedMalformedNameClearsPin(t *testing.T) {
	r := &Reconciler{}
	sk := &v1.Skill{}
	// Establish a non-nil pin via a valid canonical name first.
	sk.Spec.CanonicalName = "github.com/org/repo//skills/x@deadbee"
	r.setPinned(sk)
	require.NotNil(t, sk.Status.Pin, "precondition: pin set by valid name")
	// Now supply an unparseable name; the pin must be cleared.
	sk.Spec.CanonicalName = "not-a-canonical-name"
	r.setPinned(sk)
	assert.Nil(t, sk.Status.Pin)
}

func TestReconcile(t *testing.T) {
	cases := []struct {
		name            string
		skill           *v1.Skill
		extraObjs       []client.Object
		wantValid       metav1.ConditionStatus
		wantPinned      metav1.ConditionStatus
		wantPinReason   string
		wantPinStrength string
	}{
		{
			// LEGIT / regression guard: this is exactly what the SkillSource
			// controller produces — a git-authority name, a controller owner-ref to
			// the SkillSource, and that SkillSource's RepoURL normalizing to the
			// same authority. It MUST stay Valid=True after gating provenance on the
			// owner-ref instead of spec.Source.
			name: "frozen valid skill, MATERIALIZED via owner-ref: Valid=True, Pinned=True/Frozen",
			skill: func() *v1.Skill {
				sk := skillFixture("a", "github.com/o/r//skills/skillone@a1b2c3d", "skillone", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("src")
				return sk
			}(),
			extraObjs: []client.Object{skillSourceFixture("src")},
			wantValid: metav1.ConditionTrue, wantPinned: metav1.ConditionTrue,
			wantPinReason: v1.ReasonSkillPinFrozen, wantPinStrength: "frozen",
		},
		{
			name: "unpinned valid skill, MATERIALIZED via owner-ref: Valid=True, Pinned=False/UnpinnedRolling",
			skill: func() *v1.Skill {
				sk := skillFixture("b", "github.com/o/r//skills/skillone", "skillone", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("src")
				return sk
			}(),
			extraObjs: []client.Object{skillSourceFixture("src")},
			wantValid: metav1.ConditionTrue, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
		{
			name: "invalid spec, unpinned: Valid=False, Pinned=False",
			skill: func() *v1.Skill {
				sk := skillFixture("c", "github.com/o/r//skills/skillone", "MISMATCH", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("src")
				return sk
			}(),
			extraObjs: []client.Object{skillSourceFixture("src")},
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
			// Owner-ref present but the SkillSource it names doesn't exist.
			name: "owner-ref to non-existent SkillSource: Valid=False",
			skill: func() *v1.Skill {
				sk := skillFixture("g", "github.com/o/r//skills/skillone", "skillone", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("does-not-exist")
				return sk
			}(),
			wantValid: metav1.ConditionFalse, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
		{
			// Owner-ref present, the SkillSource exists, but its authority is a
			// DIFFERENT org than the canonical name claims.
			name: "owner-ref to real SkillSource with mismatched authority: Valid=False",
			skill: func() *v1.Skill {
				sk := skillFixture("h", "github.com/trusted-org/skills//evil", "evil", "Use for X.", "Body.", gitProvenance())
				sk.OwnerReferences = materializedOwnerRef("src")
				return sk
			}(),
			extraObjs: []client.Object{skillSourceFixture("src")}, // authority "github.com/o/r", not "github.com/trusted-org/skills"
			wantValid: metav1.ConditionFalse, wantPinned: metav1.ConditionFalse,
			wantPinReason: v1.ReasonSkillUnpinnedRolling, wantPinStrength: "unpinned",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, append([]client.Object{tc.skill}, tc.extraObjs...)...)
			reconcileOnce(t, c, tc.skill.Name)

			var got v1.Skill
			require.NoError(t, c.Get(context.Background(),
				types.NamespacedName{Namespace: "ns", Name: tc.skill.Name}, &got))

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
