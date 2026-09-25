// pkg/controllers/agentclass/skills_validation_test.go
//
// Part-1 fail-closed-skill behavior: an AgentClass that opts into a skill
// (spec.skills, a canonical name) is parked Valid=False until that skill
// materializes as a Skill/ClusterSkill that is itself Valid=True. These tests
// drive the full Reconcile (fake client) with AllowTestProvider=true so a
// class that passes every OTHER check reaches Valid=True — isolating the skill
// gate as the only thing that can flip it.
package agentclass_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// skillClass builds a minimal, otherwise-valid AgentClass that opts into the
// given canonical skill names (the local Name handle is unused by the skill-
// resolution logic these tests exercise — validateSkills matches on Ref — so
// it's just a synthesized "skillN" placeholder distinct from the ref; it has
// to be a real value now, and not the ref itself, because validateSkillsSpec
// (fix round 1, Task 10) rejects a Name outside [a-z0-9_-]{1,32} and a
// canonical ref is full of "/", ".", and "@").
// provider=test + AllowTestProvider=true keeps the rest of validation
// trivially green so the skill gate is the only variable.
func skillClass(name string, skills ...string) *spiceboxv1alpha1.AgentClass {
	agentSkills := make([]spiceboxv1alpha1.AgentSkill, len(skills))
	for i, ref := range skills {
		agentSkills[i] = spiceboxv1alpha1.AgentSkill{Name: fmt.Sprintf("skill%d", i), Ref: ref}
	}
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test",
				Name:     "scripted",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 1, MaxTokens: 100, MaxDuration: metav1.Duration{Duration: time.Minute},
			},
			Skills: agentSkills,
		},
	}
}

// nsSkill builds a namespace Skill with the given canonical name and Valid
// condition status (+ optional message).
func nsSkill(name, canonical string, valid metav1.ConditionStatus, msg string) *spiceboxv1alpha1.Skill {
	return &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.SkillSpec{CanonicalName: canonical, Description: "d", Body: "b"},
		Status: spiceboxv1alpha1.SkillStatus{
			Conditions: []metav1.Condition{{
				Type:    spiceboxv1alpha1.SkillConditionValid,
				Status:  valid,
				Reason:  "Test",
				Message: msg,
			}},
		},
	}
}

// clusterSkill builds a cluster-scoped ClusterSkill (no namespace) with a
// Valid condition status.
func clusterSkill(name, canonical string, valid metav1.ConditionStatus) *spiceboxv1alpha1.ClusterSkill {
	return &spiceboxv1alpha1.ClusterSkill{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spiceboxv1alpha1.SkillSpec{CanonicalName: canonical, Description: "d", Body: "b"},
		Status: spiceboxv1alpha1.SkillStatus{
			Conditions: []metav1.Condition{{
				Type:   spiceboxv1alpha1.SkillConditionValid,
				Status: valid,
				Reason: "Test",
			}},
		},
	}
}

// reconcileSkillClass builds a fake client over the given objects, reconciles
// the named class, and returns its observed Valid condition.
func reconcileSkillClass(t *testing.T, scheme *runtime.Scheme, className string, objs ...client.Object) *metav1.Condition {
	t.Helper()
	placeholder := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("unused")},
	}
	all := append([]client.Object{placeholder}, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		WithObjects(all...).Build()
	r := &agentclass.Reconciler{Client: c, AllowTestProvider: true}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: className, Namespace: "default"},
	})
	require.NoError(t, err, "Reconcile must not return an error (failures surface on Valid)")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: className, Namespace: "default"}, &got), "Get after Reconcile")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition must be set")
	return cond
}

const skillCanonical = "github.com/someorg/somerepo//skills/skillone@v1.2.0"

func TestAgentClass_Skills_Validation(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	cases := []struct {
		name       string
		objs       []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string // substring; "" = no substring check
	}{
		{
			name:       "opted-in skill with no materialized Skill: Valid=False/AgentClassSkillMissing",
			objs:       nil,
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentClassSkillMissing,
			wantMsg:    "discoveryProblems",
		},
		{
			name:       "materialized namespace Skill Valid=True: skill gate passes, class Valid=True",
			objs:       []client.Object{nsSkill("skillone", skillCanonical, metav1.ConditionTrue, "")},
			wantStatus: metav1.ConditionTrue,
		},
		{
			name:       "materialized namespace Skill Valid=False: Valid=False/AgentClassSkillInvalid",
			objs:       []client.Object{nsSkill("skillone", skillCanonical, metav1.ConditionFalse, "frontmatter name mismatch")},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentClassSkillInvalid,
			wantMsg:    "frontmatter name mismatch",
		},
		{
			name:       "ClusterSkill Valid=True, no namespace Skill: skill gate passes, class Valid=True",
			objs:       []client.Object{clusterSkill("skillone", skillCanonical, metav1.ConditionTrue)},
			wantStatus: metav1.ConditionTrue,
		},
		{
			name: "namespace Skill (Valid=False) shadows a Valid ClusterSkill of the same name: namespace validity decides → Valid=False",
			objs: []client.Object{
				clusterSkill("skillone", skillCanonical, metav1.ConditionTrue),
				nsSkill("skillone", skillCanonical, metav1.ConditionFalse, "ns shadow rejected"),
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentClassSkillInvalid,
			wantMsg:    "ns shadow rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cond := reconcileSkillClass(t, scheme, "ac-skills", append([]client.Object{skillClass("ac-skills", skillCanonical)}, tc.objs...)...)
			assert.Equal(t, tc.wantStatus, cond.Status, "Valid status; reason=%s msg=%q", cond.Reason, cond.Message)
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, cond.Reason, "Valid reason; msg=%q", cond.Message)
			}
			if tc.wantMsg != "" {
				assert.Contains(t, cond.Message, tc.wantMsg, "Valid message")
			}
		})
	}
}

// TestAgentClass_SkillMissing_NamesEveryPlaceThatHoldsTheAnswer pins the
// SkillMissing diagnostic to the places that can actually explain it. When a
// SkillSource fetches successfully but materializes nothing, this message is
// the only thing anywhere that looks wrong — and status.discoveryProblems on
// its own is a dead end for a source that rejected no SKILL.md. The source's
// Ready condition carries that case (Ready=False/NoSkillsDiscovered), so the
// message has to send the reader to both.
func TestAgentClass_SkillMissing_NamesEveryPlaceThatHoldsTheAnswer(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	cond := reconcileSkillClass(t, scheme, "ac-missing", skillClass("ac-missing", skillCanonical))

	require.Equal(t, spiceboxv1alpha1.ReasonAgentClassSkillMissing, cond.Reason)
	assert.Contains(t, cond.Message, "Ready condition",
		"the message must send the reader to the SkillSource's Ready condition")
	assert.Contains(t, cond.Message, "status.discoveryProblems",
		"the message must still name the per-SKILL.md rejection list")
}

// TestAgentClass_Skills_RecoversWhenSkillMaterializes simulates the recover
// path the Skill/ClusterSkill watch drives: a class parked SkillMissing flips
// to Valid=True on the next reconcile once a Valid Skill appears.
func TestAgentClass_Skills_RecoversWhenSkillMaterializes(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	// First reconcile: no Skill → parked Valid=False/AgentClassSkillMissing.
	missing := reconcileSkillClass(t, scheme, "ac-recover", skillClass("ac-recover", skillCanonical))
	require.Equal(t, metav1.ConditionFalse, missing.Status)
	require.Equal(t, spiceboxv1alpha1.ReasonAgentClassSkillMissing, missing.Reason)

	// Second reconcile with the Skill now materialized Valid=True → recovers.
	recovered := reconcileSkillClass(t, scheme, "ac-recover",
		skillClass("ac-recover", skillCanonical),
		nsSkill("skillone", skillCanonical, metav1.ConditionTrue, ""))
	assert.Equal(t, metav1.ConditionTrue, recovered.Status,
		"class must recover to Valid=True once the skill materializes; reason=%s msg=%q",
		recovered.Reason, recovered.Message)
}

// TestAgentClass_Skills_StageSkillsWiredIntoReconcile proves validateSkillsSpec
// (the structural stageSkills/duplicate-name rules; see the pure-function
// table in stage_skills_validation_test.go) is actually reached by Reconcile,
// not just callable in isolation: a duplicate AgentSkill.Name parks the class
// at Valid=False before the skill-resolution gate even runs (no Skill/
// ClusterSkill object is created for either duplicate, so a pass-through
// bug here would otherwise surface as AgentClassSkillMissing instead).
func TestAgentClass_Skills_StageSkillsWiredIntoReconcile(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	ac := skillClass("ac-dup-skill", skillCanonical)
	ac.Spec.Skills = append(ac.Spec.Skills, spiceboxv1alpha1.AgentSkill{
		Name: ac.Spec.Skills[0].Name, // same local Name as the first skill, different Ref
		Ref:  "github.com/someorg/somerepo//skills/skilltwo@v1.0.0",
	})

	cond := reconcileSkillClass(t, scheme, "ac-dup-skill", ac)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonSpecInvalid, cond.Reason)
	assert.Contains(t, cond.Message, "duplicate name")
}
