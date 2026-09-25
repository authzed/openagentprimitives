package projectors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestSkillsProjector(t *testing.T) {
	nsSkill := &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "pdf", Namespace: "ns1"},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: "github.com/o/r//skills/pdf@v1",
			Bundle:        &spiceboxv1alpha1.SkillBundleRef{Digest: "sha256:x", CacheKey: "k"},
		},
		Status: spiceboxv1alpha1.SkillStatus{
			Pin:        &spiceboxv1alpha1.PinRecord{Kind: "skill", Strength: "frozen"},
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.SkillConditionValid, metav1.ConditionTrue, "Valid")},
		},
	}
	clusterSkill := &spiceboxv1alpha1.ClusterSkill{
		ObjectMeta: metav1.ObjectMeta{Name: "global"},
		Spec:       spiceboxv1alpha1.SkillSpec{CanonicalName: "github.com/o/r//skills/global@main"},
		// No conditions → Unknown; no bundle → instruction-only.
	}

	c := newClient(t, nsSkill, clusterSkill)
	rows, err := skillsProjector{}.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	s := rowByName(t, rows, "pdf", "skill")
	assert.Equal(t, "namespaced", s.Scope)
	assert.Equal(t, "Valid", s.Status)
	assert.Equal(t, "executable", badgeVal(s, "delivery"))
	assert.Equal(t, "frozen", badgeVal(s, "pin"))
	assert.Equal(t, "kubectl edit skill pdf -n ns1", s.ManageCmd)

	cs := rowByName(t, rows, "global", "clusterskill")
	assert.Equal(t, "cluster", cs.Scope)
	assert.Equal(t, "Unknown", cs.Status)
	assert.Equal(t, "instruction", badgeVal(cs, "delivery"))
	assert.Equal(t, "kubectl edit clusterskill global", cs.ManageCmd)
}
