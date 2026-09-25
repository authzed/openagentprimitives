package conditions_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// genObj is a tiny stand-in for a CR that exposes a Generation.
type genObj struct{ gen int64 }

func (g *genObj) GetGeneration() int64 { return g.gen }

func TestSet_FillsObservedGeneration(t *testing.T) {
	obj := &genObj{gen: 7}
	var conds []metav1.Condition
	conditions.Set(obj, &conds, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK",
	})
	require.Len(t, conds, 1, "1 condition")
	assert.Equal(t, int64(7), conds[0].ObservedGeneration, "ObservedGeneration")
}

func TestSetTrue_Shape(t *testing.T) {
	obj := &genObj{gen: 3}
	var conds []metav1.Condition
	conditions.SetTrue(obj, &conds, "Ready", "AllGood")
	require.Len(t, conds, 1, "1 condition")
	c := conds[0]
	assert.Equal(t, "Ready", c.Type, "Type")
	assert.Equal(t, metav1.ConditionTrue, c.Status, "Status")
	assert.Equal(t, "AllGood", c.Reason, "Reason")
	assert.Equal(t, int64(3), c.ObservedGeneration, "ObservedGeneration")
	assert.Empty(t, c.Message, "Message")
}

func TestSetFalse_Shape(t *testing.T) {
	obj := &genObj{gen: 4}
	var conds []metav1.Condition
	conditions.SetFalse(obj, &conds, "Valid", "BadInput", "field foo is wrong")
	require.Len(t, conds, 1, "1 condition")
	c := conds[0]
	assert.Equal(t, "Valid", c.Type, "Type")
	assert.Equal(t, metav1.ConditionFalse, c.Status, "Status")
	assert.Equal(t, "BadInput", c.Reason, "Reason")
	assert.Equal(t, "field foo is wrong", c.Message, "Message")
	assert.Equal(t, int64(4), c.ObservedGeneration, "ObservedGeneration")
}

func TestFind_NilForUnknown(t *testing.T) {
	conds := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	assert.Nil(t, conditions.Find(conds, "Other"), "unknown type → nil")
	assert.NotNil(t, conditions.Find(conds, "Ready"), "known type → non-nil")
}

func TestIsTrue(t *testing.T) {
	cases := []struct {
		name  string
		conds []metav1.Condition
		t     string
		want  bool
	}{
		{"missing condition → false", []metav1.Condition{}, "Ready", false},
		{"Status=True → true", []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}, "Ready", true},
		{"Status=False → false", []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Nope"}}, "Ready", false},
		{"Status=Unknown → false", []metav1.Condition{{Type: "Ready", Status: metav1.ConditionUnknown, Reason: "?"}}, "Ready", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, conditions.IsTrue(tc.conds, tc.t), "IsTrue")
		})
	}
}

func TestSet_Idempotent_KeepsTransitionTimestamp(t *testing.T) {
	obj := &genObj{gen: 1}
	var conds []metav1.Condition
	conditions.SetTrue(obj, &conds, "Ready", "OK")
	require.Len(t, conds, 1, "1 condition after first set")
	first := conds[0].LastTransitionTime
	require.False(t, first.IsZero(), "first transition time non-zero")

	// Sleep a hair so that if SetStatusCondition incorrectly bumped the
	// transition timestamp on an equal-state set we'd see a different value.
	time.Sleep(2 * time.Millisecond)

	conditions.SetTrue(obj, &conds, "Ready", "OK")
	require.Len(t, conds, 1, "1 condition after second equal set")
	assert.True(t, conds[0].LastTransitionTime.Equal(&first),
		"LastTransitionTime must NOT bump on equal-state set: was %v now %v",
		first, conds[0].LastTransitionTime)
}
