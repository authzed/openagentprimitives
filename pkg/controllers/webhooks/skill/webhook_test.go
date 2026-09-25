package skill

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func newWebhook(t *testing.T, extraObjs ...client.Object) *Webhook {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(extraObjs...).Build()
	return NewSkillWebhook(c, admission.NewDecoder(s))
}

func req(t *testing.T, skill *v1.Skill) admission.Request {
	t.Helper()
	raw, err := json.Marshal(skill)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Object: runtime.RawExtension{Raw: raw},
	}}
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

func TestHandle(t *testing.T) {
	// A real SkillSource whose RepoURL normalizes to "github.com/o/r" — the
	// authority a legitimately-materialized Skill's owner-ref must resolve to.
	realSource := &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "ns", UID: "uid-src"},
		Spec:       v1.SkillSourceSpec{RepoURL: "https://github.com/o/r"},
	}
	// A real SkillSource for a DIFFERENT org, used to prove owner-ref existence
	// alone isn't enough — the authority must actually match.
	otherOrgSource := &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "other-src", Namespace: "ns", UID: "uid-other-src"},
		Spec:       v1.SkillSourceSpec{RepoURL: "https://github.com/other-org/repo"},
	}

	// LEGIT: a git-authority canonical name backed by a real controller
	// owner-ref to a SkillSource whose authority matches — mimics exactly what
	// the SkillSource controller produces.
	materialized := &v1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", OwnerReferences: materializedOwnerRef("src")},
		Spec: v1.SkillSpec{
			CanonicalName: "github.com/o/r//skills/skillone@a1b2c3d",
			Description:   "Use for X.", Body: "Body.",
			Frontmatter: v1.SkillFrontmatter{Name: "skillone"},
			Source:      &v1.SkillProvenance{RepoLocator: "github.com/o/r", Subpath: "skills/skillone"},
		},
	}
	invalid := materialized.DeepCopy()
	invalid.Spec.Frontmatter.Name = "MISMATCH"

	// Hand-authored skill (no Source, no owner-ref) using the reserved local//
	// authority: provenance check passes without needing any owner.
	handAuthored := materialized.DeepCopy()
	handAuthored.Spec.Source = nil
	handAuthored.Spec.CanonicalName = "local//skillone"
	handAuthored.OwnerReferences = nil

	// THE BYPASS: a tenant fabricates spec.Source AND claims a trusted
	// git-authority name, but carries NO owner-ref at all. Pre-fix, this was
	// admitted because the check only looked at spec.Source != nil.
	bypassNoOwnerRef := materialized.DeepCopy()
	bypassNoOwnerRef.OwnerReferences = nil

	// Owner-ref present but pointing at a SkillSource that doesn't exist.
	danglingOwnerRef := materialized.DeepCopy()
	danglingOwnerRef.OwnerReferences = materializedOwnerRef("does-not-exist")

	// Owner-ref present and the SkillSource exists, but its authority is a
	// DIFFERENT org than the one the canonical name claims.
	mismatchedAuthority := materialized.DeepCopy()
	mismatchedAuthority.OwnerReferences = materializedOwnerRef("other-src")

	cases := []struct {
		name        string
		skill       *v1.Skill
		wantAllowed bool
	}{
		{name: "valid materialized skill: allowed", skill: materialized, wantAllowed: true},
		{name: "name mismatch: denied", skill: invalid, wantAllowed: false},
		{name: "hand-authored local name: allowed", skill: handAuthored, wantAllowed: true},
		{name: "THE BYPASS: fabricated Source, git name, no owner-ref: denied", skill: bypassNoOwnerRef, wantAllowed: false},
		{name: "owner-ref to non-existent SkillSource: denied", skill: danglingOwnerRef, wantAllowed: false},
		{name: "owner-ref to real SkillSource with mismatched authority: denied", skill: mismatchedAuthority, wantAllowed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWebhook(t, realSource, otherOrgSource)
			resp := w.Handle(context.Background(), req(t, tc.skill))
			assert.Equal(t, tc.wantAllowed, resp.Allowed)
		})
	}
}

func TestHandleBadPayload(t *testing.T) {
	resp := newWebhook(t).Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: []byte("not json")}},
	})
	assert.False(t, resp.Allowed)
	require.NotNil(t, resp.Result)
	assert.Equal(t, int32(http.StatusBadRequest), resp.Result.Code)
}
