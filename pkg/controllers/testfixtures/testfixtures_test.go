package testfixtures_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

func TestMustCreate_Success(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
	}
	testfixtures.MustCreate(t, c, cm)
}

func TestEventually_PassesWhenConditionMetEarly(t *testing.T) {
	calls := 0
	testfixtures.Eventually(t, 1*time.Second, func() bool {
		calls++
		return calls >= 2
	})
	assert.GreaterOrEqual(t, calls, 2)
}

func TestHasCondition_StatusAndReason(t *testing.T) {
	conds := []metav1.Condition{{
		Type: "Valid", Status: metav1.ConditionTrue, Reason: "OK",
	}}
	cases := []struct {
		name     string
		typ      string
		status   metav1.ConditionStatus
		reason   string
		expected bool
	}{
		{name: "type+status+reason match: true", typ: "Valid", status: metav1.ConditionTrue, reason: "OK", expected: true},
		{name: "type+status match, empty reason matches any: true", typ: "Valid", status: metav1.ConditionTrue, reason: "", expected: true},
		{name: "wrong status: false", typ: "Valid", status: metav1.ConditionFalse, reason: "", expected: false},
		{name: "wrong type: false", typ: "Other", status: metav1.ConditionTrue, reason: "", expected: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, testfixtures.HasCondition(conds, tc.typ, tc.status, tc.reason))
		})
	}
}

func TestHasConditionTrue_Shorthand(t *testing.T) {
	conds := []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue}}
	assert.True(t, testfixtures.HasConditionTrue(conds, "Valid"))
}

func TestNewScheme_DefaultsAndExtras(t *testing.T) {
	t.Run("defaults register corev1 and v1alpha1", func(t *testing.T) {
		s := testfixtures.NewScheme(t)
		gvk, _, err := s.ObjectKinds(&corev1.ConfigMap{})
		assert.NoError(t, err)
		assert.NotEmpty(t, gvk)
		gvk, _, err = s.ObjectKinds(&spiceboxv1alpha1.AgentClass{})
		assert.NoError(t, err)
		assert.NotEmpty(t, gvk)
	})
	t.Run("extras are appended", func(t *testing.T) {
		s := testfixtures.NewScheme(t, rbacv1.AddToScheme)
		gvk, _, err := s.ObjectKinds(&rbacv1.ClusterRole{})
		assert.NoError(t, err)
		assert.NotEmpty(t, gvk)
	})
}
