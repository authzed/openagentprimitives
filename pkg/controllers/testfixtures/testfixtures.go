// Package testfixtures collects test helpers shared across controller
// test packages. Six controllers' _test.go files each copied a near-
// identical mustCreate / eventually / hasTrueCondition trio (with
// 50/100/200 ms sleep deltas); centralizing here means a timing-flake
// fix is one edit instead of six.
package testfixtures

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// PollInterval is the polling cadence for Eventually. Picked to match
// what the existing per-package helpers used most often (50ms).
const PollInterval = 50 * time.Millisecond

// NewScheme builds a runtime.Scheme pre-registered with corev1 and the
// agentprimitives v1alpha1 group — the two scheme families every
// controller test in this repo needs. Extras (rbac, batch, etc.) can
// be appended for tests that touch additional APIs:
//
//	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)
//
// Registering an unused scheme is harmless, so the default fits even
// tests that only touch one of the two.
func NewScheme(t *testing.T, extras ...func(*runtime.Scheme) error) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	registrars := append([]func(*runtime.Scheme) error{
		corev1.AddToScheme,
		spiceboxv1alpha1.AddToScheme,
	}, extras...)
	for _, fn := range registrars {
		require.NoError(t, fn(s), "AddToScheme")
	}
	return s
}

// MustCreate t.Fatals on error. Replaces the per-package mustCreate
// copies (agentidentity, spiceboxclass, spiceboxsession,
// spiceboxtoolspec).
func MustCreate(t *testing.T, c client.Client, o client.Object) {
	t.Helper()
	if err := c.Create(context.Background(), o); err != nil {
		t.Fatalf("create %T %s: %v", o, o.GetName(), err)
	}
}

// Eventually polls fn until it returns true or the timeout elapses,
// failing the test if the timeout fires. Sleeps PollInterval between
// attempts.
func Eventually(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(PollInterval)
	}
	t.Fatalf("condition never became true within %v", timeout)
}

// HasCondition reports whether the named condition exists with the
// given status; an empty wantReason matches any reason. Replaces the
// per-type hasCondition / hasTrueCondition copies. Pass the
// CR-specific Status.Conditions slice — the helper is type-agnostic.
func HasCondition(conds []metav1.Condition, condType string, want metav1.ConditionStatus, wantReason string) bool {
	c := meta.FindStatusCondition(conds, condType)
	if c == nil {
		return false
	}
	return c.Status == want && (wantReason == "" || c.Reason == wantReason)
}

// HasConditionTrue is shorthand for HasCondition with status=True.
func HasConditionTrue(conds []metav1.Condition, condType string) bool {
	return HasCondition(conds, condType, metav1.ConditionTrue, "")
}
